// Package openstack provides the compatibility layer used by the existing
// cleanup implementation while it is moved to domain owned resource adapters.
package openstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

const (
	// KeepProperty is the OpenStack volume metadata key that marks a volume as user-kept.
	KeepProperty = "janitor.capi.azimuth-cloud.com/keep"
)

// AuthenticationError is returned when OpenStack authentication fails.
type AuthenticationError struct {
	UserID string
}

func (e *AuthenticationError) Error() string {
	return fmt.Sprintf("failed to authenticate as user: %s", e.UserID)
}

// UnsupportedAuthTypeError is returned when clouds.yaml uses an unsupported
// authentication method.
type UnsupportedAuthTypeError = openstackclient.UnsupportedAuthTypeError

// CatalogError is returned when a required service is absent from the
// OpenStack catalog.
type CatalogError struct {
	ServiceType string
}

func (e *CatalogError) Error() string {
	return fmt.Sprintf("service type %s not found in OpenStack service catalog", e.ServiceType)
}

// Session temporarily preserves the API used by the existing cleanup code.
// Authentication, catalog discovery and request handling are delegated to
// Gophercloud. Resource-specific methods will move behind internal/cleanup
// interfaces in the resource-adapter change.
type Session struct {
	client *openstackclient.Client

	// httpClient is retained only for small package-level transport tests. A
	// production Session created by Authenticate always uses client.Request.
	httpClient *http.Client

	// SleepFunc is called instead of time.Sleep for polling waits.
	// A nil value defaults to time.Sleep.
	SleepFunc func(time.Duration)
}

// Authenticate creates a Gophercloud provider from the selected in-memory
// clouds.yaml entry.
func Authenticate(ctx context.Context, cloudsYAML, cloudName, caCert string) (*Session, error) {
	client, err := openstackclient.NewClient(ctx, openstackclient.Options{
		CloudsYAML: cloudsYAML,
		CloudName:  cloudName,
		CACert:     caCert,
	})
	if err != nil {
		return nil, err
	}
	return &Session{client: client}, nil
}

// IsAuthenticated reports whether authentication and catalog discovery both
// completed successfully.
func (s *Session) IsAuthenticated() bool {
	return s != nil && s.client != nil && s.client.IsAuthenticated()
}

// UserID returns the ID of the authenticated user.
func (s *Session) UserID() string {
	if s == nil || s.client == nil {
		return ""
	}
	return s.client.UserID()
}

// HasEndpoint reports whether the selected catalog contains a matching
// endpoint for the configured interface and region.
func (s *Session) HasEndpoint(serviceType string) bool {
	if s == nil || s.client == nil {
		return false
	}
	_, err := s.client.Endpoint(serviceType)
	return err == nil
}

func (s *Session) endpointFor(serviceTypes ...string) (string, error) {
	if s == nil || s.client == nil {
		return "", &CatalogError{ServiceType: strings.Join(serviceTypes, " or ")}
	}
	for _, serviceType := range serviceTypes {
		endpoint, err := s.client.Endpoint(serviceType)
		if err == nil {
			return strings.TrimRight(endpoint, "/"), nil
		}
		var endpointNotFound *gophercloud.ErrEndpointNotFound
		var serviceNotFound *gophercloud.ErrServiceNotFound
		if !errors.As(err, &endpointNotFound) && !errors.As(err, &serviceNotFound) {
			return "", err
		}
	}
	return "", &CatalogError{ServiceType: strings.Join(serviceTypes, " or ")}
}

// doGet issues an authenticated Gophercloud GET and returns its response body.
func (s *Session) doGet(ctx context.Context, url string) ([]byte, error) {
	if s.client != nil {
		response, err := s.client.Request(ctx, http.MethodGet, url, &gophercloud.RequestOpts{KeepResponseBody: true})
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		return io.ReadAll(response.Body)
	}

	// Package tests can exercise body read failures without constructing an
	// authenticated provider. Production sessions never use this branch.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

// doDelete issues an idempotent authenticated Gophercloud DELETE.
func (s *Session) doDelete(ctx context.Context, url string) error {
	_, err := s.client.Request(ctx, http.MethodDelete, url, &gophercloud.RequestOpts{
		OkCodes: []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound},
	})
	return err
}

// isTransient returns true for the OpenStack states that the existing cleanup
// path verifies by listing the resource again.
func isTransient(err error) bool {
	return gophercloud.ResponseCodeIs(err, http.StatusBadRequest) || gophercloud.ResponseCodeIs(err, http.StatusConflict)
}

// AppCredentialID extracts the application credential ID from the selected
// clouds.yaml entry. Password authentication returns an empty ID.
func AppCredentialID(cloudsYAML, cloudName string) (string, error) {
	return openstackclient.ApplicationCredentialID(cloudsYAML, cloudName)
}

func (s *Session) sleep(duration time.Duration) {
	if s.SleepFunc != nil {
		s.SleepFunc(duration)
		return
	}
	time.Sleep(duration)
}
