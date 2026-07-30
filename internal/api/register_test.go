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
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// TestDefaultClientBindings_DerivedFromDefaultInstallSet pins the register-time
// implicit workspace binding set (used when a manifest declares no
// client_bindings — effectiveClientBindings, consumed by registerOneLanguage
// and registerOneLanguageSupervised) to the
// install-time default-install set. A bare `mcphub register` of a
// workspace-scoped manifest with no client_bindings must bind exactly the
// default-install clients (claude-code, codex-cli), never the opt-in cursor —
// the same invariant `mcphub install` enforces via DefaultInstallClientNames.
// Deriving the bindings from clients.DefaultInstallClientNames() is what keeps
// the two paths in lockstep; this test fails if cursor (or any opt-in client)
// leaks back into the register-time default.
//
// LOCALAPPDATA is redirected to an empty temp dir so SettingsPath() resolves to
// a NON-EXISTENT gui-preferences.yaml: this test pins the NO-OVERRIDE
// (compile-time) branch of defaultClientBindingsNow, and must never read — or
// depend on the contents of — the operator's real preferences file. The
// override branch is pinned separately by
// TestRegisterBindings_HonorPersistedDefaultClientsOverride.
func TestDefaultClientBindings_DerivedFromDefaultInstallSet(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpState)
	t.Setenv("XDG_DATA_HOME", tmpState)

	got := map[string]string{} // client -> URLPath
	bindings, droppedRelayStdio := defaultClientBindingsNow()
	// No-override branch: the compile-time set is {claude-code, codex-cli}, both
	// URL-native, so nothing may be dropped and no warning may fire.
	if len(droppedRelayStdio) != 0 {
		t.Fatalf("no-override branch dropped relay-stdio clients %v; the compile-time default set is URL-native", droppedRelayStdio)
	}
	for _, b := range bindings {
		if _, dup := got[b.Client]; dup {
			t.Fatalf("duplicate client %q in default bindings", b.Client)
		}
		got[b.Client] = b.URLPath
	}

	// It must equal the default-install set minus relay-stdio adapters (which
	// cannot take a URL-only workspace binding). This asserts the derivation is
	// faithful — not a hand-maintained parallel list that can drift.
	want := map[string]bool{}
	for _, name := range clients.DefaultInstallClientNames() {
		if clients.IsRelayStdio(name) {
			continue
		}
		want[name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("default bindings clients = %v, want (default-install minus relay-stdio) = %v", got, want)
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("default-install client %q missing from default bindings: %v", name, got)
		}
	}

	// The invariant the operator asked for: cursor is opt-in, so the register-
	// time default must NOT bind it.
	if _, leaked := got["cursor"]; leaked {
		t.Fatalf("cursor is opt-in and must not be in the register-time default bindings: %v", got)
	}
	// Concrete pin for today's default set.
	for _, name := range []string{"claude-code", "codex-cli"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("default client %q missing from default bindings: %v", name, got)
		}
	}
	// Every binding carries the standard /mcp URL path (register writes URL
	// entries, so a blank/other path would break the workspace binding).
	for client, urlPath := range got {
		if urlPath != "/mcp" {
			t.Fatalf("client %q default binding URLPath = %q, want /mcp", client, urlPath)
		}
	}
}

func TestRegister_NoLegacyManagedRouterPolicyOwners(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve register_test.go path")
	}
	files := []string{
		filepath.Join(filepath.Dir(currentFile), "register.go"),
		currentFile,
	}
	banned := []string{
		"Managed" + "GUIIdentity",
		"probeManaged" + "Router",
		"cleanupDirectLanguageServerEntriesAfterRegister" + "WithDeps",
		"directCleanup" + "Deps",
		"RegisterOpts." + "GUIPort",
		"managedLanguageRoute" + "ProbeFn",
	}
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, symbol := range banned {
			if bytes.Contains(content, []byte(symbol)) {
				t.Errorf("legacy managed-router policy owner %q remains in %s", symbol, filepath.Base(path))
			}
		}
	}
}

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

// effectiveBoundClientsForTest is the test-side shorthand for "which clients
// would THIS manifest's registration write to". Production resolves this once
// per register and threads it down; tests only ever need the name set, so this
// drops the relay-stdio diagnostic the production caller turns into a warning.
func effectiveBoundClientsForTest(m *config.ServerManifest) map[string]bool {
	bindings, _ := effectiveClientBindings(m)
	return boundClientNames(bindings)
}

func newRegisterHarness(t *testing.T) *registerHarness {
	t.Helper()
	dir := hardenedTempDir(t)
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

	// Redirect SettingsPath() to an empty temp dir. The register path resolves
	// its implicit client bindings through defaultClientBindingsNow, which reads
	// the operator's persisted `clients.default_install` from
	// gui-preferences.yaml at CALL TIME. Without this redirect every harness
	// test would read the developer's REAL %LOCALAPPDATA% preferences file, so a
	// machine that happens to have an override persisted would silently change
	// which clients these tests expect — a host-dependent result masquerading as
	// a pass. An absent file means "no override" and yields the compile-time
	// default set, which is what the harness tests assume. Tests that WANT an
	// override write one into this temp dir via SetDefaultInstallClientNames.
	settingsRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", settingsRoot)
	t.Setenv("XDG_DATA_HOME", settingsRoot)

	origSchedulerNew := testSchedulerFactory
	origClientFactory := testClientFactory
	origRegistryPath := testRegistryPathOverride
	origReadiness := proxyReadinessFn
	origCanonical := testCanonicalMcphubPathOverride
	origBless := registerBlessTrustedRootFn
	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	origPortAvailable := portAvailable
	origExcludedTCPPortRanges := excludedTCPPortRanges

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
	killByPortFn = func(port int, timeout time.Duration) error { return nil }
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		return portKillNoListener, nil
	}
	// Register harness tests exercise registry/scheduler/client wiring, not
	// the host's live TCP occupancy. Keep the allocator hermetic so a running
	// developer fleet on the LSP workspace pool cannot exhaust the test pool.
	portAvailable = func(int) bool { return true }
	excludedTCPPortRanges = func() ([]tcpPortRange, error) { return nil, nil }
	fc := &fakeClientsMap{
		entries:         map[string]map[string]string{},
		disabledEntries: map[string]map[string]bool{},
		stdioEntries:    map[string]map[string]clients.LanguageServerStdioEntry{},
		allStdioEntries: map[string]map[string]clients.StdioEntry{},
		backupKeepCalls: map[string]int{},
		exists:          map[string]bool{},
		failGetEntry:    map[string]bool{},
	}
	// Pre-populate the default HTTP clients so Exists() returns true in tests.
	for _, n := range []string{"claude-code", "codex-cli", "cursor"} {
		fc.entries[n] = map[string]string{}
		fc.disabledEntries[n] = map[string]bool{}
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
			killByPortFn = origKill
			forceKillByPortFn = origForceKill
			portAvailable = origPortAvailable
			excludedTCPPortRanges = origExcludedTCPPortRanges
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
		PortPool:  &config.PortPool{Start: 9400, End: 9599},
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

type testManagedRouterLease struct {
	revalidate func(context.Context) string
	close      func() error
}

func (l *testManagedRouterLease) Revalidate(ctx context.Context) string {
	if l == nil || l.revalidate == nil {
		return ""
	}
	return l.revalidate(ctx)
}

func (l *testManagedRouterLease) Close() error {
	if l == nil || l.close == nil {
		return nil
	}
	return l.close()
}

func stableManagedRouterLeaseForTest() ManagedRouterLease {
	return &testManagedRouterLease{}
}

func allowManagedRouterAuthorizerForTest(port int) ManagedRouterAuthorizer {
	return func(_ context.Context, candidatePort int) ManagedRouterAuthorization {
		if candidatePort != port {
			return ManagedRouterAuthorization{FailureClass: "test-port-mismatch"}
		}
		return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
	}
}

func allowManagedLanguageRouteForTest(context.Context, int, string, string) managedRouteProof {
	return managedRouteProof{OK: true}
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

// TestRegister_GetEntrySnapshotErrorAbortsWithoutDeletingPrior pins bot PR #420
// finding 1 (HIGH, data-loss): when the prior-entry snapshot GetEntry returns an
// error (a multi-layer adapter like mimocode can confirm a write-target prior yet
// fail reading a malformed lower layer, returning (nil, err)), the register MUST
// abort BEFORE BackupKeep/AddEntry and MUST NOT delete the operator's prior
// entry. Before this fix the error was dropped → prior treated as nil → AddEntry
// overwrote → the nil-prior rollback branch RemoveEntry-d and DELETED the entry.
func TestRegister_GetEntrySnapshotErrorAbortsWithoutDeletingPrior(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	killByPortFn = func(port int, _ time.Duration) error { return nil }
	ws := t.TempDir()
	m := nineLanguageManifest()
	a := mustNewAPI(t)
	// First register succeeds and seeds a prior entry on every client.
	if _, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	priorBefore := countEntries(h.fakeClients)
	if priorBefore == 0 {
		t.Fatal("first register seeded no client entries")
	}
	// Snapshot the per-client entry maps so we can assert NONE were deleted /
	// corrupted across the aborted-and-rolled-back register.
	entriesBefore := map[string]map[string]string{}
	for c, m := range h.fakeClients.entries {
		cp := map[string]string{}
		for k, v := range m {
			cp[k] = v
		}
		entriesBefore[c] = cp
	}
	// BackupKeep on the failing client must NOT run — its snapshot aborts before
	// the backup. (Order-independent: codex-cli's own loop iteration aborts at
	// the GetEntry guard, before reaching its BackupKeep.)
	codexBackupBefore := h.fakeClients.backupKeepCalls["codex-cli"]

	// Force the prior-entry snapshot to error on codex-cli for the next register.
	h.fakeClients.failGetEntry["codex-cli"] = true
	_, err := a.registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("expected register to abort on the GetEntry snapshot error")
	}

	// DATA-LOSS GUARD: the operator's prior entries must ALL survive. The failing
	// client's snapshot aborts before AddEntry (no overwrite), and any client
	// whose AddEntry ran earlier in the loop is restored by the rollback — so the
	// net entry data must be byte-identical to before. Crucially, NO entry is
	// deleted by a nil-prior RemoveEntry branch (the pre-fix data-loss path).
	for c, want := range entriesBefore {
		for name, url := range want {
			got, ok := h.fakeClients.entries[c][name]
			if !ok {
				t.Errorf("prior entry %s/%s DELETED by rollback after a GetEntry snapshot error (data loss)", c, name)
				continue
			}
			if got != url {
				t.Errorf("prior entry %s/%s overwritten/not-restored: got %q want %q", c, name, got, url)
			}
		}
	}
	if n := countEntries(h.fakeClients); n != priorBefore {
		t.Errorf("client entry count changed across the aborted register: before=%d after=%d", priorBefore, n)
	}
	// The failing client's snapshot error aborts BEFORE its BackupKeep.
	if h.fakeClients.backupKeepCalls["codex-cli"] != codexBackupBefore {
		t.Errorf("BackupKeep ran on codex-cli after its GetEntry snapshot error: before=%d after=%d",
			codexBackupBefore, h.fakeClients.backupKeepCalls["codex-cli"])
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
		if p < 9400 || p > 9599 {
			t.Errorf("killed port %d outside workspace pool 9400-9599", p)
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
	m.PortPool = &config.PortPool{Start: 9400, End: 9400}
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

// TestRegister_SupervisedHonorsPersistedDefaultClientsOverride pins the
// SUPERVISED register lane to the same client-scope owner the legacy lane uses.
//
// registerOneLanguageSupervised carried a hand-rolled copy of the owner:
//
//	bindingsPre := m.ClientBindings
//	if len(bindingsPre) == 0 { bindingsPre = defaultClientBindings }
//
// byte-for-byte what effectiveClientBindings owns. It was harmless while the
// owner was a frozen compile-time snapshot, so the duplicate and the owner
// happened to agree. The moment the owner learned to read the operator's
// persisted clients.default_install, the copy did NOT — and the two lanes
// diverged in the most damaging possible way: the supervised WRITE path would
// bind the compile-time set while the cleanup gate
// (boundClientNames of the once-resolved bindings, which both lanes share)
// computed the override set. A client in the override but not the compile-time
// set would then be treated as "bound" by cleanup — its direct entry deleted —
// while the write path never actually wrote it a replacement. That is exactly
// the write-vs-cleanup divergence this branch set out to close, one lane over.
//
// The override ADDS opt-in cursor and DROPS codex-cli, so only a genuinely
// applied override yields the asserted set.
func TestRegister_SupervisedHonorsPersistedDefaultClientsOverride(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	// newRegisterHarness redirects SettingsPath() into a temp dir, so this
	// persists the override there — never into the operator's real file.
	if err := mustNewAPI(t).SetDefaultInstallClientNames([]string{"claude-code", "cursor"}); err != nil {
		t.Fatalf("persist default-install override: %v", err)
	}

	m := nineLanguageManifest()
	m.ClientBindings = nil // shipped manifest declares none → derived defaults

	ws := t.TempDir()
	report, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err != nil {
		t.Fatalf("Register supervised: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	got := report.Entries[0].ClientEntries

	if _, ok := got["cursor"]; !ok {
		t.Errorf("supervised register wrote NO entry for cursor, which the operator's persisted "+
			"clients.default_install selects — the supervised lane is resolving its bindings from a "+
			"hand-rolled copy of the compile-time default instead of effectiveClientBindings, so the "+
			"cleanup gate (which DOES read the owner) will treat cursor as bound and delete its direct "+
			"entry with no replacement; ClientEntries=%v", got)
	}
	if _, ok := got["codex-cli"]; ok {
		t.Errorf("supervised register wrote an entry for codex-cli, which the operator's persisted "+
			"clients.default_install EXCLUDES; ClientEntries=%v", got)
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

func TestRegister_SupervisedLiveLegacyKillFailureRunsPrearmedRestore(t *testing.T) {
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
	if killCalls != 2 {
		t.Fatalf("kill calls = %d, want destructive attempt plus prearmed restore cleanup", killCalls)
	}
	if len(h.fakeSch.deleteNames) != 0 {
		t.Fatalf("legacy task was deleted after kill failure: %v", h.fakeSch.deleteNames)
	}
	if !slices.Contains(h.fakeSch.importNames, taskName) || !slices.Contains(h.fakeSch.runNames, taskName) {
		t.Fatalf("legacy task prearmed restore did not run after possibly-partial kill failure: import=%v run=%v",
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

func TestRegister_SupervisedLiveNoXMLKillFailureRunsPrearmedStateRollback(t *testing.T) {
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
	if killCalls != 2 {
		t.Fatalf("kill calls = %d, want destructive attempt plus prearmed possible-proxy cleanup", killCalls)
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
		PortPool: &config.PortPool{Start: 9400, End: 9599},
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
	if runtime.GOOS == "windows" {
		var diagnostics []string
		for _, attempt := range []struct {
			label string
			flag  string
		}{
			{label: "directory symlink", flag: "/D"},
			{label: "junction", flag: "/J"},
		} {
			alias := filepath.Join(t.TempDir(), "alias")
			out, err := exec.Command("cmd", "/c", "mklink", attempt.flag, alias, realDir).CombinedOutput()
			if err != nil {
				diagnostics = append(diagnostics, attempt.label+" failed: "+err.Error()+" output="+strings.TrimSpace(string(out)))
				continue
			}
			canonical, legacy, distinct := mixedKeyWorkspacePathsForUnregister(t, alias)
			if distinct {
				return alias, canonical, legacy
			}
			diagnostics = append(diagnostics, attempt.label+" collapsed keys: canonical="+canonical+" legacy="+legacy)
		}
		t.Fatalf("mixed-key fixture did not produce distinct keys on Windows: %s", strings.Join(diagnostics, "; "))
	}

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatalf("create temp symlink: %v", err)
	}
	canonical, legacy, distinct := mixedKeyWorkspacePathsForUnregister(t, alias)
	if !distinct {
		t.Fatalf("mixed-key fixture did not produce distinct keys: canonical=%q legacy=%q", canonical, legacy)
	}
	return alias, canonical, legacy
}

func mixedKeyWorkspacePathsForUnregister(t *testing.T, alias string) (string, string, bool) {
	t.Helper()
	canonical, err := CanonicalWorkspacePathForCleanup(alias)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathForCleanup(alias): %v", err)
	}
	legacy, err := CanonicalWorkspacePathLegacyCompat(alias)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathLegacyCompat(alias): %v", err)
	}
	return canonical, legacy, WorkspaceKey(canonical) != WorkspaceKey(legacy)
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
	disabledEntries   map[string]map[string]bool
	stdioEntries      map[string]map[string]clients.LanguageServerStdioEntry
	allStdioEntries   map[string]map[string]clients.StdioEntry
	backupKeepCalls   map[string]int
	backupStdio       map[string]map[string]clients.LanguageServerStdioEntry
	exists            map[string]bool
	addEntryCount     int
	failAddEntryCalls int             // the Nth AddEntry (1-based) fails
	failGetEntry      map[string]bool // client-name -> GetEntry returns an error (finding 1)
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
	backupPath := fmt.Sprintf("/backup/%s/%d", c.name, keepN)
	if c.parent.backupStdio == nil {
		c.parent.backupStdio = map[string]map[string]clients.LanguageServerStdioEntry{}
	}
	snapshot := make(map[string]clients.LanguageServerStdioEntry, len(c.parent.stdioEntries[c.name]))
	for name, entry := range c.parent.stdioEntries[c.name] {
		snapshot[name] = entry
	}
	c.parent.backupStdio[backupPath] = snapshot
	return backupPath, nil
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
func (c *fakeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	snapshot := c.parent.backupStdio[backupPath]
	entry, ok := snapshot[name]
	if !ok {
		return fmt.Errorf("fake backup %s has no entry %s", backupPath, name)
	}
	c.parent.stdioEntries[c.name][name] = entry
	return nil
}
func (c *fakeClient) GetEntry(name string) (*clients.MCPEntry, error) {
	// Finding 1 regression seam: simulate a multi-layer adapter (mimocode)
	// that confirms a write-target prior yet errors reading a malformed lower
	// layer, returning (nil, err). The register/install snapshot loops must
	// propagate this error and abort BEFORE AddEntry, never delete the prior.
	if c.parent.failGetEntry != nil && c.parent.failGetEntry[c.name] {
		return nil, fmt.Errorf("fake client %s: induced GetEntry failure (malformed lower layer)", c.name)
	}
	url, ok := c.parent.entries[c.name][name]
	if !ok {
		return nil, nil
	}
	return &clients.MCPEntry{Name: name, URL: url, Disabled: c.parent.disabledEntries[c.name][name]}, nil
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

type directCleanupInjectedErrorClient struct {
	registerClient
	findErr       error
	survivorErr   error
	findCalls     int
	failFindAfter int
	findAfterErr  error
}

type cleanupMutationInjectedClient struct {
	registerClient
	backupErr     error
	removeErrName string
	removeErr     error
}

type cleanupMutationLedgerClient struct {
	registerClient
	events *[]string
}

func (c *cleanupMutationLedgerClient) BackupKeep(keepN int) (string, error) {
	*c.events = append(*c.events, "backup")
	return c.registerClient.BackupKeep(keepN)
}

func (c *cleanupMutationLedgerClient) RemoveEntry(name string) error {
	*c.events = append(*c.events, "remove:"+name)
	return c.registerClient.RemoveEntry(name)
}

func (c *cleanupMutationInjectedClient) BackupKeep(keepN int) (string, error) {
	if c.backupErr != nil {
		return "", c.backupErr
	}
	return c.registerClient.BackupKeep(keepN)
}

func (c *cleanupMutationInjectedClient) RemoveEntry(name string) error {
	if name == c.removeErrName {
		if c.removeErr != nil {
			return c.removeErr
		}
		return errors.New("injected removal failure")
	}
	return c.registerClient.RemoveEntry(name)
}

func (c *cleanupMutationInjectedClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	restorer, ok := c.registerClient.(registerClientRollbackRestorer)
	if !ok {
		return errors.New("wrapped client does not support exact rollback restore")
	}
	return restorer.RestoreEntryFromBackupForRollback(backupPath, name)
}

func (c *cleanupMutationLedgerClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	restorer, ok := c.registerClient.(registerClientRollbackRestorer)
	if !ok {
		return errors.New("wrapped client does not support exact rollback restore")
	}
	return restorer.RestoreEntryFromBackupForRollback(backupPath, name)
}

func (c *directCleanupInjectedErrorClient) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	c.findCalls++
	if c.findErr != nil {
		return nil, c.findErr
	}
	if c.failFindAfter > 0 && c.findCalls > c.failFindAfter {
		return nil, c.findAfterErr
	}
	return c.registerClient.FindStdioLanguageServerEntries()
}

func (c *directCleanupInjectedErrorClient) ActiveStdioEntriesExcludingWriteTarget(name string) ([]clients.StdioEntry, error) {
	if c.survivorErr != nil {
		return nil, c.survivorErr
	}
	if active, ok := c.registerClient.(activeStdioExcludingClient); ok {
		return active.ActiveStdioEntriesExcludingWriteTarget(name)
	}
	return nil, nil
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
	if (port >= 9121 && port <= 9299) || (port >= 9400 && port <= 9599) {
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

func assertSingleRouterProofWarning(t *testing.T, report *RegisterReport, writer string, failureClass string) {
	t.Helper()
	got := 0
	for _, warning := range report.Warnings {
		if strings.HasPrefix(warning, failureClass+":") {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("router-proof warning count in report = %d, want 1; warnings=%v", got, report.Warnings)
	}
	if got := strings.Count(writer, "warning: "+failureClass+":"); got != 1 {
		t.Fatalf("router-proof warning count in progress output = %d, want 1; output=%q", got, writer)
	}
}

func assertNoRouterProofWarning(t *testing.T, report *RegisterReport, writer string) {
	t.Helper()
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "keeping matching direct LSP entries") {
			t.Fatalf("cleanup with no router-origin direct candidate emitted router-proof warning %q", warning)
		}
	}
	if strings.Contains(writer, "keeping matching direct LSP entries") {
		t.Fatalf("cleanup with no router-origin direct candidate wrote router-proof warning: %q", writer)
	}
}

func goCleanupSpecForTest(t *testing.T) map[string]config.LanguageSpec {
	t.Helper()
	for _, spec := range nineLanguageManifest().Languages {
		if spec.Name == "go" {
			return map[string]config.LanguageSpec{"go": spec}
		}
	}
	t.Fatal("nine-language manifest has no go specification")
	return nil
}

func assertCleanupWarningCount(t *testing.T, warnings []string, writer, want string, reportCount, writerCount int) {
	t.Helper()
	got := 0
	for _, warning := range warnings {
		if warning == want {
			got++
		}
	}
	if got != reportCount {
		t.Fatalf("warning %q count in report contribution = %d, want %d; warnings=%v", want, got, reportCount, warnings)
	}
	if got := strings.Count(writer, want); got != writerCount {
		t.Fatalf("warning %q count in progress output = %d, want %d; output=%q", want, got, writerCount, writer)
	}
}

func TestCleanupDirectLSP_AuthorizerRefusalWarnsOnceAndDoesNotMutate(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const observedPort = 19125
	canonical := mustCanonical(t, t.TempDir())
	direct := func(name string) clients.LanguageServerStdioEntry {
		return clients.LanguageServerStdioEntry{
			Name:     name,
			Command:  "mcp-language-server",
			Language: "gopls",
			Args:     []string{"--lsp", "gopls", "--workspace", canonical},
		}
	}
	const (
		boundEntry   = "legacy-go-bound-port-error"
		unboundEntry = "legacy-go-unbound-port-error"
	)
	h.fakeClients.stdioEntries["claude-code"][boundEntry] = direct(boundEntry)
	h.fakeClients.stdioEntries["cursor"][unboundEntry] = direct(unboundEntry)
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", observedPort)

	a := mustNewAPI(t)
	var authorizerCalls, routeCalls int
	var writer bytes.Buffer
	warnings := a.cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		map[string]registerClient{
			"claude-code": &fakeClient{parent: h.fakeClients, name: "claude-code"},
			"cursor":      &fakeClient{parent: h.fakeClients, name: "cursor"},
		},
		map[clientLanguageKey]bool{{Client: "claude-code", Language: "go"}: true},
		[]clientWriteReceipt{{Key: clientLanguageKey{Client: "claude-code", Language: "go"}, EntryName: boundEntry}},
		&writer,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{FailureClass: "pidport-unavailable"}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				return managedRouteProof{OK: true}
			},
			matchDirect: directLanguageServerCleanupMatches,
		},
	)

	wantWarning := "pidport-unavailable: affected_plans=1 [client=cursor,language=go,port=19125]; keeping matching direct LSP entries"
	assertCleanupWarningCount(t, warnings, writer.String(), wantWarning, 1, 1)
	if authorizerCalls != 1 || routeCalls != 0 {
		t.Fatalf("authorizer refusal calls: authorizer=%d route=%d, want 1/0", authorizerCalls, routeCalls)
	}
	if _, stillThere := h.fakeClients.stdioEntries["claude-code"][boundEntry]; stillThere {
		t.Fatalf("receipt-backed entry %q was suppressed by unrelated router failure", boundEntry)
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"][unboundEntry]; !stillThere {
		t.Fatalf("port-resolution failure removed router-derived entry %q", unboundEntry)
	}
	if got := h.fakeClients.backupKeepCalls["claude-code"]; got != 1 {
		t.Fatalf("receipt-backed client backups = %d, want 1", got)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("port-resolution failure made %d cursor backup(s), want 0", got)
	}
}

func TestCleanupDirectLSP_ProcessOwnerFailureDoesNotMutate(t *testing.T) {
	for _, failureClass := range []string{
		ManagedRouterFailureProcessOwnerMismatch,
		ManagedRouterFailureProcessOwnerUnavailable,
	} {
		t.Run(failureClass, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			canonical := mustCanonical(t, t.TempDir())
			const (
				port      = 19133
				entryName = "legacy-go-owner-refusal"
			)
			h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
				fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
			h.fakeClients.stdioEntries["cursor"][entryName] = clients.LanguageServerStdioEntry{
				Name: entryName, Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}
			routeCalls := 0
			var mutationEvents []string
			client := &cleanupMutationLedgerClient{
				registerClient: testClientFactory()["cursor"],
				events:         &mutationEvents,
			}
			var writer bytes.Buffer
			warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
				goCleanupSpecForTest(t), []string{"go"}, canonical,
				map[string]registerClient{"cursor": client},
				nil, nil, &writer,
				directCleanupPlanDeps{
					authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
						return ManagedRouterAuthorization{FailureClass: failureClass}
					},
					probeRoute: func(context.Context, int, string, string) managedRouteProof {
						routeCalls++
						return managedRouteProof{OK: true}
					},
					matchDirect: directLanguageServerCleanupMatches,
				},
			)
			if routeCalls != 0 || len(mutationEvents) != 0 || h.fakeClients.backupKeepCalls["cursor"] != 0 {
				t.Fatalf("route/mutation/backup = %d/%v/%d, want 0/[]/0", routeCalls, mutationEvents, h.fakeClients.backupKeepCalls["cursor"])
			}
			if _, exists := h.fakeClients.stdioEntries["cursor"][entryName]; !exists {
				t.Fatal("owner refusal removed the direct entry")
			}
			if len(warnings) != 1 || !strings.HasPrefix(warnings[0], failureClass+":") {
				t.Fatalf("warnings=%v, want one %s warning", warnings, failureClass)
			}
		})
	}
}

func TestCleanupDirectLSP_InvalidReceiptIsWireSafeAndDoesNotMutate(t *testing.T) {
	const rawReceiptSentinel = `C:\secret\receipt password=hunter2`
	key := clientLanguageKey{Client: "cursor", Language: "go"}
	for _, tc := range []struct {
		name       string
		receipts   []clientWriteReceipt
		wantPrefix string
	}{
		{
			name: "empty entry",
			receipts: []clientWriteReceipt{
				{Key: key, EntryName: ""},
			},
			wantPrefix: "write-entry-name-empty:",
		},
		{
			name: "conflicting entries",
			receipts: []clientWriteReceipt{
				{Key: key, EntryName: "router-go"},
				{Key: key, EntryName: rawReceiptSentinel},
			},
			wantPrefix: "write-receipt-conflict:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			canonical := mustCanonical(t, t.TempDir())
			const entryName = "legacy-go-invalid-receipt"
			h.fakeClients.stdioEntries["cursor"][entryName] = clients.LanguageServerStdioEntry{
				Name: entryName, Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}
			var mutationEvents []string
			client := &cleanupMutationLedgerClient{
				registerClient: testClientFactory()["cursor"],
				events:         &mutationEvents,
			}
			var writer bytes.Buffer
			warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
				goCleanupSpecForTest(t), []string{"go"}, canonical,
				map[string]registerClient{"cursor": client}, map[clientLanguageKey]bool{key: true},
				tc.receipts, &writer, directCleanupPlanDeps{matchDirect: directLanguageServerCleanupMatches},
			)
			if len(mutationEvents) != 0 || h.fakeClients.backupKeepCalls["cursor"] != 0 {
				t.Fatalf("invalid receipt mutation=%v backup=%d, want none", mutationEvents, h.fakeClients.backupKeepCalls["cursor"])
			}
			if _, exists := h.fakeClients.stdioEntries["cursor"][entryName]; !exists {
				t.Fatal("invalid receipt removed direct entry")
			}
			public := strings.Join(warnings, "\n") + "\n" + writer.String()
			if len(warnings) != 1 || !strings.HasPrefix(warnings[0], tc.wantPrefix) ||
				strings.Contains(public, rawReceiptSentinel) || strings.Contains(public, "hunter2") {
				t.Fatalf("unsafe invalid-receipt warning: warnings=%v writer=%q", warnings, writer.String())
			}
		})
	}
}

func TestCleanupDirectLSP_GetEntryErrorIsReturnedOnceBeforeAnyMutation(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const routerPort = 19126
	canonical := mustCanonical(t, t.TempDir())
	const directEntry = "legacy-go-get-entry-error"
	h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
		Name:     directEntry,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", canonical},
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)
	h.fakeClients.failGetEntry["cursor"] = true

	var authorizerCalls, routeCalls, matcherCalls int
	var writer bytes.Buffer
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		map[string]registerClient{
			"cursor": &fakeClient{parent: h.fakeClients, name: "cursor"},
		},
		nil,
		nil,
		&writer,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				return managedRouteProof{OK: true}
			},
			matchDirect: func(client registerClient, clientName string, aliases map[string]bool, workspace string) directCleanupMatchResult {
				matcherCalls++
				return directLanguageServerCleanupMatches(client, clientName, aliases, workspace)
			},
		},
	)

	want := "client-entry-indeterminate: client=cursor,language=go; direct LSP cleanup skipped and matching entries were kept; verify client configuration is readable and retry"
	assertCleanupWarningCount(t, warnings, writer.String(), want, 1, 0)
	if authorizerCalls != 0 || routeCalls != 0 || matcherCalls != 1 {
		t.Fatalf("GetEntry failure calls: authorizer=%d route=%d matcher=%d, want 0/0/1",
			authorizerCalls, routeCalls, matcherCalls)
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"][directEntry]; !stillThere {
		t.Fatalf("GetEntry failure removed direct entry %q", directEntry)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("GetEntry failure made %d backup(s), want 0", got)
	}
}

func TestCleanupDirectLSP_DirectScanErrorIsReturnedOnceBeforeAnySideEffect(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*directCleanupInjectedErrorClient)
		want      string
	}{
		{
			name: "candidate-scan",
			want: "direct-scan-failed: client=cursor; direct LSP cleanup skipped and matching entries were kept; verify client configuration is readable and retry",
			configure: func(client *directCleanupInjectedErrorClient) {
				client.findErr = errors.New("candidate scan sentinel")
			},
		},
		{
			name: "survivor-scan",
			want: "survivor-scan-failed: client=cursor; direct LSP cleanup skipped and matching entries were kept; verify client configuration is readable and retry",
			configure: func(client *directCleanupInjectedErrorClient) {
				client.survivorErr = errors.New("survivor scan sentinel")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()

			const routerPort = 19127
			canonical := mustCanonical(t, t.TempDir())
			const directEntry = "legacy-go-direct-scan-error"
			h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
				Name:     directEntry,
				Command:  "mcp-language-server",
				Language: "gopls",
				Args:     []string{"--lsp", "gopls", "--workspace", canonical},
			}
			h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
				fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)
			client := &directCleanupInjectedErrorClient{
				registerClient: &fakeClient{parent: h.fakeClients, name: "cursor"},
			}
			tc.configure(client)

			var authorizerCalls, routeCalls, matcherCalls int
			var writer bytes.Buffer
			warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
				goCleanupSpecForTest(t),
				[]string{"go"},
				canonical,
				map[string]registerClient{"cursor": client},
				nil,
				nil,
				&writer,
				directCleanupPlanDeps{
					authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
						authorizerCalls++
						return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
					},
					probeRoute: func(context.Context, int, string, string) managedRouteProof {
						routeCalls++
						return managedRouteProof{OK: true}
					},
					matchDirect: func(client registerClient, clientName string, aliases map[string]bool, workspace string) directCleanupMatchResult {
						matcherCalls++
						return directLanguageServerCleanupMatches(client, clientName, aliases, workspace)
					},
				},
			)

			assertCleanupWarningCount(t, warnings, writer.String(), tc.want, 1, 0)
			if authorizerCalls != 0 || routeCalls != 0 || matcherCalls != 1 {
				t.Fatalf("%s failure calls: authorizer=%d route=%d matcher=%d, want 0/0/1",
					tc.name, authorizerCalls, routeCalls, matcherCalls)
			}
			if _, stillThere := h.fakeClients.stdioEntries["cursor"][directEntry]; !stillThere {
				t.Fatalf("%s failure removed direct entry %q", tc.name, directEntry)
			}
			if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
				t.Fatalf("%s failure made %d backup(s), want 0", tc.name, got)
			}
		})
	}
}

func TestCleanupDirectLSP_StaleRouterPortIsRefusedBeforeRouteProbe(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const (
		observedPort = 19128
	)
	canonical := mustCanonical(t, t.TempDir())
	const directEntry = "legacy-go-stale-structural-port"
	h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
		Name:     directEntry,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", canonical},
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", observedPort)

	var authorizerCalls, routeCalls, matcherCalls int
	var writer bytes.Buffer
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		map[string]registerClient{
			"cursor": &fakeClient{parent: h.fakeClients, name: "cursor"},
		},
		nil,
		nil,
		&writer,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{FailureClass: "pidport-port-mismatch"}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				return managedRouteProof{OK: true}
			},
			matchDirect: func(client registerClient, clientName string, aliases map[string]bool, workspace string) directCleanupMatchResult {
				matcherCalls++
				return directLanguageServerCleanupMatches(client, clientName, aliases, workspace)
			},
		},
	)

	if authorizerCalls != 1 || routeCalls != 0 || matcherCalls != 1 {
		t.Fatalf("stale router calls: authorizer=%d route=%d matcher=%d, want 1/0/1",
			authorizerCalls, routeCalls, matcherCalls)
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"][directEntry]; !stillThere {
		t.Fatalf("stale structural router port removed direct entry %q", directEntry)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("stale structural router port made %d backup(s), want 0", got)
	}
	assertCleanupWarningCount(t, warnings, writer.String(), "pidport-port-mismatch: affected_plans=1 [client=cursor,language=go,port=19128]; keeping matching direct LSP entries", 1, 1)
}

func TestCleanupDirectLSP_MatchingOwnedCandidateRevalidatesEachClient(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const routerPort = 19129
	canonical := mustCanonical(t, t.TempDir())
	const directEntry = "legacy-go-cached-proof"
	h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
		Name:     directEntry,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", canonical},
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)
	h.fakeClients.entries["vscode"] = map[string]string{
		LSPRouterEntryName("go"): fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort),
	}
	h.fakeClients.disabledEntries["vscode"] = map[string]bool{}
	h.fakeClients.stdioEntries["vscode"] = map[string]clients.LanguageServerStdioEntry{
		"legacy-go-vscode-cached-proof": {
			Name:     "legacy-go-vscode-cached-proof",
			Command:  "mcp-language-server",
			Language: "gopls",
			Args:     []string{"--lsp", "gopls", "--workspace", canonical},
		},
	}
	h.fakeClients.allStdioEntries["vscode"] = map[string]clients.StdioEntry{}
	h.fakeClients.exists["vscode"] = true
	client := &directCleanupInjectedErrorClient{
		registerClient: &fakeClient{parent: h.fakeClients, name: "cursor"},
		failFindAfter:  1,
		findAfterErr:   errors.New("post-proof rescan sentinel"),
	}

	var authorizerCalls, routeCalls, matcherCalls int
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		map[string]registerClient{
			"cursor": client,
			"vscode": &fakeClient{parent: h.fakeClients, name: "vscode"},
		},
		nil,
		nil,
		&bytes.Buffer{},
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				return managedRouteProof{OK: true}
			},
			matchDirect: func(client registerClient, clientName string, aliases map[string]bool, workspace string) directCleanupMatchResult {
				matcherCalls++
				return directLanguageServerCleanupMatches(client, clientName, aliases, workspace)
			},
		},
	)

	if authorizerCalls != 2 || routeCalls != 10 || matcherCalls != 2 {
		t.Fatalf("matching owned candidate calls: authorizer=%d route=%d matcher=%d, want 2/10/2 (one lease plus five checkpoints per client)",
			authorizerCalls, routeCalls, matcherCalls)
	}
	if client.findCalls != 1 {
		t.Fatalf("direct candidate scan calls = %d, want 1 cached preflight scan", client.findCalls)
	}
	for clientName, entryName := range map[string]string{
		"cursor": directEntry,
		"vscode": "legacy-go-vscode-cached-proof",
	} {
		if _, stillThere := h.fakeClients.stdioEntries[clientName][entryName]; stillThere {
			t.Fatalf("%s matching owned candidate was not removed from cached plan; warnings=%v", clientName, warnings)
		}
		if got := h.fakeClients.backupKeepCalls[clientName]; got != 1 {
			t.Fatalf("%s matching owned candidate backups = %d, want 1", clientName, got)
		}
	}
}

func TestCleanupDirectLSP_NoRouterEntrySkipsResolverProbeAndWarning(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	canonical := mustCanonical(t, t.TempDir())
	const directEntry = "legacy-go-without-router-entry-preflight"
	h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
		Name:     directEntry,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", canonical},
	}

	var authorizerCalls, routeCalls, matcherCalls int
	var writer bytes.Buffer
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		map[string]registerClient{
			"cursor": &fakeClient{parent: h.fakeClients, name: "cursor"},
		},
		nil,
		nil,
		&writer,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{FailureClass: "must-not-run"}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				return managedRouteProof{FailureClass: "must-not-run"}
			},
			matchDirect: func(client registerClient, clientName string, aliases map[string]bool, workspace string) directCleanupMatchResult {
				matcherCalls++
				return directLanguageServerCleanupMatches(client, clientName, aliases, workspace)
			},
		},
	)

	if authorizerCalls != 0 || routeCalls != 0 || matcherCalls != 0 {
		t.Fatalf("no-router calls: authorizer=%d route=%d matcher=%d, want all zero",
			authorizerCalls, routeCalls, matcherCalls)
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"][directEntry]; !stillThere {
		t.Fatalf("no-router plan removed direct entry %q", directEntry)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("no-router plan made %d backup(s), want 0", got)
	}
	assertNoRouterProofWarning(t, &RegisterReport{Warnings: warnings}, writer.String())
}

func TestCleanupDirectLSP_NoDirectCandidateSkipsResolverProbeAndWarning(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const routerPort = 19130
	canonical := mustCanonical(t, t.TempDir())
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)

	var authorizerCalls, routeCalls, matcherCalls int
	var writer bytes.Buffer
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		map[string]registerClient{
			"cursor": &fakeClient{parent: h.fakeClients, name: "cursor"},
		},
		nil,
		nil,
		&writer,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{FailureClass: "must-not-run"}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				return managedRouteProof{FailureClass: "must-not-run"}
			},
			matchDirect: func(client registerClient, clientName string, aliases map[string]bool, workspace string) directCleanupMatchResult {
				matcherCalls++
				return directLanguageServerCleanupMatches(client, clientName, aliases, workspace)
			},
		},
	)

	if authorizerCalls != 0 || routeCalls != 0 || matcherCalls != 1 {
		t.Fatalf("no-direct-candidate calls: authorizer=%d route=%d matcher=%d, want 0/0/1",
			authorizerCalls, routeCalls, matcherCalls)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("no-direct-candidate plan made %d backup(s), want 0", got)
	}
	assertNoRouterProofWarning(t, &RegisterReport{Warnings: warnings}, writer.String())
}

func TestCleanupDirectLSP_BoundOnlyPlanNeverResolvesOrProbes(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	canonical := mustCanonical(t, t.TempDir())
	const directEntry = "legacy-go-bound-only-preflight"
	h.fakeClients.stdioEntries["claude-code"][directEntry] = clients.LanguageServerStdioEntry{
		Name:     directEntry,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", canonical},
	}

	var authorizerCalls, routeCalls, matcherCalls int
	var writer bytes.Buffer
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		map[string]registerClient{
			"claude-code": &fakeClient{parent: h.fakeClients, name: "claude-code"},
		},
		map[clientLanguageKey]bool{{Client: "claude-code", Language: "go"}: true},
		[]clientWriteReceipt{{Key: clientLanguageKey{Client: "claude-code", Language: "go"}, EntryName: directEntry}},
		&writer,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{FailureClass: "must-not-run"}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				return managedRouteProof{FailureClass: "must-not-run"}
			},
			matchDirect: func(client registerClient, clientName string, aliases map[string]bool, workspace string) directCleanupMatchResult {
				matcherCalls++
				return directLanguageServerCleanupMatches(client, clientName, aliases, workspace)
			},
		},
	)

	if authorizerCalls != 0 || routeCalls != 0 || matcherCalls != 1 {
		t.Fatalf("receipt-only calls: authorizer=%d route=%d matcher=%d, want 0/0/1",
			authorizerCalls, routeCalls, matcherCalls)
	}
	if _, stillThere := h.fakeClients.stdioEntries["claude-code"][directEntry]; stillThere {
		t.Fatalf("bound-only plan did not remove direct entry %q; warnings=%v", directEntry, warnings)
	}
	if got := h.fakeClients.backupKeepCalls["claude-code"]; got != 1 {
		t.Fatalf("bound-only plan made %d backup(s), want 1", got)
	}
	assertNoRouterProofWarning(t, &RegisterReport{Warnings: warnings}, writer.String())
}

func TestRegister_CleanupSkipsManagedRouterProofWithoutRegisteredLanguageRouterEntry(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	var requests atomic.Int32
	_, _ = newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"pid":31337,"version":"must-not-be-read"}`)
	})
	ws := t.TempDir()
	canonical := mustCanonical(t, ws)
	const directEntry = "legacy-go-without-router-entry"
	h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
		Name:     directEntry,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", canonical},
	}

	m := nineLanguageManifest()
	m.ClientBindings = nil
	if effectiveBoundClientsForTest(m)["cursor"] {
		t.Fatalf("test premise broken: cursor must be unbound")
	}

	var writer bytes.Buffer
	report, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go"},
		RegisterOpts{Writer: &writer},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("cleanup without a registered-language router entry made %d managed-router request(s), want 0", got)
	}
	assertNoRouterProofWarning(t, report, writer.String())
	if _, stillThere := h.fakeClients.stdioEntries["cursor"][directEntry]; !stillThere {
		t.Fatalf("cleanup without a router replacement removed direct entry %q", directEntry)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("cleanup without a router replacement made %d backup(s), want 0", got)
	}
}

func TestRegister_CleanupSkipsManagedRouterProofWithoutMatchingDirectCandidate(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const (
		expectedPID     = 42424
		expectedVersion = "no-direct-candidate"
	)
	var requests atomic.Int32
	_, routerPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"pid":%d,"version":%q}`, expectedPID, expectedVersion)
	})
	ws := t.TempDir()
	otherWorkspace := mustCanonical(t, t.TempDir())
	const nonCandidate = "legacy-go-for-other-workspace"
	h.fakeClients.stdioEntries["cursor"][nonCandidate] = clients.LanguageServerStdioEntry{
		Name:     nonCandidate,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", otherWorkspace},
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)

	m := nineLanguageManifest()
	m.ClientBindings = nil
	if effectiveBoundClientsForTest(m)["cursor"] {
		t.Fatalf("test premise broken: cursor must be unbound")
	}

	var writer bytes.Buffer
	report, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go"},
		RegisterOpts{Writer: &writer, ManagedRouterAuthorizer: allowManagedRouterAuthorizerForTest(routerPort)},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("cleanup without a matching direct candidate made %d managed-router request(s), want 0", got)
	}
	assertNoRouterProofWarning(t, report, writer.String())
	if _, stillThere := h.fakeClients.stdioEntries["cursor"][nonCandidate]; !stillThere {
		t.Fatalf("cleanup removed non-candidate direct entry %q", nonCandidate)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("cleanup without a direct candidate made %d backup(s), want 0", got)
	}
}

// TestRegister_CleanupBoundClientBypassesManagedRouterProof pins the invariant that
// broke when cursor became opt-in (bot PR #583 finding 7): the post-register
// direct-LSP cleanup must only touch clients this registration actually WROTE
// to.
//
// Before the fix, registerWithManifest handed the FULL all-clients map to
// cleanupDirectLanguageServerEntriesAfterRegister while the write path narrowed
// to the effective bindings. While cursor was still a default the two agreed by
// accident. Once it became opt-in they diverged, and cursor's working direct
// entry was backed up and deleted with nothing put in its place — leaving that
// client disconnected. Deleting a working entry and supplying no replacement is
// strictly worse than leaving it alone.
//
// Both directions are asserted, because a fix that simply stopped cleaning
// everything would be just as wrong: the BOUND client's superseded entry must
// still be removed.
func TestRegister_CleanupBoundClientBypassesManagedRouterProof(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	var requests atomic.Int32
	_, routerPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"pid":999999,"version":"must-not-be-read"}`)
	})
	ws := t.TempDir()
	canonical := mustCanonical(t, ws)

	direct := func(name string) map[string]clients.LanguageServerStdioEntry {
		return map[string]clients.LanguageServerStdioEntry{
			name: {
				Name:     name,
				Command:  "mcp-language-server",
				Language: "gopls",
				Args:     []string{"--lsp", "gopls", "--workspace", canonical},
			},
		}
	}
	// codex-cli is in the derived default bindings; cursor is opt-in and is
	// NOT, so a bare register writes a managed entry only to the former.
	h.fakeClients.stdioEntries["codex-cli"] = direct("legacy-go-bound")
	h.fakeClients.stdioEntries["cursor"] = direct("legacy-go-optin")
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)

	// The shipped mcp-language-server manifest declares NO client_bindings, so
	// a bare register falls back to the derived defaults — that is the exact
	// path the bot's finding describes. Clear them here to reproduce it.
	m := nineLanguageManifest()
	m.ClientBindings = nil

	bound := map[string]bool{}
	premiseBindings, _ := effectiveClientBindings(m)
	for _, b := range premiseBindings {
		bound[b.Client] = true
	}
	if !bound["codex-cli"] {
		t.Fatalf("test premise broken: codex-cli must be in the effective bindings; got %v", bound)
	}
	if bound["cursor"] {
		t.Fatalf("test premise broken: cursor must be opt-in (absent from the effective bindings); got %v", bound)
	}

	var writer bytes.Buffer
	report, err := mustNewAPI(t).registerWithManifest(
		m, ws, []string{"go"}, RegisterOpts{Writer: &writer},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if _, ok := h.fakeClients.stdioEntries["cursor"]["legacy-go-optin"]; !ok {
		t.Errorf("cursor's direct LSP entry was removed even though cursor received no managed replacement " +
			"— that leaves the client disconnected (bot PR #583 finding 7)")
	}
	if h.fakeClients.backupKeepCalls["cursor"] != 0 {
		t.Errorf("cursor was backed up for a cleanup it should never have been part of (%d BackupKeep calls)",
			h.fakeClients.backupKeepCalls["cursor"])
	}
	if _, ok := h.fakeClients.stdioEntries["codex-cli"]["legacy-go-bound"]; ok {
		t.Errorf("codex-cli's superseded direct LSP entry survived; the bound client's entry IS replaced " +
			"and must still be cleaned up")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("identity-absent cleanup made %d managed-router request(s), want 0", got)
	}
	assertSingleRouterProofWarning(t, report, writer.String(), "identity-not-supplied")
}

// TestRegister_CleanupRejectsRouterEntryOnStalePort pins the PORT half of the
// replacement gate: a shared LSP-router entry only counts as a working
// replacement when it names the port the router actually listens on.
//
// clientHasActiveLSPRouterReplacement delegates ownership to
// entryIsOwnedLSPRouterForLanguage, whose `reservedName || (guiPort > 0 && ...)`
// disjunction short-circuits on the reserved NAME — and the name is
// unconditionally reserved at that call site, because the entry was fetched BY
// LSPRouterEntryName(language). The probe therefore proved only "an entry with
// the reserved name exists, is enabled, and parses as /lsp/<lang>/mcp" — at ANY
// port. (Passing a non-zero guiPort into that helper would NOT fix it; the
// name short-circuit still wins. The port has to be checked in the probe.)
//
// The operator-reachable failure: change gui_server.port in Settings
// (pending-restart), and every client's
// mcp-language-server-<lang> entry still names the OLD port until
// EnsureLSPRouterClientEntries re-runs. Register would read routed=true, add
// the go/gopls aliases, and back up + RemoveEntry the client's LIVE direct
// entry — leaving one dead router entry and no working Go LSP. That is the
// "removed with nothing put in its place" defect the gate exists to prevent.
//
// cursor is unbound (opt-in) and holds router entries for BOTH languages; only
// the `go` one names the live port (9125, the gui_server.port registry
// default the harness's empty settings dir resolves to):
//
//	go     → router entry on the LIVE port  → replacement real → direct entry removed
//	python → router entry on a STALE port   → replacement dead → direct entry MUST survive
//
// Asserting both directions keeps the fix honest: refusing every router entry
// would also pass the python case while breaking the go one.
func TestRegister_CleanupRejectsRouterEntryOnStalePort(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)

	direct := func(entryName, lspLanguage string) clients.LanguageServerStdioEntry {
		return clients.LanguageServerStdioEntry{
			Name:     entryName,
			Command:  "mcp-language-server",
			Language: lspLanguage,
			Args:     []string{"--lsp", lspLanguage, "--workspace", canonical},
		}
	}
	h.fakeClients.stdioEntries["cursor"] = map[string]clients.LanguageServerStdioEntry{
		"legacy-go":     direct("legacy-go", "gopls"),
		"legacy-python": direct("legacy-python", "pyright-langserver"),
	}
	const (
		expectedPID     = 112233
		expectedVersion = "stale-port-test"
	)
	_, livePort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"pid":%d,"version":%q}`, expectedPID, expectedVersion)
	})
	stalePort := livePort + 1
	if stalePort > 65535 {
		stalePort = livePort - 1
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", livePort)
	h.fakeClients.entries["cursor"][LSPRouterEntryName("python")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/python/mcp", stalePort)

	m := nineLanguageManifest()
	m.ClientBindings = nil // shipped manifest declares none → derived defaults

	if effectiveBoundClientsForTest(m)["cursor"] {
		t.Fatalf("test premise broken: cursor must be opt-in (absent from the effective bindings)")
	}

	_, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go", "python"},
		RegisterOpts{Writer: &bytes.Buffer{}, ManagedRouterAuthorizer: allowManagedRouterAuthorizerForTest(livePort), probeManagedLanguageRoute: allowManagedLanguageRouteForTest},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-python"]; !stillThere {
		t.Errorf("cursor's direct `python` entry was deleted on the strength of a router entry pointing at "+
			"port %d while the router actually listens on %d — that entry is dead, so the client is left "+
			"with no working python LSP at all. The replacement probe must verify the PORT, not just the "+
			"reserved entry name.", stalePort, livePort)
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; stillThere {
		t.Errorf("cursor's superseded direct `go` entry survived even though its router entry names the LIVE "+
			"port %d — the port gate must not reject a genuinely working replacement, or the superseded "+
			"direct entry duplicates the router's servers/tools forever", livePort)
	}
}

func TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve stopped-listener port: %v", err)
	}
	routerPort := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close stopped-listener fixture: %v", err)
	}

	const directEntry = "legacy-go-stopped-router"
	h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
		Name:     directEntry,
		Command:  "mcp-language-server",
		Language: "gopls",
		Args:     []string{"--lsp", "gopls", "--workspace", canonical},
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)

	m := nineLanguageManifest()
	m.ClientBindings = nil
	if effectiveBoundClientsForTest(m)["cursor"] {
		t.Fatalf("test premise broken: cursor must be unbound")
	}

	var writer bytes.Buffer
	report, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go"},
		RegisterOpts{Writer: &writer, ManagedRouterAuthorizer: func(context.Context, int) ManagedRouterAuthorization {
			return ManagedRouterAuthorization{FailureClass: ManagedRouterFailurePingTransport}
		}},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if _, stillThere := h.fakeClients.stdioEntries["cursor"][directEntry]; !stillThere {
		t.Fatalf("stopped configured router authorized removal of direct entry %q", directEntry)
	}
	if got := h.fakeClients.backupKeepCalls["cursor"]; got != 0 {
		t.Fatalf("stopped configured router authorized %d backup(s), want 0", got)
	}
	assertSingleRouterProofWarning(t, report, writer.String(), ManagedRouterFailurePingTransport)
}

func TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	var requestCount atomic.Int32
	_, routerPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"pid":987654,"version":"foreign"}`)
	})

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)
	direct := func(entryName, language string) clients.LanguageServerStdioEntry {
		return clients.LanguageServerStdioEntry{
			Name:     entryName,
			Command:  "mcp-language-server",
			Language: language,
			Args:     []string{"--lsp", language, "--workspace", canonical},
		}
	}
	for _, clientName := range []string{"cursor", "vscode"} {
		if clientName == "vscode" {
			h.fakeClients.entries[clientName] = map[string]string{}
			h.fakeClients.disabledEntries[clientName] = map[string]bool{}
			h.fakeClients.stdioEntries[clientName] = map[string]clients.LanguageServerStdioEntry{}
			h.fakeClients.allStdioEntries[clientName] = map[string]clients.StdioEntry{}
			h.fakeClients.exists[clientName] = true
		}
		h.fakeClients.stdioEntries[clientName]["legacy-go-"+clientName] =
			direct("legacy-go-"+clientName, "gopls")
		h.fakeClients.stdioEntries[clientName]["legacy-python-"+clientName] =
			direct("legacy-python-"+clientName, "pyright-langserver")
		h.fakeClients.entries[clientName][LSPRouterEntryName("go")] =
			fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)
		h.fakeClients.entries[clientName][LSPRouterEntryName("python")] =
			fmt.Sprintf("http://127.0.0.1:%d/lsp/python/mcp", routerPort)
	}

	m := nineLanguageManifest()
	m.ClientBindings = nil
	for _, clientName := range []string{"cursor", "vscode"} {
		if effectiveBoundClientsForTest(m)[clientName] {
			t.Fatalf("test premise broken: %s must be unbound", clientName)
		}
	}

	var writer bytes.Buffer
	report, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go", "python"},
		RegisterOpts{Writer: &writer, ManagedRouterAuthorizer: func(context.Context, int) ManagedRouterAuthorization {
			return ManagedRouterAuthorization{FailureClass: ManagedRouterFailurePingIdentityMismatch}
		}},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	for _, clientName := range []string{"cursor", "vscode"} {
		for _, entryName := range []string{"legacy-go-" + clientName, "legacy-python-" + clientName} {
			if _, stillThere := h.fakeClients.stdioEntries[clientName][entryName]; !stillThere {
				t.Fatalf("foreign listener authorized removal of %s direct entry %q", clientName, entryName)
			}
		}
		if got := h.fakeClients.backupKeepCalls[clientName]; got != 0 {
			t.Fatalf("foreign listener authorized %d %s backup(s), want 0", got, clientName)
		}
	}
	if got := requestCount.Load(); got != 0 {
		t.Fatalf("foreign-listener route requests = %d, want 0 after identity refusal", got)
	}
	assertSingleRouterProofWarning(t, report, writer.String(), ManagedRouterFailurePingIdentityMismatch)
}

func TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const (
		expectedPID     = 424242
		expectedVersion = "test-managed-router"
	)
	var requestCount atomic.Int32
	_, routerPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("managed-router probe method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/ping" {
			t.Errorf("managed-router probe path = %q, want /api/ping", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"pid":%d,"version":%q}`, expectedPID, expectedVersion)
	})

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)
	direct := func(entryName, lspLanguage string) clients.LanguageServerStdioEntry {
		return clients.LanguageServerStdioEntry{
			Name:     entryName,
			Command:  "mcp-language-server",
			Language: lspLanguage,
			Args:     []string{"--lsp", lspLanguage, "--workspace", canonical},
		}
	}
	h.fakeClients.stdioEntries["cursor"] = map[string]clients.LanguageServerStdioEntry{
		"legacy-go":     direct("legacy-go", "gopls"),
		"legacy-python": direct("legacy-python", "pyright-langserver"),
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)

	m := nineLanguageManifest()
	m.ClientBindings = nil
	if effectiveBoundClientsForTest(m)["cursor"] {
		t.Fatalf("test premise broken: cursor must be unbound")
	}

	report, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go", "python"},
		RegisterOpts{Writer: &bytes.Buffer{}, ManagedRouterAuthorizer: allowManagedRouterAuthorizerForTest(routerPort), probeManagedLanguageRoute: allowManagedLanguageRouteForTest},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; stillThere {
		t.Fatalf("managed router with exact expected identity did not authorize removal of direct go entry")
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-python"]; !stillThere {
		t.Fatalf("managed go identity authorized removal of sibling python entry without a python router entry")
	}
	if got := requestCount.Load(); got != 0 {
		t.Fatalf("harness unexpectedly made %d route request(s)", got)
	}
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "keeping matching direct LSP entries") {
			t.Fatalf("successful managed proof emitted failure warning %q", warning)
		}
	}
}

func TestRegister_CleanupRejectsInvalidRouterEntries(t *testing.T) {
	const (
		expectedPID     = 13579
		expectedVersion = "invalid-entry-test"
	)
	cases := []struct {
		name         string
		present      bool
		disabled     bool
		url          func(int) string
		wantRemoved  bool
		wantRequests int32
	}{
		{name: "missing"},
		{name: "disabled", present: true, disabled: true, url: func(port int) string {
			return fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
		}},
		{name: "malformed", present: true, url: func(int) string { return "not-a-router-url" }},
		{name: "non-loopback", present: true, url: func(port int) string {
			return fmt.Sprintf("http://192.0.2.1:%d/lsp/go/mcp", port)
		}},
		{name: "wrong-language", present: true, url: func(port int) string {
			return fmt.Sprintf("http://127.0.0.1:%d/lsp/python/mcp", port)
		}},
		{name: "stale-port", present: true, url: func(port int) string {
			stale := port + 1
			if stale > 65535 {
				stale = port - 1
			}
			return fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", stale)
		}},
		{name: "matching-owned", present: true, wantRemoved: true, wantRequests: 0, url: func(port int) string {
			return fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()

			var requests atomic.Int32
			_, routerPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"ok":true,"pid":%d,"version":%q}`, expectedPID, expectedVersion)
			})
			ws := t.TempDir()
			canonical := mustCanonical(t, ws)
			const directEntry = "legacy-go-invalid-router-case"
			h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
				Name:     directEntry,
				Command:  "mcp-language-server",
				Language: "gopls",
				Args:     []string{"--lsp", "gopls", "--workspace", canonical},
			}
			if tc.present {
				entryName := LSPRouterEntryName("go")
				h.fakeClients.entries["cursor"][entryName] = tc.url(routerPort)
				h.fakeClients.disabledEntries["cursor"][entryName] = tc.disabled
			}

			m := nineLanguageManifest()
			m.ClientBindings = nil
			report, err := mustNewAPI(t).registerWithManifest(
				m,
				ws,
				[]string{"go"},
				RegisterOpts{Writer: &bytes.Buffer{}, ManagedRouterAuthorizer: allowManagedRouterAuthorizerForTest(routerPort), probeManagedLanguageRoute: allowManagedLanguageRouteForTest},
			)
			if err != nil {
				t.Fatalf("registerWithManifest: %v", err)
			}
			_, stillThere := h.fakeClients.stdioEntries["cursor"][directEntry]
			if stillThere == tc.wantRemoved {
				t.Fatalf("direct entry present = %v, want %v; report=%+v", stillThere, !tc.wantRemoved, report)
			}
			wantBackups := 0
			if tc.wantRemoved {
				wantBackups = 1
			}
			if got := h.fakeClients.backupKeepCalls["cursor"]; got != wantBackups {
				t.Fatalf("backup count = %d, want %d", got, wantBackups)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Fatalf("managed proof requests = %d, want %d", got, tc.wantRequests)
			}
		})
	}
}

func TestRegister_CleanupAliasAuthorizationForDirectEntryKinds(t *testing.T) {
	const (
		actualPID     = 86420
		actualVersion = "direct-kind-test"
	)
	cases := []struct {
		kind       string
		authorized bool
	}{
		{kind: "mcp-language-server", authorized: false},
		{kind: "mcp-language-server", authorized: true},
		{kind: "gopls", authorized: false},
		{kind: "gopls", authorized: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/authorized=%v", tc.kind, tc.authorized), func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()

			var requests atomic.Int32
			_, routerPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"ok":true,"pid":%d,"version":%q}`, actualPID, actualVersion)
			})
			ws := t.TempDir()
			canonical := mustCanonical(t, ws)
			const directEntry = "legacy-go-direct-kind"
			switch tc.kind {
			case "mcp-language-server":
				h.fakeClients.stdioEntries["cursor"][directEntry] = clients.LanguageServerStdioEntry{
					Name:     directEntry,
					Command:  "mcp-language-server",
					Language: "gopls",
					Args:     []string{"--lsp", "gopls", "--workspace", canonical},
				}
			case "gopls":
				h.fakeClients.allStdioEntries["cursor"][directEntry] = clients.StdioEntry{
					Name:    directEntry,
					Command: "gopls",
					Args:    []string{"mcp", "--workspace", canonical},
				}
			default:
				t.Fatalf("unknown direct-entry kind %q", tc.kind)
			}
			h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
				fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)

			m := nineLanguageManifest()
			m.ClientBindings = nil
			authorizer := allowManagedRouterAuthorizerForTest(routerPort)
			if !tc.authorized {
				authorizer = func(context.Context, int) ManagedRouterAuthorization {
					return ManagedRouterAuthorization{FailureClass: "identity-mismatch"}
				}
			}
			if _, err := mustNewAPI(t).registerWithManifest(
				m,
				ws,
				[]string{"go"},
				RegisterOpts{Writer: &bytes.Buffer{}, ManagedRouterAuthorizer: authorizer, probeManagedLanguageRoute: allowManagedLanguageRouteForTest},
			); err != nil {
				t.Fatalf("registerWithManifest: %v", err)
			}
			stillThere := false
			if tc.kind == "mcp-language-server" {
				_, stillThere = h.fakeClients.stdioEntries["cursor"][directEntry]
			} else {
				_, stillThere = h.fakeClients.allStdioEntries["cursor"][directEntry]
			}
			if stillThere == tc.authorized {
				t.Fatalf("%s direct entry present = %v, want %v", tc.kind, stillThere, !tc.authorized)
			}
			wantBackups := 0
			if tc.authorized {
				wantBackups = 1
			}
			if got := h.fakeClients.backupKeepCalls["cursor"]; got != wantBackups {
				t.Fatalf("%s backup count = %d, want %d", tc.kind, got, wantBackups)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("%s harness route requests = %d, want 0", tc.kind, got)
			}
		})
	}
}

// TestRegister_CleanupCoversOptInClientRoutedThroughSharedLSPRouter pins the
// SECOND half of the post-register cleanup invariant — the half the first
// narrowing fix got wrong (bot PR #583, follow-on finding against
// register.go:316).
//
// Narrowing cleanup to effectiveClientBindings alone was an over-correction.
// An opt-in client is absent from those bindings, but it can still hold a
// perfectly valid hub-managed replacement: ensureLSPRouterClientEntriesWithLoaded
// deliberately maintains shared /lsp/<lang>/mcp router entries for NON-default
// clients that carry existing mcphub evidence. Skipping such a client leaves its
// superseded direct stdio entry live ALONGSIDE the router entry — duplicate
// servers and tools, plus the legacy LSP process nobody reaps.
//
// The grain is per (client, LANGUAGE), and this test is built to prove exactly
// that. cursor is unbound and holds a router entry for `go` ONLY:
//
//	go      → replacement exists (router)     → direct entry MUST be removed
//	python  → no replacement whatsoever       → direct entry MUST survive
//
// A whole-client fix ("cursor has a router entry, so clean everything on
// cursor") passes the go assertion and FAILS the python one — it would delete a
// working entry with nothing to replace it, which is the original defect
// reappearing one language over.
func TestRegister_CleanupCoversOptInClientRoutedThroughSharedLSPRouter(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	const (
		expectedPID     = 998877
		expectedVersion = "opt-in-router-test"
	)
	_, routerPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"pid":%d,"version":%q}`, expectedPID, expectedVersion)
	})
	ws := t.TempDir()
	canonical := mustCanonical(t, ws)

	direct := func(entryName, lspLanguage string) clients.LanguageServerStdioEntry {
		return clients.LanguageServerStdioEntry{
			Name:     entryName,
			Command:  "mcp-language-server",
			Language: lspLanguage,
			Args:     []string{"--lsp", lspLanguage, "--workspace", canonical},
		}
	}
	// cursor is opt-in, so a bare register writes it NOTHING. It nonetheless
	// carries direct entries for both registered languages.
	h.fakeClients.stdioEntries["cursor"] = map[string]clients.LanguageServerStdioEntry{
		"legacy-go":     direct("legacy-go", "gopls"),
		"legacy-python": direct("legacy-python", "pyright-langserver"),
	}
	// ...and an ACTIVE, hub-owned shared LSP-router entry for `go` only. This is
	// what the router reconcile leaves on an explicitly enabled non-default
	// client (LSPRouterEntryName("go") pointed at the router's /lsp/go/mcp path).
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", routerPort)

	m := nineLanguageManifest()
	m.ClientBindings = nil // shipped manifest declares none → derived defaults

	if effectiveBoundClientsForTest(m)["cursor"] {
		t.Fatalf("test premise broken: cursor must be opt-in (absent from the effective bindings)")
	}

	if _, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go", "python"},
		RegisterOpts{Writer: &bytes.Buffer{}, ManagedRouterAuthorizer: allowManagedRouterAuthorizerForTest(routerPort), probeManagedLanguageRoute: allowManagedLanguageRouteForTest},
	); err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; stillThere {
		t.Errorf("cursor's superseded direct `go` entry survived even though cursor already routes `go` " +
			"through the shared hub LSP router — the direct entry now duplicates the router entry's " +
			"servers/tools and keeps the legacy process alive (bot PR #583, register.go:316)")
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-python"]; !stillThere {
		t.Errorf("cursor's direct `python` entry was removed, but cursor has NO replacement for python " +
			"(its router entry covers `go` only) — that disconnects python and reopens the very defect " +
			"the binding narrowing closed; the replacement gate must be per-LANGUAGE, not per-client")
	}
}

// TestRegister_RelayStdioOnlyOverrideWarnsInsteadOfSilentZeroWrite pins the
// state that honoring the persisted default-install override MADE REACHABLE.
//
// SetDefaultInstallClientNames validates against clients.SupportedClientNames()
// and rejects only the EMPTY set, and the Settings → Clients panel
// (ClientInstallToggleViewIn) renders EVERY supported client as a toggle —
// including the six relay-stdio ones. So `clients.default_install = zed` is a
// valid, reachable operator selection. defaultClientBindingsNow then drops zed
// via the IsRelayStdio filter, the write loop iterates zero bindings, and
// register reports "Registered 1 language(s)" having pointed NO client config
// at the proxy it just created.
//
// Before register read the override this was unreachable: the compile-time
// fallback is {claude-code, codex-cli}, both URL-native, so the filter never
// emptied the set. Making the override live made the empty case live too.
//
// Two things are asserted, and the second is why "just fall back to the
// compile-time set" is the WRONG fix:
//  1. the operator is TOLD (report warning + writer output, naming zed) —
//     a silent zero-write is the actual defect;
//  2. register does NOT write claude-code / codex-cli, which the operator
//     explicitly DESELECTED. Substituting them would mutate configs nobody
//     asked to touch and re-open the register-vs-install divergence pointing
//     the other way — register binding clients `mcphub install` would not.
func TestRegister_RelayStdioOnlyOverrideWarnsInsteadOfSilentZeroWrite(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	if !clients.IsRelayStdio("zed") {
		t.Fatalf("test premise broken: zed must be a relay-stdio client")
	}
	h.fakeClients.entries["zed"] = map[string]string{}
	h.fakeClients.stdioEntries["zed"] = map[string]clients.LanguageServerStdioEntry{}
	h.fakeClients.allStdioEntries["zed"] = map[string]clients.StdioEntry{}
	h.fakeClients.exists["zed"] = true

	// newRegisterHarness redirects SettingsPath() into a temp dir, so this
	// persists into temp — never the operator's real gui-preferences.yaml.
	if err := mustNewAPI(t).SetDefaultInstallClientNames([]string{"zed"}); err != nil {
		t.Fatalf("test premise broken: a relay-stdio-only default-install set must be persistable "+
			"(the api layer rejects only the EMPTY set), got: %v", err)
	}

	m := nineLanguageManifest()
	m.ClientBindings = nil // shipped manifest declares none → derived defaults

	// POSTURE PIN. The resolver must report that it can bind nothing, NOT
	// substitute a client set the operator did not select. See
	// relayStdioBindingWarning's POSTURE note for why the compile-time fallback
	// is the wrong instrument here.
	bindings, dropped := effectiveClientBindings(m)
	if len(bindings) != 0 {
		t.Fatalf("a zed-only override resolved to bindings %v — register substituted a client set the "+
			"operator did not select instead of reporting that it can bind nothing", bindings)
	}
	if len(dropped) != 1 || dropped[0] != "zed" {
		t.Fatalf("zed must be reported as the dropped relay-stdio client so the operator can be told which "+
			"selection could not be honored, got %v", dropped)
	}

	var buf bytes.Buffer
	report, err := mustNewAPI(t).registerWithManifest(m, t.TempDir(), []string{"go"}, RegisterOpts{Writer: &buf})
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(report.Entries))
	}
	if got := report.Entries[0].ClientEntries; len(got) != 0 {
		t.Fatalf("test premise broken: expected zero client entries for a relay-stdio-only override, got %v", got)
	}

	warned := false
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "zed") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("register wrote ZERO client entries and returned no warning naming zed — the operator "+
			"selected only relay-stdio clients, so the workspace proxy was created with no client config "+
			"pointing at it, and nothing said so. RegisterReport.Warnings is the GUI project-LSP toggle's "+
			"only channel for this; Warnings=%v", report.Warnings)
	}
	if !strings.Contains(buf.String(), "zed") {
		t.Errorf("`mcphub register` printed no warning naming zed for a registration that bound no client "+
			"at all; writer output:\n%s", buf.String())
	}

	// The substitution trap: never silently write the clients the operator
	// deselected.
	for _, deselected := range []string{"claude-code", "codex-cli"} {
		if got := h.fakeClients.entries[deselected]; len(got) != 0 {
			t.Errorf("register wrote %d entr(ies) into %q, which the operator's persisted "+
				"clients.default_install EXCLUDES — falling back to the compile-time default set when the "+
				"relay-stdio filter empties the selection mutates configs the operator deselected and makes "+
				"register bind clients `mcphub install` would not; entries=%v", len(got), deselected, got)
		}
	}
}

// TestRegister_ClientScopeResolvedOnceForTheWholeRegistration pins the client
// scope to ONE decision per Register — the TIME axis of the same defect the
// binding/cleanup divergence was on the LIST axis.
//
// effectiveClientBindings reads gui-preferences.yaml at CALL TIME. It used to be
// called once per language in the write path AND once more in the cleanup gate,
// so a register spanning tens of seconds (sch.Create + sch.Run + a readiness
// probe with a 10s ceiling, per language) sampled the operator's selection N+1
// times. Inside the GUI process `POST /api/client-install-prefs` and the
// project-LSP toggle that calls Register are independent handlers sharing no
// lock, so an operator ticking a client mid-loop split the decision:
//
//	go      written BEFORE the tick  → cursor gets NO managed entry
//	python  written AFTER  the tick  → cursor gets one
//	cleanup resolves the NEW set     → cursor counted bound for ALL languages
//	                                 → cursor's live direct `go` entry deleted
//	                                   with nothing put in its place
//
// The mid-loop mutation is injected through fakeScheduler.runHook, which fires
// at the top of Run inside the FIRST language's registerOneLanguage — after that
// language consumed its bindings and before the second language would resolve
// its own. Deterministic by construction: no timer, no goroutine, no race.
func TestRegister_EffectiveBindingsResolvedOnceForMultiLanguageCleanup(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)

	direct := func(entryName, lspLanguage string) clients.LanguageServerStdioEntry {
		return clients.LanguageServerStdioEntry{
			Name:     entryName,
			Command:  "mcp-language-server",
			Language: lspLanguage,
			Args:     []string{"--lsp", lspLanguage, "--workspace", canonical},
		}
	}
	// cursor holds working direct entries for both registered languages and is
	// NOT in the starting selection.
	h.fakeClients.stdioEntries["cursor"] = map[string]clients.LanguageServerStdioEntry{
		"legacy-go":     direct("legacy-go", "gopls"),
		"legacy-python": direct("legacy-python", "pyright-langserver"),
	}

	a := mustNewAPI(t)
	if err := a.SetDefaultInstallClientNames([]string{"claude-code"}); err != nil {
		t.Fatalf("persist starting default-install override: %v", err)
	}

	flips := 0
	h.fakeSch.runHook = func(string) {
		if flips > 0 {
			return
		}
		flips++
		// The operator ticks Cursor in Settings → Clients while the register
		// loop is between languages.
		if err := a.SetDefaultInstallClientNames([]string{"claude-code", "cursor"}); err != nil {
			t.Errorf("mid-loop default-install mutation: %v", err)
		}
	}

	m := nineLanguageManifest()
	m.ClientBindings = nil // shipped manifest declares none → derived defaults

	report, err := mustNewAPI(t).registerWithManifest(
		m, ws, []string{"go", "python"}, RegisterOpts{Writer: &bytes.Buffer{}},
	)
	if err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}
	if flips != 1 {
		t.Fatalf("test premise broken: the mid-loop settings mutation fired %d times, want exactly 1", flips)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(report.Entries))
	}

	// 1. The WRITE path must be uniform across languages: whatever cursor got
	//    for `go`, it must have gotten for `python`. A per-language re-read
	//    produces exactly the split this asserts against.
	perLanguageCursor := map[string]bool{}
	for _, entry := range report.Entries {
		_, bound := entry.ClientEntries["cursor"]
		perLanguageCursor[entry.Language] = bound
	}
	if perLanguageCursor["go"] != perLanguageCursor["python"] {
		t.Errorf("cursor was bound for go=%v but python=%v within ONE register — the client scope is being "+
			"re-resolved per language, so an operator editing Settings -> Clients mid-loop splits the "+
			"registration across two different client sets; entries=%v",
			perLanguageCursor["go"], perLanguageCursor["python"], report.Entries)
	}
	if perLanguageCursor["python"] {
		t.Errorf("cursor received a managed entry for `python` even though the selection at the START of this "+
			"register excluded it — the language loop picked up the mid-loop Settings change; entries=%v",
			report.Entries)
	}

	// 2. The DAMAGE the split causes: the cleanup gate resolving the NEW set
	//    counts cursor as bound for EVERY language and deletes the direct entry
	//    of a language cursor never got a managed replacement for.
	for _, entryName := range []string{"legacy-go", "legacy-python"} {
		if _, stillThere := h.fakeClients.stdioEntries["cursor"][entryName]; !stillThere {
			t.Errorf("cursor's live direct %q entry was deleted, but cursor is absent from the client scope "+
				"this registration actually WROTE with — the cleanup gate re-read gui-preferences.yaml after "+
				"the loop and judged every language against a client set the write path never used, so the "+
				"entry was removed with nothing put in its place. The scope must be resolved ONCE per "+
				"register and threaded, exactly as routerPort already is", entryName)
		}
	}
	if h.fakeClients.backupKeepCalls["cursor"] != 0 {
		t.Errorf("cursor was backed up for a cleanup it should never have been part of (%d BackupKeep calls)",
			h.fakeClients.backupKeepCalls["cursor"])
	}
}

// TestRegister_CleanupJudgesRouterEntriesAgainstCallerLiveGUIPort pins the port
// gate to the port the router entries were WRITTEN with, not to the persisted
// setting.
//
// The writer of those entries always uses the LIVE bound port — every
// EnsureLSPRouterClientEntries / Enable / Disable call from the GUI passes
// LSPClientRouterOpts{GUIPort: s.Port()} (internal/gui/lsp_router_control.go) —
// and resolveGuiPort (internal/cli/gui_port.go) lets an explicit `--port` beat
// the persisted gui_server.port outright. Resolving the gate's port from
// settings alone therefore compares against a number the running server may not
// be using: with gui_server.port = 9125 and the GUI actually on --port 9200, a
// STALE 9125 router entry matches, and the client's live direct entry is deleted
// in favour of a dead replacement — the very deletion the port gate exists to
// prevent, re-entered through the port's own provenance.
//
// RegisterOpts.ManagedRouterAuthorizer is the seam: the GUI caller owns its
// live listener identity and authorizes only the port observed through that
// server instance.
//
// Both directions are asserted; a gate that simply rejected everything would
// pass the first assertion and fail the second:
//
//	go     → router entry on the STALE persisted port → dead    → direct entry MUST survive
//	python → router entry on the LIVE caller port     → working → direct entry MUST be cleaned
func TestRegister_CleanupJudgesRouterEntriesAgainstCallerLiveGUIPort(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	ws := t.TempDir()
	canonical := mustCanonical(t, ws)

	direct := func(entryName, lspLanguage string) clients.LanguageServerStdioEntry {
		return clients.LanguageServerStdioEntry{
			Name:     entryName,
			Command:  "mcp-language-server",
			Language: lspLanguage,
			Args:     []string{"--lsp", lspLanguage, "--workspace", canonical},
		}
	}
	h.fakeClients.stdioEntries["cursor"] = map[string]clients.LanguageServerStdioEntry{
		"legacy-go":     direct("legacy-go", "gopls"),
		"legacy-python": direct("legacy-python", "pyright-langserver"),
	}

	// The persisted setting the harness's empty settings dir resolves to. This
	// is what a settings-only resolution would compare against.
	persistedPort, err := mustNewAPI(t).lspRouterGUIPort(0)
	if err != nil {
		t.Fatalf("resolve persisted router port: %v", err)
	}
	// The port the GUI is ACTUALLY serving on (an explicit --port that beat the
	// persisted value). Must differ, or the test proves nothing.
	const (
		expectedPID     = 556677
		expectedVersion = "live-port-test"
	)
	_, liveGUIPort := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"pid":%d,"version":%q}`, expectedPID, expectedVersion)
	})
	if persistedPort == liveGUIPort {
		t.Fatalf("test premise broken: the persisted gui_server.port (%d) must differ from the live port %d",
			persistedPort, liveGUIPort)
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", persistedPort)
	h.fakeClients.entries["cursor"][LSPRouterEntryName("python")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/python/mcp", liveGUIPort)

	m := nineLanguageManifest()
	m.ClientBindings = nil // shipped manifest declares none → derived defaults
	if effectiveBoundClientsForTest(m)["cursor"] {
		t.Fatalf("test premise broken: cursor must be opt-in (absent from the effective bindings)")
	}

	if _, err := mustNewAPI(t).registerWithManifest(
		m,
		ws,
		[]string{"go", "python"},
		RegisterOpts{Writer: &bytes.Buffer{}, ManagedRouterAuthorizer: allowManagedRouterAuthorizerForTest(liveGUIPort), probeManagedLanguageRoute: allowManagedLanguageRouteForTest},
	); err != nil {
		t.Fatalf("registerWithManifest: %v", err)
	}

	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !stillThere {
		t.Errorf("cursor's live direct `go` entry was deleted on the strength of a router entry naming the "+
			"PERSISTED gui_server.port %d, while the caller told register the GUI is actually serving %d — "+
			"that router entry is dead, so the client is left with no working Go LSP at all. The gate must "+
			"authorize the caller-owned live listener identity, not trust settings alone", persistedPort, liveGUIPort)
	}
	if _, stillThere := h.fakeClients.stdioEntries["cursor"]["legacy-python"]; stillThere {
		t.Errorf("cursor's superseded direct `python` entry survived even though its router entry names the "+
			"caller's LIVE port %d — threading the live port must not degrade into rejecting every router "+
			"entry, or superseded direct entries duplicate the router's servers/tools forever", liveGUIPort)
	}
}

func TestRegister_WriteReceiptsReflectActualSuccessfulAdds(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	h.fakeClients.exists["cursor"] = false
	entryNames := map[string]string{
		"claude-code": "mcp-language-server-go",
		"cursor":      "must-not-be-written",
	}
	transaction := newRegistrationTransaction()
	receipts, err := writeRegisteredClientEntries(
		[]config.ClientBinding{{Client: "claude-code", URLPath: "/mcp"}, {Client: "cursor", URLPath: "/mcp"}},
		testClientFactory(), entryNames, 9444, "go", &bytes.Buffer{}, transaction,
	)
	if err != nil {
		t.Fatalf("writeRegisteredClientEntries: %v", err)
	}
	if len(receipts) != 1 || receipts[0].Key != (clientLanguageKey{Client: "claude-code", Language: "go"}) || receipts[0].EntryName != entryNames["claude-code"] {
		t.Fatalf("receipts = %+v, want only the successful claude-code AddEntry", receipts)
	}
	if h.fakeClients.addEntryCount != 1 {
		t.Fatalf("AddEntry calls = %d, want 1", h.fakeClients.addEntryCount)
	}

	h.fakeClients.failAddEntryCalls = 2
	_, err = writeRegisteredClientEntries(
		[]config.ClientBinding{{Client: "codex-cli", URLPath: "/mcp"}},
		testClientFactory(), map[string]string{"codex-cli": "mcp-language-server-go"}, 9444, "go", &bytes.Buffer{}, transaction,
	)
	if err == nil {
		t.Fatal("induced AddEntry failure returned nil error")
	}
	_ = transaction.Fail(err)
}

func TestRegister_ClientPresenceTransitionNeverWritesEmptyEntryName(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	before := h.fakeClients.addEntryCount
	transaction := newRegistrationTransaction()
	receipts, err := writeRegisteredClientEntries(
		[]config.ClientBinding{{Client: "cursor", URLPath: "/mcp"}},
		testClientFactory(), map[string]string{}, 9444, "go", &bytes.Buffer{}, transaction,
	)
	if err != nil {
		t.Fatalf("presence-transition write: %v", err)
	}
	if len(receipts) != 0 || h.fakeClients.addEntryCount != before {
		t.Fatalf("empty-name transition wrote client state: receipts=%v addCalls=%d before=%d", receipts, h.fakeClients.addEntryCount, before)
	}
	if outcome := transaction.Commit(); !outcome.Committed() {
		t.Fatalf("empty write transaction commit: %v", outcome.Err)
	}
}

func TestRegisterSupervised_EveryPostAddEntryFailureDiscardsReceipts(t *testing.T) {
	tests := []struct {
		name         string
		failureStage string
		wantErr      string
		wantCalls    [4]int
		wantRestores int
	}{
		{name: "supervisor-intent-upsert", failureStage: "upsert", wantErr: "supervisor-intent-upsert failure", wantCalls: [4]int{1, 0, 0, 0}},
		{name: "running-intent-write", failureStage: "write", wantErr: "running-intent-write failure", wantCalls: [4]int{1, 1, 0, 0}, wantRestores: 1},
		{name: "reconcile", failureStage: "reconcile", wantErr: "reconcile failure", wantCalls: [4]int{1, 1, 1, 0}, wantRestores: 2},
		{name: "readiness", failureStage: "readiness", wantErr: "readiness failure", wantCalls: [4]int{1, 1, 1, 1}, wantRestores: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			m := nineLanguageManifest()
			canonical := mustCanonical(t, t.TempDir())
			h.fakeClients.stdioEntries["claude-code"]["legacy-go"] = clients.LanguageServerStdioEntry{
				Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}

			var calls [4]int
			restores := 0
			deps := supervisorPostWriteDeps{
				upsertIntent: func(WorkspaceEntry, string) (compensation, error) {
					calls[0]++
					if tt.failureStage == "upsert" {
						return nil, errors.New("injected supervisor-intent-upsert failure")
					}
					return func() error {
						restores++
						return nil
					}, nil
				},
				writeRunningIntent: func(_ string, enroll stopIntentCompensationSink) (string, error) {
					calls[1]++
					if tt.failureStage == "write" {
						return "", errors.New("injected running-intent-write failure")
					}
					enroll("restore test running intent", func() error {
						restores++
						return nil
					})
					return "\\test-task", nil
				},
				reconcile: func(context.Context, bool) (ReconcileResponse, error) {
					calls[2]++
					if tt.failureStage == "reconcile" {
						return ReconcileResponse{}, errors.New("injected reconcile failure")
					}
					return ReconcileResponse{}, nil
				},
				readiness: func(int, time.Duration) error {
					calls[3]++
					if tt.failureStage == "readiness" {
						return errors.New("injected readiness failure")
					}
					return nil
				},
			}

			var output bytes.Buffer
			transaction := newRegistrationTransaction()
			result, err := mustNewAPI(t).registerOneLanguageSupervised(
				m,
				m.Languages[2],
				canonical,
				WorkspaceKey(canonical),
				"go",
				RegisterOpts{SupervisedProxy: true, supervisorPostWriteDeps: deps, Writer: &output},
				NewRegistry(h.regPath),
				h.fakeSch,
				testClientFactory(),
				m.ClientBindings,
				&output,
				transaction,
			)
			if err != nil {
				err = transaction.Fail(err).Err
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("registerOneLanguageSupervised error = %v, want %q", err, tt.wantErr)
			}
			if h.fakeClients.addEntryCount == 0 {
				t.Fatal("injected failure occurred before AddEntry; test did not reach the guarded boundary")
			}
			if result.Entry.WorkspaceKey != "" || result.Entry.Language != "" || len(result.Receipts) != 0 {
				t.Fatalf("post-AddEntry failure leaked outward result: %+v", result)
			}
			if h.fakeClients.backupKeepCalls["claude-code"] != 0 {
				t.Fatalf("post-AddEntry failure made %d direct-cleanup backups, want 0", h.fakeClients.backupKeepCalls["claude-code"])
			}
			if calls != tt.wantCalls {
				t.Fatalf("post-write calls = %v, want %v", calls, tt.wantCalls)
			}
			if restores != tt.wantRestores {
				t.Fatalf("intent restore calls = %d, want %d", restores, tt.wantRestores)
			}
			if _, exists := h.fakeClients.stdioEntries["claude-code"]["legacy-go"]; !exists {
				t.Fatal("post-write failure reached cleanup and removed the direct entry")
			}
			for clientName, entries := range h.fakeClients.entries {
				if len(entries) != 0 {
					t.Fatalf("rollback left provisional entries for %s: %v", clientName, entries)
				}
			}
		})
	}
}

func TestSupervisorPostWriteDeps_DefaultsToCanonicalOwners(t *testing.T) {
	deps := normalizeSupervisorPostWriteDeps(mustNewAPI(t), supervisorPostWriteDeps{})
	if deps.upsertIntent == nil || deps.writeRunningIntent == nil || deps.reconcile == nil || deps.readiness == nil {
		t.Fatalf("normalized production dependencies contain a nil owner: %+v", deps)
	}

	partial := normalizeSupervisorPostWriteDeps(mustNewAPI(t), supervisorPostWriteDeps{
		readiness: func(int, time.Duration) error { return errors.New("provided-readiness") },
	})
	if partial.upsertIntent == nil || partial.writeRunningIntent == nil || partial.reconcile == nil || partial.readiness == nil {
		t.Fatalf("normalized partial dependencies contain a nil owner: %+v", partial)
	}
	if err := partial.readiness(1, time.Second); err == nil || err.Error() != "provided-readiness" {
		t.Fatalf("partial readiness override = %v, want provided-readiness", err)
	}
}

func TestSupervisorPostWriteDeps_PerCallNoCrossTalk(t *testing.T) {
	for _, label := range []string{"alpha", "beta"} {
		label := label
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			deps := normalizeSupervisorPostWriteDeps(mustNewAPI(t), supervisorPostWriteDeps{
				upsertIntent: func(WorkspaceEntry, string) (compensation, error) {
					calls.Add(1)
					return func() error { return nil }, nil
				},
				writeRunningIntent: func(string, stopIntentCompensationSink) (string, error) {
					calls.Add(1)
					return label, nil
				},
				reconcile: func(context.Context, bool) (ReconcileResponse, error) {
					calls.Add(1)
					return ReconcileResponse{}, nil
				},
				readiness: func(int, time.Duration) error {
					calls.Add(1)
					return nil
				},
			})
			if _, err := deps.upsertIntent(WorkspaceEntry{}, ""); err != nil {
				t.Fatal(err)
			}
			if got, err := deps.writeRunningIntent("", func(string, compensation) {}); err != nil || got != label {
				t.Fatalf("write result = %q, %v; want %q, nil", got, err, label)
			}
			if _, err := deps.reconcile(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if err := deps.readiness(1, time.Second); err != nil {
				t.Fatal(err)
			}
			if got := calls.Load(); got != 4 {
				t.Fatalf("per-call dependency calls = %d, want 4", got)
			}
		})
	}
}

func TestCleanupDirectLSP_FailureIsolationMatrix(t *testing.T) {
	const rawCleanupSentinel = `C:\secret\cleanup-token --password=hunter2 {raw-error}`
	h := newRegisterHarness(t)
	defer h.restore()
	canonical := mustCanonical(t, t.TempDir())
	for _, clientName := range []string{"cursor", "codex-cli"} {
		h.fakeClients.stdioEntries[clientName]["legacy-go"] = clients.LanguageServerStdioEntry{
			Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
			Args: []string{"--lsp", "gopls", "--workspace", canonical},
		}
	}
	clientsMap := testClientFactory()
	clientsMap["cursor"] = &directCleanupInjectedErrorClient{registerClient: clientsMap["cursor"], findErr: errors.New(rawCleanupSentinel)}
	selected := map[clientLanguageKey]bool{
		{Client: "cursor", Language: "go"}:    true,
		{Client: "codex-cli", Language: "go"}: true,
	}
	receipts := []clientWriteReceipt{
		{Key: clientLanguageKey{Client: "cursor", Language: "go"}, EntryName: "router-go"},
		{Key: clientLanguageKey{Client: "codex-cli", Language: "go"}, EntryName: "router-go"},
	}
	var scanWriter bytes.Buffer
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
		[]string{"go"}, canonical, clientsMap, selected, receipts, &scanWriter,
		directCleanupPlanDeps{matchDirect: directLanguageServerCleanupMatches},
	)
	if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists {
		t.Fatal("failed cursor plan was mutated")
	}
	if _, exists := h.fakeClients.stdioEntries["codex-cli"]["legacy-go"]; exists {
		t.Fatal("independent codex-cli plan was suppressed by cursor failure")
	}
	if h.fakeClients.backupKeepCalls["cursor"] != 0 || h.fakeClients.backupKeepCalls["codex-cli"] != 1 {
		t.Fatalf("backup calls cursor/codex=%d/%d, want 0/1; warnings=%v", h.fakeClients.backupKeepCalls["cursor"], h.fakeClients.backupKeepCalls["codex-cli"], warnings)
	}
	scanPublic := strings.Join(warnings, "\n") + "\n" + scanWriter.String()
	if strings.Contains(scanPublic, rawCleanupSentinel) || strings.Contains(scanPublic, "hunter2") {
		t.Fatalf("scan warning leaked raw cause: warnings=%v writer=%q", warnings, scanWriter.String())
	}

	t.Run("route failure is plan local", func(t *testing.T) {
		h := newRegisterHarness(t)
		defer h.restore()
		canonical := mustCanonical(t, t.TempDir())
		for _, clientName := range []string{"cursor", "codex-cli"} {
			h.fakeClients.entries[clientName][LSPRouterEntryName("go")] = "http://127.0.0.1:19125/lsp/go/mcp"
			h.fakeClients.entries[clientName][LSPRouterEntryName("python")] = "http://127.0.0.1:19126/lsp/python/mcp"
			h.fakeClients.stdioEntries[clientName]["legacy-go"] = clients.LanguageServerStdioEntry{
				Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}
			h.fakeClients.stdioEntries[clientName]["legacy-python"] = clients.LanguageServerStdioEntry{
				Name: "legacy-python", Command: "mcp-language-server", Language: "pylsp",
				Args: []string{"--lsp", "pylsp", "--workspace", canonical},
			}
		}
		warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
			map[string]config.LanguageSpec{
				"go":     {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"},
				"python": {Name: "python", Backend: "pylsp-mcp", LspCommand: "pylsp"},
			},
			[]string{"go", "python"}, canonical, testClientFactory(), nil, nil, &bytes.Buffer{},
			directCleanupPlanDeps{
				authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
					return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
				},
				probeRoute: func(_ context.Context, _ int, language, _ string) managedRouteProof {
					if language == "go" {
						return managedRouteProof{FailureClass: "route-jsonrpc-error"}
					}
					return managedRouteProof{OK: true}
				},
				matchDirect: directLanguageServerCleanupMatches,
			},
		)
		for _, clientName := range []string{"cursor", "codex-cli"} {
			if _, exists := h.fakeClients.stdioEntries[clientName]["legacy-go"]; !exists {
				t.Fatalf("%s go plan was removed through failed route proof", clientName)
			}
			if _, exists := h.fakeClients.stdioEntries[clientName]["legacy-python"]; exists {
				t.Fatalf("%s independent python plan was suppressed; warnings=%v", clientName, warnings)
			}
		}
	})

	t.Run("backup failure is client local", func(t *testing.T) {
		h := newRegisterHarness(t)
		defer h.restore()
		canonical := mustCanonical(t, t.TempDir())
		for _, clientName := range []string{"cursor", "codex-cli"} {
			h.fakeClients.stdioEntries[clientName]["legacy-go"] = clients.LanguageServerStdioEntry{
				Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}
		}
		clientsMap := testClientFactory()
		clientsMap["cursor"] = &cleanupMutationInjectedClient{registerClient: clientsMap["cursor"], backupErr: errors.New(rawCleanupSentinel)}
		keys := map[clientLanguageKey]bool{
			{Client: "cursor", Language: "go"}: true, {Client: "codex-cli", Language: "go"}: true,
		}
		receipts := []clientWriteReceipt{
			{Key: clientLanguageKey{Client: "cursor", Language: "go"}, EntryName: "router-go"},
			{Key: clientLanguageKey{Client: "codex-cli", Language: "go"}, EntryName: "router-go"},
		}
		var backupWriter bytes.Buffer
		warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
			map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
			[]string{"go"}, canonical, clientsMap, keys, receipts, &backupWriter,
			directCleanupPlanDeps{matchDirect: directLanguageServerCleanupMatches},
		)
		if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists {
			t.Fatal("backup-failed cursor entry was removed")
		}
		if _, exists := h.fakeClients.stdioEntries["codex-cli"]["legacy-go"]; exists {
			t.Fatalf("independent codex client was suppressed; warnings=%v", warnings)
		}
		backupPublic := strings.Join(warnings, "\n") + "\n" + backupWriter.String()
		if strings.Contains(backupPublic, rawCleanupSentinel) || strings.Contains(backupPublic, "hunter2") {
			t.Fatalf("backup warning leaked raw cause: warnings=%v writer=%q", warnings, backupWriter.String())
		}
	})

	t.Run("removal failure is entry local", func(t *testing.T) {
		h := newRegisterHarness(t)
		defer h.restore()
		canonical := mustCanonical(t, t.TempDir())
		for _, name := range []string{"legacy-go", "legacy-go-second"} {
			h.fakeClients.stdioEntries["cursor"][name] = clients.LanguageServerStdioEntry{
				Name: name, Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}
		}
		clientsMap := testClientFactory()
		clientsMap["cursor"] = &cleanupMutationInjectedClient{
			registerClient: clientsMap["cursor"], removeErrName: "legacy-go", removeErr: errors.New(rawCleanupSentinel),
		}
		key := clientLanguageKey{Client: "cursor", Language: "go"}
		var removeWriter bytes.Buffer
		transaction := newRegistrationTransaction()
		warnings, cleanupErr := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDepsTransaction(
			map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
			[]string{"go"}, canonical, clientsMap, map[clientLanguageKey]bool{key: true},
			[]clientWriteReceipt{{Key: key, EntryName: "router-go"}}, &removeWriter,
			directCleanupPlanDeps{matchDirect: directLanguageServerCleanupMatches}, transaction,
		)
		if cleanupErr == nil {
			outcome := transaction.Commit()
			cleanupErr = outcome.Err
		} else {
			cleanupErr = transaction.Fail(cleanupErr).Err
		}
		if cleanupErr != nil {
			t.Fatalf("entry-local removal transaction failed: %v", cleanupErr)
		}
		if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists {
			t.Fatal("failed removal did not preserve its entry")
		}
		if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go-second"]; exists {
			t.Fatalf("independent removal was suppressed; warnings=%v", warnings)
		}
		removePublic := strings.Join(warnings, "\n") + "\n" + removeWriter.String()
		if strings.Contains(removePublic, rawCleanupSentinel) || strings.Contains(removePublic, "hunter2") {
			t.Fatalf("remove warning leaked raw cause: warnings=%v writer=%q", warnings, removeWriter.String())
		}
	})
}

func TestCleanupDirectLSP_ClientWideIndeterminatePreservesClientAndContinuesOthers(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	canonical := mustCanonical(t, t.TempDir())
	for _, clientName := range []string{"cursor", "codex-cli"} {
		h.fakeClients.stdioEntries[clientName]["legacy-go"] = clients.LanguageServerStdioEntry{
			Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
			Args: []string{"--lsp", "gopls", "--workspace", canonical},
		}
	}
	clientsMap := testClientFactory()
	clientsMap["cursor"] = &directCleanupInjectedErrorClient{
		registerClient: clientsMap["cursor"],
		survivorErr:    errors.New("injected client-wide survivor scan failure"),
	}
	selected := map[clientLanguageKey]bool{
		{Client: "cursor", Language: "go"}:    true,
		{Client: "codex-cli", Language: "go"}: true,
	}
	receipts := []clientWriteReceipt{
		{Key: clientLanguageKey{Client: "cursor", Language: "go"}, EntryName: "router-go"},
		{Key: clientLanguageKey{Client: "codex-cli", Language: "go"}, EntryName: "router-go"},
	}
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
		[]string{"go"}, canonical, clientsMap, selected, receipts, &bytes.Buffer{},
		directCleanupPlanDeps{matchDirect: directLanguageServerCleanupMatches},
	)
	if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists {
		t.Fatal("client-wide-indeterminate cursor plan was mutated")
	}
	if _, exists := h.fakeClients.stdioEntries["codex-cli"]["legacy-go"]; exists {
		t.Fatalf("independent codex-cli plan was suppressed; warnings=%v", warnings)
	}
	if !slices.ContainsFunc(warnings, func(v string) bool { return strings.HasPrefix(v, "survivor-scan-failed:") }) {
		t.Fatalf("warnings=%v, want survivor-scan-failed discriminator", warnings)
	}
}

func TestCleanupDirectLSP_PingOnlyServerFailsLanguageRouteOracle(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	_, port := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"pid":1,"version":"test"}`)
			return
		}
		http.NotFound(w, r)
	})
	canonical := mustCanonical(t, t.TempDir())
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] = fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
	h.fakeClients.stdioEntries["cursor"]["legacy-go"] = clients.LanguageServerStdioEntry{
		Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
		Args: []string{"--lsp", "gopls", "--workspace", canonical},
	}
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
		[]string{"go"}, canonical, testClientFactory(), nil, nil, &bytes.Buffer{},
		directCleanupPlanDeps{
			authorizeRouter: allowManagedRouterAuthorizerForTest(port),
			probeRoute:      probeManagedLanguageRoute,
			matchDirect:     directLanguageServerCleanupMatches,
		},
	)
	if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists {
		t.Fatal("ping-only server authorized direct-entry removal")
	}
	if h.fakeClients.backupKeepCalls["cursor"] != 0 || !slices.ContainsFunc(warnings, func(v string) bool { return strings.Contains(v, "route-http-status") }) {
		t.Fatalf("ping-only result backups=%d warnings=%v", h.fakeClients.backupKeepCalls["cursor"], warnings)
	}
}

func TestCleanupDirectLSP_RealGUIRouteToolsListPassesOracle(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	_, port := newReadinessHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lsp/go/mcp" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response, err := SyntheticToolsListResponse(request.ID, "gopls-mcp")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	})
	canonical := mustCanonical(t, t.TempDir())
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] = fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
	h.fakeClients.stdioEntries["cursor"]["legacy-go"] = clients.LanguageServerStdioEntry{
		Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
		Args: []string{"--lsp", "gopls", "--workspace", canonical},
	}
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
		[]string{"go"}, canonical, testClientFactory(), nil, nil, &bytes.Buffer{},
		directCleanupPlanDeps{
			authorizeRouter: allowManagedRouterAuthorizerForTest(port),
			probeRoute:      probeManagedLanguageRoute,
			matchDirect:     directLanguageServerCleanupMatches,
		},
	)
	if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; exists {
		t.Fatalf("proven real-route-shape cleanup did not remove entry; warnings=%v", warnings)
	}
	if h.fakeClients.backupKeepCalls["cursor"] != 1 {
		t.Fatalf("backup calls=%d, want 1", h.fakeClients.backupKeepCalls["cursor"])
	}
}

func TestCleanupDirectLSP_RevalidationFailurePreservesAffectedRouterMatches(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	canonical := mustCanonical(t, t.TempDir())
	const port = 9125
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] = fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
	h.fakeClients.stdioEntries["cursor"]["legacy-go"] = clients.LanguageServerStdioEntry{
		Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
		Args: []string{"--lsp", "gopls", "--workspace", canonical},
	}
	var authorizations, revalidations int
	authorizer := func(context.Context, int) ManagedRouterAuthorization {
		authorizations++
		return ManagedRouterAuthorization{Lease: &testManagedRouterLease{
			revalidate: func(context.Context) string {
				revalidations++
				return ManagedRouterFailureIdentityChanged
			},
		}}
	}
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
		[]string{"go"}, canonical, testClientFactory(), nil, nil, &bytes.Buffer{},
		directCleanupPlanDeps{
			authorizeRouter: authorizer,
			probeRoute:      func(context.Context, int, string, string) managedRouteProof { return managedRouteProof{OK: true} },
			matchDirect:     directLanguageServerCleanupMatches,
		},
	)
	if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists || h.fakeClients.backupKeepCalls["cursor"] != 0 {
		t.Fatalf("revalidation failure mutated entry: exists=%v backups=%d warnings=%v", exists, h.fakeClients.backupKeepCalls["cursor"], warnings)
	}
	if authorizations != 1 || revalidations != 1 {
		t.Fatalf("lease acquisition/revalidation = %d/%d, want 1/1", authorizations, revalidations)
	}
}

func TestCleanupDirectLSP_RouteRevalidationFailurePreservesAffectedPlanAndContinuesReceiptPlan(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	canonical := mustCanonical(t, t.TempDir())
	const port = 19131
	for _, clientName := range []string{"cursor", "claude-code"} {
		h.fakeClients.stdioEntries[clientName]["legacy-go"] = clients.LanguageServerStdioEntry{
			Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
			Args: []string{"--lsp", "gopls", "--workspace", canonical},
		}
	}
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] = fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
	authorizerCalls, routeCalls := 0, 0
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t), []string{"go"}, canonical, testClientFactory(),
		map[clientLanguageKey]bool{{Client: "claude-code", Language: "go"}: true},
		[]clientWriteReceipt{{Key: clientLanguageKey{Client: "claude-code", Language: "go"}, EntryName: "legacy-go"}},
		&bytes.Buffer{}, directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				authorizerCalls++
				return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				routeCalls++
				if routeCalls == 1 {
					return managedRouteProof{OK: true}
				}
				return managedRouteProof{FailureClass: "route-jsonrpc-error"}
			},
			matchDirect: directLanguageServerCleanupMatches,
		},
	)
	if authorizerCalls != 1 || routeCalls != 2 {
		t.Fatalf("revalidation calls authorizer/route=%d/%d, want 1/2 when post-backup route proof fails", authorizerCalls, routeCalls)
	}
	if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists || h.fakeClients.backupKeepCalls["cursor"] != 1 {
		t.Fatalf("route-revalidation failure mutated router plan: exists=%v backups=%d", exists, h.fakeClients.backupKeepCalls["cursor"])
	}
	if _, exists := h.fakeClients.stdioEntries["claude-code"]["legacy-go"]; exists || h.fakeClients.backupKeepCalls["claude-code"] != 1 {
		t.Fatalf("independent receipt plan did not proceed: exists=%v backups=%d", exists, h.fakeClients.backupKeepCalls["claude-code"])
	}
	want := "route-jsonrpc-error: affected_plans=1 [client=cursor,language=go,port=19131]; keeping matching direct LSP entries"
	if !slices.Contains(warnings, want) {
		t.Fatalf("warnings=%v, want %q", warnings, want)
	}
}

func TestManagedCleanup_TakeoverCheckpointMatrix(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	canonical := mustCanonical(t, t.TempDir())
	const (
		goPort     = 19131
		pythonPort = 19132
	)
	for language, port := range map[string]int{"go": goPort, "python": pythonPort} {
		h.fakeClients.entries["cursor"][LSPRouterEntryName(language)] = fmt.Sprintf("http://127.0.0.1:%d/lsp/%s/mcp", port, language)
	}
	for name, entry := range map[string]clients.LanguageServerStdioEntry{
		"legacy-go": {
			Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
			Args: []string{"--lsp", "gopls", "--workspace", canonical},
		},
		"legacy-python": {
			Name: "legacy-python", Command: "mcp-language-server", Language: "pylsp",
			Args: []string{"--lsp", "pylsp", "--workspace", canonical},
		},
		"legacy-rust": {
			Name: "legacy-rust", Command: "mcp-language-server", Language: "rust-analyzer",
			Args: []string{"--lsp", "rust-analyzer", "--workspace", canonical},
		},
	} {
		h.fakeClients.stdioEntries["cursor"][name] = entry
	}

	events := []string{}
	clientsMap := testClientFactory()
	clientsMap["cursor"] = &cleanupMutationLedgerClient{registerClient: clientsMap["cursor"], events: &events}
	authorizerCalls := map[int]int{}
	revalidationCalls := map[int]int{}
	routeCalls := 0
	replacementInjected := false
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		map[string]config.LanguageSpec{
			"go":     {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"},
			"python": {Name: "python", Backend: "pylsp-mcp", LspCommand: "pylsp"},
			"rust":   {Name: "rust", Backend: "rust-mcp", LspCommand: "rust-analyzer"},
		},
		[]string{"go", "python", "rust"}, canonical, clientsMap,
		map[clientLanguageKey]bool{{Client: "cursor", Language: "rust"}: true},
		[]clientWriteReceipt{{Key: clientLanguageKey{Client: "cursor", Language: "rust"}, EntryName: "router-rust"}},
		&bytes.Buffer{}, directCleanupPlanDeps{
			authorizeRouter: func(_ context.Context, port int) ManagedRouterAuthorization {
				authorizerCalls[port]++
				events = append(events, fmt.Sprintf("authorize:%d", port))
				return ManagedRouterAuthorization{Lease: &testManagedRouterLease{
					revalidate: func(context.Context) string {
						revalidationCalls[port]++
						events = append(events, fmt.Sprintf("revalidate:%d:%d", port, revalidationCalls[port]))
						if port == goPort && replacementInjected {
							return ManagedRouterFailureIdentityChanged
						}
						return ""
					},
				}}
			},
			probeRoute: func(_ context.Context, port int, language, _ string) managedRouteProof {
				routeCalls++
				events = append(events, fmt.Sprintf("route:%s:%d:%d", language, port, routeCalls))
				if routeCalls == 4 {
					replacementInjected = true
					events = append(events, "route:settled")
				}
				return managedRouteProof{OK: true}
			},
			matchDirect: directLanguageServerCleanupMatches,
		},
	)

	if routeCalls != 7 ||
		authorizerCalls[goPort] != 1 ||
		authorizerCalls[pythonPort] != 1 ||
		revalidationCalls[goPort] != 3 ||
		revalidationCalls[pythonPort] != 5 {
		t.Fatalf(
			"proof calls routes=%d authorizer=%v revalidation=%v, want routes=7 authorizer=1/port revalidation=go:3 python:5; events=%v",
			routeCalls, authorizerCalls, revalidationCalls, events,
		)
	}
	wantTail := []string{
		"route:settled",
		fmt.Sprintf("revalidate:%d:3", goPort),
	}
	settled := slices.Index(events, "route:settled")
	if settled < 0 || settled+len(wantTail) > len(events) {
		t.Fatalf("missing route-settled/takeover boundary; events=%v", events)
	}
	if got := events[settled : settled+len(wantTail)]; !slices.Equal(got, wantTail) {
		t.Fatalf("takeover validation block = %v, want %v; full events=%v", got, wantTail, events)
	}
	if _, exists := h.fakeClients.stdioEntries["cursor"]["legacy-go"]; !exists {
		t.Fatal("replacement after route settlement removed the affected go entry")
	}
	for _, name := range []string{"legacy-python", "legacy-rust"} {
		if _, exists := h.fakeClients.stdioEntries["cursor"][name]; exists {
			t.Fatalf("independent eligible plan %q did not continue; warnings=%v events=%v", name, warnings, events)
		}
	}
	if h.fakeClients.backupKeepCalls["cursor"] != 1 || slices.Contains(events, "remove:legacy-go") {
		t.Fatalf("mutation batch backup=%d events=%v, want one backup and zero affected-go removals", h.fakeClients.backupKeepCalls["cursor"], events)
	}
	wantWarning := "identity-changed: affected_plans=1 [client=cursor,language=go,port=19131]; keeping matching direct LSP entries"
	if !slices.Contains(warnings, wantWarning) {
		t.Fatalf("warnings=%v, want %q", warnings, wantWarning)
	}
}

func TestManagedCleanup_LeaseFinalizerFailureRollsBack(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	canonical := mustCanonical(t, t.TempDir())
	const (
		port      = 19135
		entryName = "legacy-go-close-failure"
	)
	h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
		fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
	h.fakeClients.stdioEntries["cursor"][entryName] = clients.LanguageServerStdioEntry{
		Name: entryName, Command: "mcp-language-server", Language: "gopls",
		Args: []string{"--lsp", "gopls", "--workspace", canonical},
	}
	closeErr := errors.New("injected lease close failure")
	closeCalls := 0
	lease := &testManagedRouterLease{
		close: func() error {
			closeCalls++
			return closeErr
		},
	}
	var output bytes.Buffer
	transaction := newRegistrationTransaction()
	warnings, err := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDepsTransaction(
		goCleanupSpecForTest(t),
		[]string{"go"},
		canonical,
		testClientFactory(),
		nil,
		nil,
		&output,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				return ManagedRouterAuthorization{Lease: lease}
			},
			probeRoute:  func(context.Context, int, string, string) managedRouteProof { return managedRouteProof{OK: true} },
			matchDirect: directLanguageServerCleanupMatches,
		},
		transaction,
	)
	if err != nil {
		t.Fatalf("cleanup before settlement: %v", err)
	}
	if _, exists := h.fakeClients.stdioEntries["cursor"][entryName]; exists {
		t.Fatal("test did not reach destructive removal before lease settlement")
	}
	outcome := transaction.Commit()
	if outcome.State != registrationTransactionRolledBack || !errors.Is(outcome.Err, closeErr) {
		t.Fatalf("outcome = %+v, want rolled-back lease close failure", outcome)
	}
	if closeCalls != 1 {
		t.Fatalf("lease close calls = %d, want exactly 1", closeCalls)
	}
	if _, exists := h.fakeClients.stdioEntries["cursor"][entryName]; !exists {
		t.Fatal("lease close failure did not restore the removed direct entry")
	}
	if output.Len() != 0 {
		t.Fatalf("lease close failure exposed success output: %q", output.String())
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected cleanup warnings before settlement: %v", warnings)
	}
}

type managedRouteDoerFunc func(*http.Request) (*http.Response, error)

func (f managedRouteDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

type managedRouteReadCloser struct {
	reader io.Reader
	closed *bool
}

func (r *managedRouteReadCloser) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r *managedRouteReadCloser) Close() error {
	*r.closed = true
	return nil
}

type managedRouteErrorReader struct{}

func (managedRouteErrorReader) Read([]byte) (int, error) { return 0, errors.New("read sentinel") }

func TestProbeManagedLanguageRoute_RefusalMatrix(t *testing.T) {
	type responseSpec struct {
		status      int
		contentType string
		body        func(string) string
		readErr     bool
	}
	exact := func(id string) string {
		response, err := SyntheticToolsListResponse(json.RawMessage(strconv.Quote(id)), "gopls-mcp")
		if err != nil {
			t.Fatalf("synthetic response: %v", err)
		}
		return string(response)
	}
	base := responseSpec{status: http.StatusOK, contentType: "application/json", body: exact}
	tests := []struct {
		name         string
		port         int
		backend      string
		entropy      io.Reader
		catalog      func(string) (ToolCatalog, bool)
		response     responseSpec
		transportErr error
		want         string
		wantOK       bool
		wantDoCalls  int
	}{
		{name: "invalid port", port: 0, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: base, want: "route-transport"},
		{name: "entropy error", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(nil), response: base, want: "route-transport"},
		{name: "transport refused", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: base, transportErr: errors.New("refused"), want: "route-transport", wantDoCalls: 1},
		{name: "response read error", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", readErr: true}, want: "route-transport", wantDoCalls: 1},
		{name: "unknown catalog", port: 9125, backend: "unknown", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: base, want: "route-catalog-mismatch"},
		{name: "expected catalog missing", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), catalog: func(string) (ToolCatalog, bool) { return ToolCatalog{}, true }, response: base, want: "route-catalog-mismatch"},
		{name: "expected catalog duplicate", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), catalog: func(string) (ToolCatalog, bool) {
			return ToolCatalog{Tools: []ToolSchema{{Name: "dup"}, {Name: "dup"}}}, true
		}, response: base, want: "route-catalog-mismatch", wantDoCalls: 1},
		{name: "created with exact body", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: http.StatusCreated, contentType: "application/json", body: exact}, want: "route-http-status", wantDoCalls: 1},
		{name: "redirect", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 302, contentType: "application/json", body: func(string) string { return `{}` }}, want: "route-http-status", wantDoCalls: 1},
		{name: "non-2xx", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 503, contentType: "application/json", body: func(string) string { return `{}` }}, want: "route-http-status", wantDoCalls: 1},
		{name: "missing content type", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, body: exact}, want: "route-content-type", wantDoCalls: 1},
		{name: "malformed content type", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json; =", body: exact}, want: "route-content-type", wantDoCalls: 1},
		{name: "non-json content type", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "text/plain", body: exact}, want: "route-content-type", wantDoCalls: 1},
		{name: "oversized", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(string) string { return strings.Repeat("x", managedRouterRouteBodyMax+1) }}, want: "route-response-too-large", wantDoCalls: 1},
		{name: "malformed json", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(string) string { return `{` }}, want: "route-malformed", wantDoCalls: 1},
		{name: "trailing object", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string { return exact(id) + `{}` }}, want: "route-malformed", wantDoCalls: 1},
		{name: "wrong jsonrpc", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string {
			return fmt.Sprintf(`{"jsonrpc":"1.0","id":%q,"result":{"tools":[{"name":"go_workspace"}]}}`, id)
		}}, want: "route-malformed", wantDoCalls: 1},
		{name: "missing id", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(string) string { return `{"jsonrpc":"2.0","result":{"tools":[{"name":"go_workspace"}]}}` }}, want: "route-id-mismatch", wantDoCalls: 1},
		{name: "wrong id type", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(string) string { return `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"go_workspace"}]}}` }}, want: "route-id-mismatch", wantDoCalls: 1},
		{name: "wrong id value", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(string) string {
			return `{"jsonrpc":"2.0","id":"wrong","result":{"tools":[{"name":"go_workspace"}]}}`
		}}, want: "route-id-mismatch", wantDoCalls: 1},
		{name: "jsonrpc error", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-1},"result":{"tools":[]}}`, id)
		}}, want: "route-jsonrpc-error", wantDoCalls: 1},
		{name: "missing tools", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{}}`, id) }}, want: "route-tools-invalid", wantDoCalls: 1},
		{name: "blank tool", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"tools":[{"name":" "}]}}`, id)
		}}, want: "route-tools-invalid", wantDoCalls: 1},
		{name: "duplicate tool", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"tools":[{"name":"go_workspace"},{"name":"go_workspace"}]}}`, id)
		}}, want: "route-tools-invalid", wantDoCalls: 1},
		{name: "unknown tool", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"tools":[{"name":"unknown"}]}}`, id)
		}}, want: "route-tools-invalid", wantDoCalls: 1},
		{name: "incomplete catalog", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: responseSpec{status: 200, contentType: "application/json", body: func(id string) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"tools":[{"name":"go_workspace"}]}}`, id)
		}}, want: "route-catalog-mismatch", wantDoCalls: 1},
		{name: "success", port: 9125, backend: "gopls-mcp", entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), response: base, wantOK: true, wantDoCalls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doCalls, closed := 0, false
			doer := managedRouteDoerFunc(func(req *http.Request) (*http.Response, error) {
				doCalls++
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				var request struct {
					ID string `json:"id"`
				}
				if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				var reader io.Reader
				if tc.response.readErr {
					reader = managedRouteErrorReader{}
				} else {
					reader = strings.NewReader(tc.response.body(request.ID))
				}
				return &http.Response{StatusCode: tc.response.status, Header: http.Header{"Content-Type": []string{tc.response.contentType}}, Body: &managedRouteReadCloser{reader: reader, closed: &closed}}, nil
			})
			catalog := tc.catalog
			if catalog == nil {
				catalog = ToolCatalogForBackend
			}
			got := probeManagedLanguageRouteWithDeps(context.Background(), tc.port, "go", tc.backend, managedRouteProbeDeps{do: doer, entropy: tc.entropy, catalog: catalog})
			if got.OK != tc.wantOK || got.FailureClass != tc.want {
				t.Fatalf("proof=%+v, want ok=%v class=%q", got, tc.wantOK, tc.want)
			}
			if doCalls != tc.wantDoCalls {
				t.Fatalf("Do calls=%d, want %d", doCalls, tc.wantDoCalls)
			}
			if doCalls > 0 && tc.transportErr == nil && !closed {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestCleanupDirectLSP_Alternate2xxRouteRefusalDoesNotMutate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantMutate bool
	}{
		{name: "201 refuses", status: http.StatusCreated},
		{name: "200 authorizes", status: http.StatusOK, wantMutate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			canonical := mustCanonical(t, t.TempDir())
			const (
				port      = 19134
				entryName = "legacy-go-exact-status"
			)
			h.fakeClients.entries["cursor"][LSPRouterEntryName("go")] =
				fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
			h.fakeClients.stdioEntries["cursor"][entryName] = clients.LanguageServerStdioEntry{
				Name: entryName, Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}
			var mutationEvents []string
			client := &cleanupMutationLedgerClient{
				registerClient: testClientFactory()["cursor"],
				events:         &mutationEvents,
			}
			probe := func(ctx context.Context, candidatePort int, language, backend string) managedRouteProof {
				doer := managedRouteDoerFunc(func(req *http.Request) (*http.Response, error) {
					var request struct {
						ID string `json:"id"`
					}
					if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
						t.Fatalf("decode route request: %v", err)
					}
					body, err := SyntheticToolsListResponse(json.RawMessage(strconv.Quote(request.ID)), backend)
					if err != nil {
						t.Fatalf("synthetic tools/list response: %v", err)
					}
					return &http.Response{
						StatusCode: tc.status,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(bytes.NewReader(body)),
					}, nil
				})
				return probeManagedLanguageRouteWithDeps(ctx, candidatePort, language, backend, managedRouteProbeDeps{
					do: doer, entropy: bytes.NewReader(make([]byte, managedRouterRequestIDBytes)), catalog: ToolCatalogForBackend,
				})
			}
			warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
				goCleanupSpecForTest(t), []string{"go"}, canonical,
				map[string]registerClient{"cursor": client}, nil, nil, &bytes.Buffer{},
				directCleanupPlanDeps{
					authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
						return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
					},
					probeRoute: probe, matchDirect: directLanguageServerCleanupMatches,
				},
			)
			_, stillThere := h.fakeClients.stdioEntries["cursor"][entryName]
			if tc.wantMutate {
				if stillThere || !slices.Equal(mutationEvents, []string{"backup", "remove:" + entryName}) {
					t.Fatalf("200 mutation state: stillThere=%v events=%v warnings=%v", stillThere, mutationEvents, warnings)
				}
				return
			}
			if !stillThere || len(mutationEvents) != 0 || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "route-http-status:") {
				t.Fatalf("201 refusal state: stillThere=%v events=%v warnings=%v", stillThere, mutationEvents, warnings)
			}
		})
	}
}

func TestCleanupDirectLSP_ProofWarningAggregatesAllPlansByClass(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	canonical := mustCanonical(t, t.TempDir())
	const port = 19132
	for _, clientName := range []string{"cursor", "codex-cli"} {
		h.fakeClients.entries[clientName][LSPRouterEntryName("go")] = fmt.Sprintf("http://127.0.0.1:%d/lsp/go/mcp", port)
		h.fakeClients.stdioEntries[clientName]["legacy-go"] = clients.LanguageServerStdioEntry{Name: "legacy-go", Command: "mcp-language-server", Language: "gopls", Args: []string{"--lsp", "gopls", "--workspace", canonical}}
	}
	var writer bytes.Buffer
	warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t), []string{"go"}, canonical, testClientFactory(), nil, nil, &writer,
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				return managedRouteProof{FailureClass: "route-jsonrpc-error"}
			},
			matchDirect: directLanguageServerCleanupMatches,
		},
	)
	want := "route-jsonrpc-error: affected_plans=2 [client=codex-cli,language=go,port=19132; client=cursor,language=go,port=19132]; keeping matching direct LSP entries"
	assertCleanupWarningCount(t, warnings, writer.String(), want, 1, 1)

	rawSentinel := `C:\\secret\\token --password=hunter2 {raw-body}`
	warnings = mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t), []string{"go"}, canonical, map[string]registerClient{"cursor": testClientFactory()["cursor"]}, nil, nil, &bytes.Buffer{},
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				return ManagedRouterAuthorization{FailureClass: rawSentinel}
			},
			probeRoute:  func(context.Context, int, string, string) managedRouteProof { return managedRouteProof{OK: true} },
			matchDirect: directLanguageServerCleanupMatches,
		},
	)
	if len(warnings) != 1 || strings.Contains(warnings[0], rawSentinel) || strings.Contains(warnings[0], "hunter2") || !strings.HasPrefix(warnings[0], "identity-unavailable:") {
		t.Fatalf("unsanitized or incomplete proof aggregate: %v", warnings)
	}

	warnings = mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
		goCleanupSpecForTest(t), []string{"go"}, canonical, map[string]registerClient{"cursor": testClientFactory()["cursor"]}, nil, nil, &bytes.Buffer{},
		directCleanupPlanDeps{
			authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
				return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
			},
			probeRoute: func(context.Context, int, string, string) managedRouteProof {
				return managedRouteProof{FailureClass: rawSentinel}
			},
			matchDirect: directLanguageServerCleanupMatches,
		},
	)
	if len(warnings) != 1 || strings.Contains(warnings[0], rawSentinel) || strings.Contains(warnings[0], "hunter2") || !strings.HasPrefix(warnings[0], "route-transport:") {
		t.Fatalf("unsafe route fallback aggregate: %v", warnings)
	}
}

func TestCleanupDirectLSP_RouterReadFailureWithZeroDirectCandidatesIsSilent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		matches  []clients.LanguageServerStdioEntry
		wantWarn bool
	}{
		{name: "zero candidates"},
		{name: "one candidate", matches: []clients.LanguageServerStdioEntry{{Name: "legacy-go", Language: "gopls"}}, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			h.fakeClients.failGetEntry["cursor"] = true
			authorizerCalls, routeCalls, matcherCalls := 0, 0, 0
			warnings := mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
				goCleanupSpecForTest(t), []string{"go"}, mustCanonical(t, t.TempDir()),
				map[string]registerClient{"cursor": testClientFactory()["cursor"]}, nil, nil, &bytes.Buffer{},
				directCleanupPlanDeps{
					authorizeRouter: func(context.Context, int) ManagedRouterAuthorization {
						authorizerCalls++
						return ManagedRouterAuthorization{Lease: stableManagedRouterLeaseForTest()}
					},
					probeRoute: func(context.Context, int, string, string) managedRouteProof {
						routeCalls++
						return managedRouteProof{OK: true}
					},
					matchDirect: func(registerClient, string, map[string]bool, string) directCleanupMatchResult {
						matcherCalls++
						return directCleanupMatchResult{matches: tc.matches, complete: true}
					},
				},
			)
			if matcherCalls != 1 || authorizerCalls != 0 || routeCalls != 0 || h.fakeClients.backupKeepCalls["cursor"] != 0 {
				t.Fatalf("calls matcher/authorizer/route/backup=%d/%d/%d/%d", matcherCalls, authorizerCalls, routeCalls, h.fakeClients.backupKeepCalls["cursor"])
			}
			if tc.wantWarn != (len(warnings) == 1) {
				t.Fatalf("warnings=%v, wantWarn=%v", warnings, tc.wantWarn)
			}
			if !tc.wantWarn && len(warnings) != 0 {
				t.Fatalf("zero-candidate plan emitted warning: %v", warnings)
			}
		})
	}
}

func TestRegisterReportJSONExactKeys(t *testing.T) {
	entry := WorkspaceEntry{WorkspaceKey: "key", WorkspacePath: "path", Language: "go", Backend: "gopls-mcp", Port: 9125, TaskName: "task", ClientEntries: map[string]string{"cursor": "entry"}, WeeklyRefresh: true, Lifecycle: "ready", LastMaterializedAt: time.Unix(1, 0).UTC(), LastToolsCallAt: time.Unix(2, 0).UTC(), LastError: "err", RegisteredAt: time.Unix(3, 0).UTC(), RegisteredVia: "manual", Languages: []string{"go"}}
	for _, tc := range []struct {
		name    string
		report  RegisterReport
		wantTop []string
	}{
		{name: "clean", report: RegisterReport{Workspace: "path", WorkspaceKey: "key", Entries: []WorkspaceEntry{entry}}, wantTop: []string{"entries", "workspace", "workspace_key"}},
		{name: "warning", report: RegisterReport{Workspace: "path", WorkspaceKey: "key", Entries: []WorkspaceEntry{entry}, Warnings: []string{"warning"}}, wantTop: []string{"entries", "warnings", "workspace", "workspace_key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := sortedAnyMapKeys(decoded); !slices.Equal(got, tc.wantTop) {
				t.Fatalf("top keys=%v want=%v", got, tc.wantTop)
			}
			entries := decoded["entries"].([]any)
			nested := entries[0].(map[string]any)
			wantNested := []string{"Backend", "ClientEntries", "Language", "Languages", "LastError", "LastMaterializedAt", "LastToolsCallAt", "Lifecycle", "Port", "RegisteredAt", "RegisteredVia", "TaskName", "WeeklyRefresh", "WorkspaceKey", "WorkspacePath"}
			if got := sortedAnyMapKeys(nested); !slices.Equal(got, wantNested) {
				t.Fatalf("entry keys=%v want=%v", got, wantNested)
			}
		})
	}
}

func sortedAnyMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type orderedCleanupClient struct {
	registerClient
	calls     *[]string
	backupErr error
}

func (c *orderedCleanupClient) BackupKeep(int) (string, error) {
	*c.calls = append(*c.calls, "backup")
	if c.backupErr != nil {
		return "", c.backupErr
	}
	return "/backup/test", nil
}

func (c *orderedCleanupClient) RemoveEntry(name string) error {
	*c.calls = append(*c.calls, "remove:"+name)
	return c.registerClient.RemoveEntry(name)
}

func TestCleanupDirectLSP_BackupBeforeRemovePerClient(t *testing.T) {
	for _, tc := range []struct {
		name      string
		backupErr error
		wantCalls []string
	}{
		{name: "success", wantCalls: []string{"backup", "remove:legacy-go"}},
		{name: "backup failure", backupErr: errors.New("injected backup failure"), wantCalls: []string{"backup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			canonical := mustCanonical(t, t.TempDir())
			h.fakeClients.stdioEntries["cursor"]["legacy-go"] = clients.LanguageServerStdioEntry{
				Name: "legacy-go", Command: "mcp-language-server", Language: "gopls",
				Args: []string{"--lsp", "gopls", "--workspace", canonical},
			}
			clientsMap := testClientFactory()
			var calls []string
			clientsMap["cursor"] = &orderedCleanupClient{registerClient: clientsMap["cursor"], calls: &calls, backupErr: tc.backupErr}
			key := clientLanguageKey{Client: "cursor", Language: "go"}
			mustNewAPI(t).cleanupDirectLanguageServerEntriesAfterRegisterWithPlanDeps(
				map[string]config.LanguageSpec{"go": {Name: "go", Backend: "gopls-mcp", LspCommand: "gopls"}},
				[]string{"go"}, canonical, clientsMap, map[clientLanguageKey]bool{key: true},
				[]clientWriteReceipt{{Key: key, EntryName: "router-go"}}, &bytes.Buffer{},
				directCleanupPlanDeps{matchDirect: directLanguageServerCleanupMatches},
			)
			if !slices.Equal(calls, tc.wantCalls) {
				t.Fatalf("calls=%v, want %v", calls, tc.wantCalls)
			}
		})
	}
}
