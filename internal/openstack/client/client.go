// Package client creates authenticated OpenStack clients from credentials
// stored by a CAPO OpenStackCluster.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	defaultCloudName = "openstack"
	requestTimeout   = 30 * time.Second
	userAgent        = "capi-janitor-openstack-go"
)

// UnsupportedAuthTypeError is returned when a cloud uses an authentication
// method outside the two methods supported by the Janitor.
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

// Client owns an authenticated Gophercloud provider and the endpoint selection
// resolved from clouds.yaml.
//
// OpenStack resource adapters may use ProviderClient and EndpointOpts with the
// typed constructors in Gophercloud. Cleanup policy and Kubernetes
// reconciliation should depend on the interfaces in internal/cleanup instead.
type Client struct {
	provider                *gophercloud.ProviderClient
	endpointOpts            gophercloud.EndpointOpts
	userID                  string
	projectID               string
	applicationCredentialID string
	authenticated           bool
}

// NewClient parses an in-memory clouds.yaml entry and authenticates with
// Gophercloud. Both v3 application credential and v3 password authentication
// are supported. Other authentication methods fail closed.
func NewClient(ctx context.Context, opts Options) (*Client, error) {
	loader, err := newYAMLLoader(opts.CloudsYAML)
	if err != nil {
		return nil, err
	}

	cloudName := opts.CloudName
	if cloudName == "" {
		cloudName = defaultCloudName
	}

	clientOpts := &clientconfig.ClientOpts{
		Cloud:     cloudName,
		EnvPrefix: "CAPI_JANITOR_OPENSTACK_",
		YAMLOpts:  loader,
	}
	cloud, err := clientconfig.GetCloudFromYAML(clientOpts)
	if err != nil {
		return nil, err
	}
	if cloud.AuthInfo == nil {
		return nil, fmt.Errorf("cloud %q has no auth configuration", cloudName)
	}

	authType, err := resolveAuthType(cloud)
	if err != nil {
		return nil, err
	}
	cloud.AuthType = authType
	cloud.AuthInfo.AllowReauth = true
	loader.clouds[cloudName] = *cloud

	httpClient, err := newHTTPClient(cloud, opts.CACert)
	if err != nil {
		return nil, err
	}

	authOpts, err := clientconfig.AuthOptions(clientOpts)
	if err != nil {
		return nil, err
	}
	authOpts.AllowReauth = true

	provider, err := openstack.NewClient(authOpts.IdentityEndpoint)
	if err != nil {
		return nil, err
	}
	provider.HTTPClient = *httpClient
	provider.UserAgent.Prepend(userAgent)

	client := &Client{
		provider: provider,
		endpointOpts: gophercloud.EndpointOpts{
			Region:       cloud.RegionName,
			Availability: clientconfig.GetEndpointType(cloud.EndpointType),
		},
		applicationCredentialID: cloud.AuthInfo.ApplicationCredentialID,
	}

	if err := openstack.Authenticate(ctx, provider, *authOpts); err != nil {
		if authType == clientconfig.AuthV3ApplicationCredential && gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return client, nil
		}
		return nil, err
	}

	if err := client.loadIdentity(ctx); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return client, nil
		}
		return nil, err
	}

	return client, nil
}

// ApplicationCredentialID returns the application credential ID from the
// selected cloud entry without authenticating. Password-authenticated entries
// return an empty ID.
func ApplicationCredentialID(cloudsYAML, cloudName string) (string, error) {
	loader, err := newYAMLLoader(cloudsYAML)
	if err != nil {
		return "", err
	}
	if cloudName == "" {
		cloudName = defaultCloudName
	}
	cloud, err := clientconfig.GetCloudFromYAML(&clientconfig.ClientOpts{
		Cloud:     cloudName,
		EnvPrefix: "CAPI_JANITOR_OPENSTACK_",
		YAMLOpts:  loader,
	})
	if err != nil {
		return "", err
	}
	if cloud.AuthInfo == nil {
		return "", nil
	}
	return cloud.AuthInfo.ApplicationCredentialID, nil
}

func (c *Client) loadIdentity(ctx context.Context) error {
	if result := c.provider.GetAuthResult(); result != nil {
		if extractor, ok := result.(interface {
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
		if extractor, ok := result.(interface {
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

	identity, err := openstack.NewIdentityV3(c.provider, gophercloud.EndpointOpts{})
	if err != nil {
		return err
	}
	var entries []tokens.CatalogEntry
	if err := catalog.List(identity).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		pageEntries, err := catalog.ExtractServiceCatalog(page)
		if err != nil {
			return false, err
		}
		entries = append(entries, pageEntries...)
		return true, nil
	}); err != nil {
		return err
	}

	serviceCatalog := &tokens.ServiceCatalog{Entries: entries}
	c.provider.EndpointLocator = func(opts gophercloud.EndpointOpts) (string, error) {
		return openstack.V3Endpoint(context.TODO(), c.provider, serviceCatalog, opts)
	}
	c.authenticated = len(entries) > 0
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

// ProjectID returns the project ID carried by the current token when present.
func (c *Client) ProjectID() string {
	if c == nil {
		return ""
	}
	return c.projectID
}

// ApplicationCredentialID returns the exact ID from the selected cloud entry.
// It is empty for password authentication.
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

// Endpoint locates a service endpoint using the region and interface selected
// from clouds.yaml. It is retained for the legacy resource implementation and
// will be removed after typed Gophercloud adapters replace that implementation.
func (c *Client) Endpoint(serviceType string) (string, error) {
	if c == nil || c.provider == nil || c.provider.EndpointLocator == nil {
		return "", &gophercloud.ErrEndpointNotFound{}
	}
	opts := c.endpointOpts
	opts.ApplyDefaults(serviceType)
	return c.provider.EndpointLocator(opts)
}

// Request performs an authenticated Gophercloud request. It is retained for
// the legacy resource implementation. New resource adapters should use typed
// Gophercloud service packages instead.
func (c *Client) Request(ctx context.Context, method, url string, opts *gophercloud.RequestOpts) (*http.Response, error) {
	return c.provider.Request(ctx, method, url, opts)
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

func resolveAuthType(cloud *clientconfig.Cloud) (clientconfig.AuthType, error) {
	if cloud.AuthInfo == nil {
		return "", &UnsupportedAuthTypeError{AuthType: string(cloud.AuthType)}
	}

	switch cloud.AuthType {
	case clientconfig.AuthV3ApplicationCredential, clientconfig.AuthV3Password:
		return cloud.AuthType, nil
	case "", clientconfig.AuthPassword:
		if cloud.AuthInfo.ApplicationCredentialID != "" || cloud.AuthInfo.ApplicationCredentialSecret != "" {
			return clientconfig.AuthV3ApplicationCredential, nil
		}
		if cloud.AuthInfo.Username != "" || cloud.AuthInfo.UserID != "" {
			return clientconfig.AuthV3Password, nil
		}
	}

	return "", &UnsupportedAuthTypeError{AuthType: string(cloud.AuthType)}
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
	return &http.Client{Transport: transport, Timeout: requestTimeout}, nil
}
