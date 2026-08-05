package loadbalancer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/loadbalancer"
)

const loadBalancersPath = "/octavia/v2.0/lbaas/loadbalancers"

func TestListLoadBalancersListsEveryPageWithinProject(t *testing.T) {
	t.Parallel()

	var serverURL string
	requests := 0
	service := newService(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Query().Get("project_id") != "project-1" {
			t.Errorf("expected project_id project-1, got %q", r.URL.Query().Get("project_id"))
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("marker") {
		case "":
			writeJSON(t, w, map[string]any{
				"loadbalancers": []any{
					map[string]any{"id": "lb-1", "name": "first", "project_id": "project-1"},
					map[string]any{"id": "other-1", "name": "foreign", "project_id": "project-2"},
				},
				"loadbalancers_links": []any{map[string]any{
					"rel":  "next",
					"href": serverURL + loadBalancersPath + "?marker=lb-1&project_id=project-1",
				}},
			})
		case "lb-1":
			writeJSON(t, w, map[string]any{
				"loadbalancers": []any{
					map[string]any{"id": "lb-2", "name": "second", "project_id": "project-1"},
				},
				"loadbalancers_links": []any{},
			})
		default:
			t.Errorf("unexpected marker %q", r.URL.Query().Get("marker"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}, &serverURL)

	got, err := service.ListLoadBalancers(context.Background())
	if err != nil {
		t.Fatalf("listing loadbalancers: %v", err)
	}
	want := []cleanup.LoadBalancer{
		{ID: "lb-1", Name: "first"},
		{ID: "lb-2", Name: "second"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %#v, got %#v", want, got)
	}
	if requests != 2 {
		t.Fatalf("expected two page requests, got %d", requests)
	}
}

func TestListLoadBalancersRejectsMissingProjectIdentity(t *testing.T) {
	t.Parallel()

	var serverURL string
	service := newService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(t, w, map[string]any{
			"loadbalancers":       []any{map[string]any{"id": "lb-unknown", "name": "unknown-project"}},
			"loadbalancers_links": []any{},
		})
	}, &serverURL)

	items, err := service.ListLoadBalancers(context.Background())
	if err == nil {
		t.Fatal("expected missing project identity to fail inventory")
	}
	if items != nil {
		t.Fatalf("expected invalid inventory to be discarded, got %#v", items)
	}
}

func TestListLoadBalancersReturnsNoPartialInventory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		secondPage func(http.ResponseWriter)
	}{
		{
			name: "malformed later page",
			secondPage: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("{"))
			},
		},
		{
			name: "later page server error",
			secondPage: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var serverURL string
			service := newService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("marker") == "lb-1" {
					tt.secondPage(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				writeJSON(t, w, map[string]any{
					"loadbalancers": []any{map[string]any{
						"id": "lb-1", "name": "first", "project_id": "project-1",
					}},
					"loadbalancers_links": []any{map[string]any{
						"rel":  "next",
						"href": serverURL + loadBalancersPath + "?marker=lb-1&project_id=project-1",
					}},
				})
			}, &serverURL)

			got, err := service.ListLoadBalancers(context.Background())
			if err == nil {
				t.Fatal("expected list error")
			}
			if got != nil {
				t.Fatalf("expected nil inventory after an incomplete list, got %#v", got)
			}
		})
	}
}

func TestListLoadBalancersRejectsInvalidCollectionEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body any
	}{
		{name: "top-level array", body: []any{}},
		{name: "empty object", body: map[string]any{}},
		{
			name: "missing collection key",
			body: map[string]any{"loadbalancers_links": []any{}},
		},
		{name: "null collection", body: map[string]any{"loadbalancers": nil}},
		{name: "non-array collection", body: map[string]any{"loadbalancers": map[string]any{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var serverURL string
			service := newService(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, tt.body)
			}, &serverURL)

			got, err := service.ListLoadBalancers(context.Background())
			if err == nil {
				t.Fatal("expected invalid collection envelope to fail")
			}
			if got != nil {
				t.Fatalf("expected invalid inventory to be discarded, got %#v", got)
			}
			if !strings.Contains(err.Error(), "loadbalancers") {
				t.Fatalf("expected collection key in error, got %v", err)
			}
		})
	}
}

func TestListLoadBalancersUsesContext(t *testing.T) {
	t.Parallel()

	requests := 0
	var serverURL string
	service := newService(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeJSON(t, w, map[string]any{"loadbalancers": []any{}})
	}, &serverURL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := service.ListLoadBalancers(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil inventory, got %#v", got)
	}
	if requests != 0 {
		t.Fatalf("expected no request for a cancelled context, got %d", requests)
	}
}

func TestDeleteLoadBalancerUsesExactIDAndCascade(t *testing.T) {
	t.Parallel()

	requests := 0
	var serverURL string
	service := newService(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != loadBalancersPath+"/lb-1" {
			t.Errorf("expected exact loadbalancer path, got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("cascade"); got != "true" {
			t.Errorf("expected cascade=true, got %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}, &serverURL)

	if err := service.DeleteLoadBalancer(context.Background(), "lb-1"); err != nil {
		t.Fatalf("deleting loadbalancer: %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected one delete request, got %d", requests)
	}
}

func TestDeleteLoadBalancerRejectsEmptyID(t *testing.T) {
	t.Parallel()

	requests := 0
	var serverURL string
	service := newService(t, func(http.ResponseWriter, *http.Request) {
		requests++
	}, &serverURL)

	if err := service.DeleteLoadBalancer(context.Background(), ""); err == nil {
		t.Fatal("expected empty ID to be rejected")
	}
	if requests != 0 {
		t.Fatalf("expected no delete request, got %d", requests)
	}
}

func TestDeleteLoadBalancerClassifiesResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		statusCode  int
		wantPending bool
	}{
		{name: "not found is idempotent", statusCode: http.StatusNotFound},
		{name: "bad request is pending", statusCode: http.StatusBadRequest, wantPending: true},
		{name: "conflict is pending", statusCode: http.StatusConflict, wantPending: true},
		{name: "server error propagates", statusCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var serverURL string
			service := newService(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}, &serverURL)

			err := service.DeleteLoadBalancer(context.Background(), "lb-1")
			if tt.statusCode == http.StatusNotFound {
				if err != nil {
					t.Fatalf("expected idempotent success, got %v", err)
				}
				return
			}
			if tt.wantPending && !errors.Is(err, cleanup.ErrDeletePending) {
				t.Fatalf("expected pending classification, got %v", err)
			}
			if !tt.wantPending && errors.Is(err, cleanup.ErrDeletePending) {
				t.Fatalf("expected unclassified response error, got %v", err)
			}
			var responseErr gophercloud.ErrUnexpectedResponseCode
			if !errors.As(err, &responseErr) {
				t.Fatalf("expected response error cause, got %v", err)
			}
			if responseErr.Actual != tt.statusCode {
				t.Fatalf("expected status %d, got %d", tt.statusCode, responseErr.Actual)
			}
		})
	}
}

func newService(t *testing.T, resourceHandler http.HandlerFunc, serverURL *string) *loadbalancer.Service {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	*serverURL = server.URL

	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Subject-Token", "token-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, map[string]any{
			"token": map[string]any{
				"user":    map[string]any{"id": "user-1"},
				"project": map[string]any{"id": "project-1"},
				"catalog": []any{},
			},
		})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(t, w, map[string]any{
			"catalog": []any{map[string]any{
				"id":   "octavia",
				"name": "octavia",
				"type": "load-balancer",
				"endpoints": []any{
					map[string]any{"interface": "internal", "region_id": "RegionOne", "url": server.URL + "/wrong-interface/v2.0/"},
					map[string]any{"interface": "public", "region_id": "RegionTwo", "url": server.URL + "/wrong-region/v2.0/"},
					map[string]any{"interface": "public", "region_id": "RegionOne", "url": server.URL + "/octavia/v2.0/"},
				},
			}},
		})
	})
	mux.HandleFunc(loadBalancersPath, resourceHandler)
	mux.HandleFunc(loadBalancersPath+"/", resourceHandler)

	cloudsYAML := fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s/v3
      application_credential_id: appcred-1
      application_credential_secret: secret
    region_name: RegionOne
    interface: public
`, server.URL)
	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: cloudsYAML,
	})
	if err != nil {
		t.Fatalf("creating OpenStack client: %v", err)
	}
	service, err := loadbalancer.New(client)
	if err != nil {
		t.Fatalf("creating loadbalancer service: %v", err)
	}
	return service
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encoding fixture response: %v", err)
	}
}
