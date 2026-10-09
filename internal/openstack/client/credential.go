package client

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

// CredentialConfig describes a cloud entry without secret values.
// User and project IDs are optional because Keystone supplies them on login.
type CredentialConfig struct {
	Authority    string `json:"authority"`
	ProjectID    string `json:"projectID,omitempty"`
	UserID       string `json:"userID,omitempty"`
	Region       string `json:"region"`
	Interface    string `json:"interface"`
	CloudName    string `json:"cloudName"`
	CredentialID string `json:"credentialID"`
}

// ParseCredentialConfig validates the selected cloud without authenticating.
func ParseCredentialConfig(options Options) (CredentialConfig, error) {
	_, config, err := loadCredential(options)
	return config, err
}

func loadCredential(options Options) (*clientconfig.Cloud, CredentialConfig, error) {
	loader, err := newYAMLLoader(options.CloudsYAML)
	if err != nil {
		return nil, CredentialConfig{}, err
	}
	cloudName := options.CloudName
	if cloudName == "" {
		cloudName = defaultCloudName
	}
	cloud, err := clientconfig.GetCloudFromYAML(&clientconfig.ClientOpts{
		Cloud: cloudName, EnvPrefix: "CAPI_JANITOR_OPENSTACK_", YAMLOpts: loader,
	})
	if err != nil {
		return nil, CredentialConfig{}, fmt.Errorf("could not load cloud %q from clouds.yaml", cloudName)
	}
	if cloud.AuthInfo == nil {
		return nil, CredentialConfig{}, fmt.Errorf("cloud %q has no auth configuration", cloudName)
	}
	if cloud.AuthType != clientconfig.AuthV3ApplicationCredential {
		return nil, CredentialConfig{}, &UnsupportedAuthTypeError{AuthType: string(cloud.AuthType)}
	}
	if strings.TrimSpace(cloud.AuthInfo.ApplicationCredentialID) == "" {
		return nil, CredentialConfig{}, errors.New("application credential ID is empty")
	}
	if strings.TrimSpace(cloud.AuthInfo.ApplicationCredentialSecret) == "" {
		return nil, CredentialConfig{}, errors.New("application credential secret is empty")
	}
	authority, err := NormalizeAuthority(cloud.AuthInfo.AuthURL)
	if err != nil {
		return nil, CredentialConfig{}, err
	}
	config := CredentialConfig{
		Authority: authority, ProjectID: cloud.AuthInfo.ProjectID, UserID: cloud.AuthInfo.UserID,
		Region: cloud.RegionName, Interface: string(clientconfig.GetEndpointType(cloud.EndpointType)),
		CloudName: cloudName, CredentialID: cloud.AuthInfo.ApplicationCredentialID,
	}
	return cloud, config, nil
}

// NormalizeAuthority normalizes the host, default port and trailing slashes of a Keystone URL.
// It preserves deployment paths and omits the URL from validation errors.
func NormalizeAuthority(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("authentication URL is empty")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("authentication URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("authentication URL must not contain user information, a query, or a fragment")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	if (parsed.Scheme == "http" && parsed.Port() == "80") || (parsed.Scheme == "https" && parsed.Port() == "443") {
		parsed.Host = strings.TrimSuffix(parsed.Host, ":"+parsed.Port())
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	return parsed.String(), nil
}
