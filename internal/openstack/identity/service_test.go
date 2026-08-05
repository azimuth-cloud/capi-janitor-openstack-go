package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

const testUserID = "user-1"

func newTestService(t *testing.T) (*Service, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	provider := &gophercloud.ProviderClient{TokenID: "token"}
	provider.HTTPClient = *server.Client()
	return &Service{
		client: &gophercloud.ServiceClient{
			ProviderClient: provider,
			Endpoint:       server.URL + "/v3/",
		},
		userID: testUserID,
	}, mux
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
	userID string,
	catalog func(string) []any,
) (*openstackclient.Client, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Subject-Token", "token")
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"token": map[string]any{
				"user":    map[string]string{"id": userID},
				"project": map[string]string{"id": "project-1"},
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
	return client, mux
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

func TestNewRejectsAuthenticatedClientWithoutUser(t *testing.T) {
	t.Parallel()
	client, _ := newAuthenticatedClient(t, "", func(baseURL string) []any {
		return []any{map[string]any{
			"type": "identity",
			"endpoints": []any{map[string]string{
				"interface": "public", "region": "RegionOne", "region_id": "RegionOne",
				"url": baseURL + "/identity/v3/",
			}},
		}}
	})
	if _, err := New(client); err == nil || !strings.Contains(err.Error(), "user ID is empty") {
		t.Fatalf("expected empty user ID error, got %v", err)
	}
}

func TestNewSelectsAndNormalizesConfiguredIdentityEndpoint(t *testing.T) {
	t.Parallel()
	client, mux := newAuthenticatedClient(t, testUserID, func(baseURL string) []any {
		return []any{map[string]any{
			"type": "identity",
			"endpoints": []any{
				map[string]string{
					"interface": "internal", "region": "RegionOne", "region_id": "RegionOne",
					"url": baseURL + "/wrong-interface/v3/",
				},
				map[string]string{
					"interface": "public", "region": "RegionTwo", "region_id": "RegionTwo",
					"url": baseURL + "/wrong-region/v3/",
				},
				map[string]string{
					"interface": "public", "region": "RegionOne", "region_id": "RegionOne",
					"url": baseURL + "/selected-identity/v2.0/",
				},
			},
		}}
	})
	service, err := New(client)
	if err != nil {
		t.Fatalf("creating identity service: %v", err)
	}

	selectedRequests := 0
	mux.HandleFunc("/selected-identity/v3/users/user-1/application_credentials/appcred-1", func(w http.ResponseWriter, r *http.Request) {
		selectedRequests++
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/wrong-interface/", func(http.ResponseWriter, *http.Request) {
		t.Error("used internal identity endpoint")
	})
	mux.HandleFunc("/wrong-region/", func(http.ResponseWriter, *http.Request) {
		t.Error("used wrong-region identity endpoint")
	})

	if err := service.DeleteApplicationCredential(context.Background(), "appcred-1"); err != nil {
		t.Fatalf("deleting through selected endpoint: %v", err)
	}
	if selectedRequests != 1 {
		t.Fatalf("expected one request to normalized selected endpoint, got %d", selectedRequests)
	}
}

func TestDeleteApplicationCredentialUsesExactIDAndClassifiesResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		status        int
		wantErr       bool
		wantPending   bool
		wantForbidden bool
	}{
		{name: "deleted", status: http.StatusNoContent},
		{name: "already absent", status: http.StatusNotFound},
		{name: "bad request", status: http.StatusBadRequest, wantErr: true, wantPending: true},
		{name: "conflict", status: http.StatusConflict, wantErr: true, wantPending: true},
		{name: "self deletion forbidden", status: http.StatusForbidden, wantErr: true, wantForbidden: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, mux := newTestService(t)
			requests := 0
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodDelete {
					t.Errorf("application credential service must not list, got %s", r.Method)
				}
				wantPath := "/v3/users/user-1/application_credentials/appcred-1"
				if r.URL.Path != wantPath {
					t.Errorf("unexpected identity path: got %q, want %q", r.URL.Path, wantPath)
				}
				w.WriteHeader(tt.status)
			})

			err := service.DeleteApplicationCredential(context.Background(), "appcred-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if errors.Is(err, cleanup.ErrDeletePending) != tt.wantPending {
				t.Fatalf("unexpected pending classification: %v", err)
			}
			if errors.Is(err, cleanup.ErrApplicationCredentialForbidden) != tt.wantForbidden {
				t.Fatalf("unexpected forbidden classification: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "appcred-1") {
				t.Fatalf("operation error does not identify the credential: %v", err)
			}
			if requests != 1 {
				t.Fatalf("expected exactly one delete and no list, got %d requests", requests)
			}
		})
	}
}

func TestDeleteApplicationCredentialRejectsEmptyIDWithoutRequest(t *testing.T) {
	t.Parallel()
	service, mux := newTestService(t)
	requests := 0
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	})

	err := service.DeleteApplicationCredential(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "ID is empty") {
		t.Fatalf("expected empty ID validation error, got %v", err)
	}
	if requests != 0 {
		t.Fatalf("empty ID made %d identity requests", requests)
	}
}

func TestDeleteApplicationCredentialPropagatesContextCancellation(t *testing.T) {
	t.Parallel()
	service, mux := newTestService(t)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := service.DeleteApplicationCredential(ctx, "appcred-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
