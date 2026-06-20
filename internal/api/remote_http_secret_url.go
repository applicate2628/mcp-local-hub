package api

import (
	"fmt"
	"net/url"

	"mcp-local-hub/internal/config"
)

func expandRemoteHTTPURLSecrets(raw string, lookup SecretLookup) (string, error) {
	expanded, err := ExpandSecrets(raw, lookup)
	if err != nil {
		return "", err
	}
	if !config.RemoteHTTPURLHasSecretPlaceholderHost(raw) {
		return expanded, nil
	}
	if err := validateExpandedRemoteHTTPPlaceholderHost(raw, expanded); err != nil {
		return "", err
	}
	return expanded, nil
}

func validateExpandedRemoteHTTPPlaceholderHost(displayURL, expandedURL string) error {
	u, err := url.Parse(expandedURL)
	if err != nil {
		return scrubbedURLErrorf(err, expandedURL, displayURL, "expanded remote-http url is invalid")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("expanded remote-http url must use https:// (got scheme %q)", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("expanded remote-http url must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("expanded remote-http url must not embed credentials")
	}
	if err := rejectMarketplaceLocalOrPrivateHost(u.Hostname()); err != nil {
		return fmt.Errorf("expanded remote-http url host rejected: %w", err)
	}
	return nil
}
