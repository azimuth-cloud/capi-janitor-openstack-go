package volume

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
)

const (
	testProjectID = "project-1"
	testUserID    = "user-1"
)

func newTestService(t *testing.T) (*Service, *http.ServeMux, string) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	provider := &gophercloud.ProviderClient{TokenID: "token"}
	provider.HTTPClient = *server.Client()
	return &Service{
		client: &gophercloud.ServiceClient{
			ProviderClient: provider,
			Endpoint:       server.URL + "/",
		},
		projectID: testProjectID,
	}, mux, server.URL
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func newAuthenticatedClient(
	t *testing.T,
	userID, projectID string,
	catalog func(string) []any,
) (*openstackclient.Client, *http.ServeMux, string) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Subject-Token", "token")
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"token": map[string]any{
				"user":    map[string]string{"id": userID},
				"project": map[string]string{"id": projectID},
				"catalog": []any{},
			},
		})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"catalog": catalog(server.URL)})
	})

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
	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{CloudsYAML: cloudsYAML})
	if err != nil {
		t.Fatalf("creating authenticated OpenStack client: %v", err)
	}
	return client, mux, server.URL
}

func TestNewRejectsInvalidClient(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Fatal("expected nil client to be rejected")
	}
	if _, err := New(&openstackclient.Client{}); err == nil {
		t.Fatal("expected unauthenticated client to be rejected")
	}
}

func TestNewSelectsConfiguredBlockStorageEndpoint(t *testing.T) {
	t.Parallel()
	client, mux, _ := newAuthenticatedClient(t, testUserID, testProjectID, func(baseURL string) []any {
		return []any{map[string]any{
			"type": "block-storage",
			"endpoints": []any{
				map[string]string{
					"interface": "internal", "region": "RegionOne", "region_id": "RegionOne",
					"url": baseURL + "/wrong-interface/v3/project-1/",
				},
				map[string]string{
					"interface": "public", "region": "RegionTwo", "region_id": "RegionTwo",
					"url": baseURL + "/wrong-region/v3/project-1/",
				},
				map[string]string{
					"interface": "public", "region": "RegionOne", "region_id": "RegionOne",
					"url": baseURL + "/selected-volume/v3/project-1/",
				},
			},
		}}
	})
	service, err := New(client)
	if err != nil {
		t.Fatalf("creating volume service: %v", err)
	}

	selectedRequests := 0
	mux.HandleFunc("/selected-volume/v3/project-1/volumes/detail", func(w http.ResponseWriter, _ *http.Request) {
		selectedRequests++
		writeJSON(t, w, http.StatusOK, map[string]any{"volumes": []any{}})
	})
	mux.HandleFunc("/wrong-interface/", func(http.ResponseWriter, *http.Request) {
		t.Error("used internal block-storage endpoint")
	})
	mux.HandleFunc("/wrong-region/", func(http.ResponseWriter, *http.Request) {
		t.Error("used wrong-region block-storage endpoint")
	})

	items, err := service.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("listing through selected endpoint: %v", err)
	}
	if len(items) != 0 || selectedRequests != 1 {
		t.Fatalf("expected one selected-endpoint request and empty inventory, got %d, %#v", selectedRequests, items)
	}
}

func TestNewAcceptsVolumeV3Type(t *testing.T) {
	t.Parallel()
	client, mux, _ := newAuthenticatedClient(t, testUserID, testProjectID, func(baseURL string) []any {
		return []any{map[string]any{
			"type": "volumev3",
			"endpoints": []any{map[string]string{
				"interface": "public", "region": "RegionOne", "region_id": "RegionOne",
				"url": baseURL + "/volume-v3/v3/project-1/",
			}},
		}}
	})
	mux.HandleFunc("/volume-v3/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"versions": []any{map[string]any{"id": "v3.0", "status": "CURRENT"}},
		})
	})
	service, err := New(client)
	if err != nil {
		t.Fatalf("creating volume service: %v", err)
	}

	mux.HandleFunc("/volume-v3/v3/project-1/volumes/detail", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"volumes": []any{}})
	})
	if _, err := service.ListVolumes(context.Background()); err != nil {
		t.Fatalf("listing through volumev3 endpoint: %v", err)
	}
}

func TestNewFailsWhenBlockStorageEndpointIsMissing(t *testing.T) {
	t.Parallel()
	client, _, _ := newAuthenticatedClient(t, testUserID, testProjectID, func(baseURL string) []any {
		return []any{map[string]any{
			"type": "identity",
			"endpoints": []any{map[string]string{
				"interface": "public", "region": "RegionOne", "region_id": "RegionOne",
				"url": baseURL + "/identity/v3/",
			}},
		}}
	})
	if _, err := New(client); err == nil {
		t.Fatal("expected missing block-storage endpoint error")
	}
}

func TestListSnapshotsMapsAllPagesAndFiltersProject(t *testing.T) {
	t.Parallel()
	service, mux, baseURL := newTestService(t)
	requests := 0
	mux.HandleFunc("/snapshots/detail", func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("all_tenants") != "" {
			t.Errorf("unexpected all_tenants query: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("marker") == "next" {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"snapshots": []any{map[string]any{
					"id": "snapshot-page-2", "created_at": "2017-05-30T03:35:03.000000",
					"metadata": map[string]string{"page": "two"},
					"os-extended-snapshot-attributes:project_id": testProjectID,
				}},
			})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"snapshots": []any{
				map[string]any{
					"id": "snapshot-owned", "created_at": "2017-05-30T03:35:03.000000",
					"metadata": map[string]string{"cluster": "demo"},
					"os-extended-snapshot-attributes:project_id": testProjectID,
				},
				map[string]any{
					"id": "snapshot-other-project", "created_at": "2017-05-30T03:35:03.000000",
					"metadata": map[string]string{"cluster": "demo"},
					"os-extended-snapshot-attributes:project_id": "project-2",
				},
				map[string]any{
					"id": "snapshot-project-omitted", "created_at": "2017-05-30T03:35:03.000000",
					"metadata": map[string]string{"project": "omitted"},
				},
			},
			"snapshots_links": []any{map[string]string{
				"rel": "next", "href": baseURL + "/snapshots/detail?marker=next",
			}},
		})
	})

	got, err := service.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	want := []cleanup.Snapshot{
		{ID: "snapshot-owned", Metadata: map[string]string{"cluster": "demo"}},
		{ID: "snapshot-project-omitted", Metadata: map[string]string{"project": "omitted"}},
		{ID: "snapshot-page-2", Metadata: map[string]string{"page": "two"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected snapshots:\n got: %#v\nwant: %#v", got, want)
	}
	if requests != 2 {
		t.Fatalf("expected two snapshot pages, got %d requests", requests)
	}
}

func TestListVolumesMapsAllPagesAndFiltersProject(t *testing.T) {
	t.Parallel()
	service, mux, baseURL := newTestService(t)
	requests := 0
	mux.HandleFunc("/volumes/detail", func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("all_tenants") != "" {
			t.Errorf("unexpected all_tenants query: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("marker") == "next" {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"volumes": []any{map[string]any{
					"id": "volume-page-2", "created_at": "2017-05-30T03:35:03.000000",
					"metadata":                     map[string]string{"page": "two"},
					"os-vol-tenant-attr:tenant_id": testProjectID,
				}},
			})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"volumes": []any{
				map[string]any{
					"id": "volume-owned", "created_at": "2017-05-30T03:35:03.000000",
					"metadata":                     map[string]string{"cluster": "demo"},
					"os-vol-tenant-attr:tenant_id": testProjectID,
				},
				map[string]any{
					"id": "volume-other-project", "created_at": "2017-05-30T03:35:03.000000",
					"metadata":                     map[string]string{"cluster": "demo"},
					"os-vol-tenant-attr:tenant_id": "project-2",
				},
				map[string]any{
					"id": "volume-project-omitted", "created_at": "2017-05-30T03:35:03.000000",
					"metadata": map[string]string{"project": "omitted"},
				},
			},
			"volumes_links": []any{map[string]string{
				"rel": "next", "href": baseURL + "/volumes/detail?marker=next",
			}},
		})
	})

	got, err := service.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("listing volumes: %v", err)
	}
	want := []cleanup.Volume{
		{ID: "volume-owned", Metadata: map[string]string{"cluster": "demo"}},
		{ID: "volume-project-omitted", Metadata: map[string]string{"project": "omitted"}},
		{ID: "volume-page-2", Metadata: map[string]string{"page": "two"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected volumes:\n got: %#v\nwant: %#v", got, want)
	}
	if requests != 2 {
		t.Fatalf("expected two volume pages, got %d requests", requests)
	}
}

func TestListReturnsNoPartialInventoryOnFailure(t *testing.T) {
	t.Parallel()

	t.Run("later snapshot page", func(t *testing.T) {
		t.Parallel()
		service, mux, baseURL := newTestService(t)
		mux.HandleFunc("/snapshots/detail", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("marker") == "next" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{
				"snapshots": []any{map[string]any{
					"id": "partial", "created_at": "2017-05-30T03:35:03.000000",
				}},
				"snapshots_links": []any{map[string]string{
					"rel": "next", "href": baseURL + "/snapshots/detail?marker=next",
				}},
			})
		})
		got, err := service.ListSnapshots(context.Background())
		if err == nil || got != nil {
			t.Fatalf("expected nil inventory and later-page error, got %#v, %v", got, err)
		}
	})

	t.Run("malformed volume", func(t *testing.T) {
		t.Parallel()
		service, mux, _ := newTestService(t)
		mux.HandleFunc("/volumes/detail", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"volumes": []any{map[string]any{"id": 123}},
			})
		})
		got, err := service.ListVolumes(context.Background())
		if err == nil || got != nil {
			t.Fatalf("expected nil inventory and extraction error, got %#v, %v", got, err)
		}
	})
}

func TestListRejectsInvalidCollectionEnvelope(t *testing.T) {
	t.Parallel()

	resources := []struct {
		name string
		path string
		key  string
		list func(*testing.T, *Service) error
	}{
		{
			name: "snapshots",
			path: "/snapshots/detail",
			key:  "snapshots",
			list: func(t *testing.T, service *Service) error {
				items, err := service.ListSnapshots(context.Background())
				if items != nil {
					t.Fatalf("expected invalid inventory to be discarded, got %#v", items)
				}
				return err
			},
		},
		{
			name: "volumes",
			path: "/volumes/detail",
			key:  "volumes",
			list: func(t *testing.T, service *Service) error {
				items, err := service.ListVolumes(context.Background())
				if items != nil {
					t.Fatalf("expected invalid inventory to be discarded, got %#v", items)
				}
				return err
			},
		},
	}

	for _, resource := range resources {
		tests := []struct {
			name string
			body any
		}{
			{name: "top-level array", body: []any{}},
			{name: "empty object", body: map[string]any{}},
			{
				name: "missing collection key",
				body: map[string]any{resource.key + "_links": []any{}},
			},
			{name: "null collection", body: map[string]any{resource.key: nil}},
			{name: "non-array collection", body: map[string]any{resource.key: map[string]any{}}},
		}

		for _, tt := range tests {
			t.Run(resource.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				service, mux, _ := newTestService(t)
				mux.HandleFunc(resource.path, func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(t, w, http.StatusOK, tt.body)
				})

				err := resource.list(t, service)
				if err == nil {
					t.Fatal("expected invalid collection envelope to fail")
				}
				if !strings.Contains(err.Error(), resource.key) {
					t.Fatalf("expected collection key in error, got %v", err)
				}
			})
		}
	}
}

func TestListPropagatesContextCancellation(t *testing.T) {
	t.Parallel()
	service, mux, _ := newTestService(t)
	mux.HandleFunc("/snapshots/detail", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"snapshots": []any{}})
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := service.ListSnapshots(ctx)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled list with nil inventory, got %#v, %v", got, err)
	}
}

func TestDeleteSnapshotClassifiesResponsesAndValidatesID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		status      int
		wantErr     bool
		wantPending bool
	}{
		{name: "accepted", status: http.StatusNoContent},
		{name: "already absent", status: http.StatusNotFound},
		{name: "bad request", status: http.StatusBadRequest, wantErr: true, wantPending: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, mux, _ := newTestService(t)
			mux.HandleFunc("/snapshots/snapshot-1", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("expected DELETE, got %s", r.Method)
				}
				w.WriteHeader(tt.status)
			})
			err := service.DeleteSnapshot(context.Background(), "snapshot-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if errors.Is(err, cleanup.ErrDeletePending) != tt.wantPending {
				t.Fatalf("unexpected pending classification: %v", err)
			}
		})
	}

	service, _, _ := newTestService(t)
	if err := service.DeleteSnapshot(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "ID is empty") {
		t.Fatalf("expected empty ID validation error, got %v", err)
	}
}

func TestDeleteVolumeDoesNotCascadeAndClassifiesConflict(t *testing.T) {
	t.Parallel()
	service, mux, _ := newTestService(t)
	mux.HandleFunc("/volumes/volume-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("volume deletion must not cascade, got query %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusConflict)
	})
	err := service.DeleteVolume(context.Background(), "volume-1")
	if !errors.Is(err, cleanup.ErrDeletePending) {
		t.Fatalf("expected pending conflict, got %v", err)
	}
	if err := service.DeleteVolume(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "ID is empty") {
		t.Fatalf("expected empty ID validation error, got %v", err)
	}
}
