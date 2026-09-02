package apierrors_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/apierrors"
)

func TestClassifyDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantNil    bool
		want       error
	}{
		{name: "OK is successful", statusCode: http.StatusOK, wantNil: true},
		{name: "created is successful", statusCode: http.StatusCreated, wantNil: true},
		{name: "not found is idempotent", statusCode: http.StatusNotFound, wantNil: true},
		{name: "bad request is pending", statusCode: http.StatusBadRequest, want: cleanup.ErrDeletePending},
		{name: "conflict is pending", statusCode: http.StatusConflict, want: cleanup.ErrDeletePending},
		{name: "generic forbidden is unclassified", statusCode: http.StatusForbidden},
		{name: "redirection is unclassified", statusCode: http.StatusMultipleChoices},
		{name: "server error is unclassified", statusCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			responseErr := gophercloud.ErrUnexpectedResponseCode{
				Actual: tt.statusCode,
			}
			err := apierrors.ClassifyDelete(responseErr)
			if tt.wantNil {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("expected error to match %v, got %v", tt.want, err)
			}
			if errors.Is(err, cleanup.ErrApplicationCredentialForbidden) {
				t.Fatalf("generic delete unexpectedly used credential-only classification: %v", err)
			}
			var gotResponseErr gophercloud.ErrUnexpectedResponseCode
			if !errors.As(err, &gotResponseErr) {
				t.Fatalf("expected response cause to be preserved, got %v", err)
			}
		})
	}
}

func TestClassifyApplicationCredentialDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantNil    bool
		want       error
	}{
		{name: "no content is successful", statusCode: http.StatusNoContent, wantNil: true},
		{name: "not found is idempotent", statusCode: http.StatusNotFound, wantNil: true},
		{name: "unexpected OK is rejected", statusCode: http.StatusOK},
		{name: "bad request is unclassified", statusCode: http.StatusBadRequest},
		{name: "forbidden is credential-specific", statusCode: http.StatusForbidden, want: cleanup.ErrApplicationCredentialForbidden},
		{name: "server error is unclassified", statusCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			responseErr := gophercloud.ErrUnexpectedResponseCode{Actual: tt.statusCode}
			err := apierrors.ClassifyApplicationCredentialDelete(responseErr)
			if tt.wantNil {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected delete error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("expected error to match %v, got %v", tt.want, err)
			}
			var gotResponseErr gophercloud.ErrUnexpectedResponseCode
			if !errors.As(err, &gotResponseErr) || gotResponseErr.Actual != tt.statusCode {
				t.Fatalf("expected response status %d to be preserved, got %v", tt.statusCode, err)
			}
		})
	}
}

func TestClassifyDeleteAfterSuccessfulReauthentication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantNil    bool
		want       error
	}{
		{name: "OK is successful", statusCode: http.StatusOK, wantNil: true},
		{name: "not found is idempotent", statusCode: http.StatusNotFound, wantNil: true},
		{name: "bad request is pending", statusCode: http.StatusBadRequest, want: cleanup.ErrDeletePending},
		{name: "conflict is pending", statusCode: http.StatusConflict, want: cleanup.ErrDeletePending},
		{name: "generic forbidden is unclassified", statusCode: http.StatusForbidden},
		{name: "server error is unclassified", statusCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			responseErr := gophercloud.ErrUnexpectedResponseCode{Actual: tt.statusCode}
			afterReauthentication := &gophercloud.ErrErrorAfterReauthentication{
				ErrOriginal: responseErr,
			}
			err := apierrors.ClassifyDelete(afterReauthentication)
			if tt.wantNil {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected classified delete error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("expected error to match %v, got %v", tt.want, err)
			}
			if errors.Is(err, cleanup.ErrApplicationCredentialForbidden) {
				t.Fatalf("generic delete unexpectedly used credential-only classification: %v", err)
			}

			var gotAfterReauthentication *gophercloud.ErrErrorAfterReauthentication
			if !errors.As(err, &gotAfterReauthentication) {
				t.Fatalf("expected reauthentication wrapper to be preserved, got %v", err)
			}
			var gotResponseErr gophercloud.ErrUnexpectedResponseCode
			if !errors.As(err, &gotResponseErr) || gotResponseErr.Actual != tt.statusCode {
				t.Fatalf("expected response status %d to be preserved, got %v", tt.statusCode, err)
			}
		})
	}
}

func TestClassifyApplicationCredentialDeleteAfterSuccessfulReauthentication(t *testing.T) {
	t.Parallel()

	responseErr := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusForbidden}
	afterReauthentication := &gophercloud.ErrErrorAfterReauthentication{
		ErrOriginal: responseErr,
	}
	err := apierrors.ClassifyApplicationCredentialDelete(afterReauthentication)
	if !errors.Is(err, cleanup.ErrApplicationCredentialForbidden) {
		t.Fatalf("expected forbidden classification, got %v", err)
	}
	var gotAfterReauthentication *gophercloud.ErrErrorAfterReauthentication
	if !errors.As(err, &gotAfterReauthentication) {
		t.Fatalf("expected reauthentication wrapper to be preserved, got %v", err)
	}
	var gotResponseErr gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &gotResponseErr) {
		t.Fatalf("expected response cause to be preserved, got %v", err)
	}
}

func TestClassifyDeletePreservesReauthenticationCancellation(t *testing.T) {
	t.Parallel()

	targetErr := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusUnauthorized}
	reauthenticationCause := fmt.Errorf("reauthenticating provider: %w", context.Canceled)
	reauthenticationErr := &gophercloud.ErrUnableToReauthenticate{
		ErrOriginal: targetErr,
		ErrReauth:   reauthenticationCause,
	}
	outerErr := fmt.Errorf("deleting resource: %w", reauthenticationErr)

	err := apierrors.ClassifyDelete(outerErr)
	if err == nil {
		t.Fatal("expected failed reauthentication to block delete completion")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation cause to be preserved, got %v", err)
	}
	var gotReauthenticationErr *gophercloud.ErrUnableToReauthenticate
	if !errors.As(err, &gotReauthenticationErr) || gotReauthenticationErr != reauthenticationErr {
		t.Fatalf("expected Gophercloud reauthentication wrapper to be preserved, got %v", err)
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusUnauthorized) {
		t.Fatalf("expected target response status to be preserved, got %v", err)
	}
}

func TestPreserveReauthenticationErrorsForListRequests(t *testing.T) {
	t.Parallel()

	t.Run("failed reauthentication", func(t *testing.T) {
		t.Parallel()

		targetErr := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusUnauthorized}
		reauthenticationErr := &gophercloud.ErrUnableToReauthenticate{
			ErrOriginal: targetErr,
			ErrReauth:   fmt.Errorf("reauthenticating list request: %w", context.Canceled),
		}

		err := apierrors.PreserveReauthenticationErrors(reauthenticationErr)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation cause to be preserved, got %v", err)
		}
		var gotWrapper *gophercloud.ErrUnableToReauthenticate
		if !errors.As(err, &gotWrapper) || gotWrapper != reauthenticationErr {
			t.Fatalf("expected Gophercloud wrapper to be preserved, got %v", err)
		}
	})

	t.Run("request failed after reauthentication", func(t *testing.T) {
		t.Parallel()

		afterReauthentication := &gophercloud.ErrErrorAfterReauthentication{
			ErrOriginal: context.DeadlineExceeded,
		}
		err := apierrors.PreserveReauthenticationErrors(afterReauthentication)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected request cause to be preserved, got %v", err)
		}
		var gotWrapper *gophercloud.ErrErrorAfterReauthentication
		if !errors.As(err, &gotWrapper) || gotWrapper != afterReauthentication {
			t.Fatalf("expected Gophercloud wrapper to be preserved, got %v", err)
		}
	})
}

func TestClassifyDeleteDoesNotTreatKeystoneNotFoundAsTargetNotFound(t *testing.T) {
	t.Parallel()

	targetErr := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusUnauthorized}
	keystoneErr := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusNotFound}
	reauthenticationErr := &gophercloud.ErrUnableToReauthenticate{
		ErrOriginal: targetErr,
		ErrReauth:   keystoneErr,
	}

	err := apierrors.ClassifyDelete(reauthenticationErr)
	if err == nil {
		t.Fatal("Keystone 404 must not make the target delete idempotently successful")
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusUnauthorized) {
		t.Fatalf("expected target 401 to remain the exposed resource status, got %v", err)
	}
	if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		t.Fatalf("Keystone 404 was exposed as the target resource status: %v", err)
	}

	var gotReauthenticationErr *gophercloud.ErrUnableToReauthenticate
	if !errors.As(err, &gotReauthenticationErr) {
		t.Fatalf("expected reauthentication wrapper, got %v", err)
	}
	if !gophercloud.ResponseCodeIs(gotReauthenticationErr.ErrReauth, http.StatusNotFound) {
		t.Fatalf("expected Keystone 404 cause to remain available on the wrapper, got %v", err)
	}
}

func TestClassifyApplicationCredentialDeleteDoesNotTreatKeystoneForbiddenAsTargetForbidden(t *testing.T) {
	t.Parallel()

	targetErr := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusUnauthorized}
	keystoneErr := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusForbidden}
	reauthenticationErr := &gophercloud.ErrUnableToReauthenticate{
		ErrOriginal: targetErr,
		ErrReauth:   keystoneErr,
	}

	err := apierrors.ClassifyApplicationCredentialDelete(reauthenticationErr)
	if err == nil {
		t.Fatal("expected failed reauthentication to block credential delete completion")
	}
	if errors.Is(err, cleanup.ErrApplicationCredentialForbidden) {
		t.Fatalf("Keystone 403 used the target-only credential classification: %v", err)
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusUnauthorized) {
		t.Fatalf("expected target 401 to remain the exposed resource status, got %v", err)
	}
}
