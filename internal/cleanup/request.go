package cleanup

// Scope identifies the OpenStack resources that may be considered during a
// cleanup. ProjectID and Region record the boundary selected while resolving
// the cluster identity. Resource-specific ownership checks still use
// ClusterName and are applied separately.
type Scope struct {
	ClusterName string
	ProjectID   string
	Region      string
}

// Policy contains the deletion decisions resolved before cleanup starts.
// Security groups and floating IPs do not have separate policy gates in the
// Python implementation, so they are not represented here.
type Policy struct {
	DeleteLoadBalancers         bool
	DeleteVolumes               bool
	DeleteApplicationCredential bool
}

// Request is the input to one cleanup iteration.
type Request struct {
	Scope  Scope
	Policy Policy
}
