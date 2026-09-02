package client_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

func TestClientReauthenticatesExpiredToken(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	tokenCalls := 0
	resourceCalls := 0
	resourceUserAgent := ""

	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		tokenCalls++
		token := fmt.Sprintf("token-%d", tokenCalls)
		mu.Unlock()

		w.Header().Set("X-Subject-Token", token)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": map[string]any{
				"user":    map[string]any{"id": "user-1"},
				"project": map[string]any{"id": "project-1"},
				"catalog": []any{},
			},
		})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, r *http.Request) {
		endpoint := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"catalog": []any{map[string]any{
				"type": "network",
				"endpoints": []any{map[string]any{
					"interface": "public",
					"region_id": "RegionOne",
					"url":       endpoint,
				}},
			}},
		})
	})
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		resourceCalls++
		call := resourceCalls
		resourceUserAgent = r.UserAgent()
		mu.Unlock()

		if call == 1 {
			if got := r.Header.Get("X-Auth-Token"); got != "token-1" {
				t.Errorf("first request used token %q", got)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("X-Auth-Token"); got != "token-2" {
			t.Errorf("retried request used token %q", got)
		}
		w.WriteHeader(http.StatusOK)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cloudsYAML := fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s
      application_credential_id: appcred-1
      application_credential_secret: secret
    region_name: RegionOne
    interface: public
`, server.URL+"/v3")

	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: cloudsYAML,
		CloudName:  "openstack",
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	if !client.IsAuthenticated() {
		t.Fatal("expected authenticated client")
	}
	if client.UserID() != "user-1" {
		t.Errorf("expected user ID user-1, got %q", client.UserID())
	}
	if client.ProjectID() != "project-1" {
		t.Errorf("expected project ID project-1, got %q", client.ProjectID())
	}
	if client.ApplicationCredentialID() != "appcred-1" {
		t.Errorf("expected application credential ID appcred-1, got %q", client.ApplicationCredentialID())
	}
	if client.ProviderClient() == nil {
		t.Fatal("expected provider client")
	}
	if got := client.ProviderClient().HTTPClient.Timeout; got != 60*time.Second {
		t.Errorf("expected request timeout 60s, got %s", got)
	}
	if got := client.EndpointOpts().Region; got != "RegionOne" {
		t.Errorf("expected endpoint region RegionOne, got %q", got)
	}

	if _, err := client.ProviderClient().Request(context.Background(), http.MethodGet, server.URL+"/resource", &gophercloud.RequestOpts{}); err != nil {
		t.Fatalf("request after token expiry: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 2 {
		t.Errorf("expected two token requests, got %d", tokenCalls)
	}
	if resourceCalls != 2 {
		t.Errorf("expected resource request to be retried once, got %d calls", resourceCalls)
	}
	if !strings.Contains(resourceUserAgent, "capi-janitor-openstack-go") {
		t.Errorf("expected Janitor user agent, got %q", resourceUserAgent)
	}
}

func TestClientSelectsCredentialIDByCloud(t *testing.T) {
	t.Parallel()

	server := newKeystoneServer(t, http.StatusCreated, http.StatusOK, "user-1", "project-1")
	cloudsYAML := fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s/v3
      application_credential_id: appcred-default
      application_credential_secret: secret
  selected:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s/v3
      application_credential_id: appcred-selected
      application_credential_secret: secret
`, server.URL, server.URL)

	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: cloudsYAML,
		CloudName:  "selected",
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	if got := client.ApplicationCredentialID(); got != "appcred-selected" {
		t.Fatalf("application credential ID = %q, want %q", got, "appcred-selected")
	}
}

func TestClientUsesOnlyApplicationCredential(t *testing.T) {
	t.Parallel()

	type authRequest struct {
		Auth struct {
			Identity struct {
				Methods               []string       `json:"methods"`
				ApplicationCredential map[string]any `json:"application_credential"`
				Token                 map[string]any `json:"token"`
				Password              map[string]any `json:"password"`
			} `json:"identity"`
		} `json:"auth"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		var request authRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decoding authentication request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		identity := request.Auth.Identity
		if len(identity.Methods) != 1 || identity.Methods[0] != "application_credential" {
			t.Errorf("authentication methods = %v, want application_credential", identity.Methods)
		}
		if identity.ApplicationCredential["id"] != "appcred-1" {
			t.Errorf("application credential ID = %v, want appcred-1", identity.ApplicationCredential["id"])
		}
		if identity.ApplicationCredential["secret"] != "appcred-secret" {
			t.Errorf("application credential secret = %v, want appcred-secret", identity.ApplicationCredential["secret"])
		}
		if identity.Token != nil {
			t.Errorf("token authentication was included: %v", identity.Token)
		}
		if identity.Password != nil {
			t.Errorf("password authentication was included: %v", identity.Password)
		}

		w.Header().Set("X-Subject-Token", "token-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": map[string]any{
				"user":    map[string]any{"id": "user-1"},
				"project": map[string]any{"id": "project-1"},
				"catalog": []any{},
			},
		})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"catalog": []any{}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cloudsYAML := fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s/v3
      application_credential_id: appcred-1
      application_credential_secret: appcred-secret
      token: ignored-token
      username: ignored-user
      password: ignored-password
`, server.URL)
	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: cloudsYAML,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	if !client.IsAuthenticated() {
		t.Fatal("expected authenticated client")
	}
}

func TestClientRequiresTokenUserAndProjectIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		userID    string
		projectID string
		wantError string
	}{
		{name: "empty user ID", projectID: "project-1", wantError: "authenticated user ID is empty"},
		{name: "empty project ID", userID: "user-1", wantError: "authenticated project ID is empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newKeystoneServer(t, http.StatusCreated, http.StatusOK, tt.userID, tt.projectID)
			_, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
				CloudsYAML: buildCredentialCloudsYAML(server.URL, "appcred-1"),
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestClientReturnsAuthAndCatalogErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		tokenStatus   int
		catalogStatus int
	}{
		{name: "authentication 404", tokenStatus: http.StatusNotFound, catalogStatus: http.StatusOK},
		{name: "authentication 500", tokenStatus: http.StatusInternalServerError, catalogStatus: http.StatusOK},
		{name: "catalog 404", tokenStatus: http.StatusCreated, catalogStatus: http.StatusNotFound},
		{name: "catalog 500", tokenStatus: http.StatusCreated, catalogStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newKeystoneServer(t, tt.tokenStatus, tt.catalogStatus, "user-1", "project-1")
			client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
				CloudsYAML: buildCredentialCloudsYAML(server.URL, "appcred-1"),
			})
			if err == nil {
				t.Fatalf("expected OpenStack error, got client %#v", client)
			}
		})
	}
}

func TestClientRequiresExplicitApplicationCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		authType         string
		credentialID     string
		credentialSecret string
		wantUnsupported  bool
		wantError        string
	}{
		{
			name:            "password",
			authType:        "v3password",
			wantUnsupported: true,
		},
		{
			name:             "implicit application credential",
			credentialID:     "appcred-1",
			credentialSecret: "secret",
			wantUnsupported:  true,
		},
		{
			name:             "missing application credential ID",
			authType:         "v3applicationcredential",
			credentialSecret: "secret",
			wantError:        "application credential ID is empty",
		},
		{
			name:         "missing application credential secret",
			authType:     "v3applicationcredential",
			credentialID: "appcred-1",
			wantError:    "application credential secret is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cloudsYAML := fmt.Sprintf(`
clouds:
  openstack:
    auth_type: %s
    auth:
      auth_url: https://identity.example/v3
      application_credential_id: %s
      application_credential_secret: %s
`, tt.authType, tt.credentialID, tt.credentialSecret)

			_, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
				CloudsYAML: cloudsYAML,
			})
			if err == nil {
				t.Fatal("expected authentication input to be rejected")
			}

			if tt.wantUnsupported {
				var unsupported *openstackclient.UnsupportedAuthTypeError
				if !errors.As(err, &unsupported) {
					t.Fatalf("expected UnsupportedAuthTypeError, got %v", err)
				}
				return
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestClientRejectsInvalidCACertificate(t *testing.T) {
	t.Parallel()

	cloudsYAML := `
clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://identity.example/v3
      application_credential_id: appcred-1
      application_credential_secret: secret
`

	_, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: cloudsYAML,
		CACert:     "not a PEM certificate",
	})
	if err == nil || !strings.Contains(err.Error(), "failed to append CA certificate") {
		t.Fatalf("expected invalid CA certificate error, got %v", err)
	}
}

func TestClientUsesCustomCACertificate(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Subject-Token", "token-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": map[string]any{
				"user":    map[string]any{"id": "user-1"},
				"project": map[string]any{"id": "project-1"},
				"catalog": []any{},
			},
		})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"catalog": []any{}})
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	caCert := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: buildCredentialCloudsYAML(server.URL, "appcred-1"),
		CACert:     string(caCert),
	})
	if err != nil {
		t.Fatalf("creating client with custom CA: %v", err)
	}
	if !client.IsAuthenticated() {
		t.Fatal("expected authenticated client")
	}
}

func newKeystoneServer(
	t *testing.T,
	tokenStatus int,
	catalogStatus int,
	userID string,
	projectID string,
) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		if tokenStatus != http.StatusCreated {
			w.WriteHeader(tokenStatus)
			return
		}
		w.Header().Set("X-Subject-Token", "token-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": map[string]any{
				"user":    map[string]any{"id": userID},
				"project": map[string]any{"id": projectID},
				"catalog": []any{},
			},
		})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, _ *http.Request) {
		if catalogStatus != http.StatusOK {
			w.WriteHeader(catalogStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"catalog": []any{}})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func buildCredentialCloudsYAML(serverURL, credentialID string) string {
	return fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s/v3
      application_credential_id: %s
      application_credential_secret: secret
`, serverURL, credentialID)
}
