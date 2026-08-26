package cleanup

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	floatingIPDescriptionPrefix    = "Floating IP for Kubernetes external service"
	floatingIPDescriptionSuffix    = "from cluster "
	loadBalancerServicePrefix      = "kube_service_"
	securityGroupDescriptionPrefix = "Security Group for"
	securityGroupDescriptionSuffix = "Service LoadBalancer in cluster "
	cinderClusterMetadataKey       = "cinder.csi.openstack.org/cluster"
	volumeKeepMetadataKey          = "janitor.capi.azimuth-cloud.com/keep"
)

type resourceCleanupRunner struct {
	services Services
}

// NewRunner returns a runner that performs one bounded cleanup iteration.
func NewRunner(services Services) Runner {
	return &resourceCleanupRunner{services: services}
}

func (r *resourceCleanupRunner) Run(ctx context.Context, request Request) (Result, error) {
	if request.Scope.ClusterName == "" {
		return Result{}, errors.New("running cleanup: cluster name is empty")
	}
	if r.services.LoadBalancers == nil {
		return Result{}, errors.New("running cleanup: load balancer service is nil")
	}
	if r.services.FloatingIPs == nil {
		return Result{}, errors.New("running cleanup: floating IP service is nil")
	}

	loadBalancers, err := r.services.LoadBalancers.ListLoadBalancers(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing load balancers before selecting resources for deletion: %w", err)
	}
	floatingIPs, err := r.services.FloatingIPs.ListFloatingIPs(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing floating IPs before selecting resources for deletion: %w", err)
	}

	resourcesToDelete, err := selectLoadBalancersAndFloatingIPsForDeletion(
		request.Scope.ClusterName,
		loadBalancers,
		floatingIPs,
	)
	if err != nil {
		return Result{}, err
	}
	if len(resourcesToDelete.floatingIPIDsToDelete) > 0 {
		return deleteResourcesForCurrentPhase(
			ctx,
			"floating IP",
			resourcesToDelete.floatingIPIDsToDelete,
			r.services.FloatingIPs.DeleteFloatingIP,
		)
	}
	if len(resourcesToDelete.loadBalancerIDsToDelete) > 0 {
		return deleteResourcesForCurrentPhase(
			ctx,
			"load balancer",
			resourcesToDelete.loadBalancerIDsToDelete,
			r.services.LoadBalancers.DeleteLoadBalancer,
		)
	}

	if r.services.SecurityGroups == nil {
		return Result{}, errors.New("running cleanup: security group service is nil")
	}
	securityGroups, err := r.services.SecurityGroups.ListSecurityGroups(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing security groups: %w", err)
	}
	securityGroupIDs := selectResourceIDsForDeletion(securityGroups, func(securityGroup SecurityGroup) bool {
		return securityGroupDescriptionMatchesCluster(securityGroup, request.Scope.ClusterName)
	}, func(securityGroup SecurityGroup) string {
		return securityGroup.ID
	})
	if len(securityGroupIDs) > 0 {
		return deleteResourcesForCurrentPhase(
			ctx,
			"security group",
			securityGroupIDs,
			r.services.SecurityGroups.DeleteSecurityGroup,
		)
	}

	if !request.Policy.DeleteVolumes {
		return Result{Outcome: OutcomeComplete}, nil
	}
	if r.services.Snapshots == nil {
		return Result{}, errors.New("running cleanup: snapshot service is nil")
	}

	snapshots, err := r.services.Snapshots.ListSnapshots(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing snapshots: %w", err)
	}
	snapshotIDs := selectResourceIDsForDeletion(snapshots, func(snapshot Snapshot) bool {
		return snapshotMetadataMatchesCluster(snapshot, request.Scope.ClusterName)
	}, func(snapshot Snapshot) string {
		return snapshot.ID
	})
	if len(snapshotIDs) > 0 {
		return deleteResourcesForCurrentPhase(ctx, "snapshot", snapshotIDs, r.services.Snapshots.DeleteSnapshot)
	}

	if r.services.Volumes == nil {
		return Result{}, errors.New("running cleanup: volume service is nil")
	}
	volumes, err := r.services.Volumes.ListVolumes(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing volumes: %w", err)
	}
	volumeIDs := selectResourceIDsForDeletion(volumes, func(volume Volume) bool {
		return volumeShouldBeDeletedForCluster(volume, request.Scope.ClusterName)
	}, func(volume Volume) string {
		return volume.ID
	})
	if len(volumeIDs) > 0 {
		return deleteResourcesForCurrentPhase(ctx, "volume", volumeIDs, r.services.Volumes.DeleteVolume)
	}

	return Result{Outcome: OutcomeComplete}, nil
}

type loadBalancerAndFloatingIPSelection struct {
	floatingIPIDsToDelete   []string
	loadBalancerIDsToDelete []string
}

func selectLoadBalancersAndFloatingIPsForDeletion(
	clusterName string,
	loadBalancers []LoadBalancer,
	floatingIPs []FloatingIP,
) (loadBalancerAndFloatingIPSelection, error) {
	protectedLoadBalancerVIPPortIDs := make(map[string]struct{})
	loadBalancerIDsToDelete := make([]string, 0)

	for _, loadBalancer := range loadBalancers {
		if !loadBalancerNameMatchesCluster(loadBalancer, clusterName) {
			continue
		}
		if loadBalancer.ID == "" {
			return loadBalancerAndFloatingIPSelection{}, errors.New("checking load balancer ownership: selected resource ID is empty")
		}
		if loadBalancerHasForeignOrInvalidServiceTag(loadBalancer, clusterName) {
			if loadBalancer.VIPPortID == "" {
				return loadBalancerAndFloatingIPSelection{}, fmt.Errorf(
					"checking load balancer %q shared ownership: VIP port ID is empty",
					loadBalancer.ID,
				)
			}
			protectedLoadBalancerVIPPortIDs[loadBalancer.VIPPortID] = struct{}{}
			continue
		}
		loadBalancerIDsToDelete = append(loadBalancerIDsToDelete, loadBalancer.ID)
	}

	floatingIPIDsToDelete := make([]string, 0)
	for _, floatingIP := range floatingIPs {
		if !floatingIPDescriptionMatchesCluster(floatingIP, clusterName) {
			continue
		}
		if floatingIP.ID == "" {
			return loadBalancerAndFloatingIPSelection{}, errors.New("checking floating IP ownership: selected resource ID is empty")
		}
		if _, protected := protectedLoadBalancerVIPPortIDs[floatingIP.AttachedPortID]; protected {
			continue
		}
		floatingIPIDsToDelete = append(floatingIPIDsToDelete, floatingIP.ID)
	}

	return loadBalancerAndFloatingIPSelection{
		floatingIPIDsToDelete:   floatingIPIDsToDelete,
		loadBalancerIDsToDelete: loadBalancerIDsToDelete,
	}, nil
}

func floatingIPDescriptionMatchesCluster(floatingIP FloatingIP, clusterName string) bool {
	return strings.HasPrefix(floatingIP.Description, floatingIPDescriptionPrefix) &&
		strings.HasSuffix(floatingIP.Description, floatingIPDescriptionSuffix+clusterName)
}

func loadBalancerNameMatchesCluster(loadBalancer LoadBalancer, clusterName string) bool {
	return strings.HasPrefix(loadBalancer.Name, loadBalancerServicePrefix+clusterName+"_")
}

func securityGroupDescriptionMatchesCluster(securityGroup SecurityGroup, clusterName string) bool {
	return strings.HasPrefix(securityGroup.Description, securityGroupDescriptionPrefix) &&
		strings.HasSuffix(securityGroup.Description, securityGroupDescriptionSuffix+clusterName)
}

func snapshotMetadataMatchesCluster(snapshot Snapshot, clusterName string) bool {
	return snapshot.Metadata[cinderClusterMetadataKey] == clusterName
}

func volumeShouldBeDeletedForCluster(volume Volume, clusterName string) bool {
	return volume.Metadata[cinderClusterMetadataKey] == clusterName &&
		volume.Metadata[volumeKeepMetadataKey] != "true"
}

func loadBalancerHasForeignOrInvalidServiceTag(loadBalancer LoadBalancer, clusterName string) bool {
	for _, tag := range loadBalancer.Tags {
		if !strings.HasPrefix(tag, loadBalancerServicePrefix) {
			continue
		}
		if !isValidLoadBalancerServiceTagForCluster(tag, clusterName) {
			return true
		}
	}
	return false
}

func isValidLoadBalancerServiceTagForCluster(tag, clusterName string) bool {
	namespaceAndService, hasClusterPrefix := strings.CutPrefix(tag, loadBalancerServicePrefix+clusterName+"_")
	if !hasClusterPrefix {
		return false
	}
	serviceNameParts := strings.Split(namespaceAndService, "_")
	return len(serviceNameParts) == 2 && serviceNameParts[0] != "" && serviceNameParts[1] != ""
}

func selectResourceIDsForDeletion[T any](
	resources []T,
	shouldDelete func(T) bool,
	resourceID func(T) string,
) []string {
	selectedResourceIDs := make([]string, 0)
	for _, resource := range resources {
		if shouldDelete(resource) {
			selectedResourceIDs = append(selectedResourceIDs, resourceID(resource))
		}
	}
	return selectedResourceIDs
}

func deleteResourcesForCurrentPhase(
	ctx context.Context,
	resourceKind string,
	resourceIDs []string,
	deleteByID func(context.Context, string) error,
) (Result, error) {
	for _, resourceID := range resourceIDs {
		if resourceID == "" {
			return Result{}, fmt.Errorf("deleting %s: selected resource ID is empty", resourceKind)
		}
	}

	for _, resourceID := range resourceIDs {
		err := deleteByID(ctx, resourceID)
		if errors.Is(err, ErrDeletePending) {
			continue
		}
		if err != nil {
			return Result{}, fmt.Errorf("deleting %s %q: %w", resourceKind, resourceID, err)
		}
	}
	return Result{Outcome: OutcomeWaiting}, nil
}
