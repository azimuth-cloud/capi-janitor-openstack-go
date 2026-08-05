package openstack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/identity"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/loadbalancer"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/network"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/volume"
)

const (
	clusterMetadataKey = "cinder.csi.openstack.org/cluster"
	keepMetadataKey    = "janitor.capi.azimuth-cloud.com/keep"
	maxPollAttempts    = 6
	pollInterval       = 5 * time.Second
)

type networkService interface {
	cleanup.FloatingIPService
	cleanup.SecurityGroupService
}

type volumeService interface {
	cleanup.SnapshotService
	cleanup.VolumeService
}

type resourceFactory interface {
	network() (networkService, error)
	loadBalancer() (cleanup.LoadBalancerService, error)
	volume() (volumeService, error)
	identity() (cleanup.ApplicationCredentialService, error)
}

// gophercloudResourceFactory creates service clients when cleanup needs them.
// Disabled cleanup stages do not require catalog endpoints.
type gophercloudResourceFactory struct {
	client *openstackclient.Client
}

func (f *gophercloudResourceFactory) network() (networkService, error) {
	return network.New(f.client)
}

func (f *gophercloudResourceFactory) loadBalancer() (cleanup.LoadBalancerService, error) {
	return loadbalancer.New(f.client)
}

func (f *gophercloudResourceFactory) volume() (volumeService, error) {
	return volume.New(f.client)
}

func (f *gophercloudResourceFactory) identity() (cleanup.ApplicationCredentialService, error) {
	return identity.New(f.client)
}

type waitFunc func(context.Context, time.Duration) error

// resourcePurger connects PurgeResources to the cleanup service interfaces.
// It uses bounded polling to verify each deletion.
type resourcePurger struct {
	factory                 resourceFactory
	clusterName             string
	includeLoadBalancers    bool
	includeVolumes          bool
	includeAppCredential    bool
	applicationCredentialID string
	logger                  logr.Logger
	wait                    waitFunc
}

func (p *resourcePurger) purge(ctx context.Context) error {
	if p.factory == nil {
		return errors.New("purging OpenStack resources: resource factory is nil")
	}
	if p.clusterName == "" {
		return errors.New("purging OpenStack resources: cluster name is empty")
	}
	if p.wait == nil {
		p.wait = waitForNextObservation
	}

	networkService, err := p.factory.network()
	if err != nil {
		return fmt.Errorf("creating network cleanup service: %w", err)
	}
	if err := p.purgeFloatingIPs(ctx, networkService); err != nil {
		return err
	}

	if p.includeLoadBalancers {
		loadBalancerService, err := p.factory.loadBalancer()
		if err != nil {
			return fmt.Errorf("creating load balancer cleanup service: %w", err)
		}
		if err := p.purgeLoadBalancers(ctx, loadBalancerService); err != nil {
			return err
		}
	}

	if err := p.purgeSecurityGroups(ctx, networkService); err != nil {
		return err
	}

	if p.includeVolumes {
		volumeService, err := p.factory.volume()
		if err != nil {
			return fmt.Errorf("creating block storage cleanup service: %w", err)
		}
		if err := p.purgeSnapshots(ctx, volumeService); err != nil {
			return err
		}
		if err := p.purgeVolumes(ctx, volumeService); err != nil {
			return err
		}
	}

	if !p.includeAppCredential || p.applicationCredentialID == "" {
		if p.includeAppCredential {
			p.logger.Info("No application credential is in use, skipping application credential deletion")
		}
		return nil
	}

	identityService, err := p.factory.identity()
	if err != nil {
		return fmt.Errorf("creating identity cleanup service: %w", err)
	}
	if err := identityService.DeleteApplicationCredential(ctx, p.applicationCredentialID); err != nil {
		if errors.Is(err, cleanup.ErrApplicationCredentialForbidden) {
			p.logger.Info("Application credential could not delete itself, continuing cleanup")
			return nil
		}
		return fmt.Errorf("deleting application credential: %w", err)
	}
	p.logger.Info("Deleted application credential")
	return nil
}

func (p *resourcePurger) purgeFloatingIPs(ctx context.Context, service cleanup.FloatingIPService) error {
	match := func(item cleanup.FloatingIP) bool {
		return strings.HasPrefix(item.Description, "Floating IP for Kubernetes external service") &&
			strings.HasSuffix(item.Description, "from cluster "+p.clusterName)
	}
	return purgeResourceKind(
		ctx,
		p.logger,
		p.wait,
		"floating IP",
		service.ListFloatingIPs,
		service.DeleteFloatingIP,
		match,
		func(item cleanup.FloatingIP) string { return item.ID },
	)
}

func (p *resourcePurger) purgeLoadBalancers(ctx context.Context, service cleanup.LoadBalancerService) error {
	match := func(item cleanup.LoadBalancer) bool {
		return strings.HasPrefix(item.Name, "kube_service_"+p.clusterName+"_")
	}
	return purgeResourceKind(
		ctx,
		p.logger,
		p.wait,
		"load balancer",
		service.ListLoadBalancers,
		service.DeleteLoadBalancer,
		match,
		func(item cleanup.LoadBalancer) string { return item.ID },
	)
}

func (p *resourcePurger) purgeSecurityGroups(ctx context.Context, service cleanup.SecurityGroupService) error {
	match := func(item cleanup.SecurityGroup) bool {
		return strings.HasPrefix(item.Description, "Security Group for") &&
			strings.HasSuffix(item.Description, "Service LoadBalancer in cluster "+p.clusterName)
	}
	return purgeResourceKind(
		ctx,
		p.logger,
		p.wait,
		"security group",
		service.ListSecurityGroups,
		service.DeleteSecurityGroup,
		match,
		func(item cleanup.SecurityGroup) string { return item.ID },
	)
}

func (p *resourcePurger) purgeSnapshots(ctx context.Context, service cleanup.SnapshotService) error {
	match := func(item cleanup.Snapshot) bool {
		return item.Metadata[clusterMetadataKey] == p.clusterName
	}
	return purgeResourceKind(
		ctx,
		p.logger,
		p.wait,
		"snapshot",
		service.ListSnapshots,
		service.DeleteSnapshot,
		match,
		func(item cleanup.Snapshot) string { return item.ID },
	)
}

func (p *resourcePurger) purgeVolumes(ctx context.Context, service cleanup.VolumeService) error {
	match := func(item cleanup.Volume) bool {
		return item.Metadata[clusterMetadataKey] == p.clusterName && item.Metadata[keepMetadataKey] != "true"
	}
	return purgeResourceKind(
		ctx,
		p.logger,
		p.wait,
		"volume",
		service.ListVolumes,
		service.DeleteVolume,
		match,
		func(item cleanup.Volume) string { return item.ID },
	)
}

func purgeResourceKind[T any](
	ctx context.Context,
	logger logr.Logger,
	wait waitFunc,
	kind string,
	list func(context.Context) ([]T, error),
	deleteResource func(context.Context, string) error,
	matches func(T) bool,
	idOf func(T) string,
) error {
	items, err := list(ctx)
	if err != nil {
		return fmt.Errorf("listing %ss: %w", kind, err)
	}

	candidateIDs := make([]string, 0)
	for _, item := range items {
		if matches(item) {
			candidateIDs = append(candidateIDs, idOf(item))
		}
	}
	if len(candidateIDs) == 0 {
		return nil
	}

	for _, id := range candidateIDs {
		logger.Info("Deleting OpenStack resource", "resourceKind", kind, "id", id)
		if err := deleteResource(ctx, id); err != nil {
			if errors.Is(err, cleanup.ErrDeletePending) {
				logger.Info("OpenStack resource deletion is pending", "resourceKind", kind, "id", id)
				continue
			}
			return fmt.Errorf("deleting %s %q: %w", kind, id, err)
		}
	}

	for attempt := 0; attempt < maxPollAttempts; attempt++ {
		if attempt > 0 {
			if err := wait(ctx, pollInterval); err != nil {
				return fmt.Errorf("waiting to verify %s deletion: %w", kind, err)
			}
		}

		items, err = list(ctx)
		if err != nil {
			return fmt.Errorf("verifying %s deletion: %w", kind, err)
		}
		remaining := 0
		for _, item := range items {
			if matches(item) {
				remaining++
			}
		}
		if remaining == 0 {
			return nil
		}
		logger.Info(
			"OpenStack resources are still present",
			"resourceKind", kind,
			"remaining", remaining,
			"attempt", attempt+1,
			"maxAttempts", maxPollAttempts,
		)
	}

	return fmt.Errorf("%w: %ss are still present", cleanup.ErrDeletePending, kind)
}

func waitForNextObservation(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
