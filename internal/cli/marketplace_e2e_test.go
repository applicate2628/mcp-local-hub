// internal/cli/marketplace_e2e_test.go — G5 Phase 5 end-to-end smoke.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// Plan: docs/superpowers/plans/2026-05-13-g5-marketplace-draft-import.md §Phase 5.
//
// Exercises the full search → show → generate sequence end-to-end
// against an httptest.NewTLSServer fixture catalog. Pins four
// contracts in a single test (codex deep-sec PR #163 lane 3 P1 + P2
// closure: cache-hits invariant is no longer a SKIP on Windows; the
// stdio→stdio-bridge mapping is asserted from CLI output, not only
// from internal/api tests):
//
//  1. Each subcommand's stdout carries the expected substring
//     (search: entry id; show: "Transport: stdio"; generate:
//     "name: filesys", "transport: stdio-bridge", "command: npx").
//  2. Subsequent subcommand invocations reuse the on-disk cache so
//     the fixture server is hit exactly once (the "cache hits == 1"
//     invariant). State root is routed through apitest.HardenedTempDir
//     so SecureWriteClientConfig accepts the parent dir on Windows.
//  3. The cache-age surface (api.MarketplaceCacheAge) compiles and
//     returns without panicking after a successful fresh fetch.
//  4. The generated draft carries the stdio-bridge transport (NOT
//     native-http) — pins codex r1 P1 closure from a cross-package
//     perspective.

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
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/config"
)

func TestMarketplaceE2E_SearchShowGenerate(t *testing.T) {
	// Per-test DACL-hardened state dir so SecureWriteClientConfig
	// accepts the marketplace-cache.json write on Windows (parent
	// dir on %TEMP% inherits Authenticated Users otherwise; that
	// fails the production allowlist gate). On POSIX
	// apitest.HardenedTempDir is just `t.TempDir() + 0700 subdir`.
	root := apitest.HardenedTempDir(t)
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

	// generate's stdout carries the full draft YAML; capture it
	// separately so we can assert the stdio-bridge mapping after
	// the loop (codex r5 P2 closure).
	var generateStdout string

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
		if sub.args[0] == "generate" {
			generateStdout = stdout.String()
		}
	}

	// Pin the stdio-bridge mapping at the CLI layer (codex r5 P2
	// closure). The in-package API test pins the same fact via
	// config.TransportStdioBridge; here we re-pin from a real CLI
	// invocation so a regression that bypasses the API test is
	// still caught.
	wantTransport := "transport: " + config.TransportStdioBridge
	if !strings.Contains(generateStdout, wantTransport) {
		t.Errorf("generate stdout missing %q (stdio→stdio-bridge mapping regressed)\n---\n%s", wantTransport, generateStdout)
	}
	if strings.Contains(generateStdout, "transport: native-http") ||
		strings.Contains(generateStdout, "transport: "+config.TransportNativeHTTP) {
		t.Errorf("generate stdout contains native-http transport (codex r1 P1 regression)\n---\n%s", generateStdout)
	}
	if !strings.Contains(generateStdout, "command: npx") {
		t.Errorf("generate stdout missing command: npx\n---\n%s", generateStdout)
	}

	// First call was a fresh fetch; subsequent ones must hit the
	// cache. With HardenedTempDir the cache write succeeds, so the
	// SKIP guard no longer applies (codex deep-sec PR #163 lane 3
	// P1 closure: this used to silently skip on Windows because the
	// %TEMP%-inherited DACL was rejected). If the cache age is
	// still 0 after a successful fresh fetch, something is wrong
	// with the hardened temp dir or the cache writer.
	if api.MarketplaceCacheAge() == 0 {
		t.Fatalf("marketplace-cache.json not persisted under %s (hardened temp dir should accept the write; check apitest.HardenedTempDir DACL output)", root)
	}
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Errorf("registry hits = %d, want 1 (subsequent calls should use cache)", h)
	}
}
