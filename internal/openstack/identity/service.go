package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/applicationcredentials"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/apierrors"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

// Service deletes the Keystone application credential selected by the cleanup
// engine. It does not list or select credentials.
type Service struct {
	client *gophercloud.ServiceClient
	userID string
}

var _ cleanup.ApplicationCredentialService = (*Service)(nil)

// New returns a Keystone v3 service for the authenticated user and endpoint.
func New(client *openstackclient.Client) (*Service, error) {
	if client == nil {
		return nil, errors.New("creating identity service: OpenStack client is nil")
	}
	if !client.IsAuthenticated() {
		return nil, errors.New("creating identity service: OpenStack client is not authenticated")
	}
	if client.UserID() == "" {
		return nil, errors.New("creating identity service: authenticated user ID is empty")
	}

	serviceClient, err := openstack.NewIdentityV3(client.ProviderClient(), client.EndpointOpts())
	if err != nil {
		return nil, fmt.Errorf("creating identity service client: %w", err)
	}

	return &Service{
		client: serviceClient,
		userID: client.UserID(),
	}, nil
}

// DeleteApplicationCredential deletes the application credential with the
// given ID for the authenticated user.
func (s *Service) DeleteApplicationCredential(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("deleting application credential: ID is empty")
	}

	err := apierrors.ClassifyApplicationCredentialDelete(
		applicationcredentials.Delete(ctx, s.client, s.userID, id).ExtractErr(),
	)
	if err != nil {
		return fmt.Errorf("deleting application credential %q: %w", id, err)
	}
	return nil
}
