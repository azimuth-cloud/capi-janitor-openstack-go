package cleanup

import "errors"

var (
	// ErrDeletePending marks an OpenStack delete response that requires the
	// resource to be observed again before cleanup can advance. The resource
	// service uses this for the 400 and 409 responses retained from the Python
	// controller's retry behaviour.
	ErrDeletePending = errors.New("OpenStack resource deletion is pending")

	// ErrApplicationCredentialForbidden marks a 403 response from the exact
	// application credential DELETE request. Callers must not use this
	// classification for another OpenStack resource.
	ErrApplicationCredentialForbidden = errors.New("application credential deletion is forbidden")
)
