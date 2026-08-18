package cleanup

// FloatingIP contains only the Neutron fields needed by the ownership rule.
type FloatingIP struct {
	ID          string
	Description string
}

// LoadBalancer contains only the Octavia fields needed by the ownership rule.
type LoadBalancer struct {
	ID   string
	Name string
}

// SecurityGroup contains only the Neutron fields needed by the ownership
// rule.
type SecurityGroup struct {
	ID          string
	Description string
}

// Snapshot contains only the Cinder fields needed by the ownership rule.
type Snapshot struct {
	ID       string
	Metadata map[string]string
}

// Volume contains only the Cinder fields needed by the ownership rule.
type Volume struct {
	ID       string
	Metadata map[string]string
}
