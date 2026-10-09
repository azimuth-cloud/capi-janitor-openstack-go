package openstack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/identity"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/loadbalancer"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/network"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/volume"
)

// PurgeOptions holds parameters for cleaning up OpenStack resources
// associated with a deleted Cluster API cluster.
type PurgeOptions struct {
	// CloudsYAML is the decoded content of the clouds.yaml credential file.
	CloudsYAML string
	// CloudName is the entry name within clouds.yaml to use.
	CloudName string
	// CACert is an optional PEM-encoded CA certificate for TLS verification.
	CACert string
	// ClusterName is the CAPI cluster name used to identify owned resources.
	ClusterName string
	// DeleteVolumes controls whether Cinder volumes and snapshots are deleted.
	DeleteVolumes bool
}

// PurgeResources removes the OpenStack resources created by OCCM and CSI for
// the given cluster. Application credential cleanup is a separate phase.
func PurgeResources(ctx context.Context, options PurgeOptions) error {
	session, err := NewCleanupSession(ctx, options)
	if err != nil {
		return err
	}
	return session.Cleanup(ctx)
}

// CredentialConfig contains cloud settings without secret values.
type CredentialConfig = openstackclient.CredentialConfig

// CredentialBinding identifies the authority, scope and credential used by
// an authenticated cleanup session. UserID and ProjectID come from its token.
type CredentialBinding struct {
	Authority    string `json:"authority"`
	ProjectID    string `json:"projectID"`
	UserID       string `json:"userID"`
	Region       string `json:"region"`
	Interface    string `json:"interface"`
	CloudName    string `json:"cloudName"`
	CredentialID string `json:"credentialID"`
}

// ValidateCredentialBinding checks a stored binding without contacting OpenStack.
func ValidateCredentialBinding(binding CredentialBinding) error {
	authority, err := openstackclient.NormalizeAuthority(binding.Authority)
	if err != nil {
		return fmt.Errorf("invalid credential binding authority: %w", err)
	}
	if authority != binding.Authority {
		return errors.New("credential binding authority is not canonical")
	}
	if binding.Interface != "public" && binding.Interface != "internal" && binding.Interface != "admin" {
		return errors.New("credential binding interface is unsupported")
	}
	for _, field := range []struct{ name, value string }{
		{"project ID", binding.ProjectID}, {"user ID", binding.UserID},
		{"cloud name", binding.CloudName}, {"credential ID", binding.CredentialID},
	} {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("credential binding %s is empty or has surrounding whitespace", field.name)
		}
	}
	if strings.TrimSpace(binding.Region) != binding.Region {
		return errors.New("credential binding region has surrounding whitespace")
	}
	return nil
}

// CleanupSession uses the same authenticated client for resource inventory
// and application credential deletion.
type CleanupSession interface {
	Cleanup(context.Context) error
	Binding() CredentialBinding
	DeleteApplicationCredential(context.Context, string) error
}

type cleanupSession struct {
	client  *openstackclient.Client
	request cleanup.Request
}

// DescribeCredential reads the selected cloud settings without authenticating.
func DescribeCredential(options PurgeOptions) (CredentialConfig, error) {
	return openstackclient.ParseCredentialConfig(openstackclient.Options{
		CloudsYAML: options.CloudsYAML, CloudName: options.CloudName, CACert: options.CACert,
	})
}

// NewCleanupSession creates a client shared by resource cleanup and credential deletion.
func NewCleanupSession(ctx context.Context, options PurgeOptions) (CleanupSession, error) {
	if strings.TrimSpace(options.ClusterName) == "" {
		return nil, errors.New("creating cleanup session: cluster name is empty")
	}
	cloudClient, err := openstackclient.NewClient(ctx, openstackclient.Options{
		CloudsYAML: options.CloudsYAML,
		CloudName:  options.CloudName,
		CACert:     options.CACert,
	})
	if err != nil {
		return nil, fmt.Errorf("creating OpenStack client: %w", err)
	}
	return &cleanupSession{
		client: cloudClient,
		request: cleanup.Request{
			Scope:  cleanup.Scope{ClusterName: options.ClusterName},
			Policy: cleanup.Policy{DeleteVolumes: options.DeleteVolumes},
		},
	}, nil
}

func (s *cleanupSession) Binding() CredentialBinding {
	config := s.client.CredentialConfig()
	return CredentialBinding{
		Authority: config.Authority, ProjectID: s.client.ProjectID(), UserID: s.client.UserID(),
		Region: config.Region, Interface: config.Interface, CloudName: config.CloudName, CredentialID: config.CredentialID,
	}
}

func (s *cleanupSession) DeleteApplicationCredential(ctx context.Context, credentialID string) error {
	if credentialID == "" || credentialID != s.client.ApplicationCredentialID() {
		return errors.New("deleting application credential: ID does not match the authenticated credential")
	}
	service, err := identity.New(s.client)
	if err != nil {
		return s.client.RedactError(err)
	}
	return s.client.RedactError(service.DeleteApplicationCredential(ctx, credentialID))
}

func (s *cleanupSession) Cleanup(ctx context.Context) error {
	return s.client.RedactError(s.cleanupResources(ctx))
}

func (s *cleanupSession) cleanupResources(ctx context.Context) error {
	networkService, err := network.New(s.client)
	if err != nil {
		return err
	}
	loadBalancerService, err := loadbalancer.New(s.client)
	if err != nil {
		return err
	}

	resourceServices := cleanup.Services{
		FloatingIPs:    networkService,
		LoadBalancers:  loadBalancerService,
		SecurityGroups: networkService,
	}

	if s.request.Policy.DeleteVolumes {
		volumeService, err := volume.New(s.client)
		if err != nil {
			return err
		}
		resourceServices.Snapshots = volumeService
		resourceServices.Volumes = volumeService
	}

	cleanupResult, err := cleanup.NewRunner(resourceServices).Run(ctx, s.request)
	if err != nil {
		return err
	}
	if cleanupResult.Outcome == cleanup.OutcomeWaiting {
		return cleanup.ErrDeletePending
	}
	if cleanupResult.Outcome != cleanup.OutcomeComplete {
		return fmt.Errorf("cleanup returned unexpected outcome %q", cleanupResult.Outcome)
	}

	return nil
}
