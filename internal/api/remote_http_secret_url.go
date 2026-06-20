package api

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
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
	expandedHost, err := expandedRemoteHTTPPlaceholderHostValue(displayURL, expandedURL)
	if err != nil {
		return fmt.Errorf("expanded remote-http host for %s is invalid: %w", urlredact.MarketplaceURLForError(expandedURL, displayURL), err)
	}
	if err := validateExpandedRemoteHTTPHostAuthority(expandedHost); err != nil {
		return fmt.Errorf("expanded remote-http host for %s is invalid: %w", urlredact.MarketplaceURLForError(expandedURL, displayURL), err)
	}
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

func expandedRemoteHTTPPlaceholderHostValue(displayURL, expandedURL string) (string, error) {
	const prefix = "https://"
	if !strings.HasPrefix(displayURL, prefix) || !strings.HasPrefix(expandedURL, prefix) {
		return "", fmt.Errorf("placeholder URL must use https://")
	}
	rawRest := displayURL[len(prefix):]
	rawAuthority := rawRest
	rawSuffix := ""
	if end := strings.IndexAny(rawRest, "/?#"); end >= 0 {
		rawAuthority = rawRest[:end]
		rawSuffix = rawRest[end:]
	}
	closeBrace := strings.IndexByte(rawAuthority, '}')
	if closeBrace < 0 {
		return "", fmt.Errorf("placeholder host is malformed")
	}
	rawAuthorityTail := rawAuthority[closeBrace+1:]
	expandedRest := expandedURL[len(prefix):]
	expectedSuffix := rawAuthorityTail + rawSuffix
	if !strings.HasSuffix(expandedRest, expectedSuffix) {
		return "", fmt.Errorf("expanded URL shape changed")
	}
	expandedHost := strings.TrimSuffix(expandedRest, expectedSuffix)
	if expandedHost == "" {
		return "", fmt.Errorf("expanded host is empty")
	}
	return expandedHost, nil
}

func validateExpandedRemoteHTTPHostAuthority(authority string) error {
	if strings.ContainsAny(authority, "/@#?") {
		return fmt.Errorf("contains path, query, fragment, or authority delimiter")
	}
	if r, ok := firstUnsafeMarketplaceTextRune(authority); ok {
		return fmt.Errorf("contains unsafe control or bidi rune U+%04X", r)
	}
	host, port, err := splitExpandedRemoteHTTPHostAuthority(authority)
	if err != nil {
		return err
	}
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if !isDecimalPort(port) || err != nil || portNumber > 65535 {
			return fmt.Errorf("port is invalid")
		}
	}
	hostForIP := host
	if i := strings.LastIndexByte(hostForIP, '%'); i >= 0 {
		hostForIP = hostForIP[:i]
	}
	if _, err := netip.ParseAddr(strings.TrimSuffix(hostForIP, ".")); err == nil {
		return nil
	}
	if !isValidExpandedRemoteHTTPHostname(host) {
		return fmt.Errorf("hostname contains invalid characters")
	}
	return nil
}

func splitExpandedRemoteHTTPHostAuthority(authority string) (host, port string, err error) {
	if strings.HasPrefix(authority, "[") {
		end := strings.LastIndexByte(authority, ']')
		if end < 0 {
			return "", "", fmt.Errorf("bracketed IP address is missing closing bracket")
		}
		host = authority[1:end]
		if host == "" {
			return "", "", fmt.Errorf("host is empty")
		}
		if _, err := netip.ParseAddr(host); err != nil {
			return "", "", fmt.Errorf("bracketed host is not a valid IP address")
		}
		tail := authority[end+1:]
		if tail == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(tail, ":") || len(tail) == 1 {
			return "", "", fmt.Errorf("port is invalid")
		}
		return host, tail[1:], nil
	}
	if strings.ContainsAny(authority, "[]") {
		return "", "", fmt.Errorf("host contains invalid bracket")
	}
	if strings.Count(authority, ":") > 1 {
		return "", "", fmt.Errorf("IPv6 address must be bracketed")
	}
	host, port, hasPort := strings.Cut(authority, ":")
	if host == "" {
		return "", "", fmt.Errorf("host is empty")
	}
	if hasPort && port == "" {
		return "", "", fmt.Errorf("port is invalid")
	}
	return host, port, nil
}

func isDecimalPort(port string) bool {
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return port != ""
}

func isValidExpandedRemoteHTTPHostname(host string) bool {
	trimmed := strings.TrimSuffix(host, ".")
	if trimmed == "" || len(trimmed) > 253 {
		return false
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
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
