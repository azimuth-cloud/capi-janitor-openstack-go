package volume

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/apierrors"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/pageutil"
)

// Service lists and deletes Cinder snapshots and volumes. The cleanup engine
// decides which resources belong to a cluster and which policies apply.
type Service struct {
	client    *gophercloud.ServiceClient
	projectID string
}

var (
	_ cleanup.SnapshotService = (*Service)(nil)
	_ cleanup.VolumeService   = (*Service)(nil)
)

// New returns a Cinder v3 service for the selected project and endpoint.
func New(client *openstackclient.Client) (*Service, error) {
	if client == nil {
		return nil, errors.New("creating block storage service: OpenStack client is nil")
	}
	if !client.IsAuthenticated() {
		return nil, errors.New("creating block storage service: OpenStack client is not authenticated")
	}
	if client.ProjectID() == "" {
		return nil, errors.New("creating block storage service: authenticated project ID is empty")
	}

	serviceClient, err := openstack.NewBlockStorageV3(client.ProviderClient(), client.EndpointOpts())
	if err != nil {
		return nil, fmt.Errorf("creating block storage service client: %w", err)
	}

	return &Service{
		client:    serviceClient,
		projectID: client.ProjectID(),
	}, nil
}

// ListSnapshots returns all snapshots from the selected project endpoint.
// The cleanup engine checks cluster ownership.
func (s *Service) ListSnapshots(ctx context.Context) ([]cleanup.Snapshot, error) {
	result := make([]cleanup.Snapshot, 0)
	pager := snapshots.ListDetail(s.client, nil).WithPageCreator(
		func(pageResult pagination.PageResult) pagination.Page {
			return pageutil.NewValidatedCollectionPage(
				snapshots.SnapshotPage{
					LinkedPageBase: pagination.LinkedPageBase{PageResult: pageResult},
				},
				"snapshots",
				pageResult.StatusCode,
			)
		},
	)
	err := pager.EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		items, err := snapshots.ExtractSnapshots(pageutil.UnwrapCollectionPage(page))
		if err != nil {
			return false, fmt.Errorf("extracting snapshot page: %w", err)
		}

		for _, item := range items {
			// Cinder scopes the list to the authenticated project. Some clouds
			// omit this extended attribute. Check it when Cinder returns it.
			if item.ProjectID != "" && item.ProjectID != s.projectID {
				continue
			}
			result = append(result, cleanup.Snapshot{
				ID:       item.ID,
				Metadata: maps.Clone(item.Metadata),
			})
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", apierrors.PreserveReauthenticationErrors(err))
	}
	return result, nil
}

// DeleteSnapshot deletes the snapshot with the given ID.
func (s *Service) DeleteSnapshot(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("deleting snapshot: ID is empty")
	}

	err := apierrors.ClassifyDelete(snapshots.Delete(ctx, s.client, id).ExtractErr())
	if err != nil {
		return fmt.Errorf("deleting snapshot %q: %w", id, err)
	}
	return nil
}

// ListVolumes returns all volumes from the selected project endpoint.
// The cleanup engine checks cluster ownership.
func (s *Service) ListVolumes(ctx context.Context) ([]cleanup.Volume, error) {
	result := make([]cleanup.Volume, 0)
	pager := volumes.List(s.client, nil).WithPageCreator(
		func(pageResult pagination.PageResult) pagination.Page {
			return pageutil.NewValidatedCollectionPage(
				volumes.VolumePage{
					LinkedPageBase: pagination.LinkedPageBase{PageResult: pageResult},
				},
				"volumes",
				pageResult.StatusCode,
			)
		},
	)
	err := pager.EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		items, err := volumes.ExtractVolumes(pageutil.UnwrapCollectionPage(page))
		if err != nil {
			return false, fmt.Errorf("extracting volume page: %w", err)
		}

		for _, item := range items {
			// Cinder scopes the list to the authenticated project. Some clouds
			// omit this extended attribute. Check it when Cinder returns it.
			if item.TenantID != "" && item.TenantID != s.projectID {
				continue
			}
			result = append(result, cleanup.Volume{
				ID:       item.ID,
				Metadata: maps.Clone(item.Metadata),
			})
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", apierrors.PreserveReauthenticationErrors(err))
	}
	return result, nil
}

// DeleteVolume deletes the volume with the given ID. It does not delete
// snapshots. The cleanup engine manages snapshot deletion.
func (s *Service) DeleteVolume(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("deleting volume: ID is empty")
	}

	err := apierrors.ClassifyDelete(volumes.Delete(ctx, s.client, id, nil).ExtractErr())
	if err != nil {
		return fmt.Errorf("deleting volume %q: %w", id, err)
	}
	return nil
}
