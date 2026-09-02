package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/apierrors"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

// Service deletes the application credential ID provided by its caller.
// It does not list or select credentials.
type Service struct {
	client *gophercloud.ServiceClient
	userID string
}

var _ cleanup.ApplicationCredentialService = (*Service)(nil)

// New returns a Keystone v3 service for the authenticated user and endpoint.
func New(cloudClient *openstackclient.Client) (*Service, error) {
	if cloudClient == nil {
		return nil, errors.New("creating identity service: OpenStack client is nil")
	}
	if !cloudClient.IsAuthenticated() {
		return nil, errors.New("creating identity service: OpenStack client is not authenticated")
	}
	if cloudClient.UserID() == "" {
		return nil, errors.New("creating identity service: authenticated user ID is empty")
	}

	serviceClient, err := openstack.NewIdentityV3(cloudClient.ProviderClient(), cloudClient.EndpointOpts())
	if err != nil {
		return nil, fmt.Errorf("creating identity service client: %w", err)
	}

	return &Service{
		client: serviceClient,
		userID: cloudClient.UserID(),
	}, nil
}

// DeleteApplicationCredential deletes the application credential with the
// given ID for the authenticated user.
func (s *Service) DeleteApplicationCredential(ctx context.Context, credentialID string) error {
	if credentialID == "" {
		return errors.New("deleting application credential: ID is empty")
	}

	// Keystone documents 204 as the successful response for this request.
	// Accept it explicitly so another 2xx response cannot complete cleanup.
	_, deleteErr := s.client.Delete(
		ctx,
		s.client.ServiceURL("users", s.userID, "application_credentials", credentialID),
		&gophercloud.RequestOpts{OkCodes: []int{204}},
	)
	err := apierrors.ClassifyApplicationCredentialDelete(deleteErr)
	if err != nil {
		return fmt.Errorf("deleting application credential %q: %w", credentialID, err)
	}
	return nil
}
