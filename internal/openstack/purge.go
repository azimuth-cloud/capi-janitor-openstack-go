package openstack

import (
	"context"
	"fmt"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
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
	cloudClient, err := openstackclient.NewClient(ctx, openstackclient.Options{
		CloudsYAML: options.CloudsYAML,
		CloudName:  options.CloudName,
		CACert:     options.CACert,
	})
	if err != nil {
		return fmt.Errorf("creating OpenStack client: %w", err)
	}

	networkService, err := network.New(cloudClient)
	if err != nil {
		return err
	}
	loadBalancerService, err := loadbalancer.New(cloudClient)
	if err != nil {
		return err
	}

	resourceServices := cleanup.Services{
		FloatingIPs:    networkService,
		LoadBalancers:  loadBalancerService,
		SecurityGroups: networkService,
	}

	if options.DeleteVolumes {
		volumeService, err := volume.New(cloudClient)
		if err != nil {
			return err
		}
		resourceServices.Snapshots = volumeService
		resourceServices.Volumes = volumeService
	}

	cleanupResult, err := cleanup.NewRunner(resourceServices).Run(ctx, cleanup.Request{
		Scope:  cleanup.Scope{ClusterName: options.ClusterName},
		Policy: cleanup.Policy{DeleteVolumes: options.DeleteVolumes},
	})
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
