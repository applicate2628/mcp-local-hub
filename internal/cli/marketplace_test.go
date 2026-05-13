// internal/cli/marketplace_test.go — G5 Phase 4 CLI test surface.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"CLI surface".
// Plan: docs/superpowers/plans/2026-05-13-g5-marketplace-draft-import.md §Phase 4.
//
// The three smokes pin the contract that ships in v0.3.0:
//   - search prints entry ids to stdout
//   - show prints metadata block including `Readme URL:` STRING (no README body fetch)
//   - generate refuses http entries with a stderr G6-deferral note + empty stdout
//
// Tests inject a TLS-trusting client via api.InstallMarketplaceTestClientForCLI
// (the hook is defined in internal/api/marketplace_testhook.go so cross-package
// test code can call it). No CLI-visible `--insecure-tls-for-tests` flag —
// rejected as a footgun (plan §Phase 4 prelude).

package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestMarketplaceSearch_HappyPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"filesystem","name":"Filesystem","summary":"sandboxed fs","transport":"stdio","command":"npx","categories":["fs"]}
		]}`))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallMarketplaceTestClientForCLI(srv))
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"search", "fs", "--registry", srv.URL})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "filesystem") {
		t.Errorf("search output missing entry id\n---\n%s", stdout.String())
	}
}

func TestMarketplaceShow_PrintsMetadataNotReadmeBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"filesystem","name":"Filesystem","transport":"stdio","command":"npx","readme_url":"https://example.com/README.md"}
		]}`))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallMarketplaceTestClientForCLI(srv))
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"show", "filesystem", "--registry", srv.URL})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("show: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"ID:", "Transport:", "Readme URL:", "https://example.com/README.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("show stdout missing %q\n---\n%s", want, out)
		}
	}
}

func TestMarketplaceGenerate_HttpEntrySkipsToStderr(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"ctx7","name":"Context7","transport":"http","url":"https://mcp.context7.com/mcp"}
		]}`))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallMarketplaceTestClientForCLI(srv))
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"generate", "ctx7", "--registry", srv.URL})
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected non-zero exit for http entry")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout must be empty on G6-deferral; got: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "G6") {
		t.Errorf("stderr missing G6 deferral note\n---\n%s", stderr.String())
	}
}
