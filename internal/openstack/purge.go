package openstack

import (
	"context"

	"github.com/go-logr/logr"

	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

// PurgeOptions contains settings for OpenStack resource cleanup.
type PurgeOptions struct {
	// CloudsYAML is the decoded content of the clouds.yaml credential file.
	CloudsYAML string
	// CloudName is the entry name within clouds.yaml to use.
	CloudName string
	// CACert is an optional PEM CA certificate for TLS verification.
	CACert string
	// ClusterName is the CAPI cluster name used to identify owned resources.
	ClusterName string
	// IncludeLoadBalancers controls Octavia load balancer deletion.
	IncludeLoadBalancers bool
	// IncludeVolumes controls Cinder volume and snapshot deletion.
	IncludeVolumes bool
	// IncludeAppcred controls application credential deletion.
	IncludeAppcred bool
	// Logger receives structured log messages during cleanup.
	Logger logr.Logger
}

// PurgeResources removes OpenStack resources created for a cluster.
func PurgeResources(ctx context.Context, opts PurgeOptions) error {
	client, err := openstackclient.NewClient(ctx, openstackclient.Options{
		CloudsYAML: opts.CloudsYAML,
		CloudName:  opts.CloudName,
		CACert:     opts.CACert,
	})
	if err != nil {
		return err
	}

	// Authentication failure does not show that cleanup is complete. Return an
	// error so that the controller keeps the finalizer and retries.
	if !client.IsAuthenticated() {
		return &AuthenticationError{UserID: client.UserID()}
	}

	purger := resourcePurger{
		factory:                 &gophercloudResourceFactory{client: client},
		clusterName:             opts.ClusterName,
		includeLoadBalancers:    opts.IncludeLoadBalancers,
		includeVolumes:          opts.IncludeVolumes,
		includeAppCredential:    opts.IncludeAppcred,
		applicationCredentialID: client.ApplicationCredentialID(),
		logger:                  opts.Logger,
		wait:                    waitForNextObservation,
	}
	return purger.purge(ctx)
}
