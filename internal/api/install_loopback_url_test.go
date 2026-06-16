package api

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// TestExpectedHubURL_Uses127NotLocalhost is a LATENCY regression guard: the hub
// must write the client binding URL with the explicit IPv4 loopback 127.0.0.1,
// NOT "localhost". "localhost" resolves ::1 first on an IPv6-preferring host
// and, since daemons bind 127.0.0.1 only, every client connection paid a ~2s
// ::1 connect-timeout fallback (measured 2008ms vs 18ms). 127.0.0.1 also dodges
// VPN / hosts-file re-routing of "localhost".
func TestExpectedHubURL_Uses127NotLocalhost(t *testing.T) {
	m := &config.ServerManifest{
		Name:    "demo",
		Daemons: []config.DaemonSpec{{Name: "default", Port: 9134}},
	}
	got := expectedHubURL(m, config.ClientBinding{Client: "claude-code", Daemon: "default", URLPath: "/mcp"})
	if !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Fatalf("binding URL must use 127.0.0.1 (not localhost — IPv6/VPN latency): %q", got)
	}
	if strings.Contains(got, "localhost") {
		t.Errorf("binding URL must NOT contain 'localhost': %q", got)
	}
	if got != "http://127.0.0.1:9134/mcp" {
		t.Errorf("binding URL = %q, want http://127.0.0.1:9134/mcp", got)
	}
}

// TestHubLoopbackEquivalentURL verifies the host-agnostic ownership match so a
// pre-existing http://localhost:<port>/mcp entry is still recognized as
// hub-owned now that the hub writes the 127.0.0.1 form (a re-install REPLACES
// it instead of orphaning + duplicating).
func TestHubLoopbackEquivalentURL(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://localhost:9134/mcp", "http://127.0.0.1:9134/mcp", true},
		{"http://[::1]:9134/mcp", "http://127.0.0.1:9134/mcp", true},
		{"http://127.0.0.1:9134/mcp", "http://127.0.0.1:9134/mcp", true},
		{"http://localhost:9134/mcp", "http://127.0.0.1:9135/mcp", false},   // different port
		{"http://localhost:9134/mcp", "http://127.0.0.1:9134/other", false}, // different path
		{"http://example.com:9134/mcp", "http://127.0.0.1:9134/mcp", false}, // non-loopback
	}
	for _, c := range cases {
		if got := hubLoopbackEquivalentURL(c.a, c.b); got != c.want {
			t.Errorf("hubLoopbackEquivalentURL(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if !clients.IsHubHTTPURL("http://localhost:1/mcp") {
		t.Error("precondition: IsHubHTTPURL should accept localhost")
	}
}
