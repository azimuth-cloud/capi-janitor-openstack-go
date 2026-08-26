package openstack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

type cleanupFixture struct {
	server *httptest.Server

	mu               sync.Mutex
	networkInCatalog bool
	octaviaInCatalog bool
	cinderInCatalog  bool
	floatingIPs      []map[string]any
	loadBalancers    []map[string]any
	securityGroups   []map[string]any
	snapshots        []map[string]any
	volumes          []map[string]any
	requestLog       []string
	deletePaths      []string
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	t.Helper()

	fixture := &cleanupFixture{
		networkInCatalog: true,
		octaviaInCatalog: true,
		cinderInCatalog:  true,
		floatingIPs:      []map[string]any{},
		loadBalancers:    []map[string]any{},
		securityGroups:   []map[string]any{},
		snapshots:        []map[string]any{},
		volumes:          []map[string]any{},
	}
	mux := http.NewServeMux()
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.requestLog = append(fixture.requestLog, r.Method+" "+r.URL.Path)
		fixture.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(fixture.server.Close)

	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Subject-Token", "token-1")
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"token": map[string]any{
				"user":    map[string]any{"id": "user-1"},
				"project": map[string]any{"id": "project-1"},
				"catalog": []any{},
			},
		})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		networkInCatalog := fixture.networkInCatalog
		octaviaInCatalog := fixture.octaviaInCatalog
		cinderInCatalog := fixture.cinderInCatalog
		fixture.mu.Unlock()

		catalog := make([]any, 0, 3)
		if networkInCatalog {
			catalog = append(catalog, newCatalogEntry("network", fixture.server.URL+"/network/"))
		}
		if octaviaInCatalog {
			catalog = append(catalog, newCatalogEntry("load-balancer", fixture.server.URL+"/octavia/v2.0/"))
		}
		if cinderInCatalog {
			catalog = append(catalog, newCatalogEntry("volumev3", fixture.server.URL+"/cinder/v3/project-1/"))
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"catalog": catalog})
	})
	mux.HandleFunc("/network/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"versions": []any{map[string]any{"id": "v2.0", "status": "CURRENT"}},
		})
	})
	mux.HandleFunc("/cinder/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"versions": []any{map[string]any{"id": "v3.0", "status": "CURRENT"}},
		})
	})

	mux.HandleFunc("/network/v2.0/floatingips", fixture.newListHandler(t, "floatingips", func() []map[string]any {
		return fixture.floatingIPs
	}))
	mux.HandleFunc("/network/v2.0/floatingips/", fixture.handleDelete)
	mux.HandleFunc("/network/v2.0/security-groups", fixture.newListHandler(t, "security_groups", func() []map[string]any {
		return fixture.securityGroups
	}))
	mux.HandleFunc("/network/v2.0/security-groups/", fixture.handleDelete)
	mux.HandleFunc("/octavia/v2.0/lbaas/loadbalancers", fixture.newListHandler(t, "loadbalancers", func() []map[string]any {
		return fixture.loadBalancers
	}))
	mux.HandleFunc("/octavia/v2.0/lbaas/loadbalancers/", fixture.handleDelete)
	mux.HandleFunc("/cinder/v3/project-1/snapshots/detail", fixture.newListHandler(t, "snapshots", func() []map[string]any {
		return fixture.snapshots
	}))
	mux.HandleFunc("/cinder/v3/project-1/snapshots/", fixture.handleDelete)
	mux.HandleFunc("/cinder/v3/project-1/volumes/detail", fixture.newListHandler(t, "volumes", func() []map[string]any {
		return fixture.volumes
	}))
	mux.HandleFunc("/cinder/v3/project-1/volumes/", fixture.handleDelete)

	return fixture
}

func (f *cleanupFixture) newListHandler(
	t *testing.T,
	responseKey string,
	getItems func() []map[string]any,
) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET for %s inventory, got %s", responseKey, r.Method)
		}
		f.mu.Lock()
		items := slices.Clone(getItems())
		f.mu.Unlock()
		writeJSON(t, w, http.StatusOK, map[string]any{responseKey: items})
	}
}

func (f *cleanupFixture) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	f.deletePaths = append(f.deletePaths, r.URL.Path)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *cleanupFixture) buildOptions() openstack.PurgeOptions {
	return openstack.PurgeOptions{
		CloudsYAML: fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s/v3
      application_credential_id: appcred-1
      application_credential_secret: secret
    region_name: RegionOne
    interface: public
`, f.server.URL),
		CloudName:   "openstack",
		ClusterName: "demo",
	}
}

func newCatalogEntry(serviceType, endpoint string) map[string]any {
	return map[string]any{
		"id":   serviceType,
		"name": serviceType,
		"type": serviceType,
		"endpoints": []any{map[string]any{
			"interface": "public",
			"region_id": "RegionOne",
			"url":       endpoint,
		}},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func TestPurgeResourcesUsesTypedServices(t *testing.T) {
	fixture := newCleanupFixture(t)
	options := fixture.buildOptions()

	if err := openstack.PurgeResources(context.Background(), options); err != nil {
		t.Fatalf("purging empty project: %v", err)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.deletePaths) != 0 {
		t.Fatalf("expected no deletion requests, got %v", fixture.deletePaths)
	}
	if !slices.Contains(fixture.requestLog, "GET /octavia/v2.0/lbaas/loadbalancers") {
		t.Fatalf("expected typed Octavia inventory request, got %v", fixture.requestLog)
	}
	if !slices.Contains(fixture.requestLog, "GET /network/v2.0/floatingips") {
		t.Fatalf("expected typed Neutron inventory request, got %v", fixture.requestLog)
	}
	if !slices.Contains(fixture.requestLog, "GET /network/v2.0/security-groups") {
		t.Fatalf("expected typed security group inventory request, got %v", fixture.requestLog)
	}
	if slices.Contains(fixture.requestLog, "GET /cinder/v3/project-1/snapshots/detail") {
		t.Fatalf("did not expect Cinder inventory when volume deletion is disabled: %v", fixture.requestLog)
	}
}

func TestPurgeResourcesStopsAfterOnePhase(t *testing.T) {
	fixture := newCleanupFixture(t)
	fixture.floatingIPs = []map[string]any{
		{
			"id":          "fip-1",
			"description": "Floating IP for Kubernetes external service default/web from cluster demo",
			"project_id":  "project-1",
			"port_id":     nil,
		},
	}

	err := openstack.PurgeResources(context.Background(), fixture.buildOptions())
	if !errors.Is(err, cleanup.ErrDeletePending) {
		t.Fatalf("expected pending cleanup after floating IP deletion, got %v", err)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	want := []string{"/network/v2.0/floatingips/fip-1"}
	if !slices.Equal(fixture.deletePaths, want) {
		t.Fatalf("expected one floating IP deletion, got %v", fixture.deletePaths)
	}
	if slices.Contains(fixture.requestLog, "GET /network/v2.0/security-groups") {
		t.Fatalf("security group phase ran before floating IP verification: %v", fixture.requestLog)
	}
}

func TestPurgeResourcesBlocksWithoutOctavia(t *testing.T) {
	fixture := newCleanupFixture(t)
	fixture.octaviaInCatalog = false
	fixture.floatingIPs = []map[string]any{
		{
			"id":          "fip-1",
			"description": "Floating IP for Kubernetes external service default/web from cluster demo",
			"project_id":  "project-1",
			"port_id":     nil,
		},
	}

	err := openstack.PurgeResources(context.Background(), fixture.buildOptions())
	if err == nil {
		t.Fatal("expected missing Octavia endpoint to block cleanup")
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.deletePaths) != 0 {
		t.Fatalf("expected no mutation when Octavia is unavailable, got %v", fixture.deletePaths)
	}
}

func TestPurgeResourcesKeepsSharedLoadBalancer(t *testing.T) {
	fixture := newCleanupFixture(t)
	fixture.loadBalancers = []map[string]any{
		{
			"id":          "lb-shared",
			"name":        "kube_service_demo_default_web",
			"project_id":  "project-1",
			"vip_port_id": "vip-shared",
			"tags": []any{
				"kube_service_demo_default_web",
				"kube_service_other_default_api",
			},
		},
	}
	fixture.floatingIPs = []map[string]any{
		{
			"id":          "fip-shared",
			"description": "Floating IP for Kubernetes external service default/web from cluster demo",
			"project_id":  "project-1",
			"port_id":     "vip-shared",
		},
	}

	if err := openstack.PurgeResources(context.Background(), fixture.buildOptions()); err != nil {
		t.Fatalf("purging project with shared load balancer: %v", err)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.deletePaths) != 0 {
		t.Fatalf("expected shared load balancer and floating IP to be preserved, got %v", fixture.deletePaths)
	}
}

func TestPurgeResourcesUsesCinderForVolumes(t *testing.T) {
	withCinder := newCleanupFixture(t)
	options := withCinder.buildOptions()
	options.DeleteVolumes = true
	if err := openstack.PurgeResources(context.Background(), options); err != nil {
		t.Fatalf("cleanup with volume deletion: %v", err)
	}
	withCinder.mu.Lock()
	if !slices.Contains(withCinder.requestLog, "GET /cinder/v3/project-1/snapshots/detail") ||
		!slices.Contains(withCinder.requestLog, "GET /cinder/v3/project-1/volumes/detail") {
		t.Fatalf("expected typed Cinder inventory, got %v", withCinder.requestLog)
	}
	withCinder.mu.Unlock()

	fixture := newCleanupFixture(t)
	fixture.cinderInCatalog = false

	withoutVolumes := fixture.buildOptions()
	if err := openstack.PurgeResources(context.Background(), withoutVolumes); err != nil {
		t.Fatalf("cleanup without volume deletion required Cinder: %v", err)
	}

	missingCinder := newCleanupFixture(t)
	missingCinder.cinderInCatalog = false
	missingCinder.floatingIPs = []map[string]any{
		{
			"id":          "fip-1",
			"description": "Floating IP for Kubernetes external service default/web from cluster demo",
			"project_id":  "project-1",
			"port_id":     nil,
		},
	}
	withVolumes := missingCinder.buildOptions()
	withVolumes.DeleteVolumes = true
	if err := openstack.PurgeResources(context.Background(), withVolumes); err == nil {
		t.Fatal("expected enabled volume cleanup to require Cinder")
	}
	missingCinder.mu.Lock()
	defer missingCinder.mu.Unlock()
	if len(missingCinder.deletePaths) != 0 {
		t.Fatalf("expected missing Cinder to block every mutation, got %v", missingCinder.deletePaths)
	}
}
