package cleanup

import "errors"

var (
	// ErrDeletePending indicates that cleanup must check the resource again
	// before it continues. It maps the 400 and 409 responses handled by the
	// Python controller.
	ErrDeletePending = errors.New("OpenStack resource deletion is pending")

	// ErrApplicationCredentialForbidden indicates that Keystone returned 403
	// when deleting the current application credential. This error applies only
	// to application credential deletion.
	ErrApplicationCredentialForbidden = errors.New("application credential deletion is forbidden")
)
