// Package openstack runs cleanup and supports the legacy HTTP implementation.
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
	// KeepProperty is the OpenStack volume metadata key that preserves a volume.
	KeepProperty = "janitor.capi.azimuth-cloud.com/keep"
)

// AuthenticationError indicates that OpenStack authentication failed.
type AuthenticationError struct {
	UserID string
}

func (e *AuthenticationError) Error() string {
	return fmt.Sprintf("failed to authenticate as user: %s", e.UserID)
}

// UnsupportedAuthTypeError indicates that clouds.yaml uses an unsupported
// authentication method.
type UnsupportedAuthTypeError = openstackclient.UnsupportedAuthTypeError

// CatalogError indicates that the OpenStack catalog does not contain a
// required service.
type CatalogError struct {
	ServiceType string
}

func (e *CatalogError) Error() string {
	return fmt.Sprintf("service type %s not found in OpenStack service catalog", e.ServiceType)
}

// Session provides the API used by the legacy HTTP cleanup implementation.
// Gophercloud handles authentication and requests.
type Session struct {
	client *openstackclient.Client

	// httpClient supports transport tests in this package. Authenticate creates
	// sessions that use client.Request.
	httpClient *http.Client

	// SleepFunc replaces time.Sleep during polling. A nil value uses time.Sleep.
	SleepFunc func(time.Duration)
}

// Authenticate creates a Gophercloud provider from the selected clouds.yaml
// entry.
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

// doGet sends an authenticated GET request and returns the response body.
func (s *Session) doGet(ctx context.Context, url string) ([]byte, error) {
	if s.client != nil {
		response, err := s.client.Request(ctx, http.MethodGet, url, &gophercloud.RequestOpts{KeepResponseBody: true})
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		return io.ReadAll(response.Body)
	}

	// Package tests use this branch to test body read failures without an
	// authenticated provider. Sessions from Authenticate do not use this branch.
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

// doDelete sends an idempotent authenticated DELETE request.
func (s *Session) doDelete(ctx context.Context, url string) error {
	_, err := s.client.Request(ctx, http.MethodDelete, url, &gophercloud.RequestOpts{
		OkCodes: []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound},
	})
	return err
}

// isTransient reports whether cleanup must list a resource again after a
// delete request.
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
