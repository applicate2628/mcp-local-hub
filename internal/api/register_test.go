package api

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
	restore     func()
}

func newRegisterHarness(t *testing.T) *registerHarness {
	t.Helper()
	dir := t.TempDir()
	regPath := filepath.Join(dir, "workspaces.yaml")

	origSchedulerNew := testSchedulerFactory
	origClientFactory := testClientFactory
	origRegistryPath := testRegistryPathOverride

	sch := &fakeScheduler{tasks: map[string]bool{}, xml: map[string][]byte{}}
	testSchedulerFactory = func() (testScheduler, error) { return sch, nil }

	fc := &fakeClientsMap{entries: map[string]map[string]string{}, exists: map[string]bool{}}
	// Pre-populate the three HTTP clients so Exists() returns true in tests.
	for _, n := range []string{"codex-cli", "claude-code", "gemini-cli"} {
		fc.entries[n] = map[string]string{}
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
		regPath:     regPath,
		fakeSch:     sch,
		fakeClients: fc,
		restore: func() {
			testSchedulerFactory = origSchedulerNew
			testClientFactory = origClientFactory
			testRegistryPathOverride = origRegistryPath
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
			{Client: "codex-cli", URLPath: "/mcp"},
			{Client: "claude-code", URLPath: "/mcp"},
			{Client: "gemini-cli", URLPath: "/mcp"},
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
	if len(h.fakeSch.createdSpecs) != 9 {
		t.Fatalf("scheduler Create called %d times, want 9", len(h.fakeSch.createdSpecs))
	}
	sawWorkspaceProxy := false
	for _, s := range h.fakeSch.createdSpecs {
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
	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{WeeklyRefresh: false, Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(h.regPath)
	_ = reg.Load()
	if reg.Workspaces[0].WeeklyRefresh {
		t.Error("expected WeeklyRefresh=false in registry entry")
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
	tasks            map[string]bool
	xml              map[string][]byte
	failCreateAfterN int // Create calls after the Nth succeed; the (N+1)th fails
	createCount      int
	createdSpecs     []scheduler.TaskSpec
	failRunForTask   string // if non-empty, Run(name) returns an induced error for this task name
}

func (f *fakeScheduler) Create(spec scheduler.TaskSpec) error {
	if f.failCreateAfterN > 0 && f.createCount >= f.failCreateAfterN {
		return fmt.Errorf("fake scheduler: induced failure after %d Create calls", f.failCreateAfterN)
	}
	f.createCount++
	f.tasks[spec.Name] = true
	f.createdSpecs = append(f.createdSpecs, spec)
	return nil
}
func (f *fakeScheduler) Delete(name string) error { delete(f.tasks, name); return nil }
func (f *fakeScheduler) Run(name string) error {
	if f.failRunForTask != "" && f.failRunForTask == name {
		return fmt.Errorf("fake scheduler: induced Run failure for %s", name)
	}
	return nil
}
func (f *fakeScheduler) ExportXML(name string) ([]byte, error) {
	if b, ok := f.xml[name]; ok {
		return b, nil
	}
	return nil, scheduler.ErrTaskNotFound
}
func (f *fakeScheduler) ImportXML(name string, xml []byte) error {
	if f.xml == nil {
		f.xml = map[string][]byte{}
	}
	f.xml[name] = xml
	f.tasks[name] = true
	return nil
}

type fakeClientsMap struct {
	entries           map[string]map[string]string // client-name -> entry-name -> URL
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
	return nil
}
func (c *fakeClient) GetEntry(name string) (*clients.MCPEntry, error) {
	url, ok := c.parent.entries[c.name][name]
	if !ok {
		return nil, nil
	}
	return &clients.MCPEntry{Name: name, URL: url}, nil
}

func countEntries(fc *fakeClientsMap) int {
	n := 0
	for _, m := range fc.entries {
		n += len(m)
	}
	return n
}
