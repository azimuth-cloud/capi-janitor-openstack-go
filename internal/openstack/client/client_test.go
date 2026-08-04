package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
	if client.ProviderClient() == nil {
		t.Fatal("expected provider client")
	}
	if got := client.EndpointOpts().Region; got != "RegionOne" {
		t.Errorf("expected endpoint region RegionOne, got %q", got)
	}

	if _, err := client.Request(context.Background(), http.MethodGet, server.URL+"/resource", &gophercloud.RequestOpts{}); err != nil {
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

func TestClientAuthenticatesWithPassword(t *testing.T) {
	t.Parallel()

	var tokenRequest map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&tokenRequest); err != nil {
			t.Errorf("decoding token request: %v", err)
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

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cloudsYAML := fmt.Sprintf(`
clouds:
  openstack:
    auth_type: v3password
    auth:
      auth_url: %s
      username: alice
      password: secret
      user_domain_name: Default
      project_id: project-1
    region_name: RegionOne
    interface: public
`, server.URL+"/v3")

	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: cloudsYAML,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	if !client.IsAuthenticated() {
		t.Fatal("expected authenticated client")
	}
	if client.ApplicationCredentialID() != "" {
		t.Fatalf("expected no application credential ID, got %q", client.ApplicationCredentialID())
	}

	auth, ok := tokenRequest["auth"].(map[string]any)
	if !ok {
		t.Fatalf("expected auth object, got %#v", tokenRequest["auth"])
	}
	identity, ok := auth["identity"].(map[string]any)
	if !ok {
		t.Fatalf("expected identity object, got %#v", auth["identity"])
	}
	methods, ok := identity["methods"].([]any)
	if !ok || len(methods) != 1 || methods[0] != "password" {
		t.Fatalf("expected password method, got %#v", identity["methods"])
	}
}
