package cleanup

// Scope identifies the OpenStack resources that may be considered during a
// cleanup. The typed services already bind every request to the authenticated
// project and selected region.
type Scope struct {
	ClusterName string
}

// Policy contains the deletion decisions resolved before cleanup starts.
// Only Cinder cleanup has a resource policy gate.
type Policy struct {
	DeleteVolumes bool
}

// Request is the input to one cleanup iteration.
type Request struct {
	Scope  Scope
	Policy Policy
}
