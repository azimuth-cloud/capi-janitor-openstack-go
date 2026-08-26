package loadbalancer

import (
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/apierrors"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/pageutil"
)

var _ cleanup.LoadBalancerService = (*Service)(nil)

// Service uses Octavia to list and delete load balancers in the authenticated
// project.
type Service struct {
	client    *gophercloud.ServiceClient
	projectID string
}

// New returns an Octavia service for the authenticated project.
func New(client *openstackclient.Client) (*Service, error) {
	if client == nil {
		return nil, errors.New("creating load balancer service: OpenStack client is nil")
	}
	if !client.IsAuthenticated() {
		return nil, errors.New("creating load balancer service: OpenStack client is not authenticated")
	}
	if client.ProjectID() == "" {
		return nil, errors.New("creating load balancer service: project ID is empty")
	}

	serviceClient, err := openstack.NewLoadBalancerV2(client.ProviderClient(), client.EndpointOpts())
	if err != nil {
		return nil, fmt.Errorf("creating load balancer service client: %w", err)
	}

	return &Service{
		client:    serviceClient,
		projectID: client.ProjectID(),
	}, nil
}

// ListLoadBalancers lists all load balancers in the authenticated project.
func (s *Service) ListLoadBalancers(ctx context.Context) ([]cleanup.LoadBalancer, error) {
	var listedLoadBalancers []cleanup.LoadBalancer
	pager := loadbalancers.List(s.client, loadbalancers.ListOpts{
		ProjectID: s.projectID,
	}).WithPageCreator(func(pageResult pagination.PageResult) pagination.Page {
		return pageutil.NewValidatedCollectionPage(
			loadbalancers.LoadBalancerPage{
				LinkedPageBase: pagination.LinkedPageBase{PageResult: pageResult},
			},
			"loadbalancers",
			pageResult.StatusCode,
		)
	})
	err := pager.EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		protectionFields, err := extractLoadBalancerProtectionFields(page)
		if err != nil {
			return false, err
		}

		decodedLoadBalancers, err := loadbalancers.ExtractLoadBalancers(pageutil.UnwrapCollectionPage(page))
		if err != nil {
			return false, fmt.Errorf("extracting load balancer page: %w", err)
		}
		if len(decodedLoadBalancers) != len(protectionFields) {
			return false, fmt.Errorf(
				"validating load balancer page: extracted %d resources from %d raw resources",
				len(decodedLoadBalancers),
				len(protectionFields),
			)
		}

		for index, loadBalancer := range decodedLoadBalancers {
			if loadBalancer.ProjectID == "" {
				return false, fmt.Errorf("validating loadbalancer %q project ownership: project_id is empty", loadBalancer.ID)
			}
			if loadBalancer.ProjectID != s.projectID {
				continue
			}
			listedLoadBalancers = append(listedLoadBalancers, cleanup.LoadBalancer{
				ID:        loadBalancer.ID,
				Name:      loadBalancer.Name,
				Tags:      append([]string(nil), protectionFields[index].tags...),
				VIPPortID: protectionFields[index].vipPortID,
			})
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing load balancers: %w", apierrors.PreserveReauthenticationErrors(err))
	}

	return listedLoadBalancers, nil
}

type loadBalancerProtectionFields struct {
	tags      []string
	vipPortID string
}

func extractLoadBalancerProtectionFields(page pagination.Page) ([]loadBalancerProtectionFields, error) {
	responseBody, ok := page.GetBody().(map[string]any)
	if !ok {
		return nil, fmt.Errorf("validating load balancer protection fields: response body must be an object, got %T", page.GetBody())
	}

	rawLoadBalancers, ok := responseBody["loadbalancers"].([]any)
	if !ok {
		return nil, fmt.Errorf("validating load balancer protection fields: loadbalancers must be an array, got %T", responseBody["loadbalancers"])
	}

	protectionFields := make([]loadBalancerProtectionFields, len(rawLoadBalancers))
	for index, rawLoadBalancer := range rawLoadBalancers {
		loadBalancerFields, ok := rawLoadBalancer.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("validating load balancer protection fields: loadbalancers[%d] must be an object, got %T", index, rawLoadBalancer)
		}

		tagsValue, hasTags := loadBalancerFields["tags"]
		if !hasTags {
			return nil, fmt.Errorf("validating load balancer protection fields: loadbalancers[%d].tags is missing", index)
		}
		rawTags, ok := tagsValue.([]any)
		if !ok {
			return nil, fmt.Errorf(
				"validating load balancer protection fields: loadbalancers[%d].tags must be an array, got %T",
				index,
				tagsValue,
			)
		}
		tags := make([]string, len(rawTags))
		for tagIndex, rawTag := range rawTags {
			tag, ok := rawTag.(string)
			if !ok {
				return nil, fmt.Errorf(
					"validating load balancer protection fields: loadbalancers[%d].tags[%d] must be a string, got %T",
					index,
					tagIndex,
					rawTag,
				)
			}
			tags[tagIndex] = tag
		}

		rawVIPPortID, hasVIPPortID := loadBalancerFields["vip_port_id"]
		if !hasVIPPortID {
			return nil, fmt.Errorf("validating load balancer protection fields: loadbalancers[%d].vip_port_id is missing", index)
		}
		vipPortID, ok := rawVIPPortID.(string)
		if !ok {
			return nil, fmt.Errorf(
				"validating load balancer protection fields: loadbalancers[%d].vip_port_id must be a string, got %T",
				index,
				rawVIPPortID,
			)
		}

		protectionFields[index] = loadBalancerProtectionFields{
			tags:      tags,
			vipPortID: vipPortID,
		}
	}

	return protectionFields, nil
}

// DeleteLoadBalancer deletes a load balancer and its child resources.
func (s *Service) DeleteLoadBalancer(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("deleting load balancer: ID is empty")
	}

	err := apierrors.ClassifyDelete(loadbalancers.Delete(ctx, s.client, id, loadbalancers.DeleteOpts{
		Cascade: true,
	}).ExtractErr())
	if err != nil {
		return fmt.Errorf("deleting load balancer %q: %w", id, err)
	}
	return nil
}
