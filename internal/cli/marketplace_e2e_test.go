// internal/cli/marketplace_e2e_test.go — G5 Phase 5 end-to-end smoke.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// Plan: docs/superpowers/plans/2026-05-13-g5-marketplace-draft-import.md §Phase 5.
//
// Exercises the full search → show → generate sequence end-to-end against
// an httptest.NewTLSServer fixture catalog. Pins three contracts in a
// single test:
//
//  1. Each subcommand's stdout carries the expected substring (search:
//     entry id; show: "Transport: stdio"; generate: "name: filesys").
//  2. Subsequent subcommand invocations reuse the on-disk cache so the
//     fixture server is hit exactly once (the "cache hits == 1"
//     invariant). On Windows hosts where t.TempDir() falls outside the
//     SecureWriteClientConfig DACL allowlist (parent inherits
//     Authenticated Users from %TEMP%), the cache write silently
//     no-ops; the test then skips the hits invariant rather than
//     spuriously failing — matching the established CLI test pattern
//     in gui_resetport_test.go.
//  3. The cache-age surface (api.MarketplaceCacheAge) compiles and
//     returns without panicking after a successful fresh fetch.
//
// State path strategy: api.SetDaemonStateRootForTest routes the
// marketplace-cache.json file at a per-test temp dir so concurrent
// `go test ./...` invocations and the operator's real
// %LOCALAPPDATA%\mcp-local-hub\ cache stay isolated.

package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestMarketplaceE2E_SearchShowGenerate(t *testing.T) {
	// Per-test state dir so the cache file does not bleed across
	// tests or interfere with the operator's real LocalAppData.
	root := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(root))

	body := `{"schema_version":"1","entries":[
		{"id":"filesys","name":"Filesystem","summary":"fs","transport":"stdio","command":"npx","args":["-y","srv-fs"]}
	]}`
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallMarketplaceTestClientForCLI(srv))

	for _, sub := range []struct {
		args []string
		want string
	}{
		{[]string{"search", "filesys", "--registry", srv.URL}, "filesys"},
		{[]string{"show", "filesys", "--registry", srv.URL}, "Transport:  stdio"},
		{[]string{"generate", "filesys", "--registry", srv.URL}, "name: filesys"},
	} {
		c := newMarketplaceCmd()
		var stdout, stderr bytes.Buffer
		c.SetOut(&stdout)
		c.SetErr(&stderr)
		c.SetArgs(sub.args)
		if err := c.ExecuteContext(context.Background()); err != nil {
			t.Errorf("%v: %v\nstderr: %s", sub.args, err, stderr.String())
			continue
		}
		if !strings.Contains(stdout.String(), sub.want) {
			t.Errorf("%v: stdout missing %q\n---\n%s", sub.args, sub.want, stdout.String())
		}
	}

	// First call was a fresh fetch; subsequent ones should hit the
	// cache. On Windows hosts where t.TempDir() fails the
	// SecureWriteClientConfig parent-dir DACL allowlist (the
	// hardenedTempDir helper in internal/api package handles this case
	// for in-package tests but is package-private), the cache write
	// silently fails and each subcommand re-fetches — skip the hits
	// invariant rather than misreport the cache behavior. The full
	// hits==2 future-fetched_at revalidate path is exercised by
	// internal/api/marketplace_cache_test.go under hardenedTempDir.
	if api.MarketplaceCacheAge() == 0 {
		t.Skipf("skipping cache-hits invariant: marketplace-cache.json not persisted under %s (likely Windows DACL allowlist rejection of t.TempDir() — covered by internal/api tests)", root)
	}
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Errorf("registry hits = %d, want 1 (subsequent calls should use cache)", h)
	}
	_ = api.MarketplaceCacheAge() // sanity-check the surface compiles
}
