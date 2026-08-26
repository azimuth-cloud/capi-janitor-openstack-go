// Package client creates authenticated OpenStack clients from credentials
// stored by a CAPO OpenStackCluster.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/catalog"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
	"sigs.k8s.io/yaml"
)

const (
	defaultCloudName   = "openstack"
	httpRequestTimeout = 60 * time.Second
	userAgent          = "capi-janitor-openstack-go"
)

// UnsupportedAuthTypeError is returned when a cloud uses an authentication
// method outside the application credential method supported by the Janitor.
type UnsupportedAuthTypeError struct {
	AuthType string
}

func (e *UnsupportedAuthTypeError) Error() string {
	return fmt.Sprintf("unsupported authentication type: %s", e.AuthType)
}

// Options contains the credential material needed to create a client.
type Options struct {
	CloudsYAML string
	CloudName  string
	CACert     string
}

// Client holds an authenticated Gophercloud provider and the endpoint settings
// read from clouds.yaml.
//
// Resource services use ProviderClient and EndpointOpts to create typed
// Gophercloud clients. Cleanup and controller code use the small interfaces in
// internal/cleanup.
type Client struct {
	provider                *gophercloud.ProviderClient
	endpointOpts            gophercloud.EndpointOpts
	userID                  string
	projectID               string
	applicationCredentialID string
	authenticated           bool
}

// NewClient parses an in-memory clouds.yaml entry and authenticates with
// Gophercloud using an explicit v3 application credential.
func NewClient(ctx context.Context, options Options) (*Client, error) {
	cloudLoader, err := newYAMLLoader(options.CloudsYAML)
	if err != nil {
		return nil, err
	}

	cloudName := options.CloudName
	if cloudName == "" {
		cloudName = defaultCloudName
	}

	clientOptions := &clientconfig.ClientOpts{
		Cloud:     cloudName,
		EnvPrefix: "CAPI_JANITOR_OPENSTACK_",
		YAMLOpts:  cloudLoader,
	}
	cloud, err := clientconfig.GetCloudFromYAML(clientOptions)
	if err != nil {
		return nil, err
	}
	if cloud.AuthInfo == nil {
		return nil, fmt.Errorf("cloud %q has no auth configuration", cloudName)
	}

	if err := requireApplicationCredential(cloud); err != nil {
		return nil, err
	}
	cloud.AuthInfo.AllowReauth = true
	cloudLoader.clouds[cloudName] = *cloud

	httpClient, err := newHTTPClient(cloud, options.CACert)
	if err != nil {
		return nil, err
	}

	configuredAuth, err := clientconfig.AuthOptions(clientOptions)
	if err != nil {
		return nil, err
	}
	credentialAuth := gophercloud.AuthOptions{
		IdentityEndpoint:            configuredAuth.IdentityEndpoint,
		ApplicationCredentialID:     cloud.AuthInfo.ApplicationCredentialID,
		ApplicationCredentialSecret: cloud.AuthInfo.ApplicationCredentialSecret,
		AllowReauth:                 true,
	}

	providerClient, err := openstack.NewClient(credentialAuth.IdentityEndpoint)
	if err != nil {
		return nil, err
	}
	providerClient.HTTPClient = *httpClient
	providerClient.UserAgent.Prepend(userAgent)

	client := &Client{
		provider: providerClient,
		endpointOpts: gophercloud.EndpointOpts{
			Region:       cloud.RegionName,
			Availability: clientconfig.GetEndpointType(cloud.EndpointType),
		},
		applicationCredentialID: cloud.AuthInfo.ApplicationCredentialID,
	}

	if err := openstack.Authenticate(ctx, providerClient, credentialAuth); err != nil {
		return nil, err
	}

	if err := client.loadTokenAndCatalog(ctx); err != nil {
		return nil, err
	}
	if client.userID == "" {
		return nil, errors.New("authenticated user ID is empty")
	}
	if client.projectID == "" {
		return nil, errors.New("authenticated project ID is empty")
	}

	return client, nil
}

func (c *Client) loadTokenAndCatalog(ctx context.Context) error {
	if authResult := c.provider.GetAuthResult(); authResult != nil {
		if extractor, ok := authResult.(interface {
			ExtractUser() (*tokens.User, error)
		}); ok {
			user, err := extractor.ExtractUser()
			if err != nil {
				return err
			}
			if user != nil {
				c.userID = user.ID
			}
		}
		if extractor, ok := authResult.(interface {
			ExtractProject() (*tokens.Project, error)
		}); ok {
			project, err := extractor.ExtractProject()
			if err != nil {
				return err
			}
			if project != nil {
				c.projectID = project.ID
			}
		}
	}

	identityService, err := openstack.NewIdentityV3(c.provider, gophercloud.EndpointOpts{})
	if err != nil {
		return err
	}
	var catalogEntries []tokens.CatalogEntry
	if err := catalog.List(identityService).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		pageEntries, err := catalog.ExtractServiceCatalog(page)
		if err != nil {
			return false, err
		}
		catalogEntries = append(catalogEntries, pageEntries...)
		return true, nil
	}); err != nil {
		return err
	}

	serviceCatalog := &tokens.ServiceCatalog{Entries: catalogEntries}
	c.provider.EndpointLocator = func(endpointOptions gophercloud.EndpointOpts) (string, error) {
		return openstack.V3Endpoint(context.TODO(), c.provider, serviceCatalog, endpointOptions)
	}
	c.authenticated = true
	return nil
}

// IsAuthenticated reports whether authentication and catalog discovery both
// completed successfully.
func (c *Client) IsAuthenticated() bool {
	return c != nil && c.authenticated
}

// UserID returns the ID of the user that owns the current token.
func (c *Client) UserID() string {
	if c == nil {
		return ""
	}
	return c.userID
}

// ProjectID returns the project ID carried by the current token.
func (c *Client) ProjectID() string {
	if c == nil {
		return ""
	}
	return c.projectID
}

// ApplicationCredentialID returns the exact ID from the selected cloud entry.
func (c *Client) ApplicationCredentialID() string {
	if c == nil {
		return ""
	}
	return c.applicationCredentialID
}

// ProviderClient returns the authenticated provider used to create typed
// Gophercloud service clients. Callers must not replace its authentication or
// HTTP transport configuration.
func (c *Client) ProviderClient() *gophercloud.ProviderClient {
	if c == nil {
		return nil
	}
	return c.provider
}

// EndpointOpts returns the region and interface selected from clouds.yaml.
func (c *Client) EndpointOpts() gophercloud.EndpointOpts {
	if c == nil {
		return gophercloud.EndpointOpts{}
	}
	return c.endpointOpts
}

type yamlLoader struct {
	clouds map[string]clientconfig.Cloud
}

func newYAMLLoader(data string) (*yamlLoader, error) {
	var parsed clientconfig.Clouds
	if err := yaml.Unmarshal([]byte(data), &parsed); err != nil {
		return nil, fmt.Errorf("parsing clouds.yaml: %w", err)
	}
	return &yamlLoader{clouds: parsed.Clouds}, nil
}

func (l *yamlLoader) LoadCloudsYAML() (map[string]clientconfig.Cloud, error) {
	return l.clouds, nil
}

func (*yamlLoader) LoadSecureCloudsYAML() (map[string]clientconfig.Cloud, error) {
	return nil, nil
}

func (*yamlLoader) LoadPublicCloudsYAML() (map[string]clientconfig.Cloud, error) {
	return nil, nil
}

func requireApplicationCredential(cloud *clientconfig.Cloud) error {
	if cloud.AuthInfo == nil {
		return &UnsupportedAuthTypeError{AuthType: string(cloud.AuthType)}
	}

	if cloud.AuthType != clientconfig.AuthV3ApplicationCredential {
		return &UnsupportedAuthTypeError{AuthType: string(cloud.AuthType)}
	}
	if cloud.AuthInfo.ApplicationCredentialID == "" {
		return errors.New("application credential ID is empty")
	}
	if cloud.AuthInfo.ApplicationCredentialSecret == "" {
		return errors.New("application credential secret is empty")
	}
	return nil
}

func newHTTPClient(cloud *clientconfig.Cloud, caCert string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cloud.Verify != nil && !*cloud.Verify {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // explicitly requested by clouds.yaml
	}
	if caCert != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(caCert)) {
			return nil, fmt.Errorf("failed to append CA certificate")
		}
		tlsConfig.RootCAs = pool
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport, Timeout: httpRequestTimeout}, nil
}
