package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// --- Test harness -------------------------------------------------------
//
// newRegisterHarness installs fake scheduler + clients + registry path
// overrides so Register runs hermetically. Returns the registry path so
// tests can assert on-disk state; the returned cleanup function restores
// the package-scoped hooks (use defer).
type registerHarness struct {
	regPath     string
	fakeSch     *fakeScheduler
	fakeClients *fakeClientsMap
	// blessedRoots records every canonical root the register path tried to
	// bless as an LSP trusted root. The harness stubs the bless seam to a
	// no-op capture so register tests never write the real %LOCALAPPDATA%
	// trusted-roots store; a dedicated test asserts the explicit-register
	// bless fires.
	blessedRoots *[]string
	restore      func()
}

func newRegisterHarness(t *testing.T) *registerHarness {
	t.Helper()
	dir := t.TempDir()
	regPath := filepath.Join(dir, "workspaces.yaml")

	// Redirect the daemon state root to an owner-only temp dir so any
	// register path that writes supervisor-intent.json (via
	// register_supervisor.go's DaemonStateDir()) lands in temp instead of
	// the operator's REAL %LOCALAPPDATA%\mcp-local-hub\supervisor-intent.json.
	// Without this, harness tests calling Register/RegisterLSP would take a
	// flock on — and clobber — the live supervisor-intent.json, killing the
	// running fleet (test-infra leak: api-tests-flock-contention). Mirrors
	// the daemonStateRootOverride save/restore pattern in
	// hub_mcp_state_test.go and the hardened-root posture of isolateStateDir
	// (an owner-only root so the absent-intent strict verdict matches
	// production). t.Cleanup restores it even if a test forgets defer
	// h.restore() or panics.
	prevStateRoot := daemonStateRootOverride
	daemonStateRootOverride = hardenedTempDir(t)
	t.Cleanup(func() { daemonStateRootOverride = prevStateRoot })

	origSchedulerNew := testSchedulerFactory
	origClientFactory := testClientFactory
	origRegistryPath := testRegistryPathOverride
	origReadiness := proxyReadinessFn
	origCanonical := testCanonicalMcphubPathOverride
	origBless := registerBlessTrustedRootFn
	origForceKill := forceKillByPortFn

	// Stub the explicit-register bless seam: capture the canonical roots
	// (so a dedicated test can assert the bless fired) AND keep every
	// register test from touching the real %LOCALAPPDATA% trusted-roots
	// store. Production wires this to BlessDefaultTrustedRoot.
	blessed := &[]string{}
	registerBlessTrustedRootFn = func(canonicalWorkspaceRoot string) error {
		*blessed = append(*blessed, canonicalWorkspaceRoot)
		return nil
	}

	// Create a stub mcphub binary so canonicalMcphubPath()'s
	// os.Stat preflight succeeds. Production code calls it to verify
	// `mcphub setup` ran; tests don't actually exec the binary, so
	// content doesn't matter — only that the path exists. Without
	// this, every Register-touching test fails on a clean CI runner
	// (no $HOME/.local/bin/mcphub) with "run `mcphub setup` once".
	// Tests that want to exercise the missing-binary path override
	// the seam back to a non-existent path inside their own bodies.
	stubPath := filepath.Join(dir, mcphubShortName)
	if err := os.WriteFile(stubPath, []byte("stub-binary\n"), 0o755); err != nil {
		t.Fatalf("create stub mcphub binary: %v", err)
	}
	testCanonicalMcphubPathOverride = stubPath

	sch := &fakeScheduler{tasks: map[string]bool{}, xml: map[string][]byte{}}
	testSchedulerFactory = func() (testScheduler, error) { return sch, nil }
	// Fake scheduler's Run is a no-op — no real proxy ever binds, so
	// the production HTTP-based readiness probe would time out. Tests
	// opt into "readiness always succeeds"; specific tests that want
	// to exercise the readiness-failure path override this again.
	proxyReadinessFn = func(port int, timeout time.Duration) error { return nil }
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		return portKillNoListener, nil
	}

	fc := &fakeClientsMap{
		entries:         map[string]map[string]string{},
		stdioEntries:    map[string]map[string]clients.LanguageServerStdioEntry{},
		allStdioEntries: map[string]map[string]clients.StdioEntry{},
		backupKeepCalls: map[string]int{},
		exists:          map[string]bool{},
	}
	// Pre-populate the default HTTP clients so Exists() returns true in tests.
	for _, n := range []string{"claude-code", "codex-cli", "cursor"} {
		fc.entries[n] = map[string]string{}
		fc.stdioEntries[n] = map[string]clients.LanguageServerStdioEntry{}
		fc.allStdioEntries[n] = map[string]clients.StdioEntry{}
		fc.exists[n] = true
	}
	testClientFactory = func() map[string]registerClient {
		out := map[string]registerClient{}
		for n := range fc.entries {
			out[n] = &fakeClient{parent: fc, name: n}
		}
		return out
	}
	testRegistryPathOverride = regPath

	return &registerHarness{
		regPath:      regPath,
		fakeSch:      sch,
		fakeClients:  fc,
		blessedRoots: blessed,
		restore: func() {
			testSchedulerFactory = origSchedulerNew
			testClientFactory = origClientFactory
			testRegistryPathOverride = origRegistryPath
			proxyReadinessFn = origReadiness
			testCanonicalMcphubPathOverride = origCanonical
			registerBlessTrustedRootFn = origBless
			forceKillByPortFn = origForceKill
		},
	}
}

// nineLanguageManifest returns a manifest identical to the shipped
// mcp-language-server but with ClientBindings populated for the fake
// client map. LSP commands are intentionally non-existent binaries to
// assert the lazy-mode contract (no preflight at register time).
func nineLanguageManifest() *config.ServerManifest {
	langs := []config.LanguageSpec{
		{Name: "clangd", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "clangd"},
		{Name: "fortran", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "fortls"},
		{Name: "go", Backend: "gopls-mcp", Transport: "stdio", LspCommand: "gopls", ExtraFlags: []string{"mcp"}},
		{Name: "javascript", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "typescript-language-server", ExtraFlags: []string{"--stdio"}},
		{Name: "python", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "pyright-langserver", ExtraFlags: []string{"--stdio"}},
		{Name: "rust", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "rust-analyzer"},
		{Name: "typescript", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "typescript-language-server", ExtraFlags: []string{"--stdio"}},
		{Name: "vscode-css", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "vscode-css-language-server", ExtraFlags: []string{"--stdio"}},
		{Name: "vscode-html", Backend: "mcp-language-server", Transport: "stdio", LspCommand: "vscode-html-language-server", ExtraFlags: []string{"--stdio"}},
	}
	return &config.ServerManifest{
		Name:      "mcp-language-server",
		Kind:      config.KindWorkspaceScoped,
		Transport: "stdio-bridge",
		Command:   "mcp-language-server",
		PortPool:  &config.PortPool{Start: 9200, End: 9299},
		Languages: langs,
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", URLPath: "/mcp"},
			{Client: "codex-cli", URLPath: "/mcp"},
			{Client: "cursor", URLPath: "/mcp"},
		},
	}
}

// mustNewAPI wraps NewAPI so tests stay terse.
func mustNewAPI(t *testing.T) *API {
	t.Helper()
	return NewAPI()
}

// --- Register tests -----------------------------------------------------

func TestRegister_DefaultAllLanguages(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	rpt, err := mustNewAPI(t).registerWithManifest(m, ws, nil, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(rpt.Entries) != 9 {
		t.Fatalf("report entries = %d, want 9", len(rpt.Entries))
	}
	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatal(err)
	}
	if len(reg.Workspaces) != 9 {
		t.Fatalf("registry entries = %d, want 9", len(reg.Workspaces))
	}
	// Every entry should be LifecycleConfigured.
	for _, e := range reg.Workspaces {
		if e.Lifecycle != LifecycleConfigured {
			t.Errorf("entry %s: lifecycle = %q, want %q", e.Language, e.Lifecycle, LifecycleConfigured)
		}
	}
	// Scheduler task args must include the lazy-proxy invariant:
	// `daemon workspace-proxy --port <p> --workspace <ws> --language <lang>`.
	// Register also creates the shared weekly-refresh task (idempotent);
	// filter it out before counting per-language tasks.
	var langSpecs []scheduler.TaskSpec
	for _, s := range h.fakeSch.createdSpecs {
		if s.Name != WeeklyRefreshTaskName {
			langSpecs = append(langSpecs, s)
		}
	}
	if len(langSpecs) != 9 {
		t.Fatalf("per-language scheduler Create called %d times, want 9", len(langSpecs))
	}
	sawWorkspaceProxy := false
	for _, s := range langSpecs {
		if len(s.Args) >= 2 && s.Args[0] == "daemon" && s.Args[1] == "workspace-proxy" {
			sawWorkspaceProxy = true
			// Confirm every flag uses double-dash form (pflag requirement).
			for _, a := range s.Args {
				if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
					t.Errorf("scheduler task arg %q uses single-dash form; pflag requires --", a)
				}
			}
		}
	}
	if !sawWorkspaceProxy {
		t.Error("no scheduler task used the `daemon workspace-proxy` subcommand")
	}
}

// TestRegister_TaskWorkingDirIsWorkspace is the regression test for
// v0.3.0-blockers bug #1 (D3 backend lookup): the scheduler task's
// WorkingDir must be the canonical workspace path, NOT the install
// directory (filepath.Dir(canonicalExe)).
//
// Prior behavior: WorkingDir was set to the install dir (~/.local/bin/),
// which (a) breaks LSP backends that expect cwd == project root for
// compile_commands.json / Cargo.toml / go.mod discovery, and (b) on
// Windows triggers Go 1.19 CVE-2022-30580 ErrDot when the install dir
// happens to contain a stale copy of the wrapper binary
// (`mcp-language-server.exe`) — exec.LookPath finds the cwd-relative
// match first and refuses to return it, surfacing as "missing binary"
// even when the wrapper IS available on PATH.
//
// The fix anchors cwd to the workspace path. Workspaces contain source
// files only, never the wrapper binary, so LookPath falls through to
// PATH cleanly.
func TestRegister_TaskWorkingDirIsWorkspace(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	var langSpec *scheduler.TaskSpec
	for i := range h.fakeSch.createdSpecs {
		s := &h.fakeSch.createdSpecs[i]
		if s.Name != WeeklyRefreshTaskName {
			langSpec = s
			break
		}
	}
	if langSpec == nil {
		t.Fatal("no per-language scheduler task spec captured")
	}
	// Compare against the canonical form because CanonicalWorkspacePath
	// (Windows) normalizes path case (e.g. r:\TEMP\... vs r:\Temp\...).
	wsCanonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("canonicalize ws: %v", err)
	}
	if langSpec.WorkingDir != wsCanonical {
		t.Errorf("scheduler task WorkingDir = %q, want %q (canonical workspace path) — "+
			"setting it to the install dir would break LSP cwd-relative discovery "+
			"AND trigger Go's exec.LookPath ErrDot on Windows when a stale wrapper "+
			"binary lives in the install dir",
			langSpec.WorkingDir, wsCanonical)
	}
}

func TestRegister_PartialLanguages(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python", "typescript"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 2 {
		t.Fatalf("registry entries = %d, want 2", len(reg.Workspaces))
	}
	got := map[string]bool{}
	for _, e := range reg.Workspaces {
		got[e.Language] = true
	}
	if !got["python"] || !got["typescript"] {
		t.Errorf("missing languages: got %+v", got)
	}
}

func TestRegister_UnknownLanguageFailsFast(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python", "not-a-language"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected error for unknown language")
	}
	if !strings.Contains(err.Error(), "not-a-language") {
		t.Errorf("error should name the unknown language: %v", err)
	}
	// No registry file should exist (no side effects).
	if _, statErr := os.Stat(h.regPath); !os.IsNotExist(statErr) {
		t.Errorf("registry created despite unknown-language failure: %v", statErr)
	}
	// No scheduler tasks either.
	if len(h.fakeSch.createdSpecs) != 0 {
		t.Errorf("scheduler Create called %d times; want 0", len(h.fakeSch.createdSpecs))
	}
}

func TestRegister_NoLspBinaryPreflightAtRegister(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	// Manifest uses LSP commands guaranteed NOT to be on PATH.
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("lazy-mode Register must succeed without LSP preflight; got %v", err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 1 {
		t.Fatalf("registry entries = %d, want 1", len(reg.Workspaces))
	}
	if reg.Workspaces[0].Lifecycle != LifecycleConfigured {
		t.Errorf("lifecycle = %q, want %q (proxy is configured; missing-binary surfaces at tools/call)",
			reg.Workspaces[0].Lifecycle, LifecycleConfigured)
	}
}

func TestRegister_RemovesMatchingDirectLanguageServerEntries(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)
	other := t.TempDir()
	otherCanonical := mustCanonical(t, other)

	h.fakeClients.stdioEntries["codex-cli"] = map[string]clients.LanguageServerStdioEntry{
		"legacy-go": {
			Name:     "legacy-go",
			Command:  "mcp-language-server",
			Language: "gopls",
			Args:     []string{"--lsp", "gopls", "--workspace", canonical},
		},
		"legacy-python": {
			Name:     "legacy-python",
			Command:  "mcp-language-server",
			Language: "pyright-langserver",
			Args:     []string{"--lsp", "pyright-langserver", "--workspace", canonical},
		},
		"python-experimental": {
			Name:     "python-experimental",
			Command:  "mcp-language-server",
			Language: "python-experimental",
			Args:     []string{"--lsp", "python-experimental", "--workspace", canonical},
		},
		"other-workspace-go": {
			Name:     "other-workspace-go",
			Command:  "mcp-language-server",
			Language: "gopls",
			Args:     []string{"--lsp", "gopls", "--workspace", otherCanonical},
		},
		"ambiguous-go": {
			Name:     "ambiguous-go",
			Command:  "mcp-language-server",
			Language: "gopls",
			Args:     []string{"--lsp", "gopls"},
		},
	}
	h.fakeClients.allStdioEntries["codex-cli"] = map[string]clients.StdioEntry{
		"go": {
			Name:    "go",
			Command: "gopls",
			Args:    []string{"mcp", "--workspace", canonical},
		},
		"other-gopls": {
			Name:    "other-gopls",
			Command: "gopls",
			Args:    []string{"mcp", "--workspace", otherCanonical},
		},
		"custom-gopls": {
			Name:    "custom-gopls",
			Command: "gopls",
			Args:    []string{"serve"},
		},
	}

	_, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, ok := h.fakeClients.stdioEntries["codex-cli"]["legacy-go"]; ok {
		t.Fatal("legacy --lsp gopls entry was not removed")
	}
	if _, ok := h.fakeClients.allStdioEntries["codex-cli"]["go"]; ok {
		t.Fatal("direct gopls mcp entry was not removed")
	}
	if _, ok := h.fakeClients.stdioEntries["codex-cli"]["other-workspace-go"]; !ok {
		t.Fatal("direct --lsp entry for another workspace was removed")
	}
	if _, ok := h.fakeClients.allStdioEntries["codex-cli"]["other-gopls"]; !ok {
		t.Fatal("direct gopls mcp entry for another workspace was removed")
	}
	if _, ok := h.fakeClients.stdioEntries["codex-cli"]["ambiguous-go"]; !ok {
		t.Fatal("direct --lsp entry without --workspace was removed")
	}
	if _, ok := h.fakeClients.allStdioEntries["codex-cli"]["custom-gopls"]; !ok {
		t.Fatal("unrelated direct gopls non-mcp entry was removed")
	}
	if _, ok := h.fakeClients.stdioEntries["codex-cli"]["legacy-python"]; !ok {
		t.Fatal("unrelated legacy python entry was removed")
	}
	if _, ok := h.fakeClients.stdioEntries["codex-cli"]["python-experimental"]; !ok {
		t.Fatal("same-workspace substring language entry was removed")
	}
	if h.fakeClients.backupKeepCalls["codex-cli"] != 1 {
		t.Errorf("BackupKeep calls for codex-cli = %d, want 1", h.fakeClients.backupKeepCalls["codex-cli"])
	}
}

func TestRegister_RollbackOnSchedulerFailure(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Fail on the 2nd scheduler.Create call — after language 1 succeeded.
	h.fakeSch.failCreateAfterN = 1
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python", "typescript", "rust"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected partial-register failure")
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("rollback failed — registry still has %d entries: %+v", len(reg.Workspaces), reg.Workspaces)
	}
	// Client entries also rolled back.
	if n := countEntries(h.fakeClients); n != 0 {
		t.Errorf("client entries not rolled back: %d remain", n)
	}
}

func TestRegister_RollbackOnClientFailure(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Fail on the FIRST client AddEntry call (any client) for language 2.
	h.fakeClients.failAddEntryCalls = 4 // 3 clients for lang 1 succeed, then fail on first call of lang 2
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python", "typescript"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected client-failure register to error")
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("registry not rolled back: %+v", reg.Workspaces)
	}
	if n := countEntries(h.fakeClients); n != 0 {
		t.Errorf("client entries not rolled back: %d remain", n)
	}
}

// TestRegister_RollbackRestoresPriorRegistryEntryOnReRegister guards
// the narrow "re-register rollback removes prior row" regression: when
// `had == true`, the registry rollback must RESTORE the prior entry,
// not delete it. Deleting turns a recoverable re-register failure into
// a persistent outage — the scheduler task gets restored from priorXML
// + restarted, but workspace-proxy exits immediately because its
// (workspaceKey, language) registry row is gone.
func TestRegister_RollbackRestoresPriorRegistryEntryOnReRegister(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	killByPortFn = func(port int, _ time.Duration) error { return nil }
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Capture the entry as it was after first register.
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	before, ok := reg.Get(WorkspaceKey(mustCanonical(t, ws)), "python")
	if !ok {
		t.Fatal("first register did not persist python entry")
	}
	// Force a client-write failure on the second register. Rollback must
	// restore the prior entry, not delete it.
	h.fakeClients.failAddEntryCalls = h.fakeClients.addEntryCount + 1
	_, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected re-register with forced client failure to error")
	}
	// After rollback, the prior entry must still be present on disk.
	reg2 := NewRegistry(h.regPath)
	_ = reg2.Load()
	after, ok := reg2.Get(before.WorkspaceKey, "python")
	if !ok {
		t.Fatal("rollback deleted prior entry instead of restoring it (workspace-proxy would exit with 'not registered')")
	}
	if after.Port != before.Port {
		t.Errorf("rollback did not preserve prior port: before=%d after=%d", before.Port, after.Port)
	}
}

// TestRegister_ReadinessFailureRollsBack guards the post-Run
// readiness probe. Windows schtasks /Run only triggers the task
// action; it never validates the action actually succeeded. Without
// the probe, a bad-XML / bound-port / startup-crash scenario would
// produce a successful-looking register whose client configs point
// at a dead port. The probe returns an error here, so register MUST
// unwind all side effects (registry entry, scheduler task, client
// entries) via the rollback stack.
func TestRegister_ReadinessFailureRollsBack(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	killByPortFn = func(port int, _ time.Duration) error { return nil }
	// Override readiness to ALWAYS fail. Harness defaults to always-OK.
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		return fmt.Errorf("induced readiness failure on :%d", port)
	}
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected register to fail when readiness probe errors")
	}
	if !strings.Contains(err.Error(), "proxy readiness") {
		t.Errorf("error should mention readiness failure: %v", err)
	}
	// Registry must be empty (no leak).
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("registry not rolled back after readiness fail: %+v", reg.Workspaces)
	}
	// No per-language client entries should remain.
	if n := countEntries(h.fakeClients); n != 0 {
		t.Errorf("client entries leaked: %d remain", n)
	}
}

// TestRegister_ReleasesFlockBeforeSchRun is the regression guard for the
// deadlock discovered during real-LSP smoke on 2026-04-22:
//
//   - Register acquired the registry flock at its top.
//   - Inside the same flock, it called sch.Run to start the scheduler-
//     backed workspace proxy, then polled readiness on the bound port for
//     up to 10s.
//   - The spawned workspace-proxy subprocess (see daemon_workspace.go)
//     tries to acquire the SAME flock on startup — it blocked waiting for
//     Register's flock for the entire readiness window.
//   - Register's readiness probe timed out (port never bound because the
//     proxy was stuck in Lock), rollback removed the registry entry, and
//     then finally released the flock. The proxy woke up, saw an empty
//     registry, and exited "not registered".
//
// Net result: every `mcphub register` against a real scheduler failed
// with a ~10s timeout. The E2E test missed this because it stubs
// sch.Run as a no-op — no second process ever competes for the flock.
//
// This test hooks into fakeScheduler.Run to TRY to acquire the flock from
// a goroutine with a bounded timeout. If Register is still holding the
// flock at sch.Run time, the goroutine blocks; the hook surfaces a clear
// error which propagates out as a Register failure.
func TestRegister_ReleasesFlockBeforeSchRun(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	h.fakeSch.runHook = func(name string) {
		reg := NewRegistry(h.regPath)
		done := make(chan error, 1)
		go func() {
			unlock, err := reg.Lock()
			if err != nil {
				done <- err
				return
			}
			unlock()
			done <- nil
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("sch.Run hook: failed to acquire registry flock: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("sch.Run hook: timed out acquiring registry flock — Register is still holding it during sch.Run (regression)")
		}
	}

	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
}

// TestRegister_SharedWeeklyTaskNotCreatedWhenCanonicalMissing guards
// against the leak where EnsureWeeklyRefreshTask used to run BEFORE
// the canonical-mcphub preflight. A user who ran register without
// `mcphub setup` would fail registration but leave the shared
// weekly-refresh task pointing at the missing binary.
func TestRegister_SharedWeeklyTaskNotCreatedWhenCanonicalMissing(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	orig := testCanonicalMcphubPathOverride
	defer func() { testCanonicalMcphubPathOverride = orig }()
	testCanonicalMcphubPathOverride = filepath.Join(t.TempDir(), "no-such-mcphub.exe")

	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected register to fail when canonical mcphub is missing")
	}
	// The shared weekly-refresh task must NOT have been created (preflight
	// fails first → no scheduler side effects at all).
	for _, s := range h.fakeSch.createdSpecs {
		if s.Name == WeeklyRefreshTaskName {
			t.Errorf("shared weekly-refresh task leaked despite missing mcphub: %+v", s)
		}
	}
}

// TestRegister_AbortsWhenCanonicalMcphubMissing guards against the
// silent-broken-registration hazard where the scheduler task's Command
// points at a non-existent mcphub binary. A fresh user who ran
// `mcphub register` before `mcphub setup` (or on a machine where the
// canonical install was never copied) would see "register succeeded"
// but the proxy never comes up — Windows schtasks /run does not
// validate the action actually executed.
func TestRegister_AbortsWhenCanonicalMcphubMissing(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Point canonicalMcphubPath at a guaranteed-nonexistent location.
	orig := testCanonicalMcphubPathOverride
	defer func() { testCanonicalMcphubPathOverride = orig }()
	testCanonicalMcphubPathOverride = filepath.Join(t.TempDir(), "no-such-mcphub.exe")

	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected register to fail when canonical mcphub is missing")
	}
	if !strings.Contains(err.Error(), "mcphub setup") {
		t.Errorf("error should mention `mcphub setup` remediation: %v", err)
	}
	// No scheduler tasks at all — not per-language, not the shared weekly
	// refresh. Prior to the preflight-reorder fix, EnsureWeeklyRefreshTask
	// ran BEFORE the canonical-mcphub check, so the shared task leaked
	// (pointing at a missing binary) even though register errored out.
	// The fail-fast contract requires ZERO scheduler side effects when
	// setup hasn't been run.
	if len(h.fakeSch.createdSpecs) != 0 {
		var names []string
		for _, s := range h.fakeSch.createdSpecs {
			names = append(names, s.Name)
		}
		t.Errorf("any scheduler task created despite missing mcphub: %v", names)
	}
}

// TestRegister_ReRegisterKillsOldProxyBeforeReplace guards the Windows
// scheduler-vs-process gap: Task Scheduler's Delete removes the task
// definition but does NOT terminate the running child. On re-register
// (priorXML present), the old proxy keeps the allocated port bound;
// sch.Run for the replacement task would then fail to bind. Register
// must kill-by-port BEFORE the Delete+Create+Run sequence.
func TestRegister_ReRegisterKillsOldProxyBeforeReplace(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	var killedPorts []int
	killByPortFn = func(port int, _ time.Duration) error {
		killedPorts = append(killedPorts, port)
		return nil
	}
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	// First register → priorXML will exist for the second call.
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	killsBefore := len(killedPorts)
	// Second register must kill-by-port BEFORE replacing the task.
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if got := len(killedPorts) - killsBefore; got < 1 {
		t.Errorf("re-register did not call killByPortFn (delta=%d); old proxy would have kept port bound", got)
	}
}

// TestRegister_ReRegisterPreservesPriorWeeklyRefresh guards the narrow
// case where a user previously registered with --no-weekly-refresh
// (WeeklyRefresh=false) and later ran a plain `mcphub register` (which
// defaults opts.WeeklyRefresh to true). Before the fix, the silent
// default would re-enable weekly refresh despite the user's original
// choice. Re-register must preserve the prior WeeklyRefresh value on
// the idempotent path.
func TestRegister_ReRegisterPreservesPriorWeeklyRefresh(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	killByPortFn = func(port int, _ time.Duration) error { return nil }
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	// First register with WeeklyRefresh=false (mimics --no-weekly-refresh).
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{
		Writer:        &bytes.Buffer{},
		WeeklyRefresh: false,
	}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Plain re-register with default opts.WeeklyRefresh=true — must
	// NOT silently re-enable the user's disabled setting.
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{
		Writer:        &bytes.Buffer{},
		WeeklyRefresh: true, // CLI default
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	e, ok := reg.Get(WorkspaceKey(mustCanonical(t, ws)), "python")
	if !ok {
		t.Fatal("entry missing after re-register")
	}
	if e.WeeklyRefresh {
		t.Errorf("re-register silently flipped WeeklyRefresh back to true (regression); want prior value (false)")
	}
}

// TestRegister_AbortsBeforeDeleteOnExportXMLError guards the narrow
// case where ExportXML fails for a reason OTHER than ErrTaskNotFound
// (permission denied, scheduler service unavailable, XML corruption).
// Ignoring the error and proceeding would Delete the existing task
// without a priorXML snapshot, so rollback could not restore it on a
// later failure — a transient export error would turn into a
// persistent outage. Register must abort BEFORE any destructive side
// effect when the export's error is not the benign "not found".
func TestRegister_AbortsBeforeDeleteOnExportXMLError(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Pre-seed an existing task + XML so the "not found" branch is not
	// how we get here — the export must fail on the NEXT register.
	h.fakeSch.xml["mcp-local-hub-lsp-placeholder"] = []byte("<Task/>")
	h.fakeSch.failExportXMLErr = fmt.Errorf("RPC_E_DISCONNECTED: scheduler service unreachable")
	deleteCountBefore := 0
	for range h.fakeSch.tasks {
		deleteCountBefore++
	}
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected register to abort on non-not-found ExportXML error")
	}
	if !strings.Contains(err.Error(), "export") {
		t.Errorf("error should mention export failure: %v", err)
	}
	// No scheduler Delete should have fired for the language task on the
	// aborted register (we allow Creates for the shared weekly-refresh
	// task only, which happens before this code path).
	if h.fakeSch.createCount > 1 { // weekly-refresh task is 1 Create
		t.Errorf("expected no per-language Create on abort, got %d total Creates", h.fakeSch.createCount)
	}
}

// TestRegister_RegistryPersistedBeforeRun guards the race between
// sch.Run and reg.Save: the daemon process spawned by Run loads
// workspaces.yaml immediately and exits if (workspaceKey, language)
// is absent. Registry must be on disk BEFORE sch.Run fires, otherwise
// the first launch races against persistence and the proxy may fail
// startup until scheduler retry / next logon.
func TestRegister_RegistryPersistedBeforeRun(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	killByPortFn = func(port int, _ time.Duration) error { return nil }
	// Instrument fakeScheduler.Run to snapshot the registry file contents
	// AT THE MOMENT Run is called. If persist-before-Run holds, the file
	// must contain this language's entry by then.
	ws := t.TempDir()
	m := nineLanguageManifest()
	var snapshot []WorkspaceEntry
	h.fakeSch.runHook = func(name string) {
		reg := NewRegistry(h.regPath)
		_ = reg.Load()
		snapshot = append([]WorkspaceEntry(nil), reg.Workspaces...)
	}
	defer func() { h.fakeSch.runHook = nil }()
	if _, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// At the moment sch.Run fired, the registry must already contain the
	// python entry on disk.
	var sawPython bool
	for _, e := range snapshot {
		if e.Language == "python" {
			sawPython = true
			break
		}
	}
	if !sawPython {
		t.Errorf("registry did not contain python entry when sch.Run fired (snapshot=%+v)", snapshot)
	}
}

// TestRegister_PriorTaskRestoredIfCreateFails guards the narrow
// "Create fails on re-register" window: the rollback closure must
// already be registered BEFORE sch.Delete runs, so a subsequent
// sch.Create failure triggers restoration of the prior task XML
// (and sch.Run to restart the prior proxy). Before this fix, the
// rollback closure was appended AFTER Create — meaning a Create
// failure returned early with the prior task already Delete'd and
// NO rollback entry to restore it.
func TestRegister_PriorTaskRestoredIfCreateFails(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	killByPortFn = func(port int, _ time.Duration) error { return nil }
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	// First register succeeds → prior task + XML exists.
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Force the NEXT Create call (second register's replace step) to fail.
	h.fakeSch.failCreateAfterN = h.fakeSch.createCount
	runsBefore := h.fakeSch.runCount
	_, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected Create failure on re-register to error")
	}
	// Rollback must have restored + restarted the prior task even though
	// Create failed BEFORE any Run on the new task.
	if got := h.fakeSch.runCount - runsBefore; got < 1 {
		t.Errorf("rollback did not restart prior proxy after Create failure (runs delta=%d, want >=1)", got)
	}
}

// TestRegister_RollbackRestartsPriorProxyOnReRegister guards the
// "re-register rollback leaves workspace offline" regression: when a
// language was already registered (priorXML captured) and a later step
// in the same Register call fails, the rollback must restore the prior
// scheduler task AND restart it — otherwise the task definition is
// back but no proxy process runs, breaking the workspace until next
// logon. Without the sch.Run after ImportXML, a recoverable re-register
// error turns into a hard outage.
func TestRegister_RollbackRestartsPriorProxyOnReRegister(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	killByPortFn = func(port int, _ time.Duration) error { return nil }
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	// First register succeeds — establishes the "prior" state.
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	runsBefore := h.fakeSch.runCount
	// Second register with same language must hit priorXML path; force
	// later-step failure via client fake so the rollback closure runs.
	// Target the NEXT AddEntry call (counter is cumulative across registers).
	h.fakeClients.failAddEntryCalls = h.fakeClients.addEntryCount + 1
	_, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected re-register with forced client failure to error")
	}
	// The rollback must have restored + restarted the prior task:
	// exactly one additional Run call beyond the initial register's Run.
	if got := h.fakeSch.runCount - runsBefore; got < 1 {
		t.Errorf("rollback did not restart prior proxy (sch.Run deltas=%d, want >=1)", got)
	}
}

// TestRegister_RollbackKillsProxyForStartedLanguage guards the Windows
// orphan-proxy leak: Register's rollback used to only call sch.Delete,
// which on Windows removes the task definition but leaves the already-
// started child process (launched by sch.Run) running and bound to the
// allocated port. A later re-register would find the port occupied.
// Rollback now kills the running proxy by port before deleting the task.
func TestRegister_RollbackKillsProxyForStartedLanguage(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Capture every port killByPortFn is asked to terminate.
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	var killed []int
	killByPortFn = func(port int, _ time.Duration) error {
		killed = append(killed, port)
		return nil
	}
	// Force a client-write failure AFTER scheduler Create + Run succeed on
	// language 1, so rollback path runs with a live (from the test's
	// perspective: "should-be-running") proxy.
	h.fakeClients.failAddEntryCalls = 1
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected forced client-failure to error")
	}
	if len(killed) == 0 {
		t.Fatal("rollback did not invoke killByPortFn — Windows would leak the started proxy process")
	}
	for _, p := range killed {
		if p < 9200 || p > 9299 {
			t.Errorf("killed port %d outside workspace pool 9200-9299", p)
		}
	}
}

// TestRegister_StartsProxyImmediately verifies the post-Create sch.Run call
// that prevents logon-triggered tasks from sitting dead until the user's
// next logon. The original Register created the scheduler task but never
// started it, so the advertised port was unbound until reboot. This test
// guards that regression.
func TestRegister_StartsProxyImmediately(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	wantLangs := []string{"python", "typescript", "rust"}
	_, err := mustNewAPI(t).registerWithManifest(m, ws, wantLangs, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Exactly one Run per registered language (weekly-refresh task is
	// Created but not Run — that fires on the weekly trigger).
	gotRuns := 0
	for _, n := range h.fakeSch.runNames {
		for _, lang := range wantLangs {
			if strings.HasSuffix(n, "-"+lang) {
				gotRuns++
				break
			}
		}
	}
	if gotRuns != len(wantLangs) {
		t.Errorf("per-language Run calls = %d, want %d; runNames=%v",
			gotRuns, len(wantLangs), h.fakeSch.runNames)
	}
}

// TestRegister_RollsBackIfRunFails verifies that when sch.Run fails for
// language N, earlier languages in the same Register batch are rolled back
// (registry rows removed, client entries reverted). Covers the new failure
// mode introduced by the Run-after-Create wiring.
func TestRegister_RollsBackIfRunFails(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	var killed []int
	killByPortFn = func(port int, _ time.Duration) error {
		killed = append(killed, port)
		return nil
	}
	ws := t.TempDir()
	m := nineLanguageManifest()
	// Compute the expected task name for the second language (typescript)
	// and induce Run failure on that task. Language 1 (python) should
	// succeed; language 2 fails on Run, triggering rollback of both.
	wsKey := WorkspaceKey(mustCanonical(t, ws))
	h.fakeSch.failRunForTask = fmt.Sprintf("mcp-local-hub-lsp-%s-%s", wsKey, "typescript")
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python", "typescript"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected Register to fail on induced Run error")
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("registry not rolled back after Run failure: %+v", reg.Workspaces)
	}
	if n := countEntries(h.fakeClients); n != 0 {
		t.Errorf("client entries not rolled back after Run failure: %d remain", n)
	}
	if len(killed) != 1 {
		t.Fatalf("killByPortFn calls = %d, want 1 (only started language should be killed); killed=%v", len(killed), killed)
	}
}

// mustCanonical mirrors the register path's workspace canonicalization so
// per-test wsKey values stay consistent with production behavior.
func mustCanonical(t *testing.T, ws string) string {
	t.Helper()
	c, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegister_RollbackOnPortExhaustion(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Shrink the port pool so only 1 fits; request 2 languages.
	m := nineLanguageManifest()
	m.PortPool = &config.PortPool{Start: 9200, End: 9200}
	ws := t.TempDir()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python", "typescript"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected port-exhaustion error")
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("registry not rolled back after port exhaustion: %+v", reg.Workspaces)
	}
}

func TestRegister_ReRegisterIsIdempotent(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	api1 := mustNewAPI(t)
	if _, err := api1.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	firstPort := reg.Workspaces[0].Port
	firstEntries := map[string]string{}
	for k, v := range reg.Workspaces[0].ClientEntries {
		firstEntries[k] = v
	}
	// Re-register the same (ws, python).
	if _, err := api1.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("second register: %v", err)
	}
	reg2 := NewRegistry(h.regPath)
	_ = reg2.Load()
	if len(reg2.Workspaces) != 1 {
		t.Fatalf("re-register created %d entries; want 1 (idempotent)", len(reg2.Workspaces))
	}
	if reg2.Workspaces[0].Port != firstPort {
		t.Errorf("port changed on re-register: %d -> %d", firstPort, reg2.Workspaces[0].Port)
	}
	for k, v := range firstEntries {
		if reg2.Workspaces[0].ClientEntries[k] != v {
			t.Errorf("entry name changed for %s: %q -> %q", k, v, reg2.Workspaces[0].ClientEntries[k])
		}
	}
}

func TestRegister_NoWeeklyRefreshOpt(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{WeeklyRefreshExplicit: true, WeeklyRefresh: false, Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if reg.Workspaces[0].WeeklyRefresh {
		t.Error("expected WeeklyRefresh=false in registry entry")
	}
}

// TestRegister_EnsuresSharedWeeklyRefreshTask verifies Register calls
// EnsureWeeklyRefreshTask so the single shared scheduler task gets created
// without requiring a separate CLI invocation. The test uses the fake
// scheduler to assert Create(mcp-local-hub-workspace-weekly-refresh, ...)
// was invoked at least once during a Register call. Register must succeed
// (the shared task's creation is a side-effect; failures there warn but do
// not abort).
func TestRegister_EnsuresSharedWeeklyRefreshTask(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	if _, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The fake scheduler records every Create call; one of them must be
	// the shared weekly-refresh task.
	sawShared := false
	for _, s := range h.fakeSch.createdSpecs {
		if s.Name == WeeklyRefreshTaskName {
			sawShared = true
			if len(s.Args) == 0 || s.Args[0] != "workspace-weekly-refresh" {
				t.Errorf("shared task args = %v; want [workspace-weekly-refresh]", s.Args)
			}
			break
		}
	}
	if !sawShared {
		t.Errorf("Register did not create %s; saw %d specs", WeeklyRefreshTaskName, len(h.fakeSch.createdSpecs))
	}
}

// TestRegister_SurvivesSharedWeeklyRefreshFailure confirms the Register
// path does not abort when EnsureWeeklyRefreshTask fails — the shared task
// is a best-effort side effect; per-workspace registration must proceed
// even if the shared scheduler write rejects.
func TestRegister_SurvivesSharedWeeklyRefreshFailure(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Induce a failure on the VERY FIRST Create call — the shared
	// weekly-refresh task is created before the per-language loop starts,
	// so the first Create in Register is the shared one.
	h.fakeSch.failCreateForName = WeeklyRefreshTaskName
	ws := t.TempDir()
	m := nineLanguageManifest()
	buf := &bytes.Buffer{}
	rpt, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: buf})
	if err != nil {
		t.Fatalf("Register should survive shared-task failure; got %v", err)
	}
	if len(rpt.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(rpt.Entries))
	}
	if !strings.Contains(buf.String(), "warning: ensure shared weekly-refresh task") {
		t.Errorf("expected warning in writer output; got:\n%s", buf.String())
	}
}

func TestRegister_SupervisedWritesIntentAndDeletesLegacyLSPTask(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	reconcileCalled := false
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalled = true
		if !apply {
			t.Fatalf("supervised register called reconcile with apply=false")
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, "go")
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9200,
		TaskName:      taskName,
		Lifecycle:     LifecycleActive,
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeSch.tasks[taskName] = true
	h.fakeSch.xml[taskName] = []byte(`<Task name="` + taskName + `"/>`)

	report, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err != nil {
		t.Fatalf("Register supervised: %v", err)
	}
	if !reconcileCalled {
		t.Fatal("supervised register did not call supervisor reconcile")
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	if report.Entries[0].TaskName != taskName {
		t.Fatalf("entry task = %q, want %q", report.Entries[0].TaskName, taskName)
	}
	for _, spec := range h.fakeSch.createdSpecs {
		if spec.Name == taskName {
			t.Fatalf("supervised register must not create legacy LSP scheduler task; created %+v", spec)
		}
	}
	if !slices.Contains(h.fakeSch.deleteNames, taskName) {
		t.Fatalf("supervised register did not delete legacy task %s; deleteNames=%v", taskName, h.fakeSch.deleteNames)
	}
	if slices.Contains(h.fakeSch.runNames, taskName) {
		t.Fatalf("supervised register must not run legacy scheduler task %s; runNames=%v", taskName, h.fakeSch.runNames)
	}

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go"))
	if row == nil {
		t.Fatalf("supervisor-intent missing LSP row for %s; rows=%+v", taskName, intent.Daemons)
	}
	if row.Server != "mcp-language-server" || row.Workspace != canonical || row.Port != report.Entries[0].Port {
		t.Fatalf("intent row mismatch: %+v, entry=%+v", row, report.Entries[0])
	}
	wantArgs := []string{"daemon", "workspace-proxy", "--port", fmt.Sprintf("%d", report.Entries[0].Port), "--workspace", canonical, "--language", "go"}
	if strings.Join(row.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("intent args = %v, want %v", row.Args, wantArgs)
	}
	if got := h.fakeClients.entries["codex-cli"][report.Entries[0].ClientEntries["codex-cli"]]; got != fmt.Sprintf("http://127.0.0.1:%d/mcp", report.Entries[0].Port) {
		t.Fatalf("codex URL = %q, want hub URL on registered port", got)
	}
}

func TestRegister_SupervisedContinuesWhenSchedulerNotImplemented(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origFactory := testSchedulerFactory
	testSchedulerFactory = func() (testScheduler, error) {
		return nil, fmt.Errorf("scheduler.New: %w", scheduler.ErrNotImplemented)
	}
	defer func() { testSchedulerFactory = origFactory }()

	reconcileCalls := 0
	dryRunCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			dryRunCalls++
			return ReconcileResponse{DryRun: true}, nil
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)

	report, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err != nil {
		t.Fatalf("Register supervised with not-implemented scheduler: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	if reconcileCalls != 2 || dryRunCalls != 1 {
		t.Fatalf("reconcile calls = %d dry-run = %d, want 2 total with 1 dry-run preflight", reconcileCalls, dryRunCalls)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "scheduler unavailable") {
		t.Fatalf("warnings = %v, want scheduler unavailable warning", report.Warnings)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")); row == nil {
		t.Fatalf("supervisor-intent missing LSP row for %s/go; rows=%+v", wsKey, intent.Daemons)
	}
}

func TestRegister_DefaultsToSupervisedWhenSchedulerNotImplemented(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origFactory := testSchedulerFactory
	testSchedulerFactory = func() (testScheduler, error) {
		return nil, fmt.Errorf("scheduler.New: %w", scheduler.ErrNotImplemented)
	}
	defer func() { testSchedulerFactory = origFactory }()

	reconcileCalls := 0
	dryRunCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			dryRunCalls++
			return ReconcileResponse{DryRun: true}, nil
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)

	report, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("plain Register with not-implemented scheduler: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	if reconcileCalls != 2 || dryRunCalls != 1 {
		t.Fatalf("reconcile calls = %d dry-run = %d, want 2 total with 1 dry-run preflight", reconcileCalls, dryRunCalls)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "scheduler unavailable") {
		t.Fatalf("warnings = %v, want scheduler unavailable warning", report.Warnings)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")); row == nil {
		t.Fatalf("plain schedulerless register did not write supervised LSP row for %s/go; rows=%+v", wsKey, intent.Daemons)
	}
}

func TestRegister_SchedulerlessSupervisorUnavailableFailsBeforeMutation(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origFactory := testSchedulerFactory
	testSchedulerFactory = func() (testScheduler, error) {
		return nil, fmt.Errorf("scheduler.New: %w", scheduler.ErrNotImplemented)
	}
	defer func() { testSchedulerFactory = origFactory }()

	reconcileCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if apply {
			t.Fatal("schedulerless preflight failure must abort before apply reconcile")
		}
		return ReconcileResponse{}, fmt.Errorf("no supervisor: %w", ErrSupervisorIPCUnavailable)
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)

	report, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer: &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected schedulerless register to fail when supervisor is unavailable")
	}
	if report == nil || !strings.Contains(strings.Join(report.Warnings, "\n"), "scheduler unavailable") {
		t.Fatalf("report warnings = %+v, want scheduler unavailable warning", report)
	}
	if !strings.Contains(err.Error(), "requires a running supervisor") ||
		!strings.Contains(err.Error(), "mcphub supervise") {
		t.Fatalf("error lacks supervisor guidance: %v", err)
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want one dry-run preflight", reconcileCalls)
	}
	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 0 {
		t.Fatalf("schedulerless preflight failure wrote registry rows: %+v", rows)
	}
	if n := countEntries(h.fakeClients); n != 0 {
		t.Fatalf("schedulerless preflight failure wrote client entries: %d", n)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if _, err := ReadSupervisorIntent(intentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supervisor intent exists after preflight failure: %v", err)
	}
}

func TestRegister_SupervisedSchedulerRealFailureFailsLoud(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origFactory := testSchedulerFactory
	testSchedulerFactory = func() (testScheduler, error) {
		return nil, errors.New("schtasks.exe resolution failed")
	}
	defer func() { testSchedulerFactory = origFactory }()

	reconcileCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	_, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err == nil {
		t.Fatal("Register supervised with real scheduler failure returned nil error")
	}
	if !strings.Contains(err.Error(), "schtasks.exe resolution failed") {
		t.Fatalf("error = %v, want scheduler failure surfaced", err)
	}
	if reconcileCalls != 0 {
		t.Fatalf("reconcile calls = %d, want 0 when scheduler constructor has a real failure", reconcileCalls)
	}
	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if len(reg.Workspaces) != 0 {
		t.Fatalf("registry rows written despite scheduler failure: %+v", reg.Workspaces)
	}
}

func TestRegister_SupervisedDeleteLegacyTaskFailureAbortsBeforeIntent(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	reconcileCalled := false
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalled = true
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, "go")
	h.fakeSch.tasks[taskName] = true
	h.fakeSch.xml[taskName] = []byte(`<Task name="` + taskName + `"/>`)
	h.fakeSch.failDeleteErr = fmt.Errorf("scheduler access denied")

	_, err = mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err == nil {
		t.Fatal("expected supervised register to fail when deleting the legacy task fails")
	}
	if !strings.Contains(err.Error(), "delete legacy task "+taskName+" before supervised promote") {
		t.Fatalf("error = %v, want legacy delete context", err)
	}
	if reconcileCalled {
		t.Fatal("supervised register reconciled after legacy task deletion failed")
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if intent != nil {
		intentTask := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")
		if row := intent.FindSupervisorDaemonByTaskName(intentTask); row != nil {
			t.Fatalf("supervisor-intent row %s was written despite delete failure: %+v", intentTask, row)
		}
	}
}

func TestRegister_SupervisedLiveLegacyKillFailureAbortsBeforeDeleteAndIntent(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	reconcileCalled := false
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalled = true
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origKill := killByPortFn
	killCalls := 0
	killByPortFn = func(port int, timeout time.Duration) error {
		killCalls++
		return fmt.Errorf("access denied killing port %d", port)
	}
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, "go")
	h.fakeSch.tasks[taskName] = true
	h.fakeSch.xml[taskName] = []byte(`<Task name="` + taskName + `"/>`)

	_, err = mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err == nil {
		t.Fatal("expected supervised register to fail when live legacy port kill fails")
	}
	if !strings.Contains(err.Error(), "kill legacy LSP proxy") {
		t.Fatalf("error = %v, want kill legacy LSP proxy context", err)
	}
	if killCalls != 1 {
		t.Fatalf("kill calls = %d, want 1", killCalls)
	}
	if len(h.fakeSch.deleteNames) != 0 {
		t.Fatalf("legacy task was deleted after kill failure: %v", h.fakeSch.deleteNames)
	}
	if len(h.fakeSch.importNames) != 0 || len(h.fakeSch.runNames) != 0 {
		t.Fatalf("legacy task was restored after pre-delete kill failure: import=%v run=%v",
			h.fakeSch.importNames, h.fakeSch.runNames)
	}
	if reconcileCalled {
		t.Fatal("supervised register reconciled after live legacy kill failed")
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if intent != nil {
		intentTask := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")
		if row := intent.FindSupervisorDaemonByTaskName(intentTask); row != nil {
			t.Fatalf("supervisor-intent row %s was written despite kill failure: %+v", intentTask, row)
		}
	}
}

func TestRegister_SupervisedLiveNoXMLKillFailureAbortsBeforeMutation(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	reconcileCalled := false
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalled = true
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origKill := killByPortFn
	killCalls := 0
	killByPortFn = func(port int, timeout time.Duration) error {
		killCalls++
		return fmt.Errorf("access denied killing port %d", port)
	}
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, "go")
	reg := NewRegistry(h.regPath)
	prior := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9200,
		TaskName:      taskName,
		ClientEntries: map[string]string{"codex-cli": "custom-go"},
		Lifecycle:     LifecycleActive,
	}
	if err := reg.PutLSP(prior); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err = mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err == nil {
		t.Fatal("expected supervised register to fail when live no-XML port kill fails")
	}
	if !strings.Contains(err.Error(), "kill legacy LSP proxy") {
		t.Fatalf("error = %v, want kill legacy LSP proxy context", err)
	}
	if killCalls != 1 {
		t.Fatalf("kill calls = %d, want 1", killCalls)
	}
	if len(h.fakeSch.deleteNames) != 0 || len(h.fakeSch.importNames) != 0 || len(h.fakeSch.runNames) != 0 {
		t.Fatalf("scheduler mutated after no-XML kill failure: delete=%v import=%v run=%v",
			h.fakeSch.deleteNames, h.fakeSch.importNames, h.fakeSch.runNames)
	}
	if reconcileCalled {
		t.Fatal("supervised register reconciled after no-XML kill failure")
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	got, ok := reg.Get(wsKey, "go")
	if !ok {
		t.Fatal("prior registry row was removed after no-XML kill failure")
	}
	if got.Port != prior.Port || got.ClientEntries["codex-cli"] != "custom-go" {
		t.Fatalf("prior registry row changed after no-XML kill failure: %+v", got)
	}
}

func TestRegister_SupervisedNoLegacyRollbackDoesNotKillUnspawnedPort(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		t.Fatal("supervised register must fail before supervisor reconcile")
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origKill := killByPortFn
	var killed []int
	killByPortFn = func(port int, timeout time.Duration) error {
		killed = append(killed, port)
		return nil
	}
	defer func() { killByPortFn = origKill }()

	h.fakeSch.failDeleteErr = scheduler.ErrTaskNotFound
	h.fakeClients.failAddEntryCalls = 1

	_, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), t.TempDir(), []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err == nil {
		t.Fatal("expected supervised register to fail on induced client write")
	}
	if !strings.Contains(err.Error(), "write claude-code entry") {
		t.Fatalf("error = %v, want induced client write failure", err)
	}
	if len(killed) != 0 {
		t.Fatalf("rollback killed unspawned no-legacy port(s): %v", killed)
	}
}

func TestRegister_EntryNameCollision(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Workspace 1 registers python first.
	ws1 := t.TempDir()
	m := nineLanguageManifest()
	if _, err := mustNewAPI(t).registerWithManifest(m, ws1, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	// Workspace 2 registers python second — the base name is taken, so
	// the 4-hex collision suffix must kick in.
	ws2 := t.TempDir()
	if _, err := mustNewAPI(t).registerWithManifest(m, ws2, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 2 {
		t.Fatalf("want 2 entries, got %d", len(reg.Workspaces))
	}
	// The second workspace's python entry must use the suffixed name.
	canonical2, _ := CanonicalWorkspacePath(ws2)
	wsKey2 := WorkspaceKey(canonical2)
	entry2, ok := reg.Get(wsKey2, "python")
	if !ok {
		t.Fatal("workspace 2 python entry missing")
	}
	for _, name := range entry2.ClientEntries {
		if name == "mcp-language-server-python" {
			t.Errorf("workspace 2 entry should not use the base name %q; collision suffix missing", name)
		}
		if !strings.HasPrefix(name, "mcp-language-server-python-") {
			t.Errorf("entry name %q: want prefix mcp-language-server-python-<hex>", name)
		}
	}
}

func TestRegister_SupervisedReservesRouterEntryNameForFirstWorkspace(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Fatalf("supervised register called reconcile with apply=false")
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	report, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err != nil {
		t.Fatalf("Register supervised: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	entryName := report.Entries[0].ClientEntries["codex-cli"]
	routerName := LSPRouterEntryName("go")
	if entryName == routerName {
		t.Fatalf("supervised first workspace used router entry name %q", entryName)
	}
	short := wsKey
	if len(short) > 4 {
		short = short[:4]
	}
	if entryName != routerName+"-"+short {
		t.Fatalf("entry name = %q, want %q", entryName, routerName+"-"+short)
	}
	if _, ok := h.fakeClients.entries["codex-cli"][entryName]; !ok {
		t.Fatalf("client-write pass did not use composed entry name %q; entries=%v", entryName, h.fakeClients.entries["codex-cli"])
	}
}

func TestRegister_LegacyReservesRouterEntryNameForFirstWorkspace(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	routerName := LSPRouterEntryName("go")
	routerURL := LSPRouterURL(7777, "go")
	h.fakeClients.entries["codex-cli"][routerName] = routerURL

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	report, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Register legacy: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	entryName := report.Entries[0].ClientEntries["codex-cli"]
	if entryName == routerName {
		t.Fatalf("legacy first workspace used router entry name %q", entryName)
	}
	short := wsKey
	if len(short) > 4 {
		short = short[:4]
	}
	if entryName != routerName+"-"+short {
		t.Fatalf("entry name = %q, want %q", entryName, routerName+"-"+short)
	}
	if got := h.fakeClients.entries["codex-cli"][routerName]; got != routerURL {
		t.Fatalf("router entry %q overwritten: got %q want %q", routerName, got, routerURL)
	}
	if _, ok := h.fakeClients.entries["codex-cli"][entryName]; !ok {
		t.Fatalf("client-write pass did not use composed entry name %q; entries=%v", entryName, h.fakeClients.entries["codex-cli"])
	}
}

func TestResolveEntryName_NoCollisionReturnsBase(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	got := ResolveEntryName(reg, "mcp-language-server", "python", "workspa1")
	if got != "mcp-language-server-python" {
		t.Errorf("got %q, want mcp-language-server-python", got)
	}
}

func TestResolveEntryName_CollisionAppendsHash(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "otherkey",
		Language:      "python",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-python"},
	})
	got := ResolveEntryName(reg, "mcp-language-server", "python", "ourkey00")
	if got != "mcp-language-server-python-ourk" {
		t.Errorf("got %q, want mcp-language-server-python-ourk", got)
	}
}

// TestResolveEntryName_ShortSuffixCollisionExtendsToFullKey guards the
// narrow case where two OTHER workspaces both collide on base AND
// share the first 4 hex chars of their workspace keys. With only
// 4-char suffixes, two workspaces like "ourk1111" and "ourk2222"
// would both map to "mcp-language-server-python-ourk", overwriting
// each other's client config entry. When the short form is taken,
// ResolveEntryName must fall back to the full 8-char key.
func TestResolveEntryName_ShortSuffixCollisionExtendsToFullKey(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	// Workspace A already holds the BASE name.
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "ffffffff",
		Language:      "python",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-python"},
	})
	// Workspace B shares the first 4 hex with our key AND already holds
	// the short-suffixed collision name — mimics the bug scenario where
	// a prior collision taking "ourk" prefix already parked the name.
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "ourk1111",
		Language:      "python",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-python-ourk"},
	})
	// Now our key "ourk2222" collides on base with A and on suffix with B.
	got := ResolveEntryName(reg, "mcp-language-server", "python", "ourk2222")
	want := "mcp-language-server-python-ourk2222"
	if got != want {
		t.Errorf("short-suffix collision must extend to full key; got %q want %q", got, want)
	}
}

func TestResolveEntryName_SameWorkspaceReturnsBase(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "ourkey00",
		Language:      "python",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-python"},
	})
	got := ResolveEntryName(reg, "mcp-language-server", "python", "ourkey00")
	if got != "mcp-language-server-python" {
		t.Errorf("got %q, want mcp-language-server-python (idempotent self-case)", got)
	}
}

// --- Unregister tests ---------------------------------------------------

func TestUnregister_FullRemovesAllLanguages(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	if _, err := a.registerWithManifest(m, ws, []string{"python", "typescript"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	rpt, err := a.unregisterWithManifest(m, ws, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	sort.Strings(rpt.Removed)
	want := []string{"python", "typescript"}
	if len(rpt.Removed) != len(want) || rpt.Removed[0] != want[0] || rpt.Removed[1] != want[1] {
		t.Errorf("Removed = %v, want %v", rpt.Removed, want)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("expected 0 entries after full unregister, got %+v", reg.Workspaces)
	}
	// Client entries removed too.
	if n := countEntries(h.fakeClients); n != 0 {
		t.Errorf("client entries remain after unregister: %d", n)
	}
}

func TestUnregister_MixedCanonicalAndLegacyKeysRemovesBoth(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	reconcileCalls := 0
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			t.Fatalf("supervised unregister called reconcile with apply=false")
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	rawPath, canonical, legacy := mixedKeyWorkspaceAliasForUnregister(t)
	wsKey := WorkspaceKey(canonical)
	legacyWSKey := WorkspaceKey(legacy)
	canonicalEntry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          0,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": "go-entry"},
		Lifecycle:     LifecycleConfigured,
	}
	legacyEntry := WorkspaceEntry{
		WorkspaceKey:  legacyWSKey,
		WorkspacePath: legacy,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          0,
		TaskName:      LSPTaskNameForWorkspaceLanguage(legacyWSKey, "python"),
		ClientEntries: map[string]string{"codex-cli": "python-entry"},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(canonicalEntry); err != nil {
		t.Fatalf("PutLSP canonical: %v", err)
	}
	if err := reg.PutLSP(legacyEntry); err != nil {
		t.Fatalf("PutLSP legacy: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeClients.entries["codex-cli"]["go-entry"] = "http://localhost:0/mcp"
	h.fakeClients.entries["codex-cli"]["python-entry"] = "http://localhost:0/mcp"
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			BuildSupervisorDaemonForLSP(canonicalEntry, testCanonicalMcphubPathOverride),
			BuildSupervisorDaemonForLSP(legacyEntry, testCanonicalMcphubPathOverride),
		},
	})

	rpt, err := mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), rawPath, []string{"go", "python"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Unregister mixed-key rows: %v", err)
	}
	sort.Strings(rpt.Removed)
	if want := []string{"go", "python"}; !slices.Equal(rpt.Removed, want) {
		t.Fatalf("Removed = %v, want %v", rpt.Removed, want)
	}
	if len(rpt.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for languages present under either key", rpt.Warnings)
	}
	if reconcileCalls != 2 {
		t.Fatalf("reconcile calls = %d, want 2 supervised descriptor removals", reconcileCalls)
	}

	reg = NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 0 {
		t.Fatalf("canonical LSP rows survived unregister: %+v", rows)
	}
	if rows := reg.ListByWorkspaceLSP(legacyWSKey); len(rows) != 0 {
		t.Fatalf("legacy LSP rows survived unregister: %+v", rows)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after unregister: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")); row != nil {
		t.Fatalf("canonical supervisor-intent row survived unregister: %+v", row)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(legacyWSKey, "python")); row != nil {
		t.Fatalf("legacy supervisor-intent row survived unregister: %+v", row)
	}
	if _, ok := h.fakeClients.entries["codex-cli"]["go-entry"]; ok {
		t.Fatal("canonical client entry survived unregister")
	}
	if _, ok := h.fakeClients.entries["codex-cli"]["python-entry"]; ok {
		t.Fatal("legacy client entry survived unregister")
	}
}

func TestUnregister_PreservesSharedLSPRouterEntry(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)
	wsKey := WorkspaceKey(canonical)
	routerName := LSPRouterEntryName("go")
	reg := NewRegistry(h.regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": routerName},
		Lifecycle:     LifecycleConfigured,
	})
	if err := reg.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	h.fakeClients.entries["codex-cli"][routerName] = LSPRouterURL(7777, "go")

	var out bytes.Buffer
	rpt, err := mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &out)
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if !slices.Contains(rpt.Removed, "go") {
		t.Fatalf("Removed = %v, want go", rpt.Removed)
	}
	if got := h.fakeClients.entries["codex-cli"][routerName]; got != LSPRouterURL(7777, "go") {
		t.Fatalf("shared router entry after unregister = %q, want preserved router URL", got)
	}
	if !strings.Contains(out.String(), "preserved shared LSP router entry") {
		t.Fatalf("output = %q, want preserved shared LSP router entry message", out.String())
	}
}

func TestUnregister_PartialKeepsOthers(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	if _, err := a.registerWithManifest(m, ws, []string{"python", "typescript", "rust"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.unregisterWithManifest(m, ws, []string{"typescript"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d: %+v", len(reg.Workspaces), reg.Workspaces)
	}
	for _, e := range reg.Workspaces {
		if e.Language == "typescript" {
			t.Errorf("typescript should have been removed: %+v", e)
		}
	}
}

func TestUnregister_UnknownWorkspaceErrors(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()
	m := nineLanguageManifest()
	if _, err := mustNewAPI(t).unregisterWithManifest(m, ws, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for unregistered workspace")
	}
}

// TestUnregister_KillsStaleProxyByPort verifies that Unregister terminates
// the running proxy for every language it removes BEFORE calling sch.Delete.
// Without this kill, the proxy keeps its port bound until the next reboot
// (sch.Delete drops the task record but does not stop the running child).
func TestUnregister_KillsStaleProxyByPort(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	// Fake forceKillByPortFn — records the ports it was asked to kill.
	origForceKill := forceKillByPortFn
	defer func() { forceKillByPortFn = origForceKill }()
	var killed []int
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		killed = append(killed, port)
		return portKillKilled, nil
	}
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	if _, err := a.registerWithManifest(m, ws, []string{"python", "typescript"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	// Read allocated ports from the registry so we can assert kill order.
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	wantPorts := map[int]bool{}
	for _, e := range reg.Workspaces {
		wantPorts[e.Port] = true
	}
	if _, err := a.unregisterWithManifest(m, ws, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if len(killed) != len(wantPorts) {
		t.Fatalf("killed %d ports, want %d: got=%v want=%v",
			len(killed), len(wantPorts), killed, wantPorts)
	}
	for _, p := range killed {
		if !wantPorts[p] {
			t.Errorf("killed unexpected port %d; wanted one of %v", p, wantPorts)
		}
	}
}

// TestUnregister_KillProxyFailureIsWarning verifies that a kill-by-port
// failure does NOT abort the teardown; the error is recorded in Warnings
// and Unregister proceeds to remove the task + registry row.
func TestUnregister_KillProxyFailureIsWarning(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origForceKill := forceKillByPortFn
	defer func() { forceKillByPortFn = origForceKill }()
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		return portKillNoListener, fmt.Errorf("induced kill failure for port %d", port)
	}
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	rpt, err := a.unregisterWithManifest(m, ws, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Unregister must not fail on kill-by-port error: %v", err)
	}
	if len(rpt.Warnings) == 0 {
		t.Error("expected at least one warning for kill-by-port failure")
	}
	// Registry row still removed despite the kill failure.
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("registry rows remain after Unregister: %+v", reg.Workspaces)
	}
}

func TestUnregister_ForeignPortOwnerNotKilled(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	defer func() {
		killByPortFn = origKill
		forceKillByPortFn = origForceKill
	}()
	legacyKillCalled := false
	killByPortFn = func(port int, timeout time.Duration) error {
		legacyKillCalled = true
		return nil
	}
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		if port != 33041 {
			t.Fatalf("forceKillByPortFn port = %d, want 33041", port)
		}
		return portKillIdentityMismatch, errors.New("port owned by foreign process")
	}

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	entryName := "mcp-language-server-go-foreign"
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          33041,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": entryName},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeClients.entries["codex-cli"][entryName] = "http://localhost:33041/mcp"

	rpt, err := mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if legacyKillCalled {
		t.Fatal("unregister used killByPortFn on a foreign owner; want identity-mismatch warning without kill")
	}
	warnings := strings.Join(rpt.Warnings, "\n")
	if !strings.Contains(warnings, "port owned by foreign process") || !strings.Contains(warnings, "not killing") {
		t.Fatalf("warnings = %v, want foreign-owner not-killing warning", rpt.Warnings)
	}
}

func TestUnregister_McphubPortOwnerKilledWithIdentityGate(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	defer func() {
		killByPortFn = origKill
		forceKillByPortFn = origForceKill
	}()
	killByPortFn = func(port int, timeout time.Duration) error {
		t.Fatalf("unregister must use outcome-aware kill path, got killByPortFn(%d)", port)
		return nil
	}
	var killed []int
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		if timeout != 5*time.Second {
			t.Errorf("kill timeout = %s, want 5s", timeout)
		}
		killed = append(killed, port)
		return portKillKilled, nil
	}

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	entryName := "mcp-language-server-go-mcphub"
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          33042,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": entryName},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeClients.entries["codex-cli"][entryName] = "http://localhost:33042/mcp"

	if _, err := mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if !slices.Equal(killed, []int{33042}) {
		t.Fatalf("forceKillByPortFn ports = %v, want [33042]", killed)
	}
}

func TestUnregister_SchedulerNotImplementedRemovesIntentAndRegistryWithWarning(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origFactory := testSchedulerFactory
	testSchedulerFactory = func() (testScheduler, error) {
		return nil, fmt.Errorf("scheduler.New: %w", scheduler.ErrNotImplemented)
	}
	defer func() { testSchedulerFactory = origFactory }()

	reconcileCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			t.Fatalf("supervised unregister called reconcile with apply=false")
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origKill := killByPortFn
	killByPortFn = func(port int, timeout time.Duration) error { return nil }
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	entryName := "mcp-language-server-go-abcd"
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": entryName},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeClients.entries["codex-cli"][entryName] = "http://localhost:9242/mcp"
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			BuildSupervisorDaemonForLSP(entry, testCanonicalMcphubPathOverride),
		},
	})

	rpt, err := mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Unregister with not-implemented scheduler: %v", err)
	}
	if !strings.Contains(strings.Join(rpt.Warnings, "\n"), "scheduler unavailable") {
		t.Fatalf("warnings = %v, want scheduler unavailable warning", rpt.Warnings)
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", reconcileCalls)
	}
	reg = NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 0 {
		t.Fatalf("registry rows remain after unregister: %+v", rows)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after unregister: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")); row != nil {
		t.Fatalf("supervisor-intent row survived unregister: %+v", row)
	}
	if _, ok := h.fakeClients.entries["codex-cli"][entryName]; ok {
		t.Fatalf("client entry %s survived unregister", entryName)
	}
}

func TestUnregister_SchedulerRealFailureLeavesRowsAndIntent(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origFactory := testSchedulerFactory
	testSchedulerFactory = func() (testScheduler, error) {
		return nil, errors.New("current user lookup failed")
	}
	defer func() { testSchedulerFactory = origFactory }()

	reconcileCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-go-abcd"},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			BuildSupervisorDaemonForLSP(entry, testCanonicalMcphubPathOverride),
		},
	})

	_, err = mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Unregister with real scheduler failure returned nil error")
	}
	if !strings.Contains(err.Error(), "current user lookup failed") {
		t.Fatalf("error = %v, want scheduler constructor failure surfaced", err)
	}
	if reconcileCalls != 0 {
		t.Fatalf("reconcile calls = %d, want 0 before unregister side effects", reconcileCalls)
	}
	reg = NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 1 {
		t.Fatalf("registry rows = %+v, want original row preserved", rows)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")); row == nil {
		t.Fatalf("supervisor intent row removed despite scheduler failure; rows=%+v", intent.Daemons)
	}
}

func TestUnregister_SupervisedRemovesIntentAndReconciles(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	reconcileCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			t.Fatalf("supervised unregister called reconcile with apply=false")
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	a := mustNewAPI(t)
	report, err := a.registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err != nil {
		t.Fatalf("Register supervised: %v", err)
	}
	if reconcileCalls != 1 {
		t.Fatalf("supervised register reconcile calls = %d, want 1", reconcileCalls)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("register entries = %d, want 1", len(report.Entries))
	}

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent before unregister: %v", err)
	}
	intentTask := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")
	if row := intent.FindSupervisorDaemonByTaskName(intentTask); row == nil {
		t.Fatalf("test setup missing supervisor-intent row %s; rows=%+v", intentTask, intent.Daemons)
	}

	if _, err := a.unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unregister supervised: %v", err)
	}
	if reconcileCalls != 2 {
		t.Fatalf("reconcile calls after unregister = %d, want 2", reconcileCalls)
	}
	intent, err = ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after unregister: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(intentTask); row != nil {
		t.Fatalf("supervisor-intent row %s survived unregister: %+v", intentTask, row)
	}
	if !slices.Contains(h.fakeSch.deleteNames, report.Entries[0].TaskName) {
		t.Fatalf("unregister did not also delete legacy scheduler task %s; deleteNames=%v", report.Entries[0].TaskName, h.fakeSch.deleteNames)
	}
}

func TestUnregister_SupervisedReconcileFailureRestoresIntentAndAborts(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	reconcileCalls := 0
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			t.Fatalf("supervised unregister called reconcile with apply=false")
		}
		return ReconcileResponse{}, errors.New("synthetic live supervisor apply failure")
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for port %d after live-supervisor reconcile failure", port)
		return portKillNoListener, nil
	}

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	entryName := "mcp-language-server-go-abcd"
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9305,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": entryName},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeClients.entries["codex-cli"][entryName] = "http://localhost:9305/mcp"
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intentTask := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			BuildSupervisorDaemonForLSP(entry, testCanonicalMcphubPathOverride),
		},
	})

	_, err = mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Unregister returned nil after live-supervisor reconcile failure")
	}
	if !strings.Contains(err.Error(), "retry") || !strings.Contains(err.Error(), "synthetic live supervisor apply failure") {
		t.Fatalf("error = %v, want loud retry error preserving reconcile cause", err)
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", reconcileCalls)
	}
	reg = NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 1 {
		t.Fatalf("registry rows = %+v, want original row preserved", rows)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(intentTask); row == nil {
		t.Fatalf("supervisor intent row was not restored after reconcile failure; rows=%+v", intent.Daemons)
	}
	if _, ok := h.fakeClients.entries["codex-cli"][entryName]; !ok {
		t.Fatalf("client entry %s removed despite aborted unregister", entryName)
	}
	if len(h.fakeSch.deleteNames) != 0 {
		t.Fatalf("scheduler deletes = %v, want none after aborted unregister", h.fakeSch.deleteNames)
	}
}

func TestUnregister_SupervisedRemovesStopWithDescriptorAndAllowsRegisterAgain(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	defer func() {
		killByPortFn = origKill
		forceKillByPortFn = origForceKill
	}()
	killByPortFn = func(port int, timeout time.Duration) error { return nil }
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		return portKillKilled, nil
	}

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")
	siblingTask := `\mcp-local-hub-lsp-sibling-python`
	entryName := "mcp-language-server-go-stop"
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          33043,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": entryName},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeClients.entries["codex-cli"][entryName] = "http://localhost:33043/mcp"

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	stopped := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			BuildSupervisorDaemonForLSP(entry, testCanonicalMcphubPathOverride),
		},
		Stops: map[string]DaemonIntent{
			taskName:    stopped,
			siblingTask: stopped,
		},
	})

	reconcileSawRegisterStop := false
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		intent, err := ReadSupervisorIntent(intentPath)
		if err != nil {
			return ReconcileResponse{}, err
		}
		if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
			if _, ok := intent.Stops[taskName]; ok {
				reconcileSawRegisterStop = true
			}
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		intent, err := ReadSupervisorIntent(intentPath)
		if err != nil {
			return err
		}
		if _, ok := intent.Stops[taskName]; ok {
			return errors.New("readiness suppressed by stale stop")
		}
		return nil
	}
	defer func() { proxyReadinessFn = origReadiness }()

	if _, err := mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unregister supervised: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after unregister: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
		t.Fatalf("descriptor %s survived unregister: %+v", taskName, row)
	}
	if _, ok := intent.Stops[taskName]; ok {
		t.Errorf("stop tombstone %s survived descriptor removal", taskName)
	}
	if _, ok := intent.Stops[siblingTask]; !ok {
		t.Fatalf("sibling stop %s was pruned with unrelated descriptor", siblingTask)
	}

	if _, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	}); err != nil {
		t.Fatalf("supervised register after unregister: %v", err)
	}
	if reconcileSawRegisterStop {
		t.Fatal("register reconcile saw the stale stop for the descriptor being re-added")
	}
	intent, err = ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after register: %v", err)
	}
	if _, ok := intent.Stops[taskName]; ok {
		t.Fatalf("stop tombstone %s survived register-again flow", taskName)
	}
	if _, ok := intent.Stops[siblingTask]; !ok {
		t.Fatalf("sibling stop %s did not survive register-again flow", siblingTask)
	}
}

func TestRegister_SupervisedClearsDescriptorAbsentStopBeforeReconcile(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")
	siblingTask := `\mcp-local-hub-lsp-sibling-python`
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	stopped := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			taskName:    stopped,
			siblingTask: stopped,
		},
	})

	reconcileCalled := false
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalled = true
		if !apply {
			t.Fatalf("supervised register reconcile apply = false, want true")
		}
		intent, err := ReadSupervisorIntent(intentPath)
		if err != nil {
			return ReconcileResponse{}, err
		}
		if row := intent.FindSupervisorDaemonByTaskName(taskName); row == nil {
			t.Fatalf("reconcile saw no descriptor %s; rows=%+v", taskName, intent.Daemons)
		}
		if _, ok := intent.Stops[taskName]; ok {
			t.Fatalf("reconcile saw stale stop %s; register must clear it before readiness", taskName)
		}
		if _, ok := intent.Stops[siblingTask]; !ok {
			t.Fatalf("reconcile lost unrelated sibling stop %s; stops=%+v", siblingTask, intent.Stops)
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	m := nineLanguageManifest()
	m.PortPool = &config.PortPool{Start: 33050, End: 33059}
	report, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err != nil {
		t.Fatalf("Register supervised with descriptor-absent stop: %v", err)
	}
	if !reconcileCalled {
		t.Fatal("register did not call supervisor reconcile")
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after register: %v", err)
	}
	if _, ok := intent.Stops[taskName]; ok {
		t.Fatalf("stop tombstone %s survived successful register", taskName)
	}
	if _, ok := intent.Stops[siblingTask]; !ok {
		t.Fatalf("sibling stop %s did not survive register; stops=%+v", siblingTask, intent.Stops)
	}
}

func TestRegister_SupervisedRollbackRestoresStopClearedBeforeReadinessFailure(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "go")
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	stopped := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			taskName: stopped,
		},
	})

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		intent, err := ReadSupervisorIntent(intentPath)
		if err != nil {
			return ReconcileResponse{}, err
		}
		if row := intent.FindSupervisorDaemonByTaskName(taskName); row == nil {
			t.Fatalf("reconcile saw no descriptor %s; rows=%+v", taskName, intent.Daemons)
		}
		if _, ok := intent.Stops[taskName]; ok {
			t.Fatalf("reconcile saw stale stop %s before readiness failure", taskName)
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		return errors.New("synthetic readiness timeout")
	}
	defer func() { proxyReadinessFn = origReadiness }()

	m := nineLanguageManifest()
	m.PortPool = &config.PortPool{Start: 33060, End: 33069}
	_, err = mustNewAPI(t).registerWithManifest(m, ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err == nil {
		t.Fatal("Register succeeded despite synthetic readiness failure")
	}
	if !strings.Contains(err.Error(), "synthetic readiness timeout") {
		t.Fatalf("error = %v, want synthetic readiness timeout", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after rollback: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
		t.Fatalf("rollback left descriptor %s after readiness failure: %+v", taskName, row)
	}
	gotStop, ok := intent.Stops[taskName]
	if !ok {
		t.Fatalf("rollback did not restore prior stop %s; stops=%+v", taskName, intent.Stops)
	}
	if gotStop.Desired != stopped.Desired || gotStop.Reason != stopped.Reason || !gotStop.UpdatedAt.Equal(stopped.UpdatedAt) {
		t.Fatalf("restored stop = %+v, want %+v", gotStop, stopped)
	}
	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 0 {
		t.Fatalf("rollback left registry rows after readiness failure: %+v", rows)
	}
}

func TestUnregister_LegacyOnlyDeletesSchedulerTaskWithoutReconcile(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	reconcileCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	a := mustNewAPI(t)
	report, err := a.registerWithManifest(nineLanguageManifest(), ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Register legacy: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("register entries = %d, want 1", len(report.Entries))
	}
	taskName := report.Entries[0].TaskName

	if _, err := a.unregisterWithManifest(nineLanguageManifest(), ws, []string{"python"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unregister legacy: %v", err)
	}
	if reconcileCalls != 0 {
		t.Fatalf("legacy-only unregister reconcile calls = %d, want 0", reconcileCalls)
	}
	if !slices.Contains(h.fakeSch.deleteNames, taskName) {
		t.Fatalf("legacy-only unregister did not delete scheduler task %s; deleteNames=%v", taskName, h.fakeSch.deleteNames)
	}
}

func TestUnregister_NoSupervisorIntentRowIsNoOpSuccess(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	reconcileCalls := 0
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	a := mustNewAPI(t)
	if _, err := a.registerWithManifest(nineLanguageManifest(), ws, []string{"rust"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Register legacy: %v", err)
	}

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	sibling := SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Command: "mcphub.exe"}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-06-03T00:00:00Z",
		Daemons:   []SupervisorDaemon{sibling},
	})

	if _, err := a.unregisterWithManifest(nineLanguageManifest(), ws, []string{"rust"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unregister with absent supervisor intent row: %v", err)
	}
	if reconcileCalls != 0 {
		t.Fatalf("absent-intent unregister reconcile calls = %d, want 0", reconcileCalls)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after unregister: %v", err)
	}
	if row := intent.FindSupervisorDaemonByTaskName(sibling.TaskName); row == nil {
		t.Fatalf("sibling supervisor-intent row was not preserved; rows=%+v", intent.Daemons)
	}
}

func TestUnregister_IntentRemovalFailureLeavesRegistryAndClients(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatal("reconcile must not run when supervisor intent removal fails")
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	var killed []int
	killByPortFn = func(port int, timeout time.Duration) error {
		killed = append(killed, port)
		return nil
	}
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		killed = append(killed, port)
		return portKillKilled, nil
	}
	defer func() {
		killByPortFn = origKill
		forceKillByPortFn = origForceKill
	}()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	entryName := "mcp-language-server-go-abcd"
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		ClientEntries: map[string]string{"codex-cli": entryName},
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeClients.entries["codex-cli"][entryName] = "http://localhost:9242/mcp"

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(intentPath), 0o700); err != nil {
		t.Fatalf("mkdir supervisor intent dir: %v", err)
	}
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"daemons":[`), 0o600); err != nil {
		t.Fatalf("write corrupt supervisor intent: %v", err)
	}

	rpt, err := mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Unregister with failed intent removal should return warnings, got error: %v", err)
	}
	if !strings.Contains(strings.Join(rpt.Warnings, "\n"), "remove supervisor intent") {
		t.Fatalf("warnings = %v, want remove supervisor intent warning", rpt.Warnings)
	}
	if slices.Contains(rpt.Removed, "go") {
		t.Fatalf("Removed = %v, want go omitted after failed intent removal", rpt.Removed)
	}
	if len(killed) != 0 {
		t.Fatalf("kill-by-port ran after failed intent removal: %v", killed)
	}
	if slices.Contains(h.fakeSch.deleteNames, entry.TaskName) {
		t.Fatalf("scheduler task %s deleted after failed intent removal; deleteNames=%v",
			entry.TaskName, h.fakeSch.deleteNames)
	}
	if got := h.fakeClients.entries["codex-cli"][entryName]; got == "" {
		t.Fatalf("client entry %s removed after failed intent removal", entryName)
	}
	reg = NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 1 || rows[0].Language != "go" {
		t.Fatalf("registry rows after failed intent removal = %+v, want original go row preserved", rows)
	}
}

// --- Install refusal for workspace-scoped manifests ---------------------

// --- KnobDefault tests (D1: knob-driven weekly_refresh_default) ----------

// D1: when caller does NOT explicitly set RegisterOpts.WeeklyRefresh,
// Register reads daemons.weekly_refresh_default from settings. Explicit
// override always wins.
func TestRegister_KnobDefault_FalseByDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	h := newRegisterHarness(t)
	defer h.restore()

	a := NewAPI()
	report, err := a.registerWithManifest(nineLanguageManifest(), t.TempDir(), []string{"python"}, RegisterOpts{
		Writer:                io.Discard,
		WeeklyRefreshExplicit: false,
		WeeklyRefresh:         false,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range report.Entries {
		if e.WeeklyRefresh != false {
			t.Errorf("entry %s WeeklyRefresh = true, want false (knob default)", e.Language)
		}
	}
}

func TestRegister_KnobDefault_HonorsExplicitTrue(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	h := newRegisterHarness(t)
	defer h.restore()

	a := NewAPI()
	report, err := a.registerWithManifest(nineLanguageManifest(), t.TempDir(), []string{"python"}, RegisterOpts{
		Writer:                io.Discard,
		WeeklyRefreshExplicit: true,
		WeeklyRefresh:         true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range report.Entries {
		if !e.WeeklyRefresh {
			t.Errorf("entry %s WeeklyRefresh = false, want true (explicit override)", e.Language)
		}
	}
}

func TestRegister_KnobDefault_ReadsKnobTrue(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	h := newRegisterHarness(t)
	defer h.restore()

	a := NewAPI()
	if err := a.SettingsSet("daemons.weekly_refresh_default", "true"); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	report, err := a.registerWithManifest(nineLanguageManifest(), t.TempDir(), []string{"python"}, RegisterOpts{
		Writer:                io.Discard,
		WeeklyRefreshExplicit: false,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range report.Entries {
		if !e.WeeklyRefresh {
			t.Errorf("entry %s WeeklyRefresh = false, want true (knob=true)", e.Language)
		}
	}
}

// --- Install refusal for workspace-scoped manifests ---------------------

func TestInstall_RefusesWorkspaceScoped(t *testing.T) {
	m := &config.ServerManifest{
		Name:     "mcp-language-server",
		Kind:     config.KindWorkspaceScoped,
		PortPool: &config.PortPool{Start: 9200, End: 9299},
	}
	buf := &bytes.Buffer{}
	err := refuseWorkspaceScopedInstall(m, buf)
	if err == nil {
		t.Fatal("expected error for workspace-scoped on install path")
	}
	if !strings.Contains(err.Error(), "register") {
		t.Errorf("error should point at `mcphub register`: %v", err)
	}
	if !strings.Contains(buf.String(), "register") {
		t.Errorf("stderr should point at `mcphub register`: %s", buf.String())
	}
}

func TestInstall_AcceptsGlobalManifestStill(t *testing.T) {
	m := &config.ServerManifest{
		Name: "serena",
		Kind: config.KindGlobal,
	}
	if err := refuseWorkspaceScopedInstall(m, &bytes.Buffer{}); err != nil {
		t.Errorf("global manifest should not be refused: %v", err)
	}
}

// --- Test doubles -------------------------------------------------------

type fakeScheduler struct {
	tasks             map[string]bool
	xml               map[string][]byte
	failCreateAfterN  int    // Create calls after the Nth succeed; the (N+1)th fails
	failCreateForName string // if non-empty, Create with Name==this value returns an induced error
	createCount       int
	createdSpecs      []scheduler.TaskSpec
	failExportXMLErr  error                  // if non-nil, ExportXML returns this error (instead of ErrTaskNotFound)
	failRunForTask    string                 // if non-empty, Run(name) returns an induced error for this task name
	runCount          int                    // total Run invocations
	runHook           func(name string)      // optional hook called at the top of Run before the induced-failure check
	runNames          []string               // ordered list of task names passed to Run
	listSeed          []scheduler.TaskStatus // pre-seeded tasks returned (prefix-filtered) by List; empty by default
	failDeleteErr     error                  // if non-nil, Delete returns this error before mutating state
	deleteNames       []string               // ordered list of task names passed to Delete (prune observability)
	importNames       []string               // ordered list of task names passed to ImportXML
}

func writeSupervisorIntentForRegisterTest(t *testing.T, path string, f *SupervisorIntentFile) {
	t.Helper()
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("marshal supervisor intent seed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir supervisor intent dir: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write supervisor intent seed: %v", err)
	}
}

func mixedKeyWorkspaceAliasForUnregister(t *testing.T) (string, string, string) {
	t.Helper()
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real workspace: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", alias, realDir).CombinedOutput()
		if err != nil {
			t.Fatalf("create temp junction: %v; output=%s", err, out)
		}
	} else if err := os.Symlink(realDir, alias); err != nil {
		t.Fatalf("create temp symlink: %v", err)
	}
	canonical, err := CanonicalWorkspacePathForCleanup(alias)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathForCleanup(alias): %v", err)
	}
	legacy, err := CanonicalWorkspacePathLegacyCompat(alias)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathLegacyCompat(alias): %v", err)
	}
	if WorkspaceKey(canonical) == WorkspaceKey(legacy) {
		t.Fatalf("mixed-key fixture did not produce distinct keys: canonical=%q legacy=%q", canonical, legacy)
	}
	return alias, canonical, legacy
}

func (f *fakeScheduler) Create(spec scheduler.TaskSpec) error {
	if f.failCreateForName != "" && spec.Name == f.failCreateForName {
		return fmt.Errorf("fake scheduler: induced failure for task %s", spec.Name)
	}
	if f.failCreateAfterN > 0 && f.createCount >= f.failCreateAfterN {
		return fmt.Errorf("fake scheduler: induced failure after %d Create calls", f.failCreateAfterN)
	}
	f.createCount++
	f.tasks[spec.Name] = true
	f.createdSpecs = append(f.createdSpecs, spec)
	// Mirror real scheduler behavior: a created task has an exportable
	// XML snapshot. Stored XML is opaque to the test; rollback/re-register
	// paths depend on ExportXML returning non-empty for an existing task.
	if f.xml == nil {
		f.xml = map[string][]byte{}
	}
	if _, exists := f.xml[spec.Name]; !exists {
		f.xml[spec.Name] = []byte("<Task name=\"" + spec.Name + "\"/>")
	}
	return nil
}
func (f *fakeScheduler) Delete(name string) error {
	if f.failDeleteErr != nil {
		f.deleteNames = append(f.deleteNames, name)
		return f.failDeleteErr
	}
	delete(f.tasks, name)
	f.deleteNames = append(f.deleteNames, name)
	// Mirror real Delete on the seeded-List surface so a pruned task no longer
	// appears in subsequent List calls (and tests can assert deletion via the
	// remaining listSeed). Match on bare form (List strips the leading "\").
	bare := strings.TrimPrefix(name, "\\")
	kept := f.listSeed[:0]
	for _, t := range f.listSeed {
		if strings.TrimPrefix(t.Name, "\\") != bare {
			kept = append(kept, t)
		}
	}
	f.listSeed = kept
	return nil
}
func (f *fakeScheduler) Run(name string) error {
	if f.runHook != nil {
		f.runHook(name)
	}
	f.runCount++
	f.runNames = append(f.runNames, name)
	if f.failRunForTask != "" && f.failRunForTask == name {
		return fmt.Errorf("fake scheduler: induced Run failure for %s", name)
	}
	return nil
}
func (f *fakeScheduler) ExportXML(name string) ([]byte, error) {
	if f.failExportXMLErr != nil {
		return nil, f.failExportXMLErr
	}
	if b, ok := f.xml[name]; ok {
		return b, nil
	}
	return nil, scheduler.ErrTaskNotFound
}
func (f *fakeScheduler) ImportXML(name string, xml []byte) error {
	if f.xml == nil {
		f.xml = map[string][]byte{}
	}
	f.importNames = append(f.importNames, name)
	f.xml[name] = xml
	f.tasks[name] = true
	return nil
}

type fakeClientsMap struct {
	entries           map[string]map[string]string // client-name -> entry-name -> URL
	stdioEntries      map[string]map[string]clients.LanguageServerStdioEntry
	allStdioEntries   map[string]map[string]clients.StdioEntry
	backupKeepCalls   map[string]int
	exists            map[string]bool
	addEntryCount     int
	failAddEntryCalls int // the Nth AddEntry (1-based) fails
}

type fakeClient struct {
	parent *fakeClientsMap
	name   string
}

func (c *fakeClient) Exists() bool {
	return c.parent.exists[c.name]
}
func (c *fakeClient) BackupKeep(keepN int) (string, error) {
	c.parent.backupKeepCalls[c.name]++
	return fmt.Sprintf("/backup/%s/%d", c.name, keepN), nil
}
func (c *fakeClient) AddEntry(e clients.MCPEntry) error {
	c.parent.addEntryCount++
	if c.parent.failAddEntryCalls > 0 && c.parent.addEntryCount == c.parent.failAddEntryCalls {
		return fmt.Errorf("fake client %s: induced AddEntry failure on call #%d", c.name, c.parent.addEntryCount)
	}
	c.parent.entries[c.name][e.Name] = e.URL
	return nil
}
func (c *fakeClient) RemoveEntry(name string) error {
	delete(c.parent.entries[c.name], name)
	delete(c.parent.stdioEntries[c.name], name)
	delete(c.parent.allStdioEntries[c.name], name)
	return nil
}
func (c *fakeClient) GetEntry(name string) (*clients.MCPEntry, error) {
	url, ok := c.parent.entries[c.name][name]
	if !ok {
		return nil, nil
	}
	return &clients.MCPEntry{Name: name, URL: url}, nil
}
func (c *fakeClient) AllStdioEntries() ([]clients.StdioEntry, error) {
	raw := c.parent.allStdioEntries[c.name]
	out := make([]clients.StdioEntry, 0, len(raw))
	for _, entry := range raw {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (c *fakeClient) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	raw := c.parent.stdioEntries[c.name]
	out := make([]clients.LanguageServerStdioEntry, 0, len(raw))
	for _, entry := range raw {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func countEntries(fc *fakeClientsMap) int {
	n := 0
	for _, m := range fc.entries {
		n += len(m)
	}
	return n
}

func TestVerifyReadinessServerInfo_AllowsSerenaJSONAndSSE(t *testing.T) {
	allowed := map[string]struct{}{"serena": {}}
	jsonBody := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"serena","version":"1"},"capabilities":{}}}`)
	if err := verifyReadinessServerInfo(jsonBody, allowed); err != nil {
		t.Fatalf("JSON serena readiness rejected: %v", err)
	}

	sseBody := []byte(": ping\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"serverInfo\":{\"name\":\"Serena\"}}}\n\n")
	if err := verifyReadinessServerInfo(sseBody, allowed); err != nil {
		t.Fatalf("SSE serena readiness rejected: %v", err)
	}
}

func TestVerifyReadinessServerInfo_MultiEventSSESelectsResponseEvent(t *testing.T) {
	allowed := map[string]struct{}{"serena": {}}
	progress := "event: progress\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n"

	body := []byte(progress +
		"event: response\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"serverInfo\":{\"name\":\"serena\"}}}\n\n")
	if err := verifyReadinessServerInfo(body, allowed); err != nil {
		t.Fatalf("multi-event SSE readiness result rejected: %v", err)
	}

	wrongName := []byte(progress +
		"event: response\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"serverInfo\":{\"name\":\"not-serena\"}}}\n\n")
	err := verifyReadinessServerInfo(wrongName, allowed)
	if err == nil {
		t.Fatal("multi-event SSE readiness result with wrong name must be rejected")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("wrong-name SSE should reach serverInfo validation, got: %v", err)
	}
}

func TestVerifyReadinessServerInfo_RejectsWrongOrMissingServerName(t *testing.T) {
	allowed := map[string]struct{}{"serena": {}}
	for name, body := range map[string][]byte{
		"wrong":   []byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"not-serena"}}}`),
		"missing": []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyReadinessServerInfo(body, allowed); err == nil {
				t.Fatal("readiness without serena serverInfo.name must be rejected")
			}
		})
	}
}

func TestVerifyProxyReadyForServerNames_AcceptsJSONFiniteAndHeldOpenSSE(t *testing.T) {
	allowed := map[string]struct{}{"serena": {}}
	jsonResult := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"serena","version":"1"},"capabilities":{}}}`
	sseResult := "event: response\n" +
		"data: " + jsonResult + "\n\n"

	cases := map[string]struct {
		contentType string
		body        string
		holdOpen    bool
	}{
		"plain-json":    {contentType: "application/json", body: jsonResult},
		"finite-sse":    {contentType: "text/event-stream", body: sseResult},
		"held-open-sse": {contentType: "text/event-stream", body: sseResult, holdOpen: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var release func()
			hold := make(chan struct{})
			if tc.holdOpen {
				release = func() {
					select {
					case <-hold:
					default:
						close(hold)
					}
				}
				t.Cleanup(release)
			}
			_, port := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				if tc.holdOpen {
					select {
					case <-hold:
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
				}
			})

			err := verifyReadinessPromptly(t, port, 3*time.Second, allowed, release)
			if err != nil {
				t.Fatalf("readiness probe rejected %s response: %v", name, err)
			}
		})
	}
}

func TestVerifyProxyReady_GenericAllowlistDoesNotReadHeldOpenBody(t *testing.T) {
	hold := make(chan struct{})
	release := func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}
	t.Cleanup(release)

	_, port := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})

	if err := verifyReadinessPromptly(t, port, 3*time.Second, nil, release); err != nil {
		t.Fatalf("generic readiness probe with nil allowlist must accept HTTP 200 without body read: %v", err)
	}
}

func newReadinessHTTPTestServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on ephemeral loopback port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if port >= 9121 && port <= 9299 {
		_ = ln.Close()
		t.Fatalf("httptest allocated live mcphub port %d", port)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	portText := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	parsedPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse httptest port from %q: %v", srv.URL, err)
	}
	if parsedPort != port {
		t.Fatalf("httptest URL port %d != listener port %d", parsedPort, port)
	}
	return srv, port
}

func verifyReadinessPromptly(t *testing.T, port int, timeout time.Duration, allowed map[string]struct{}, release func()) error {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- verifyProxyReadyForServerNames(port, timeout, allowed)
	}()
	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed > 750*time.Millisecond {
			t.Fatalf("readiness probe returned after %s; expected prompt return before stream timeout", elapsed)
		}
		return err
	case <-time.After(750 * time.Millisecond):
		if release != nil {
			release()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		t.Fatalf("readiness probe blocked on response body; expected prompt return")
		return nil
	}
}
