package cleanup

import "errors"

var (
	// ErrDeletePending marks an OpenStack delete response that requires the
	// resource to be observed again before cleanup can advance. The resource
	// adapter uses this for the 400 and 409 responses retained from the Python
	// controller's retry behaviour.
	ErrDeletePending = errors.New("OpenStack resource deletion is pending")

	// ErrApplicationCredentialForbidden marks the narrow 403 response allowed
	// by the application credential self-deletion policy. Callers must not use
	// this classification for any other OpenStack resource.
	ErrApplicationCredentialForbidden = errors.New("application credential deletion is forbidden")
)
