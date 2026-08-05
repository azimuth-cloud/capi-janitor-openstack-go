package cleanup

import "context"

// Runner performs one bounded cleanup iteration. The controller will consume
// this interface when the new cleanup path is integrated.
type Runner interface {
	Run(context.Context, Request) (Result, error)
}

// FloatingIPService supplies the complete floating IP inventory for the
// selected project and deletes an already-authorised floating IP by ID.
type FloatingIPService interface {
	ListFloatingIPs(context.Context) ([]FloatingIP, error)
	// DeleteFloatingIP treats an absent resource as complete and returns
	// ErrDeletePending when OpenStack requires a later observation.
	DeleteFloatingIP(context.Context, string) error
}

// LoadBalancerService supplies the complete load balancer inventory for the
// selected project and region and deletes an already-authorised load balancer
// by ID.
type LoadBalancerService interface {
	ListLoadBalancers(context.Context) ([]LoadBalancer, error)
	// DeleteLoadBalancer treats an absent resource as complete and returns
	// ErrDeletePending when OpenStack requires a later observation.
	DeleteLoadBalancer(context.Context, string) error
}

// SecurityGroupService supplies the complete security group inventory for the
// selected project and deletes an already-authorised security group by ID.
type SecurityGroupService interface {
	ListSecurityGroups(context.Context) ([]SecurityGroup, error)
	// DeleteSecurityGroup treats an absent resource as complete and returns
	// ErrDeletePending when OpenStack requires a later observation.
	DeleteSecurityGroup(context.Context, string) error
}

// SnapshotService supplies the complete snapshot inventory for the selected
// project and region and deletes an already-authorised snapshot by ID.
type SnapshotService interface {
	ListSnapshots(context.Context) ([]Snapshot, error)
	// DeleteSnapshot treats an absent resource as complete and returns
	// ErrDeletePending when OpenStack requires a later observation.
	DeleteSnapshot(context.Context, string) error
}

// VolumeService supplies the complete volume inventory for the selected
// project and region and deletes an already-authorised volume by ID.
type VolumeService interface {
	ListVolumes(context.Context) ([]Volume, error)
	// DeleteVolume treats an absent resource as complete and returns
	// ErrDeletePending when OpenStack requires a later observation.
	DeleteVolume(context.Context, string) error
}

// ApplicationCredentialService deletes the exact application credential ID
// from the selected cloud configuration. It follows the common delete
// contract and returns ErrApplicationCredentialForbidden only when Keystone
// rejects application credential self-deletion with HTTP 403.
type ApplicationCredentialService interface {
	DeleteApplicationCredential(context.Context, string) error
}
