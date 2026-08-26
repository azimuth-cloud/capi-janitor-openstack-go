package network

import (
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/apierrors"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/pageutil"
)

var (
	_ cleanup.FloatingIPService    = (*Service)(nil)
	_ cleanup.SecurityGroupService = (*Service)(nil)
)

// Service uses Neutron to list and delete network resources in the
// authenticated project.
type Service struct {
	client    *gophercloud.ServiceClient
	projectID string
}

// New returns a Neutron service for the authenticated project.
func New(client *openstackclient.Client) (*Service, error) {
	if client == nil {
		return nil, errors.New("creating network service: OpenStack client is nil")
	}
	if !client.IsAuthenticated() {
		return nil, errors.New("creating network service: OpenStack client is not authenticated")
	}
	if client.ProjectID() == "" {
		return nil, errors.New("creating network service: project ID is empty")
	}

	serviceClient, err := openstack.NewNetworkV2(client.ProviderClient(), client.EndpointOpts())
	if err != nil {
		return nil, fmt.Errorf("creating network service client: %w", err)
	}

	return &Service{
		client:    serviceClient,
		projectID: client.ProjectID(),
	}, nil
}

// ListFloatingIPs lists all floating IPs in the authenticated project.
func (s *Service) ListFloatingIPs(ctx context.Context) ([]cleanup.FloatingIP, error) {
	var listedFloatingIPs []cleanup.FloatingIP
	pager := floatingips.List(s.client, floatingips.ListOpts{ProjectID: s.projectID}).WithPageCreator(
		func(pageResult pagination.PageResult) pagination.Page {
			return pageutil.NewValidatedCollectionPage(
				floatingips.FloatingIPPage{
					LinkedPageBase: pagination.LinkedPageBase{PageResult: pageResult},
				},
				"floatingips",
				pageResult.StatusCode,
			)
		},
	)
	err := pager.EachPage(
		ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			attachedPortIDs, err := extractFloatingIPAttachedPortIDs(page)
			if err != nil {
				return false, err
			}

			decodedFloatingIPs, err := floatingips.ExtractFloatingIPs(pageutil.UnwrapCollectionPage(page))
			if err != nil {
				return false, fmt.Errorf("extracting floating IP page: %w", err)
			}
			if len(decodedFloatingIPs) != len(attachedPortIDs) {
				return false, fmt.Errorf(
					"validating floating IP page: extracted %d resources from %d raw resources",
					len(decodedFloatingIPs),
					len(attachedPortIDs),
				)
			}

			for index, floatingIP := range decodedFloatingIPs {
				belongsToSelectedProject, err := resourceBelongsToProject(floatingIP.ProjectID, floatingIP.TenantID, s.projectID)
				if err != nil {
					return false, fmt.Errorf("validating floating IP %q project ownership: %w", floatingIP.ID, err)
				}
				if !belongsToSelectedProject {
					continue
				}
				listedFloatingIPs = append(listedFloatingIPs, cleanup.FloatingIP{
					ID:             floatingIP.ID,
					Description:    floatingIP.Description,
					AttachedPortID: attachedPortIDs[index],
				})
			}

			return true, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("listing floating IPs: %w", apierrors.PreserveReauthenticationErrors(err))
	}
	return listedFloatingIPs, nil
}

func extractFloatingIPAttachedPortIDs(page pagination.Page) ([]string, error) {
	responseBody, ok := page.GetBody().(map[string]any)
	if !ok {
		return nil, fmt.Errorf("validating floating IP attached port IDs: response body must be an object, got %T", page.GetBody())
	}

	rawFloatingIPs, ok := responseBody["floatingips"].([]any)
	if !ok {
		return nil, fmt.Errorf("validating floating IP attached port IDs: floatingips must be an array, got %T", responseBody["floatingips"])
	}

	attachedPortIDs := make([]string, len(rawFloatingIPs))
	for index, rawFloatingIP := range rawFloatingIPs {
		floatingIPFields, ok := rawFloatingIP.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("validating floating IP attached port IDs: floatingips[%d] must be an object, got %T", index, rawFloatingIP)
		}

		rawPortID, hasPortID := floatingIPFields["port_id"]
		if !hasPortID {
			return nil, fmt.Errorf("validating floating IP attached port IDs: floatingips[%d].port_id is missing", index)
		}
		if rawPortID == nil {
			continue
		}

		portID, ok := rawPortID.(string)
		if !ok {
			return nil, fmt.Errorf(
				"validating floating IP attached port IDs: floatingips[%d].port_id must be a string or null, got %T",
				index,
				rawPortID,
			)
		}
		attachedPortIDs[index] = portID
	}

	return attachedPortIDs, nil
}

// DeleteFloatingIP deletes the floating IP with the given ID.
func (s *Service) DeleteFloatingIP(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("deleting floating IP: ID is empty")
	}

	err := apierrors.ClassifyDelete(floatingips.Delete(ctx, s.client, id).ExtractErr())
	if err != nil {
		return fmt.Errorf("deleting floating IP %q: %w", id, err)
	}
	return nil
}

// ListSecurityGroups lists all security groups in the authenticated project.
func (s *Service) ListSecurityGroups(ctx context.Context) ([]cleanup.SecurityGroup, error) {
	var result []cleanup.SecurityGroup
	pager := groups.List(s.client, groups.ListOpts{ProjectID: s.projectID}).WithPageCreator(
		func(pageResult pagination.PageResult) pagination.Page {
			return pageutil.NewValidatedCollectionPage(
				groups.SecGroupPage{
					LinkedPageBase: pagination.LinkedPageBase{PageResult: pageResult},
				},
				"security_groups",
				pageResult.StatusCode,
			)
		},
	)
	err := pager.EachPage(
		ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			pageSecurityGroups, err := groups.ExtractGroups(pageutil.UnwrapCollectionPage(page))
			if err != nil {
				return false, fmt.Errorf("extracting security group page: %w", err)
			}

			for _, securityGroup := range pageSecurityGroups {
				belongs, err := resourceBelongsToProject(securityGroup.ProjectID, securityGroup.TenantID, s.projectID)
				if err != nil {
					return false, fmt.Errorf("validating security group %q project ownership: %w", securityGroup.ID, err)
				}
				if !belongs {
					continue
				}
				result = append(result, cleanup.SecurityGroup{
					ID:          securityGroup.ID,
					Description: securityGroup.Description,
				})
			}

			return true, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("listing security groups: %w", apierrors.PreserveReauthenticationErrors(err))
	}
	return result, nil
}

// DeleteSecurityGroup deletes the security group with the given ID.
func (s *Service) DeleteSecurityGroup(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("deleting security group: ID is empty")
	}

	err := apierrors.ClassifyDelete(groups.Delete(ctx, s.client, id).ExtractErr())
	if err != nil {
		return fmt.Errorf("deleting security group %q: %w", id, err)
	}
	return nil
}

func resourceBelongsToProject(projectID, tenantID, selectedProjectID string) (bool, error) {
	if projectID == "" && tenantID == "" {
		return false, errors.New("both project_id and tenant_id are empty")
	}
	if projectID != "" && tenantID != "" && projectID != tenantID {
		return false, fmt.Errorf("project_id %q conflicts with tenant_id %q", projectID, tenantID)
	}
	if projectID != "" {
		return projectID == selectedProjectID, nil
	}
	return tenantID == selectedProjectID, nil
}
