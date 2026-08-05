package openstack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

const purgeProjectID = "purge-project"

// purgeTestServer advertises version-aware service endpoints and records the
// typed Gophercloud requests made by PurgeResources. Successful deletes remove
// resources from subsequent list responses so the compatibility purger can
// verify absence without waiting.
type purgeTestServer struct {
	*httptest.Server
	t  *testing.T
	mu sync.Mutex

	tokenStatus  int
	endpoints    map[string]bool
	resources    map[string][]map[string]any
	fipPageTwo   []map[string]any
	listStatus   map[string]int
	deleteStatus map[string]int

	listCalls       map[string]int
	deleted         map[string][]string
	deleteQueries   map[string][]url.Values
	requestOrder    []string
	unknownRequests []string
	deletedUserID   string
	deletedAppcred  string
}

func newPurgeTestServer(t *testing.T) *purgeTestServer {
	t.Helper()

	srv := &purgeTestServer{
		t:           t,
		tokenStatus: http.StatusCreated,
		endpoints: map[string]bool{
			"network":       true,
			"load-balancer": true,
			"volumev3":      true,
			"identity":      true,
		},
		resources:     make(map[string][]map[string]any),
		listStatus:    make(map[string]int),
		deleteStatus:  make(map[string]int),
		listCalls:     make(map[string]int),
		deleted:       make(map[string][]string),
		deleteQueries: make(map[string][]url.Values),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", srv.handleTokens)
	mux.HandleFunc("/v3/auth/catalog", srv.handleCatalog)
	mux.HandleFunc("/v2.0/floatingips", srv.handleList("floatingips", true))
	mux.HandleFunc("/v2.0/floatingips/", srv.handleDelete("floatingips", "/v2.0/floatingips/"))
	mux.HandleFunc("/v2.0/lbaas/loadbalancers", srv.handleList("loadbalancers", true))
	mux.HandleFunc("/v2.0/lbaas/loadbalancers/", srv.handleDelete("loadbalancers", "/v2.0/lbaas/loadbalancers/"))
	mux.HandleFunc("/v2.0/security-groups", srv.handleList("security_groups", true))
	mux.HandleFunc("/v2.0/security-groups/", srv.handleDelete("security_groups", "/v2.0/security-groups/"))
	mux.HandleFunc("/v3/"+purgeProjectID+"/snapshots/detail", srv.handleList("snapshots", false))
	mux.HandleFunc("/v3/"+purgeProjectID+"/snapshots/", srv.handleDelete("snapshots", "/v3/"+purgeProjectID+"/snapshots/"))
	mux.HandleFunc("/v3/"+purgeProjectID+"/volumes/detail", srv.handleList("volumes", false))
	mux.HandleFunc("/v3/"+purgeProjectID+"/volumes/", srv.handleDelete("volumes", "/v3/"+purgeProjectID+"/volumes/"))
	mux.HandleFunc("/v3/users/", srv.handleApplicationCredentialDelete)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			srv.writeJSON(w, map[string]any{
				"versions": []any{
					map[string]any{"id": "v2.0", "status": "CURRENT"},
					map[string]any{"id": "v3.0", "status": "CURRENT"},
				},
			})
			return
		}
		srv.mu.Lock()
		srv.unknownRequests = append(srv.unknownRequests, r.Method+" "+r.URL.RequestURI())
		srv.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})

	srv.Server = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *purgeTestServer) handleTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	status := s.tokenStatus
	s.requestOrder = append(s.requestOrder, "authenticate")
	s.mu.Unlock()
	if status >= http.StatusBadRequest {
		w.WriteHeader(status)
		return
	}

	w.Header().Set("X-Subject-Token", "purge-token")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	s.writeJSON(w, map[string]any{
		"token": map[string]any{
			"user":    map[string]any{"id": "purge-user"},
			"project": map[string]any{"id": purgeProjectID},
			"catalog": []any{},
		},
	})
}

func (s *purgeTestServer) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	endpoints := make(map[string]bool, len(s.endpoints))
	for serviceType, enabled := range s.endpoints {
		endpoints[serviceType] = enabled
	}
	s.requestOrder = append(s.requestOrder, "catalog")
	s.mu.Unlock()

	entries := make([]any, 0, len(endpoints))
	for _, serviceType := range []string{"network", "load-balancer", "volumev3", "identity"} {
		if !endpoints[serviceType] {
			continue
		}
		endpointURL := s.URL
		if serviceType == "volumev3" {
			endpointURL += "/v3/" + purgeProjectID
		}
		entries = append(entries, map[string]any{
			"type": serviceType,
			"endpoints": []any{map[string]any{
				"interface": "public",
				"region_id": "RegionOne",
				"url":       endpointURL,
			}},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	s.writeJSON(w, map[string]any{"catalog": entries})
}

func (s *purgeTestServer) handleList(kind string, assertProjectQuery bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if assertProjectQuery && r.URL.Query().Get("project_id") != purgeProjectID {
			s.t.Errorf("%s list used project_id %q, want %q", kind, r.URL.Query().Get("project_id"), purgeProjectID)
		}

		s.mu.Lock()
		s.listCalls[kind]++
		s.requestOrder = append(s.requestOrder, "list:"+kind)
		status := s.listStatus[kind]
		items := append([]map[string]any{}, s.resources[kind]...)
		secondPage := append([]map[string]any{}, s.fipPageTwo...)
		s.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			return
		}

		response := map[string]any{kind: items}
		if kind == "floatingips" && len(secondPage) > 0 {
			if r.URL.Query().Get("page") == "2" {
				response[kind] = secondPage
				response["floatingips_links"] = []any{}
			} else {
				response["floatingips_links"] = []any{map[string]any{
					"rel":  "next",
					"href": s.URL + "/v2.0/floatingips?page=2&project_id=" + purgeProjectID,
				}}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		s.writeJSON(w, response)
	}
}

func (s *purgeTestServer) handleDelete(kind, pathPrefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, pathPrefix)
		if id == "" || strings.Contains(id, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		s.mu.Lock()
		s.deleted[kind] = append(s.deleted[kind], id)
		s.deleteQueries[kind] = append(s.deleteQueries[kind], r.URL.Query())
		s.requestOrder = append(s.requestOrder, "delete:"+kind+":"+id)
		status := s.deleteStatus[kind+"/"+id]
		if status == 0 {
			status = http.StatusNoContent
		}
		if status == http.StatusNotFound || status >= http.StatusOK && status < http.StatusMultipleChoices {
			s.resources[kind] = removeResource(s.resources[kind], id)
			if kind == "floatingips" {
				s.fipPageTwo = removeResource(s.fipPageTwo, id)
			}
		}
		s.mu.Unlock()

		w.WriteHeader(status)
	}
}

func (s *purgeTestServer) handleApplicationCredentialDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	const prefix = "/v3/users/"
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/application_credentials/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	s.mu.Lock()
	s.deletedUserID = parts[0]
	s.deletedAppcred = parts[1]
	s.requestOrder = append(s.requestOrder, "delete:application_credentials:"+parts[1])
	status := s.deleteStatus["application_credentials/"+parts[1]]
	if status == 0 {
		status = http.StatusNoContent
	}
	s.mu.Unlock()
	w.WriteHeader(status)
}

func (s *purgeTestServer) writeJSON(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		s.t.Errorf("encoding fixture response: %v", err)
	}
}

func (s *purgeTestServer) setEndpoint(serviceType string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoints[serviceType] = enabled
}

func (s *purgeTestServer) setResources(kind string, resources ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[kind] = resources
}

func removeResource(resources []map[string]any, id string) []map[string]any {
	return slices.DeleteFunc(resources, func(resource map[string]any) bool {
		return resource["id"] == id
	})
}

func buildPurgeCloudsYAML(authURL, cloudName, appcredID string) string {
	return fmt.Sprintf(`
clouds:
  %s:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s
      application_credential_id: %s
      application_credential_secret: purge-secret
    interface: public
    region_name: RegionOne
`, cloudName, identityV3URL(authURL), appcredID)
}

func buildPurgePasswordCloudsYAML(authURL string) string {
	return fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3password
    auth:
      auth_url: %s
      username: janitor
      password: purge-secret
      project_id: %s
      user_domain_name: Default
    interface: public
    region_name: RegionOne
`, identityV3URL(authURL), purgeProjectID)
}

func projectResource(id string, fields map[string]any) map[string]any {
	result := map[string]any{"id": id, "project_id": purgeProjectID}
	for key, value := range fields {
		result[key] = value
	}
	return result
}

func snapshotResource(id, cluster string) map[string]any {
	return map[string]any{
		"id":       id,
		"metadata": map[string]string{"cinder.csi.openstack.org/cluster": cluster},
		"os-extended-snapshot-attributes:project_id": purgeProjectID,
	}
}

func volumeResource(id, cluster, keep string) map[string]any {
	metadata := map[string]string{"cinder.csi.openstack.org/cluster": cluster}
	if keep != "" {
		metadata[openstack.KeepProperty] = keep
	}
	return map[string]any{
		"id":                           id,
		"metadata":                     metadata,
		"os-vol-tenant-attr:tenant_id": purgeProjectID,
	}
}

func TestPurgeResources_AuthenticateErrorPropagates(t *testing.T) {
	err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
		CloudsYAML: "not: valid: yaml: :",
		CloudName:  "openstack",
		Logger:     logr.Discard(),
	})
	if err == nil {
		t.Fatal("expected authentication error")
	}
}

func TestPurgeResources_UnauthenticatedAlwaysBlocksUntilCredentialCheckpoint(t *testing.T) {
	tests := []struct {
		name           string
		includeAppcred bool
	}{
		{name: "credential cleanup enabled", includeAppcred: true},
		{name: "credential cleanup disabled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newPurgeTestServer(t)
			srv.tokenStatus = http.StatusNotFound

			err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
				CloudsYAML:     buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
				CloudName:      "openstack",
				ClusterName:    "demo",
				IncludeAppcred: tt.includeAppcred,
				Logger:         logr.Discard(),
			})
			var authErr *openstack.AuthenticationError
			if !errors.As(err, &authErr) {
				t.Fatalf("expected AuthenticationError, got %T: %v", err, err)
			}
		})
	}
}

func TestPurgeResources_UsesTypedGophercloudResourcesEndToEnd(t *testing.T) {
	srv := newPurgeTestServer(t)
	cluster := "demo"

	srv.setResources("floatingips",
		projectResource("near-fip", map[string]any{"description": "Floating IP for Kubernetes API from cluster " + cluster}),
		projectResource("other-fip", map[string]any{"description": "Floating IP for Kubernetes external service default/web from cluster demo-2"}),
		map[string]any{"id": "foreign-fip", "project_id": "other-project", "description": fipDesc(cluster)},
	)
	srv.fipPageTwo = []map[string]any{
		projectResource("owned-fip", map[string]any{"description": "Floating IP for Kubernetes external service default/web from cluster " + cluster}),
	}
	srv.setResources("loadbalancers",
		projectResource("owned-lb", map[string]any{"name": lbKubeName(cluster, "default_web")}),
		projectResource("near-lb", map[string]any{"name": "kube_service_demo2_default_web"}),
	)
	srv.setResources("security_groups",
		projectResource("owned-sg", map[string]any{"description": sgDesc(cluster)}),
		projectResource("near-sg", map[string]any{"description": "Security Group for worker nodes in cluster " + cluster}),
	)
	srv.setResources("snapshots", snapshotResource("owned-snapshot", cluster), snapshotResource("other-snapshot", "demo-2"))
	srv.setResources("volumes",
		volumeResource("owned-volume", cluster, ""),
		volumeResource("kept-volume", cluster, "true"),
		volumeResource("other-volume", "demo-2", ""),
	)

	err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
		CloudsYAML:           buildPurgeCloudsYAML(srv.URL, "selected", "selected-appcred-id"),
		CloudName:            "selected",
		ClusterName:          cluster,
		IncludeLoadBalancers: true,
		IncludeVolumes:       true,
		IncludeAppcred:       true,
		Logger:               logr.Discard(),
	})
	if err != nil {
		t.Fatalf("purging resources: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assertStringSlice(t, srv.deleted["floatingips"], "owned-fip")
	assertStringSlice(t, srv.deleted["loadbalancers"], "owned-lb")
	assertStringSlice(t, srv.deleted["security_groups"], "owned-sg")
	assertStringSlice(t, srv.deleted["snapshots"], "owned-snapshot")
	assertStringSlice(t, srv.deleted["volumes"], "owned-volume")
	if srv.deletedUserID != "purge-user" || srv.deletedAppcred != "selected-appcred-id" {
		t.Fatalf("expected exact selected credential path, got user=%q credential=%q", srv.deletedUserID, srv.deletedAppcred)
	}
	if len(srv.deleteQueries["loadbalancers"]) != 1 || srv.deleteQueries["loadbalancers"][0].Get("cascade") != "true" {
		t.Fatalf("expected cascade load balancer delete, got %v", srv.deleteQueries["loadbalancers"])
	}
	if len(srv.deleteQueries["volumes"]) != 1 || srv.deleteQueries["volumes"][0].Get("cascade") != "" {
		t.Fatalf("expected non-cascade volume delete, got %v", srv.deleteQueries["volumes"])
	}
	if srv.listCalls["floatingips"] != 3 {
		t.Fatalf("expected a paginated inventory followed by verification for floating IPs, got %d requests", srv.listCalls["floatingips"])
	}
	for _, kind := range []string{"loadbalancers", "security_groups", "snapshots", "volumes"} {
		if srv.listCalls[kind] != 2 {
			t.Errorf("expected initial and verification lists for %s, got %d", kind, srv.listCalls[kind])
		}
	}
	if len(srv.unknownRequests) != 0 {
		t.Fatalf("typed runtime called unexpected or legacy paths: %v", srv.unknownRequests)
	}
	assertCleanupOrder(t, srv.requestOrder, []resourcePhase{
		{kind: "floatingips", id: "owned-fip"},
		{kind: "loadbalancers", id: "owned-lb"},
		{kind: "security_groups", id: "owned-sg"},
		{kind: "snapshots", id: "owned-snapshot"},
		{kind: "volumes", id: "owned-volume"},
	},
		"delete:application_credentials:selected-appcred-id",
	)
}

func TestPurgeResources_FloatingIPListFailureShortCircuits(t *testing.T) {
	srv := newPurgeTestServer(t)
	srv.listStatus["floatingips"] = http.StatusInternalServerError

	err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
		CloudsYAML:     buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
		CloudName:      "openstack",
		ClusterName:    "demo",
		IncludeVolumes: true,
		IncludeAppcred: true,
		Logger:         logr.Discard(),
	})
	if err == nil {
		t.Fatal("expected floating IP list error")
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, kind := range []string{"loadbalancers", "security_groups", "snapshots", "volumes"} {
		if srv.listCalls[kind] != 0 {
			t.Errorf("expected %s phase not to run, got %d list calls", kind, srv.listCalls[kind])
		}
	}
	if srv.deletedAppcred != "" {
		t.Errorf("expected application credential not to be deleted, got %q", srv.deletedAppcred)
	}
}

func TestPurgeResources_LoadBalancerPolicyAndFailureBoundary(t *testing.T) {
	t.Run("disabled phase does not require endpoint", func(t *testing.T) {
		srv := newPurgeTestServer(t)
		srv.setEndpoint("load-balancer", false)

		err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
			CloudsYAML:  buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
			CloudName:   "openstack",
			ClusterName: "demo",
			Logger:      logr.Discard(),
		})
		if err != nil {
			t.Fatalf("disabled load balancer phase unexpectedly required endpoint: %v", err)
		}

		srv.mu.Lock()
		defer srv.mu.Unlock()
		if srv.listCalls["loadbalancers"] != 0 || srv.listCalls["security_groups"] != 1 {
			t.Fatalf("expected LB disabled and SG continued, got lb=%d sg=%d", srv.listCalls["loadbalancers"], srv.listCalls["security_groups"])
		}
	})

	t.Run("enabled phase requires endpoint", func(t *testing.T) {
		srv := newPurgeTestServer(t)
		srv.setEndpoint("load-balancer", false)

		err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
			CloudsYAML:           buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
			CloudName:            "openstack",
			ClusterName:          "demo",
			IncludeLoadBalancers: true,
			Logger:               logr.Discard(),
		})
		if err == nil {
			t.Fatal("expected missing Octavia endpoint error")
		}

		srv.mu.Lock()
		defer srv.mu.Unlock()
		if srv.listCalls["security_groups"] != 0 {
			t.Fatalf("expected security group phase not to run, got %d calls", srv.listCalls["security_groups"])
		}
	})

	t.Run("list failure is fail closed", func(t *testing.T) {
		srv := newPurgeTestServer(t)
		srv.listStatus["loadbalancers"] = http.StatusInternalServerError

		err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
			CloudsYAML:           buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
			CloudName:            "openstack",
			ClusterName:          "demo",
			IncludeLoadBalancers: true,
			Logger:               logr.Discard(),
		})
		if err == nil {
			t.Fatal("expected Octavia inventory failure")
		}

		srv.mu.Lock()
		defer srv.mu.Unlock()
		if srv.listCalls["security_groups"] != 0 {
			t.Fatalf("expected security group phase not to run, got %d calls", srv.listCalls["security_groups"])
		}
	})
}

func TestPurgeResources_OptionalClientsAreLazy(t *testing.T) {
	t.Run("disabled volume phase does not require Cinder", func(t *testing.T) {
		srv := newPurgeTestServer(t)
		srv.setEndpoint("volumev3", false)

		err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
			CloudsYAML:     buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
			CloudName:      "openstack",
			ClusterName:    "demo",
			IncludeVolumes: false,
			Logger:         logr.Discard(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("enabled volume phase requires Cinder", func(t *testing.T) {
		srv := newPurgeTestServer(t)
		srv.setEndpoint("volumev3", false)

		err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
			CloudsYAML:     buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
			CloudName:      "openstack",
			ClusterName:    "demo",
			IncludeVolumes: true,
			Logger:         logr.Discard(),
		})
		if err == nil {
			t.Fatal("expected missing Cinder endpoint error")
		}
	})

	t.Run("disabled credential phase does not require catalog identity", func(t *testing.T) {
		srv := newPurgeTestServer(t)
		srv.setEndpoint("identity", false)

		err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
			CloudsYAML:     buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
			CloudName:      "openstack",
			ClusterName:    "demo",
			IncludeAppcred: false,
			Logger:         logr.Discard(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("password auth has no credential client", func(t *testing.T) {
		srv := newPurgeTestServer(t)
		srv.setEndpoint("identity", false)

		err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
			CloudsYAML:     buildPurgePasswordCloudsYAML(srv.URL),
			CloudName:      "openstack",
			ClusterName:    "demo",
			IncludeAppcred: true,
			Logger:         logr.Discard(),
		})
		if err != nil {
			t.Fatalf("unexpected password-auth error: %v", err)
		}
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if srv.deletedAppcred != "" {
			t.Fatalf("expected no application credential delete, got %q", srv.deletedAppcred)
		}
	})
}

func TestPurgeResources_VolumeFailurePreventsCredentialDeletion(t *testing.T) {
	srv := newPurgeTestServer(t)
	srv.listStatus["volumes"] = http.StatusInternalServerError

	err := openstack.PurgeResources(context.Background(), openstack.PurgeOptions{
		CloudsYAML:     buildPurgeCloudsYAML(srv.URL, "openstack", "purge-appcred-id"),
		CloudName:      "openstack",
		ClusterName:    "demo",
		IncludeVolumes: true,
		IncludeAppcred: true,
		Logger:         logr.Discard(),
	})
	if err == nil {
		t.Fatal("expected volume inventory error")
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.deletedAppcred != "" {
		t.Fatalf("expected credential not to be deleted, got %q", srv.deletedAppcred)
	}
}

func assertStringSlice(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected values\nwant: %v\n got: %v", want, got)
	}
}

type resourcePhase struct {
	kind string
	id   string
}

func assertCleanupOrder(t *testing.T, got []string, phases []resourcePhase, terminal string) {
	t.Helper()
	previousLastList := -1
	for _, phase := range phases {
		listRequest := "list:" + phase.kind
		deleteRequest := "delete:" + phase.kind + ":" + phase.id
		firstList := slices.Index(got, listRequest)
		deleteIndex := slices.Index(got, deleteRequest)
		lastList := firstList
		for index := firstList + 1; index < len(got); index++ {
			if got[index] == listRequest {
				lastList = index
			}
		}

		if firstList < 0 || deleteIndex < 0 {
			t.Fatalf("resource phase %q was incomplete in request order %v", phase.kind, got)
		}
		if firstList <= previousLastList || deleteIndex <= firstList || lastList <= deleteIndex {
			t.Fatalf("resource phase %q did not list, delete, and verify sequentially in %v", phase.kind, got)
		}
		previousLastList = lastList
	}

	terminalIndex := slices.Index(got, terminal)
	if terminalIndex < 0 {
		t.Fatalf("terminal phase %q was missing from %v", terminal, got)
	}
	if terminalIndex <= previousLastList {
		t.Fatalf("terminal phase %q occurred before resource verification in %v", terminal, got)
	}
}
