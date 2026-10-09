package client_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	openstackclient "github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack/client"
)

func TestParseCredentialConfigNormalizesAuthorityAndInterface(t *testing.T) {
	for _, test := range []struct {
		url, iface, wantAuthority, wantInterface string
	}{
		{"https://KEYSTONE.example:443/deployment/identity/v3/", "", "https://keystone.example/deployment/identity/v3", "public"},
		{"http://KEYSTONE.example:80/v3/", "internalURL", "http://keystone.example/v3", "internal"},
		{"https://[2001:db8::1]:443/prefix/v3", "admin", "https://[2001:db8::1]/prefix/v3", "admin"},
		{"https://keystone.example:5000/other/v3", "public", "https://keystone.example:5000/other/v3", "public"},
	} {
		t.Run(test.url, func(t *testing.T) {
			config, err := openstackclient.ParseCredentialConfig(openstackclient.Options{
				CloudsYAML: fmt.Sprintf(`clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %s
      application_credential_id: appcred-1
      application_credential_secret: private-credential-value
      user_id: configured-user
      project_id: configured-project
    region_name: RegionOne
    interface: %s
`, test.url, test.iface),
			})
			if err != nil {
				t.Fatal(err)
			}
			want := openstackclient.CredentialConfig{
				Authority: test.wantAuthority, Interface: test.wantInterface, CloudName: "openstack",
				Region: "RegionOne", CredentialID: "appcred-1", UserID: "configured-user", ProjectID: "configured-project",
			}
			if config != want {
				t.Fatalf("config = %+v, want %+v", config, want)
			}
			if strings.Contains(fmt.Sprintf("%+v", config), "private-credential-value") {
				t.Fatal("credential description contains its secret")
			}
		})
	}
}

func TestParseCredentialConfigSelectsCloud(t *testing.T) {
	options := openstackclient.Options{CloudName: "selected", CloudsYAML: `clouds:
  openstack:
    auth_type: v3password
  selected:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://identity.example/v3
      application_credential_id: selected-credential
      application_credential_secret: private-credential-value
`}
	config, err := openstackclient.ParseCredentialConfig(options)
	if err != nil {
		t.Fatal(err)
	}
	if config.CloudName != "selected" || config.CredentialID != "selected-credential" {
		t.Fatalf("wrong selected cloud: %+v", config)
	}
	options.CloudName = "missing"
	if _, err := openstackclient.ParseCredentialConfig(options); err == nil {
		t.Fatal("missing cloud must not fall back to another entry")
	}
}

func TestClientRejectsUnsafeAuthenticationURLsBeforeRequests(t *testing.T) {
	for _, authURL := range []string{
		"", "ftp://identity.example/v3", "identity.example/v3", "https://identity.example/v3?token=private-credential-value",
		"https://user:private-credential-value@identity.example/v3", "https://identity.example/v3#private-credential-value",
	} {
		t.Run(authURL, func(t *testing.T) {
			_, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
				CloudsYAML: fmt.Sprintf(`clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: %q
      application_credential_id: appcred-1
      application_credential_secret: private-credential-value
`, authURL),
			})
			if err == nil {
				t.Fatal("unsafe authentication URL accepted")
			}
			if strings.Contains(err.Error(), "private-credential-value") {
				t.Fatalf("error contains credential material: %v", err)
			}
		})
	}
}

func TestClientDoesNotOverrideCloudAuthenticationFromEnvironment(t *testing.T) {
	t.Setenv("CAPI_JANITOR_OPENSTACK_AUTH_URL", "https://wrong-identity.example/v3")
	t.Setenv("CAPI_JANITOR_OPENSTACK_APPLICATION_CREDENTIAL_ID", "wrong-credential")
	t.Setenv("OS_AUTH_URL", "https://wrong-identity.example/v3")
	server := newKeystoneServer(t, http.StatusCreated, http.StatusOK, "user-1", "project-1")
	client, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: buildCredentialCloudsYAML(server.URL, "appcred-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if config := client.CredentialConfig(); config.Authority != server.URL+"/v3" || config.CredentialID != "appcred-1" {
		t.Fatalf("environment changed credential settings: %+v", config)
	}
}

func TestClientRedactsAuthenticationErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid credential private-credential-value", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	_, err := openstackclient.NewClient(context.Background(), openstackclient.Options{
		CloudsYAML: strings.ReplaceAll(buildCredentialCloudsYAML(server.URL, "appcred-1"), "secret: secret", "secret: private-credential-value"),
	})
	if err == nil || strings.Contains(err.Error(), "private-credential-value") {
		t.Fatalf("expected a redacted authentication error, got %v", err)
	}
	var responseErr gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &responseErr) || responseErr.Actual != http.StatusUnauthorized {
		t.Fatalf("redaction lost the error classification: %v", err)
	}
}

func TestParseCredentialConfigDoesNotExposeMalformedYAML(t *testing.T) {
	_, err := openstackclient.ParseCredentialConfig(openstackclient.Options{
		CloudsYAML: "clouds: [private-credential-value]",
	})
	if err == nil || strings.Contains(err.Error(), "private-credential-value") {
		t.Fatalf("expected a safe YAML error, got %v", err)
	}
}
