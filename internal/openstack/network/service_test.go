package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

const (
	testProjectID = "project-1"
	testRegion    = "RegionOne"
)

type clientFixtureOptions struct {
	tokenStatus    int
	projectID      string
	includeNetwork bool
}

func TestNewValidatesClient(t *testing.T) {
	t.Run("nil client", func(t *testing.T) {
		service, err := New(nil)
		if err == nil || service != nil {
			t.Fatalf("expected nil-client error, got service %#v and error %v", service, err)
		}
	})

	t.Run("unauthenticated client", func(t *testing.T) {
		client := newClientFixture(t, clientFixtureOptions{
			tokenStatus:    http.StatusNotFound,
			projectID:      testProjectID,
			includeNetwork: true,
		}, nil)

		service, err := New(client)
		if err == nil || service != nil {
			t.Fatalf("expected authentication error, got service %#v and error %v", service, err)
		}
		if !strings.Contains(err.Error(), "not authenticated") {
			t.Fatalf("expected authentication context, got %v", err)
		}
	})

	t.Run("empty project ID", func(t *testing.T) {
		client := newClientFixture(t, clientFixtureOptions{
			tokenStatus:    http.StatusCreated,
			includeNetwork: true,
		}, nil)

		service, err := New(client)
		if err == nil || service != nil {
			t.Fatalf("expected project error, got service %#v and error %v", service, err)
		}
		if !strings.Contains(err.Error(), "project ID is empty") {
			t.Fatalf("expected project context, got %v", err)
		}
	})

	t.Run("missing network endpoint", func(t *testing.T) {
		client := newClientFixture(t, clientFixtureOptions{
			tokenStatus: http.StatusCreated,
			projectID:   testProjectID,
		}, nil)

		service, err := New(client)
		if err == nil || service != nil {
			t.Fatalf("expected endpoint error, got service %#v and error %v", service, err)
		}
		if !strings.Contains(err.Error(), "creating network service client") {
			t.Fatalf("expected service-client context, got %v", err)
		}
	})
}

func TestListFloatingIPsReturnsAttachedPortIDsFromEveryPageInSelectedProject(t *testing.T) {
	var requests atomic.Int32
	service := newServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2.0/floatingips" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		assertProjectQuery(t, r.URL.Query())

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			writeJSON(t, w, map[string]any{
				"floatingips": []any{
					map[string]any{
						"id":                  "fip-2",
						"description":         "second page",
						"project_id":          testProjectID,
						"floating_ip_address": "192.0.2.2",
						"port_id":             "",
					},
				},
				"floatingips_links": []any{},
			})
			return
		}

		writeJSON(t, w, map[string]any{
			"floatingips": []any{
				map[string]any{
					"id":                  "fip-1",
					"description":         "first page",
					"project_id":          testProjectID,
					"tenant_id":           testProjectID,
					"floating_ip_address": "192.0.2.1",
					"port_id":             "port-1",
					"status":              "ACTIVE",
				},
				map[string]any{
					"id":          "legacy-fip",
					"description": "legacy tenant field",
					"tenant_id":   testProjectID,
					"port_id":     nil,
				},
				map[string]any{
					"id":          "foreign-fip",
					"description": "must not escape project boundary",
					"project_id":  "project-2",
					"port_id":     "foreign-port-1",
				},
				map[string]any{
					"id":          "legacy-foreign-fip",
					"description": "must not escape legacy project boundary",
					"tenant_id":   "project-2",
					"port_id":     "foreign-port-2",
				},
			},
			"floatingips_links": []any{
				map[string]any{
					"rel":  "next",
					"href": fmt.Sprintf("http://%s/v2.0/floatingips?page=2&project_id=%s", r.Host, testProjectID),
				},
			},
		})
	})

	got, err := service.ListFloatingIPs(context.Background())
	if err != nil {
		t.Fatalf("listing floating IPs: %v", err)
	}
	want := []cleanup.FloatingIP{
		{ID: "fip-1", Description: "first page", AttachedPortID: "port-1"},
		{ID: "legacy-fip", Description: "legacy tenant field", AttachedPortID: ""},
		{ID: "fip-2", Description: "second page", AttachedPortID: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected floating IP mapping\nwant: %#v\n got: %#v", want, got)
	}
	if requests.Load() != 2 {
		t.Fatalf("expected two pages, got %d requests", requests.Load())
	}
}

func TestListFloatingIPsRejectsMissingOrInvalidAttachedPortID(t *testing.T) {
	tests := []struct {
		name          string
		portIDPresent bool
		portID        any
	}{
		{name: "missing port ID"},
		{name: "numeric port ID", portIDPresent: true, portID: 42},
		{name: "boolean port ID", portIDPresent: true, portID: true},
		{name: "object port ID", portIDPresent: true, portID: map[string]any{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := newServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v2.0/floatingips" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
					w.WriteHeader(http.StatusNotFound)
					return
				}
				floatingIPResponse := map[string]any{
					"id":         "fip-1",
					"project_id": testProjectID,
				}
				if tt.portIDPresent {
					floatingIPResponse["port_id"] = tt.portID
				}
				w.Header().Set("Content-Type", "application/json")
				writeJSON(t, w, map[string]any{"floatingips": []any{floatingIPResponse}})
			})

			got, err := service.ListFloatingIPs(context.Background())
			if err == nil {
				t.Fatal("expected missing or invalid port_id to fail inventory")
			}
			if got != nil {
				t.Fatalf("expected invalid inventory to be discarded, got %#v", got)
			}
			if !strings.Contains(err.Error(), "port_id") {
				t.Fatalf("expected port_id context, got %v", err)
			}
		})
	}
}

func TestListSecurityGroupsEveryPageMapsAndScopesProject(t *testing.T) {
	var requests atomic.Int32
	service := newServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2.0/security-groups" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		assertProjectQuery(t, r.URL.Query())

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			writeJSON(t, w, map[string]any{
				"security_groups": []any{
					map[string]any{
						"id":          "sg-2",
						"description": "second page",
						"project_id":  testProjectID,
						"name":        "ignored name",
					},
				},
				"security_groups_links": []any{},
			})
			return
		}

		writeJSON(t, w, map[string]any{
			"security_groups": []any{
				map[string]any{
					"id":          "sg-1",
					"description": "first page",
					"project_id":  testProjectID,
					"tenant_id":   testProjectID,
					"name":        "ignored name",
					"tags":        []string{"ignored"},
				},
				map[string]any{
					"id":          "legacy-sg",
					"description": "legacy tenant field",
					"tenant_id":   testProjectID,
				},
				map[string]any{
					"id":          "foreign-sg",
					"description": "must not escape project boundary",
					"project_id":  "project-2",
				},
				map[string]any{
					"id":          "legacy-foreign-sg",
					"description": "must not escape legacy project boundary",
					"tenant_id":   "project-2",
				},
			},
			"security_groups_links": []any{
				map[string]any{
					"rel":  "next",
					"href": fmt.Sprintf("http://%s/v2.0/security-groups?page=2&project_id=%s", r.Host, testProjectID),
				},
			},
		})
	})

	got, err := service.ListSecurityGroups(context.Background())
	if err != nil {
		t.Fatalf("listing security groups: %v", err)
	}
	want := []cleanup.SecurityGroup{
		{ID: "sg-1", Description: "first page"},
		{ID: "legacy-sg", Description: "legacy tenant field"},
		{ID: "sg-2", Description: "second page"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected security group mapping\nwant: %#v\n got: %#v", want, got)
	}
	if requests.Load() != 2 {
		t.Fatalf("expected two pages, got %d requests", requests.Load())
	}
}

func TestListRejectsInvalidCollectionEnvelope(t *testing.T) {
	resources := []struct {
		name string
		path string
		key  string
		list func(*testing.T, *Service) error
	}{
		{
			name: "floating IPs",
			path: "/v2.0/floatingips",
			key:  "floatingips",
			list: func(t *testing.T, service *Service) error {
				items, err := service.ListFloatingIPs(context.Background())
				if items != nil {
					t.Fatalf("expected invalid inventory to be discarded, got %#v", items)
				}
				return err
			},
		},
		{
			name: "security groups",
			path: "/v2.0/security-groups",
			key:  "security_groups",
			list: func(t *testing.T, service *Service) error {
				items, err := service.ListSecurityGroups(context.Background())
				if items != nil {
					t.Fatalf("expected invalid inventory to be discarded, got %#v", items)
				}
				return err
			},
		},
	}

	for _, resource := range resources {
		envelopes := []struct {
			name string
			body string
		}{
			{name: "top-level array", body: `[]`},
			{name: "empty object", body: `{}`},
			{
				name: "missing collection key",
				body: fmt.Sprintf(`{%q: []}`, resource.key+"_links"),
			},
			{name: "null collection", body: fmt.Sprintf(`{%q: null}`, resource.key)},
			{name: "non-array collection", body: fmt.Sprintf(`{%q: {}}`, resource.key)},
		}

		for _, envelope := range envelopes {
			t.Run(resource.name+"/"+envelope.name, func(t *testing.T) {
				service := newServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != resource.path {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
						w.WriteHeader(http.StatusNotFound)
						return
					}
					assertProjectQuery(t, r.URL.Query())
					w.Header().Set("Content-Type", "application/json")
					if _, err := w.Write([]byte(envelope.body)); err != nil {
						t.Errorf("writing response: %v", err)
					}
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

func TestListRejectsMalformedProjectOwnership(t *testing.T) {
	resources := []struct {
		name string
		path string
		key  string
		list func(*testing.T, *Service) error
	}{
		{
			name: "floating IPs",
			path: "/v2.0/floatingips",
			key:  "floatingips",
			list: func(t *testing.T, service *Service) error {
				items, err := service.ListFloatingIPs(context.Background())
				if items != nil {
					t.Fatalf("expected invalid inventory to be discarded, got %#v", items)
				}
				return err
			},
		},
		{
			name: "security groups",
			path: "/v2.0/security-groups",
			key:  "security_groups",
			list: func(t *testing.T, service *Service) error {
				items, err := service.ListSecurityGroups(context.Background())
				if items != nil {
					t.Fatalf("expected invalid inventory to be discarded, got %#v", items)
				}
				return err
			},
		},
	}
	ownershipCases := []struct {
		name   string
		fields map[string]any
	}{
		{name: "missing owner fields", fields: map[string]any{}},
		{
			name: "conflicting owner fields",
			fields: map[string]any{
				"project_id": testProjectID,
				"tenant_id":  "project-2",
			},
		},
	}

	for _, resource := range resources {
		for _, ownership := range ownershipCases {
			t.Run(resource.name+"/"+ownership.name, func(t *testing.T) {
				service := newServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != resource.path {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
						w.WriteHeader(http.StatusNotFound)
						return
					}
					item := map[string]any{"id": "unknown-owner", "description": "candidate"}
					if resource.key == "floatingips" {
						item["port_id"] = nil
					}
					for key, value := range ownership.fields {
						item[key] = value
					}
					w.Header().Set("Content-Type", "application/json")
					writeJSON(t, w, map[string]any{resource.key: []any{item}})
				})

				if err := resource.list(t, service); err == nil {
					t.Fatal("expected malformed project ownership to fail inventory")
				}
			})
		}
	}
}

func TestResourceBelongsToProjectFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		tenantID  string
		want      bool
		wantErr   bool
	}{
		{name: "matching project field", projectID: testProjectID, want: true},
		{name: "foreign project field", projectID: "project-2"},
		{name: "matching legacy tenant field", tenantID: testProjectID, want: true},
		{name: "foreign legacy tenant field", tenantID: "project-2"},
		{
			name:      "matching consistent fields",
			projectID: testProjectID,
			tenantID:  testProjectID,
			want:      true,
		},
		{
			name:      "consistent foreign fields",
			projectID: "project-2",
			tenantID:  "project-2",
		},
		{name: "both ownership fields omitted", wantErr: true},
		{
			name:      "matching project conflicts with tenant",
			projectID: testProjectID,
			tenantID:  "project-2",
			wantErr:   true,
		},
		{
			name:      "matching tenant conflicts with project",
			projectID: "project-2",
			tenantID:  testProjectID,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resourceBelongsToProject(tt.projectID, tt.tenantID, testProjectID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resourceBelongsToProject() error = %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf(
					"resourceBelongsToProject(%q, %q, %q) = %t, want %t (error %v)",
					tt.projectID,
					tt.tenantID,
					testProjectID,
					got,
					tt.want,
					err,
				)
			}
		})
	}
}

func TestListLaterPageErrorsDiscardPartialInventory(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		pageStatus int
		pageBody   string
	}{
		{name: "floating IP malformed response", kind: "floatingips", pageStatus: http.StatusOK, pageBody: `{`},
		{name: "floating IP request failure", kind: "floatingips", pageStatus: http.StatusInternalServerError, pageBody: `{}`},
		{name: "security group malformed response", kind: "security-groups", pageStatus: http.StatusOK, pageBody: `{`},
		{name: "security group request failure", kind: "security-groups", pageStatus: http.StatusInternalServerError, pageBody: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := newServiceFixture(t, paginatedFailureHandler(t, tt.kind, tt.pageStatus, tt.pageBody))

			switch tt.kind {
			case "floatingips":
				got, err := service.ListFloatingIPs(context.Background())
				if err == nil {
					t.Fatal("expected later-page error")
				}
				if got != nil {
					t.Fatalf("expected partial inventory to be discarded, got %#v", got)
				}
			case "security-groups":
				got, err := service.ListSecurityGroups(context.Background())
				if err == nil {
					t.Fatal("expected later-page error")
				}
				if got != nil {
					t.Fatalf("expected partial inventory to be discarded, got %#v", got)
				}
			default:
				t.Fatalf("unknown resource kind %q", tt.kind)
			}
		})
	}
}

func TestListHonoursContextCancellation(t *testing.T) {
	tests := []struct {
		name string
		list func(context.Context, *Service) error
	}{
		{
			name: "floating IPs",
			list: func(ctx context.Context, service *Service) error {
				got, err := service.ListFloatingIPs(ctx)
				if got != nil {
					t.Fatalf("expected no inventory, got %#v", got)
				}
				return err
			},
		},
		{
			name: "security groups",
			list: func(ctx context.Context, service *Service) error {
				got, err := service.ListSecurityGroups(ctx)
				if got != nil {
					t.Fatalf("expected no inventory, got %#v", got)
				}
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			service := newServiceFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := tt.list(ctx, service)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context cancellation, got %v", err)
			}
			if requests.Load() != 0 {
				t.Fatalf("expected canceled context to prevent request, got %d requests", requests.Load())
			}
		})
	}
}

func TestDeleteUsesExactTypedResourcePath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		deleteFunc func(context.Context, *Service) error
	}{
		{
			name: "floating IP",
			path: "/v2.0/floatingips/fip-1",
			deleteFunc: func(ctx context.Context, service *Service) error {
				return service.DeleteFloatingIP(ctx, "fip-1")
			},
		},
		{
			name: "security group",
			path: "/v2.0/security-groups/sg-1",
			deleteFunc: func(ctx context.Context, service *Service) error {
				return service.DeleteSecurityGroup(ctx, "sg-1")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			service := newServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodDelete || r.URL.Path != tt.path || r.URL.RawQuery != "" {
					t.Errorf("unexpected delete request %s %s", r.Method, r.URL.RequestURI())
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})

			if err := tt.deleteFunc(context.Background(), service); err != nil {
				t.Fatalf("deleting resource: %v", err)
			}
			if requests.Load() != 1 {
				t.Fatalf("expected exactly one delete request, got %d", requests.Load())
			}
		})
	}
}

func TestDeleteRejectsEmptyIDWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	service := newServiceFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	for _, deleteFunc := range []func(context.Context, string) error{
		service.DeleteFloatingIP,
		service.DeleteSecurityGroup,
	} {
		if err := deleteFunc(context.Background(), ""); err == nil {
			t.Fatal("expected empty ID to fail")
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("expected no request for empty IDs, got %d", requests.Load())
	}
}

func TestDeleteClassifiesResponsesAndPreservesCause(t *testing.T) {
	tests := []struct {
		name        string
		kind        string
		statusCode  int
		wantNil     bool
		wantPending bool
	}{
		{name: "floating IP not found", kind: "floatingips", statusCode: http.StatusNotFound, wantNil: true},
		{name: "floating IP bad request", kind: "floatingips", statusCode: http.StatusBadRequest, wantPending: true},
		{name: "floating IP conflict", kind: "floatingips", statusCode: http.StatusConflict, wantPending: true},
		{name: "security group not found", kind: "security-groups", statusCode: http.StatusNotFound, wantNil: true},
		{name: "security group bad request", kind: "security-groups", statusCode: http.StatusBadRequest, wantPending: true},
		{name: "security group conflict", kind: "security-groups", statusCode: http.StatusConflict, wantPending: true},
		{name: "floating IP server error", kind: "floatingips", statusCode: http.StatusInternalServerError},
		{name: "security group server error", kind: "security-groups", statusCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := newServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("expected DELETE, got %s", r.Method)
				}
				w.WriteHeader(tt.statusCode)
			})

			var err error
			switch tt.kind {
			case "floatingips":
				err = service.DeleteFloatingIP(context.Background(), "resource-id")
			case "security-groups":
				err = service.DeleteSecurityGroup(context.Background(), "resource-id")
			default:
				t.Fatalf("unknown resource kind %q", tt.kind)
			}

			if tt.wantNil {
				if err != nil {
					t.Fatalf("expected idempotent success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected delete error")
			}
			if !strings.Contains(err.Error(), "resource-id") {
				t.Fatalf("expected operation to include resource ID, got %v", err)
			}
			if tt.wantPending && !errors.Is(err, cleanup.ErrDeletePending) {
				t.Fatalf("expected pending classification, got %v", err)
			}
			var responseErr gophercloud.ErrUnexpectedResponseCode
			if !errors.As(err, &responseErr) {
				t.Fatalf("expected response cause to be preserved, got %v", err)
			}
			if responseErr.Actual != tt.statusCode {
				t.Fatalf("expected status %d cause, got %d", tt.statusCode, responseErr.Actual)
			}
		})
	}
}

func newServiceFixture(t *testing.T, neutronHandler http.HandlerFunc) *Service {
	t.Helper()
	client := newClientFixture(t, clientFixtureOptions{
		tokenStatus:    http.StatusCreated,
		projectID:      testProjectID,
		includeNetwork: true,
	}, neutronHandler)
	service, err := New(client)
	if err != nil {
		t.Fatalf("creating network service: %v", err)
	}
	return service
}

func newClientFixture(t *testing.T, opts clientFixtureOptions, neutronHandler http.HandlerFunc) *openstackclient.Client {
	t.Helper()
	if opts.tokenStatus == 0 {
		opts.tokenStatus = http.StatusCreated
	}
	if neutronHandler == nil {
		neutronHandler = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		if opts.tokenStatus != http.StatusCreated {
			w.WriteHeader(opts.tokenStatus)
			return
		}

		token := map[string]any{
			"user":    map[string]any{"id": "user-1"},
			"catalog": []any{},
		}
		if opts.projectID != "" {
			token["project"] = map[string]any{"id": opts.projectID}
		}
		w.Header().Set("X-Subject-Token", "token-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, map[string]any{"token": token})
	})
	mux.HandleFunc("/v3/auth/catalog", func(w http.ResponseWriter, r *http.Request) {
		endpoint := "http://" + r.Host
		entries := []any{}
		if opts.includeNetwork {
			entries = append(entries, map[string]any{
				"type": "network",
				"endpoints": []any{map[string]any{
					"interface": "public",
					"region_id": testRegion,
					"url":       endpoint,
				}},
			})
		} else {
			entries = append(entries, map[string]any{
				"type": "compute",
				"endpoints": []any{map[string]any{
					"interface": "public",
					"region_id": testRegion,
					"url":       endpoint,
				}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(t, w, map[string]any{"catalog": entries})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{
				"versions": []any{map[string]any{
					"id":     "v2.0",
					"status": "CURRENT",
				}},
			})
			return
		}
		neutronHandler(w, r)
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
      application_credential_secret: secret
    region_name: %s
    interface: public
`, server.URL, testRegion)
	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: cloudsYAML,
		CloudName:  "openstack",
	})
	if err != nil {
		t.Fatalf("creating OpenStack client: %v", err)
	}
	return client
}

func assertProjectQuery(t *testing.T, query url.Values) {
	t.Helper()
	if got := query.Get("project_id"); got != testProjectID {
		t.Errorf("expected project_id=%q, got %q", testProjectID, got)
	}
}

func paginatedFailureHandler(t *testing.T, kind string, pageStatus int, pageBody string) http.HandlerFunc {
	t.Helper()
	path := "/v2.0/" + kind
	listKey := strings.ReplaceAll(kind, "-", "_")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assertProjectQuery(t, r.URL.Query())
		if r.URL.Query().Get("page") == "2" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(pageStatus)
			_, _ = w.Write([]byte(pageBody))
			return
		}

		resource := map[string]any{
			"id":          "partial-resource",
			"description": "must be discarded",
			"project_id":  testProjectID,
		}
		if kind == "floatingips" {
			resource["port_id"] = nil
		}
		writeJSON(t, w, map[string]any{
			listKey: []any{resource},
			listKey + "_links": []any{map[string]any{
				"rel":  "next",
				"href": fmt.Sprintf("http://%s%s?page=2&project_id=%s", r.Host, path, testProjectID),
			}},
		})
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encoding fixture response: %v", err)
	}
}
