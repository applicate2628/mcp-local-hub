package api

import (
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

func TestSerenaRouterClientURL(t *testing.T) {
	got := SerenaRouterClientURL(9125)
	want := "http://127.0.0.1:9125/serena/mcp"
	if got != want {
		t.Fatalf("SerenaRouterClientURL(9125) = %q, want %q", got, want)
	}
}

func TestIsSerenaRouterURL(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"router URL on GUI port", "http://127.0.0.1:9125/serena/mcp", true},
		{"router URL on a different port (port-agnostic)", "http://127.0.0.1:9130/serena/mcp", true},
		{"localhost spelling", "http://localhost:9125/serena/mcp", true},
		{"loopback but legacy /mcp path", "http://127.0.0.1:9121/mcp", false},
		{"loopback but other path", "http://127.0.0.1:9125/clients/cursor/mcp", false},
		{"non-loopback remote with serena path", "https://evil.example.com/serena/mcp", false},
		{"garbage", "::not a url::", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSerenaRouterURL(tc.endpoint); got != tc.want {
				t.Fatalf("IsSerenaRouterURL(%q) = %v, want %v", tc.endpoint, got, tc.want)
			}
		})
	}
}

// TestIsHubOwnedEntry_SerenaRouter is the uninstall-side guard: serena's router
// client URL must be recognized as hub-owned (so uninstall removes it) even
// though expectedURL is the legacy 9121 URL it can never match. The recognition
// is name-gated to serena.
func TestIsHubOwnedEntry_SerenaRouter(t *testing.T) {
	routerEntry := &clients.MCPEntry{Name: "serena", URL: "http://127.0.0.1:9130/serena/mcp"}
	legacyExpected := "http://127.0.0.1:9121/mcp"

	if !isHubOwnedEntry(routerEntry, "serena", "unified", legacyExpected) {
		t.Error("serena router entry not recognized as hub-owned (uninstall would orphan it)")
	}
	// Name-gated: the same router-shaped URL under a non-serena server is NOT
	// auto-recognized (it must match expectedURL the normal way).
	if isHubOwnedEntry(&clients.MCPEntry{Name: "memory", URL: "http://127.0.0.1:9130/serena/mcp"}, "memory", "default", legacyExpected) {
		t.Error("non-serena server wrongly recognized via the serena router rule")
	}
}

// stubMigrateClient is a non-nil clients.Client whose methods are never called:
// the migrateOneBinding dry-run path returns after computing the URL, before any
// adapter method runs. Embedding the interface satisfies it without implementing
// every method (a call would nil-panic, which the dry-run path never triggers).
type stubMigrateClient struct{ clients.Client }

// TestMigrateOneBinding_SerenaUsesRouterURL is the write-side guard for the
// serena-client-revert-on-manifest-sync defect: checking serena's matrix cell
// must write the /serena/mcp router URL on the live GUI port, NOT the legacy
// per-daemon 9121 URL from serena's still-legacy-shaped manifest. A non-serena
// server is unaffected, and a zero guiPort (CLI/non-GUI caller) falls back to
// the generic per-daemon URL.
func TestMigrateOneBinding_SerenaUsesRouterURL(t *testing.T) {
	serenaManifest := &config.ServerManifest{
		Name:    "serena",
		Daemons: []config.DaemonSpec{{Name: "unified", Port: 9121}},
	}
	memoryManifest := &config.ServerManifest{
		Name:    "memory",
		Daemons: []config.DaemonSpec{{Name: "default", Port: 9128}},
	}
	binding := config.ClientBinding{Client: "claude-code", Daemon: "unified", URLPath: "/mcp"}
	memBinding := config.ClientBinding{Client: "claude-code", Daemon: "default", URLPath: "/mcp"}
	allClients := map[string]clients.Client{"claude-code": stubMigrateClient{}}

	cases := []struct {
		name    string
		m       *config.ServerManifest
		server  string
		binding config.ClientBinding
		guiPort int
		wantURL string
	}{
		{
			name:    "serena + live GUI port -> router URL",
			m:       serenaManifest,
			server:  "serena",
			binding: binding,
			guiPort: 9125,
			wantURL: "http://127.0.0.1:9125/serena/mcp",
		},
		{
			name:    "serena + zero guiPort (CLI) -> legacy per-daemon URL",
			m:       serenaManifest,
			server:  "serena",
			binding: binding,
			guiPort: 0,
			wantURL: "http://127.0.0.1:9121/mcp",
		},
		{
			name:    "non-serena server + live GUI port -> legacy per-daemon URL (no router leak)",
			m:       memoryManifest,
			server:  "memory",
			binding: memBinding,
			guiPort: 9125,
			wantURL: "http://127.0.0.1:9128/mcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := &MigrateReport{}
			migrateOneBinding(report, allClients, tc.m, tc.server, tc.binding, true /*dryRun*/, 0, tc.guiPort)
			if len(report.Applied) != 1 {
				t.Fatalf("Applied = %d rows, want 1 (failed: %+v)", len(report.Applied), report.Failed)
			}
			if got := report.Applied[0].URL; got != tc.wantURL {
				t.Fatalf("URL = %q, want %q", got, tc.wantURL)
			}
		})
	}
}
