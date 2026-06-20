package api

import (
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strings"
)

// IsUnsafeMarketplaceTextRune is the single terminal/draft safety predicate for
// strings that originate in untrusted marketplace catalogs. It covers terminal
// controls plus the Trojan-Source bidi controls: LRM/RLM/ALM,
// LRE/RLE/PDF/LRO/RLO, LRI/RLI/FSI/PDI, and LS/PS.
func IsUnsafeMarketplaceTextRune(r rune) bool {
	switch {
	case r == 0x1B:
		return true
	case r < 0x20:
		return true
	case r == 0x7F:
		return true
	case r >= 0x80 && r <= 0x9F:
		return true
	case r == 0x061C:
		return true
	case r >= 0x200E && r <= 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x2028 || r == 0x2029:
		return true
	default:
		return false
	}
}

func firstUnsafeMarketplaceTextRune(s string) (rune, bool) {
	for _, r := range s {
		if IsUnsafeMarketplaceTextRune(r) {
			return r, true
		}
	}
	return 0, false
}

func containsUnsafeMarketplaceText(s string) bool {
	_, ok := firstUnsafeMarketplaceTextRune(s)
	return ok
}

func rejectUnsafeMarketplaceDraftString(field, value string) error {
	if r, ok := firstUnsafeMarketplaceTextRune(value); ok {
		return fmt.Errorf("%s contains unsafe marketplace text rune U+%04X", field, r)
	}
	return nil
}

func rejectUnsafeMarketplaceDraftStringSlice(field string, values []string) error {
	for i, value := range values {
		if err := rejectUnsafeMarketplaceDraftString(fmt.Sprintf("%s[%d]", field, i), value); err != nil {
			return err
		}
	}
	return nil
}

func rejectUnsafeMarketplaceDraftStringMap(field string, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := rejectUnsafeMarketplaceDraftString(field+" key", key); err != nil {
			return err
		}
		if err := rejectUnsafeMarketplaceDraftString(fmt.Sprintf("%s[%q]", field, key), values[key]); err != nil {
			return err
		}
	}
	return nil
}

func rejectUnsafeMarketplaceWarnings(warnings []string) error {
	return rejectUnsafeMarketplaceDraftStringSlice("warning", warnings)
}

func parseMarketplacePublicHTTPSURL(raw string) (*url.URL, error) {
	if r, ok := firstUnsafeMarketplaceTextRune(raw); ok {
		return nil, fmt.Errorf("contains unsafe control or bidi rune U+%04X", r)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if err := validateMarketplacePublicHTTPSParsedURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

func validateMarketplacePublicHTTPSParsedURL(u *url.URL) error {
	if err := rejectUnsafeMarketplaceURLParts(u); err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must use https:// (got scheme %q)", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("must not embed credentials")
	}
	if err := rejectMarketplaceLocalOrPrivateHost(u.Hostname()); err != nil {
		return err
	}
	return nil
}

func rejectUnsafeMarketplaceURLParts(u *url.URL) error {
	parts := []struct {
		name  string
		value string
	}{
		{"scheme", u.Scheme},
		{"opaque", u.Opaque},
		{"host", u.Host},
		{"path", u.Path},
		{"raw path", u.RawPath},
		{"raw query", u.RawQuery},
		{"fragment", u.Fragment},
	}
	for _, part := range parts {
		if r, ok := firstUnsafeMarketplaceTextRune(part.value); ok {
			return fmt.Errorf("%s contains unsafe control or bidi rune U+%04X", part.name, r)
		}
	}
	return nil
}

func rejectMarketplaceLocalOrPrivateHost(host string) error {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return fmt.Errorf("host %q is the localhost loopback name", host)
	}
	hostForIP := host
	if i := strings.LastIndexByte(hostForIP, '%'); i >= 0 {
		hostForIP = hostForIP[:i]
	}
	addr, err := netip.ParseAddr(hostForIP)
	if err != nil {
		return nil
	}
	return rejectMarketplaceLocalOrPrivateAddr(fmt.Sprintf("host %q", host), addr)
}

func rejectMarketplaceLocalOrPrivateAddr(subject string, addr netip.Addr) error {
	addr = addr.Unmap()
	switch {
	case addr.IsLoopback():
		return fmt.Errorf("%s is loopback address %s", subject, addr)
	case addr.IsUnspecified():
		return fmt.Errorf("%s is unspecified address %s", subject, addr)
	case addr.IsPrivate():
		return fmt.Errorf("%s is private address %s", subject, addr)
	case addr.IsLinkLocalUnicast():
		return fmt.Errorf("%s is link-local address %s", subject, addr)
	default:
		return nil
	}
}
