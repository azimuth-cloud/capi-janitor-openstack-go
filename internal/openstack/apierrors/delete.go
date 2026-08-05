// Package apierrors converts Gophercloud errors into errors understood by the
// cleanup code.
package apierrors

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
)

// ClassifyDelete converts a DELETE result into cleanup semantics.
// Successful responses and 404 mean that deletion is complete.
// Responses that require a later check are returned as cleanup.ErrDeletePending.
func ClassifyDelete(err error) error {
	responseErr, returnedErr, classifyResponse := exposeDeleteError(err)
	if !classifyResponse {
		return returnedErr
	}
	return classifyDelete(responseErr, returnedErr)
}

// ClassifyApplicationCredentialDelete uses the normal delete rules and also
// maps HTTP 403 to cleanup.ErrApplicationCredentialForbidden.
// This special case applies only when an application credential tries to delete itself.
func ClassifyApplicationCredentialDelete(err error) error {
	responseErr, returnedErr, classifyResponse := exposeDeleteError(err)
	if !classifyResponse {
		return returnedErr
	}
	if responseCodeIs(responseErr, http.StatusForbidden) {
		return fmt.Errorf("%w: %w", cleanup.ErrApplicationCredentialForbidden, returnedErr)
	}
	return classifyDelete(responseErr, returnedErr)
}

// PreserveReauthenticationErrors makes the errors inside Gophercloud's
// reauthentication wrappers available through errors.Is and errors.As.
// The returned error keeps the original wrapper, target response, and
// reauthentication failure.
func PreserveReauthenticationErrors(err error) error {
	if err == nil {
		return nil
	}

	var unableToReauthenticate *gophercloud.ErrUnableToReauthenticate
	if errors.As(err, &unableToReauthenticate) && unableToReauthenticate != nil {
		return &unableToReauthenticateError{
			wrapper:          err,
			target:           unableToReauthenticate.ErrOriginal,
			reauthentication: unableToReauthenticate.ErrReauth,
		}
	}

	var afterReauthentication *gophercloud.ErrErrorAfterReauthentication
	if errors.As(err, &afterReauthentication) && afterReauthentication != nil && afterReauthentication.ErrOriginal != nil {
		return &responseAfterReauthenticationError{
			wrapper:  err,
			response: afterReauthentication.ErrOriginal,
		}
	}

	return err
}

func classifyDelete(responseErr, returnedErr error) error {
	statusCode, hasStatusCode := responseCode(responseErr)
	switch {
	case responseErr == nil,
		hasStatusCode && statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices,
		hasStatusCode && statusCode == http.StatusNotFound:
		return nil
	case hasStatusCode && (statusCode == http.StatusBadRequest || statusCode == http.StatusConflict):
		return fmt.Errorf("%w: %w", cleanup.ErrDeletePending, returnedErr)
	default:
		return returnedErr
	}
}

func responseCodeIs(err error, statusCode int) bool {
	actual, ok := responseCode(err)
	return ok && actual == statusCode
}

func responseCode(err error) (int, bool) {
	var responseErr gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &responseErr) {
		return responseErr.Actual, true
	}

	var responseErrPointer *gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &responseErrPointer) && responseErrPointer != nil {
		return responseErrPointer.Actual, true
	}

	return 0, false
}

// exposeDeleteError separates the response from the original DELETE request
// from any later reauthentication error. A failed reauthentication attempt is
// never treated as a successful or completed deletion.
func exposeDeleteError(err error) (responseErr, returnedErr error, classifyResponse bool) {
	var unableToReauthenticate *gophercloud.ErrUnableToReauthenticate
	if errors.As(err, &unableToReauthenticate) && unableToReauthenticate != nil {
		return unableToReauthenticate.ErrOriginal, PreserveReauthenticationErrors(err), false
	}

	var afterReauthentication *gophercloud.ErrErrorAfterReauthentication
	if !errors.As(err, &afterReauthentication) || afterReauthentication == nil || afterReauthentication.ErrOriginal == nil {
		return err, err, true
	}

	return afterReauthentication.ErrOriginal, PreserveReauthenticationErrors(err), true
}

type responseAfterReauthenticationError struct {
	wrapper  error
	response error
}

func (e *responseAfterReauthenticationError) Error() string {
	return e.wrapper.Error()
}

func (e *responseAfterReauthenticationError) Unwrap() []error {
	return []error{e.wrapper, e.response}
}

// unableToReauthenticateError keeps the original DELETE response, the
// Keystone reauthentication failure, and the Gophercloud wrapper in one error
// chain.
// This lets callers inspect every cause without treating the Keystone
// response as the result of the DELETE request.
type unableToReauthenticateError struct {
	wrapper          error
	target           error
	reauthentication error
}

func (e *unableToReauthenticateError) Error() string {
	return e.wrapper.Error()
}

func (e *unableToReauthenticateError) Unwrap() []error {
	causes := []error{e.wrapper}
	if e.target != nil {
		causes = append(causes, e.target)
	}
	if e.reauthentication != nil {
		causes = append(causes, e.reauthentication)
	}
	return causes
}
