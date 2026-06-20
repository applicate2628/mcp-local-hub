package api

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/urlredact"
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
		return fmt.Errorf("expanded remote-http url host rejected for %s: %s", urlredact.MarketplaceURLForError(expandedURL, displayURL), redactExpandedSecretHostReason(err.Error(), u.Hostname()))
	}
	return nil
}

func redactExpandedSecretHostReason(reason, host string) string {
	redacted := strings.ReplaceAll(reason, host, "<redacted-host>")
	hostForIP := host
	if i := strings.LastIndexByte(hostForIP, '%'); i >= 0 {
		hostForIP = hostForIP[:i]
	}
	if addr, err := netip.ParseAddr(hostForIP); err == nil {
		redacted = strings.ReplaceAll(redacted, addr.String(), "<redacted-host>")
		redacted = strings.ReplaceAll(redacted, addr.Unmap().String(), "<redacted-host>")
	}
	return redacted
}
