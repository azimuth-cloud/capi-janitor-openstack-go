package openstack

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
)

func TestResourceSelectorsPreserveOwnershipBoundary(t *testing.T) {
	t.Parallel()

	purger := resourcePurger{
		clusterName: "demo",
		logger:      logr.Discard(),
		wait:        noWait,
	}

	t.Run("floating IP", func(t *testing.T) {
		service := &fakeNetworkService{floatingIPs: listSequence[cleanup.FloatingIP]{values: [][]cleanup.FloatingIP{
			{
				{ID: "owned", Description: "Floating IP for Kubernetes external service default/web from cluster demo"},
				{ID: "broad-prefix", Description: "Floating IP for Kubernetes API from cluster demo"},
				{ID: "other-cluster", Description: "Floating IP for Kubernetes external service default/web from cluster demo-2"},
				{ID: "wrong-suffix", Description: "Floating IP for Kubernetes external service default/web for cluster demo"},
				{ID: "empty"},
			},
			{
				{ID: "broad-prefix", Description: "Floating IP for Kubernetes API from cluster demo"},
				{ID: "other-cluster", Description: "Floating IP for Kubernetes external service default/web from cluster demo-2"},
			},
		}}}

		if err := purger.purgeFloatingIPs(context.Background(), service); err != nil {
			t.Fatalf("purging floating IPs: %v", err)
		}
		assertIDs(t, service.deletedFloatingIPs, "owned")
	})

	t.Run("load balancer", func(t *testing.T) {
		service := &fakeLoadBalancerService{loadBalancers: listSequence[cleanup.LoadBalancer]{values: [][]cleanup.LoadBalancer{
			{
				{ID: "owned", Name: "kube_service_demo_default_web"},
				{ID: "missing-suffix", Name: "kube_service_demo"},
				{ID: "other-cluster", Name: "kube_service_demo2_default_web"},
				{ID: "api-server", Name: "demo-control-plane"},
			},
			{},
		}}}

		if err := purger.purgeLoadBalancers(context.Background(), service); err != nil {
			t.Fatalf("purging load balancers: %v", err)
		}
		assertIDs(t, service.deleted, "owned")
	})

	t.Run("security group", func(t *testing.T) {
		service := &fakeNetworkService{securityGroups: listSequence[cleanup.SecurityGroup]{values: [][]cleanup.SecurityGroup{
			{
				{ID: "owned", Description: "Security Group for Service LoadBalancer in cluster demo"},
				{ID: "other-cluster", Description: "Security Group for Service LoadBalancer in cluster demo-2"},
				{ID: "wrong-prefix", Description: "Group for Service LoadBalancer in cluster demo"},
				{ID: "wrong-suffix", Description: "Security Group for Service LoadBalancer for cluster demo"},
				{ID: "empty"},
			},
			{},
		}}}

		if err := purger.purgeSecurityGroups(context.Background(), service); err != nil {
			t.Fatalf("purging security groups: %v", err)
		}
		assertIDs(t, service.deletedSecurityGroups, "owned")
	})

	t.Run("snapshot", func(t *testing.T) {
		service := &fakeVolumeService{snapshots: listSequence[cleanup.Snapshot]{values: [][]cleanup.Snapshot{
			{
				{ID: "owned", Metadata: map[string]string{clusterMetadataKey: "demo"}},
				{ID: "other-cluster", Metadata: map[string]string{clusterMetadataKey: "demo-2"}},
				{ID: "missing-metadata"},
			},
			{},
		}}}

		if err := purger.purgeSnapshots(context.Background(), service); err != nil {
			t.Fatalf("purging snapshots: %v", err)
		}
		assertIDs(t, service.deletedSnapshots, "owned")
	})

	t.Run("volume", func(t *testing.T) {
		service := &fakeVolumeService{volumes: listSequence[cleanup.Volume]{values: [][]cleanup.Volume{
			{
				{ID: "owned", Metadata: map[string]string{clusterMetadataKey: "demo"}},
				{ID: "case-sensitive-keep", Metadata: map[string]string{clusterMetadataKey: "demo", keepMetadataKey: "True"}},
				{ID: "kept", Metadata: map[string]string{clusterMetadataKey: "demo", keepMetadataKey: "true"}},
				{ID: "other-cluster", Metadata: map[string]string{clusterMetadataKey: "demo-2"}},
				{ID: "missing-metadata"},
			},
			{},
		}}}

		if err := purger.purgeVolumes(context.Background(), service); err != nil {
			t.Fatalf("purging volumes: %v", err)
		}
		assertIDs(t, service.deletedVolumes, "owned", "case-sensitive-keep")
	})
}

func TestPurgeResourceKindDeletionAndObservation(t *testing.T) {
	t.Parallel()

	type item struct {
		id      string
		matches bool
	}
	match := func(item item) bool { return item.matches }
	idOf := func(item item) string { return item.id }

	t.Run("empty candidate set completes without relisting", func(t *testing.T) {
		lists := listSequence[item]{values: [][]item{{{id: "other"}}}}
		deletes := 0
		waits := 0
		err := purgeResourceKind(
			context.Background(),
			logr.Discard(),
			func(context.Context, time.Duration) error { waits++; return nil },
			"test resource",
			lists.next,
			func(context.Context, string) error { deletes++; return nil },
			match,
			idOf,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lists.calls != 1 || deletes != 0 || waits != 0 {
			t.Fatalf("expected one list and no delete/wait, got lists=%d deletes=%d waits=%d", lists.calls, deletes, waits)
		}
	})

	t.Run("pending delete does not prevent later candidates", func(t *testing.T) {
		lists := listSequence[item]{values: [][]item{
			{{id: "first", matches: true}, {id: "second", matches: true}},
			{},
		}}
		var deleted []string
		err := purgeResourceKind(
			context.Background(),
			logr.Discard(),
			noWait,
			"test resource",
			lists.next,
			func(_ context.Context, id string) error {
				deleted = append(deleted, id)
				if id == "first" {
					return cleanup.ErrDeletePending
				}
				return nil
			},
			match,
			idOf,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIDs(t, deleted, "first", "second")
		if lists.calls != 2 {
			t.Fatalf("expected initial list and verification list, got %d", lists.calls)
		}
	})

	t.Run("non-pending delete error short-circuits", func(t *testing.T) {
		boom := errors.New("delete failed")
		lists := listSequence[item]{values: [][]item{{
			{id: "first", matches: true},
			{id: "second", matches: true},
		}}}
		var deleted []string
		err := purgeResourceKind(
			context.Background(),
			logr.Discard(),
			noWait,
			"test resource",
			lists.next,
			func(_ context.Context, id string) error {
				deleted = append(deleted, id)
				return boom
			},
			match,
			idOf,
		)
		if !errors.Is(err, boom) {
			t.Fatalf("expected delete cause, got %v", err)
		}
		assertIDs(t, deleted, "first")
		if lists.calls != 1 {
			t.Fatalf("expected no verification after fatal delete, got %d lists", lists.calls)
		}
	})

	t.Run("persistent candidate is observed until absent", func(t *testing.T) {
		lists := listSequence[item]{values: [][]item{
			{{id: "owned", matches: true}},
			{{id: "owned", matches: true}},
			{},
		}}
		var waits []time.Duration
		err := purgeResourceKind(
			context.Background(),
			logr.Discard(),
			func(_ context.Context, duration time.Duration) error {
				waits = append(waits, duration)
				return nil
			},
			"test resource",
			lists.next,
			func(context.Context, string) error { return nil },
			match,
			idOf,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lists.calls != 3 || !reflect.DeepEqual(waits, []time.Duration{pollInterval}) {
			t.Fatalf("expected three lists and one poll interval, got lists=%d waits=%v", lists.calls, waits)
		}
	})

	t.Run("context cancellation stops before another observation", func(t *testing.T) {
		lists := listSequence[item]{values: [][]item{{{id: "owned", matches: true}}}}
		err := purgeResourceKind(
			context.Background(),
			logr.Discard(),
			func(context.Context, time.Duration) error { return context.Canceled },
			"test resource",
			lists.next,
			func(context.Context, string) error { return nil },
			match,
			idOf,
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
		if lists.calls != 2 {
			t.Fatalf("expected initial and immediate verification lists, got %d", lists.calls)
		}
	})

	t.Run("exhausted observations return pending", func(t *testing.T) {
		lists := listSequence[item]{values: [][]item{{{id: "owned", matches: true}}}}
		waits := 0
		err := purgeResourceKind(
			context.Background(),
			logr.Discard(),
			func(context.Context, time.Duration) error { waits++; return nil },
			"test resource",
			lists.next,
			func(context.Context, string) error { return nil },
			match,
			idOf,
		)
		if !errors.Is(err, cleanup.ErrDeletePending) {
			t.Fatalf("expected pending result, got %v", err)
		}
		if lists.calls != 1+maxPollAttempts || waits != maxPollAttempts-1 {
			t.Fatalf("expected %d lists and %d waits, got lists=%d waits=%d", 1+maxPollAttempts, maxPollAttempts-1, lists.calls, waits)
		}
	})

	t.Run("verification list error discards progress", func(t *testing.T) {
		boom := errors.New("incomplete inventory")
		lists := listSequence[item]{
			values: [][]item{{{id: "owned", matches: true}}},
			errs:   []error{nil, boom},
		}
		err := purgeResourceKind(
			context.Background(),
			logr.Discard(),
			noWait,
			"test resource",
			lists.next,
			func(context.Context, string) error { return nil },
			match,
			idOf,
		)
		if !errors.Is(err, boom) {
			t.Fatalf("expected list cause, got %v", err)
		}
	})
}

func TestResourcePurgerCreatesOptionalServicesLazily(t *testing.T) {
	t.Parallel()

	network := &fakeNetworkService{}
	loadBalancer := &fakeLoadBalancerService{}
	volume := &fakeVolumeService{}
	identity := &fakeIdentityService{}
	factory := &fakeResourceFactory{
		networkService:      network,
		loadBalancerService: loadBalancer,
		volumeService:       volume,
		identityService:     identity,
	}
	purger := resourcePurger{
		factory:        factory,
		clusterName:    "demo",
		logger:         logr.Discard(),
		wait:           noWait,
		includeVolumes: false,
	}

	if err := purger.purge(context.Background()); err != nil {
		t.Fatalf("purging resources: %v", err)
	}
	if factory.networkCalls != 1 || factory.loadBalancerCalls != 0 {
		t.Fatalf("expected network only, got network=%d load-balancer=%d", factory.networkCalls, factory.loadBalancerCalls)
	}
	if factory.volumeCalls != 0 || factory.identityCalls != 0 {
		t.Fatalf("expected disabled services not to be created, got volume=%d identity=%d", factory.volumeCalls, factory.identityCalls)
	}

	purger.includeLoadBalancers = true
	if err := purger.purge(context.Background()); err != nil {
		t.Fatalf("purging resources with load balancers enabled: %v", err)
	}
	if factory.loadBalancerCalls != 1 {
		t.Fatalf("expected one load balancer service creation, got %d", factory.loadBalancerCalls)
	}

	purger.includeLoadBalancers = false
	purger.includeAppCredential = true
	purger.applicationCredentialID = ""
	if err := purger.purge(context.Background()); err != nil {
		t.Fatalf("purging resources for password auth: %v", err)
	}
	if factory.identityCalls != 0 {
		t.Fatalf("expected no identity service for password auth, got %d calls", factory.identityCalls)
	}
}

func TestResourcePurgerRequiresLoadBalancerEndpointOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	network := &fakeNetworkService{}
	endpointErr := errors.New("load balancer endpoint not found")
	factory := &fakeResourceFactory{
		networkService:  network,
		loadBalancerErr: endpointErr,
	}
	purger := resourcePurger{
		factory:     factory,
		clusterName: "demo",
		logger:      logr.Discard(),
		wait:        noWait,
	}

	if err := purger.purge(context.Background()); err != nil {
		t.Fatalf("disabled load balancer phase unexpectedly required endpoint: %v", err)
	}
	if factory.loadBalancerCalls != 0 {
		t.Fatalf("expected disabled load balancer client not to be created, got %d calls", factory.loadBalancerCalls)
	}

	purger.includeLoadBalancers = true
	err := purger.purge(context.Background())
	if err == nil || !errors.Is(err, endpointErr) {
		t.Fatalf("expected enabled load balancer phase to require endpoint, got %v", err)
	}
}

func TestResourcePurgerApplicationCredentialContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "success"},
		{name: "self-delete forbidden is accepted", err: cleanup.ErrApplicationCredentialForbidden},
		{name: "other error blocks cleanup", err: errors.New("identity failed"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := &fakeIdentityService{err: tt.err}
			factory := &fakeResourceFactory{
				networkService:  &fakeNetworkService{},
				identityService: identity,
			}
			purger := resourcePurger{
				factory:                 factory,
				clusterName:             "demo",
				includeAppCredential:    true,
				applicationCredentialID: "selected-id",
				logger:                  logr.Discard(),
				wait:                    noWait,
			}

			err := purger.purge(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("expected error=%v, got %v", tt.wantErr, err)
			}
			assertIDs(t, identity.deleted, "selected-id")
		})
	}
}

type listSequence[T any] struct {
	values [][]T
	errs   []error
	calls  int
}

func (s *listSequence[T]) next(context.Context) ([]T, error) {
	index := s.calls
	s.calls++
	if index < len(s.errs) && s.errs[index] != nil {
		return nil, s.errs[index]
	}
	if len(s.values) == 0 {
		return nil, nil
	}
	if index >= len(s.values) {
		index = len(s.values) - 1
	}
	return s.values[index], nil
}

type fakeNetworkService struct {
	floatingIPs            listSequence[cleanup.FloatingIP]
	securityGroups         listSequence[cleanup.SecurityGroup]
	deletedFloatingIPs     []string
	deletedSecurityGroups  []string
	floatingIPDeleteErr    error
	securityGroupDeleteErr error
}

func (s *fakeNetworkService) ListFloatingIPs(ctx context.Context) ([]cleanup.FloatingIP, error) {
	return s.floatingIPs.next(ctx)
}

func (s *fakeNetworkService) DeleteFloatingIP(_ context.Context, id string) error {
	s.deletedFloatingIPs = append(s.deletedFloatingIPs, id)
	return s.floatingIPDeleteErr
}

func (s *fakeNetworkService) ListSecurityGroups(ctx context.Context) ([]cleanup.SecurityGroup, error) {
	return s.securityGroups.next(ctx)
}

func (s *fakeNetworkService) DeleteSecurityGroup(_ context.Context, id string) error {
	s.deletedSecurityGroups = append(s.deletedSecurityGroups, id)
	return s.securityGroupDeleteErr
}

type fakeLoadBalancerService struct {
	loadBalancers listSequence[cleanup.LoadBalancer]
	deleted       []string
	deleteErr     error
}

func (s *fakeLoadBalancerService) ListLoadBalancers(ctx context.Context) ([]cleanup.LoadBalancer, error) {
	return s.loadBalancers.next(ctx)
}

func (s *fakeLoadBalancerService) DeleteLoadBalancer(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return s.deleteErr
}

type fakeVolumeService struct {
	snapshots         listSequence[cleanup.Snapshot]
	volumes           listSequence[cleanup.Volume]
	deletedSnapshots  []string
	deletedVolumes    []string
	snapshotDeleteErr error
	volumeDeleteErr   error
}

func (s *fakeVolumeService) ListSnapshots(ctx context.Context) ([]cleanup.Snapshot, error) {
	return s.snapshots.next(ctx)
}

func (s *fakeVolumeService) DeleteSnapshot(_ context.Context, id string) error {
	s.deletedSnapshots = append(s.deletedSnapshots, id)
	return s.snapshotDeleteErr
}

func (s *fakeVolumeService) ListVolumes(ctx context.Context) ([]cleanup.Volume, error) {
	return s.volumes.next(ctx)
}

func (s *fakeVolumeService) DeleteVolume(_ context.Context, id string) error {
	s.deletedVolumes = append(s.deletedVolumes, id)
	return s.volumeDeleteErr
}

type fakeIdentityService struct {
	deleted []string
	err     error
}

func (s *fakeIdentityService) DeleteApplicationCredential(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return s.err
}

type fakeResourceFactory struct {
	networkService      networkService
	loadBalancerService cleanup.LoadBalancerService
	volumeService       volumeService
	identityService     cleanup.ApplicationCredentialService
	networkErr          error
	loadBalancerErr     error
	volumeErr           error
	identityErr         error
	networkCalls        int
	loadBalancerCalls   int
	volumeCalls         int
	identityCalls       int
}

func (f *fakeResourceFactory) network() (networkService, error) {
	f.networkCalls++
	return f.networkService, f.networkErr
}

func (f *fakeResourceFactory) loadBalancer() (cleanup.LoadBalancerService, error) {
	f.loadBalancerCalls++
	return f.loadBalancerService, f.loadBalancerErr
}

func (f *fakeResourceFactory) volume() (volumeService, error) {
	f.volumeCalls++
	return f.volumeService, f.volumeErr
}

func (f *fakeResourceFactory) identity() (cleanup.ApplicationCredentialService, error) {
	f.identityCalls++
	return f.identityService, f.identityErr
}

func noWait(context.Context, time.Duration) error {
	return nil
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected IDs\nwant: %v\n got: %v", want, got)
	}
}
