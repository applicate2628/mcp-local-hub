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

var marketplaceNonPublicSpecialAddrPrefixes = []struct {
	name   string
	prefix netip.Prefix
}{
	{"this-network", netip.MustParsePrefix("0.0.0.0/8")},
	{"cgnat", netip.MustParsePrefix("100.64.0.0/10")},
	{"ietf protocol assignments", netip.MustParsePrefix("192.0.0.0/24")},
	{"documentation", netip.MustParsePrefix("192.0.2.0/24")},
	{"as112", netip.MustParsePrefix("192.31.196.0/24")},
	{"amt", netip.MustParsePrefix("192.52.193.0/24")},
	{"6to4 relay anycast", netip.MustParsePrefix("192.88.99.0/24")},
	{"as112", netip.MustParsePrefix("192.175.48.0/24")},
	{"documentation", netip.MustParsePrefix("198.51.100.0/24")},
	{"documentation", netip.MustParsePrefix("203.0.113.0/24")},
	{"benchmarking", netip.MustParsePrefix("198.18.0.0/15")},
	{"limited broadcast", netip.MustParsePrefix("255.255.255.255/32")},
	{"reserved", netip.MustParsePrefix("240.0.0.0/4")},
	{"ietf protocol assignments", netip.MustParsePrefix("::/8")},
	{"ipv4-ipv6 translation", netip.MustParsePrefix("64:ff9b::/96")},
	{"ipv4-ipv6 translation", netip.MustParsePrefix("64:ff9b:1::/48")},
	{"discard-only", netip.MustParsePrefix("100::/64")},
	{"dummy", netip.MustParsePrefix("100:0:0:1::/64")},
	{"benchmarking", netip.MustParsePrefix("2001:2::/48")},
	{"amt", netip.MustParsePrefix("2001:3::/32")},
	{"as112", netip.MustParsePrefix("2001:4:112::/48")},
	{"orchid", netip.MustParsePrefix("2001:10::/28")},
	{"orchid", netip.MustParsePrefix("2001:20::/28")},
	{"drone remote id", netip.MustParsePrefix("2001:30::/28")},
	{"ietf protocol assignments", netip.MustParsePrefix("2001::/23")},
	{"documentation", netip.MustParsePrefix("2001:db8::/32")},
	{"6to4", netip.MustParsePrefix("2002::/16")},
	{"as112", netip.MustParsePrefix("2620:4f:8000::/48")},
	{"documentation", netip.MustParsePrefix("3fff::/20")},
	{"segment routing", netip.MustParsePrefix("5f00::/16")},
}

func rejectMarketplaceLocalOrPrivateAddr(subject string, addr netip.Addr) error {
	addr = addr.Unmap()
	switch {
	case !addr.IsValid():
		return fmt.Errorf("%s is invalid address", subject)
	case addr.IsLoopback():
		return fmt.Errorf("%s is loopback address %s", subject, addr)
	case addr.IsUnspecified():
		return fmt.Errorf("%s is unspecified address %s", subject, addr)
	case addr.IsPrivate():
		return fmt.Errorf("%s is private address %s", subject, addr)
	case addr.IsLinkLocalUnicast():
		return fmt.Errorf("%s is link-local address %s", subject, addr)
	case addr.IsMulticast():
		return fmt.Errorf("%s is multicast address %s", subject, addr)
	}
	for _, special := range marketplaceNonPublicSpecialAddrPrefixes {
		if special.prefix.Contains(addr) {
			return fmt.Errorf("%s is %s address %s", subject, special.name, addr)
		}
	}
	if !addr.IsGlobalUnicast() {
		return fmt.Errorf("%s is non-global-unicast address %s", subject, addr)
	}
	return nil
}
