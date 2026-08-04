// Package cleanup contains the OpenStack cleanup domain model and the ports
// used by that model.
//
// The package deliberately has no dependency on controller-runtime or
// Gophercloud. Kubernetes reconciliation and OpenStack API transport live at
// the edges of the application and translate to and from these types.
package cleanup
