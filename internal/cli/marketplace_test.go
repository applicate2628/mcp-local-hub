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

	"github.com/spf13/cobra"

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
	c.SetArgs([]string{"search", "fs", "--registry", api.MarketplaceTestRegistryURL("/search-happy.json")})
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
	c.SetArgs([]string{"show", "filesystem", "--registry", api.MarketplaceTestRegistryURL("/show-metadata.json")})
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

// TestSanitizeCatalogField_StripsControlAndEscape pins codex deep-sec
// PR #163 lane 3 P2 closure: ANSI/OSC escape sequences and other
// terminal-control bytes coming from an untrusted catalog must be
// neutralized before they reach stdout. Catalog strings are otherwise
// pass-through; printable UTF-8 (Cyrillic, em-dash, etc.) must
// survive sanitization unchanged.
func TestSanitizeCatalogField_StripsControlAndEscape(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want string
		name string
	}{
		{"hello", "hello", "plain ascii"},
		// CSI sequence: ESC + `[31mred...` → ESC becomes '?',
		// the bracket+digits pass through (they're printable ASCII).
		{"hello\x1b[31mred\x1b[0m", "hello?[31mred?[0m", "csi color"},
		// OSC hyperlink: ESC + `]8;...` + ESC + `\`. Each ESC
		// becomes '?'; backslash is printable ASCII, passes through.
		{"hello\x1b]8;;https://evil.example/\x1b\\link\x1b]8;;\x1b\\", "hello?]8;;https://evil.example/?\\link?]8;;?\\", "osc hyperlink"},
		{"line1\nline2", "line1 line2", "lf"},
		{"col1\tcol2", "col1 col2", "tab"},
		{"return\rOverwrite", "return Overwrite", "cr"},
		{"del\x7Foops", "del?oops", "del"},
		// Raw byte 0x9B (CSI control) is an invalid UTF-8 start
		// byte — it's a C1 continuation byte by encoding rules. The
		// utf8.DecodeRuneInString path catches it as RuneError/size=1
		// and replaces with '?'.
		{"high\x9Boops", "high?oops", "raw c1 byte"},
		{"spoof\u202Etxt", "spoof?txt", "bidi rlo"},
		{"alm\u061Cmark", "alm?mark", "arabic letter mark"},
		{"mark\u200Etext", "mark?text", "left-to-right mark"},
		{"isolate\u2066text\u2069", "isolate?text?", "bidi isolate"},
		{"line\u2028next", "line?next", "unicode line separator"},
		{"paragraph\u2029next", "paragraph?next", "unicode paragraph separator"},
		{"nel\u0085next", "nel?next", "unicode next-line"},
		{"emoji✓ check", "emoji✓ check", "utf8 preserved"},
		{"кириллица", "кириллица", "cyrillic preserved"},
		{"em—dash", "em—dash", "em-dash preserved"},
	} {
		got := sanitizeCatalogField(c.raw)
		if got != c.want {
			t.Errorf("%s: sanitizeCatalogField(%q) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
}

// TestMarketplaceSearch_SanitizesHostileCatalogFields verifies that
// search output strips control characters from the live catalog
// fields, not just the standalone helper. Asserts the ESC bytes
// never reach stdout regardless of which field they entered through.
func TestMarketplaceSearch_SanitizesHostileCatalogFields(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hostile registry: embed ESC sequences in name + summary.
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"hostile","name":"AAA\u001b[31mEVIL\u001b[0m","summary":"line1\nline2","transport":"stdio","command":"npx"}
		]}`))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallMarketplaceTestClientForCLI(srv))
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"search", "hostile", "--registry", api.MarketplaceTestRegistryURL("/hostile-fields.json")})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v\nstderr: %s", err, stderr.String())
	}
	if strings.ContainsRune(stdout.String(), 0x1B) {
		t.Errorf("stdout still contains ESC (0x1B) after sanitize:\n---\n%q", stdout.String())
	}
	// The newline in summary became a space; the table should now
	// have ALL its rows on a single line per entry. We don't pin
	// the exact byte count; we just pin the ESC strip.
	if !strings.Contains(stdout.String(), "AAA?[31mEVIL?[0m") {
		t.Errorf("name field not sanitized as expected:\n---\n%s", stdout.String())
	}
}

// TestMarketplaceShow_RendersEnvSection pins codex r6 P2 closure
// (PR #163): `show` must surface entry.Env so operators inspect
// required/suspicious vars before deciding to trust the entry.
// Keys must be deterministically ordered.
func TestMarketplaceShow_RendersEnvSection(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"1","entries":[
			{"id":"envful","name":"WithEnv","transport":"stdio","command":"npx",
			 "env":{"ZULU":"third","ALPHA":"first","MIKE":"second"}}
		]}`))
	}))
	defer srv.Close()
	t.Cleanup(api.InstallMarketplaceTestClientForCLI(srv))
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"show", "envful", "--registry", api.MarketplaceTestRegistryURL("/envful.json")})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("show: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Env:") {
		t.Errorf("show stdout missing Env: section\n---\n%s", out)
	}
	for _, want := range []string{"ALPHA=first", "MIKE=second", "ZULU=third"} {
		if !strings.Contains(out, want) {
			t.Errorf("show stdout missing %q\n---\n%s", want, out)
		}
	}
	a := strings.Index(out, "ALPHA")
	m := strings.Index(out, "MIKE")
	z := strings.Index(out, "ZULU")
	if !(a < m && m < z) {
		t.Errorf("env keys not sorted alphabetically: ALPHA@%d MIKE@%d ZULU@%d\n---\n%s", a, m, z, out)
	}
}

// TestMarketplaceCmd_RejectsEmbeddedCredentialURL pins the CLI-level
// mirror of the lib-level URL.User rejection (codex r6 P1 closure).
// CLI validation runs before the lib path so the operator gets a
// tidier early error and the URL is never logged downstream.
func TestMarketplaceCmd_RejectsEmbeddedCredentialURL(t *testing.T) {
	c := newMarketplaceCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"search", "anything", "--registry", "https://user:pass@example.com/catalog.json"})
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected rejection for URL with embedded credentials; got nil")
	}
	if !strings.Contains(err.Error(), "must not embed credentials") {
		t.Errorf("error missing 'must not embed credentials' text: %v", err)
	}
}

// TestMarketplaceGenerate_HttpEntryEmitsRemoteHTTPDraft pins G6
// sub-PR 4 closure: http catalog entries now project onto a
// transport=remote-http manifest written to stdout. No stderr noise
// (no G6 deferral). Operator pipes the draft into manifest create.
func TestMarketplaceGenerate_HttpEntryEmitsRemoteHTTPDraft(t *testing.T) {
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
	c.SetArgs([]string{"generate", "ctx7", "--registry", api.MarketplaceTestRegistryURL("/generate-http.json")})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("generate: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"transport: remote-http",
		"url: https://mcp.context7.com/mcp",
		"name: ctx7",
		"manifest test-remote",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "daemons:") {
		t.Errorf("stdout must not include daemons: for remote-http\n---\n%s", out)
	}
	// No stderr noise: G6 sub-PR 4 closes the deferral surface.
	if strings.Contains(stderr.String(), "G6") {
		t.Errorf("stderr should no longer mention G6 deferral; got: %s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// S4 docs-only POINTER rows — CLI discoverability (bot #446 R3).
//
// docs_only rows live in the catalog's SEPARATE top-level docs_only[] array (NOT
// entries[]), so the CLI marketplace commands must iterate it too or they are
// GUI-discoverable but CLI-invisible. docs_only is v2-gated, so these fixtures
// declare schema_version "2".
// ---------------------------------------------------------------------------

const docsOnlyCatalogFixture = `{"schema_version":"2","entries":[
  {"id":"filesystem","name":"Filesystem","summary":"sandboxed fs","transport":"stdio","command":"npx","categories":["fs"]}
],"docs_only":[
  {"id":"cubase","name":"Cubase MCP server (docs-only)","summary":"manual-install pointer for Cubase","homepage":"https://github.com/hedidjs/cubase-mcp","readme_url":"https://raw.githubusercontent.com/hedidjs/cubase-mcp/main/README.md","manual_install":"git clone the repo and configure a virtual-MIDI port in Cubase.","license":"MIT","categories":["music","daw"]}
]}`

func newDocsOnlyTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(docsOnlyCatalogFixture))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(api.InstallMarketplaceTestClientForCLI(srv))
	return newMarketplaceCmd()
}

// TestMarketplaceSearch_ListsDocsOnlyRowTagged proves `search` lists a docs_only
// row, marked `docs-only` in the TRANSPORT column (so the operator sees it is a
// manual-install pointer, not a one-click install). It must appear alongside the
// installable entry.
func TestMarketplaceSearch_ListsDocsOnlyRowTagged(t *testing.T) {
	c := newDocsOnlyTestCmd(t)
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"search", "--registry", api.MarketplaceTestRegistryURL("/docs-only-search.json")})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "filesystem") {
		t.Errorf("search output missing the installable entry\n---\n%s", out)
	}
	if !strings.Contains(out, "cubase") {
		t.Errorf("search output missing the docs_only row (CLI-invisible regression)\n---\n%s", out)
	}
	// The cubase row must carry the docs-only transport tag.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "cubase") && !strings.Contains(line, "docs-only") {
			t.Errorf("docs_only row not tagged `docs-only` in TRANSPORT column\nline: %q", line)
		}
	}
}

// TestMarketplaceSearch_DocsOnlyMatchesQuery proves the docs_only rows honor the
// search query (a query that matches only the docs_only row still surfaces it).
func TestMarketplaceSearch_DocsOnlyMatchesQuery(t *testing.T) {
	c := newDocsOnlyTestCmd(t)
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"search", "cubase", "--registry", api.MarketplaceTestRegistryURL("/docs-only-q.json")})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "cubase") {
		t.Errorf("query 'cubase' did not surface the docs_only row\n---\n%s", out)
	}
	// A non-matching installable entry must be filtered out.
	if strings.Contains(out, "filesystem") {
		t.Errorf("query 'cubase' should not list the non-matching installable entry\n---\n%s", out)
	}
}

// TestMarketplaceShow_DocsOnlyPrintsPointer proves `show <docs-only-id>` prints the
// pointer block (homepage / readme / manual_install steps / why docs-only) instead
// of "not found".
func TestMarketplaceShow_DocsOnlyPrintsPointer(t *testing.T) {
	c := newDocsOnlyTestCmd(t)
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"show", "cubase", "--registry", api.MarketplaceTestRegistryURL("/docs-only-show.json")})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("show docs-only: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"DOCS-ONLY pointer",
		"https://github.com/hedidjs/cubase-mcp",
		"https://raw.githubusercontent.com/hedidjs/cubase-mcp/main/README.md",
		"git clone the repo and configure a virtual-MIDI port in Cubase.",
		"Manual setup steps:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show docs-only pointer missing %q\n---\n%s", want, out)
		}
	}
	// It must NOT print the installable-entry metadata block keys.
	if strings.Contains(out, "Command:") || strings.Contains(out, "Transport:") {
		t.Errorf("docs-only show printed installable-entry metadata keys\n---\n%s", out)
	}
}

// TestMarketplaceGenerate_DocsOnlyRefuses proves `generate <docs-only-id>` REFUSES
// (a pointer is not installable — no manifest to draft), points the operator at
// `show <id>`, and leaves stdout empty (mirrors the GUI DOCS_ONLY_NOT_INSTALLABLE).
func TestMarketplaceGenerate_DocsOnlyRefuses(t *testing.T) {
	c := newDocsOnlyTestCmd(t)
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"generate", "cubase", "--registry", api.MarketplaceTestRegistryURL("/docs-only-gen.json")})
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("generate docs-only must refuse; got nil error\nstdout: %s", stdout.String())
	}
	if !strings.Contains(err.Error(), "docs-only pointer") || !strings.Contains(err.Error(), "show cubase") {
		t.Errorf("refusal error = %q, want it to name the docs-only pointer + `show cubase`", err.Error())
	}
	// stdout must stay empty (no draft).
	if strings.TrimSpace(stdout.String()) != "" {
		t.Errorf("generate docs-only must leave stdout empty; got:\n%s", stdout.String())
	}
}

// TestMarketplaceShow_UnknownIdStillErrors proves the docs_only fallback did not
// swallow the genuine not-found error: an id in NEITHER array still errors.
func TestMarketplaceShow_UnknownIdStillErrors(t *testing.T) {
	c := newDocsOnlyTestCmd(t)
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"show", "nope", "--registry", api.MarketplaceTestRegistryURL("/docs-only-404.json")})
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("show of an unknown id must error; got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want a not-found error", err.Error())
	}
}

// TestMarketplaceRefresh_ReportsDocsOnlyCount proves `refresh` reports the docs_only
// pointer count alongside the entry count.
func TestMarketplaceRefresh_ReportsDocsOnlyCount(t *testing.T) {
	c := newDocsOnlyTestCmd(t)
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"refresh", "--registry", api.MarketplaceTestRegistryURL("/docs-only-refresh.json")})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("refresh: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "docs-only pointer") {
		t.Errorf("refresh output missing docs-only pointer count\n---\n%s", out)
	}
}
