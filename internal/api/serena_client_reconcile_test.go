package api

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// okPing is a readiness ping that always reports the GUI live. Used by
// tests that are not exercising the fail-closed discovery path.
func okPing(context.Context, int) error { return nil }

// seedPidport writes a "<PID> <PORT>\n" pidport file into a temp dir and
// returns its path. Mirrors gui.formatPidport's exact byte layout so the
// api-side parser (parseGUIPidportFile) reads it identically to
// gui.ReadPidport.
func seedPidport(t *testing.T, pid, port int) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gui.pidport")
	body := strconv.Itoa(pid) + " " + strconv.Itoa(port) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("seed pidport: %v", err)
	}
	return p
}

// reconcileFakeClient is a minimal in-memory clients.Client used by the
// reconcile tests that do not need real adapter file formats. It records
// AddEntry / RemoveEntry calls and can be configured to fail AddEntry to
// exercise the "legacy removed only after rewrite success" ordering
// invariant. Named distinctly from register_test.go's fakeClient (which
// implements only a subset of the interface).
type reconcileFakeClient struct {
	name          string
	exists        bool
	entries       map[string]clients.MCPEntry
	addErr        error  // when non-nil, AddEntry returns it
	backupPath    string // path BackupKeep returns (default "/fake/bak")
	backupCount   int
	addCalls      int
	removeCalls   int
	restoreCalls  int    // RestoreEntryFromBackup + ...ForRollback invocation count
	rollbackCalls int    // RestoreEntryFromBackupForRollback invocation count only
	restoreFrom   string // last backupPath passed to a restore call
	restoreErr    error  // when non-nil, the restore call returns it
}

func newReconcileFakeClient(name string) *reconcileFakeClient {
	return &reconcileFakeClient{name: name, exists: true, entries: map[string]clients.MCPEntry{}}
}

func (f *reconcileFakeClient) Name() string       { return f.name }
func (f *reconcileFakeClient) ConfigPath() string { return "/fake/" + f.name }
func (f *reconcileFakeClient) Exists() bool       { return f.exists }
func (f *reconcileFakeClient) backupReturn() string {
	if f.backupPath != "" {
		return f.backupPath
	}
	return "/fake/bak"
}
func (f *reconcileFakeClient) Backup() (string, error) {
	f.backupCount++
	return f.backupReturn(), nil
}
func (f *reconcileFakeClient) BackupKeep(int) (string, error) {
	f.backupCount++
	return f.backupReturn(), nil
}
func (f *reconcileFakeClient) Restore(string) error { return nil }
func (f *reconcileFakeClient) AddEntry(e clients.MCPEntry) error {
	f.addCalls++
	if f.addErr != nil {
		return f.addErr
	}
	f.entries[e.Name] = e
	return nil
}
func (f *reconcileFakeClient) RemoveEntry(name string) error {
	f.removeCalls++
	delete(f.entries, name)
	return nil
}
func (f *reconcileFakeClient) GetEntry(name string) (*clients.MCPEntry, error) {
	e, ok := f.entries[name]
	if !ok {
		return nil, nil
	}
	cp := e
	return &cp, nil
}
func (f *reconcileFakeClient) LatestBackupPath() (string, bool, error) { return "", false, nil }
func (f *reconcileFakeClient) RestoreEntryFromBackup(backupPath, _ string) error {
	f.restoreCalls++
	f.restoreFrom = backupPath
	return f.restoreErr
}

// RestoreEntryFromBackupForRollback records into the SAME counters as
// RestoreEntryFromBackup so the reconcile-rollback compensator
// (RestoreSerenaReconcileApplied, which calls this guard-bypassing variant)
// is observable by the existing assertions. rollbackCalls additionally
// distinguishes which variant fired.
func (f *reconcileFakeClient) RestoreEntryFromBackupForRollback(backupPath, _ string) error {
	f.restoreCalls++
	f.rollbackCalls++
	f.restoreFrom = backupPath
	return f.restoreErr
}
func (f *reconcileFakeClient) BackupContainsEntry(string, string) (bool, error) { return false, nil }
func (f *reconcileFakeClient) AllStdioEntries() ([]clients.StdioEntry, error)   { return nil, nil }
func (f *reconcileFakeClient) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return nil, nil
}
func (f *reconcileFakeClient) InitEmpty() (bool, error) { return false, nil }

var _ clients.Client = (*reconcileFakeClient)(nil)

// fakeReconcileClientMap builds a {name -> adapter} map covering exactly the
// in-scope serena reconcile client set with reconcileFakeClient adapters.
func fakeReconcileClientMap() map[string]clients.Client {
	m := map[string]clients.Client{}
	for _, name := range serenaReconcileClientSet() {
		m[name] = newReconcileFakeClient(name)
	}
	return m
}

// TestSerenaClientReconcile_DiscoversPortFromLivePidport_FailsClosedWhenAbsent
// verifies the load-bearing fail-closed discovery contract:
//   - the router URL port comes from the seeded pidport (NOT a guess);
//   - a missing pidport fails closed with NO client write;
//   - a ping failure (e.g. a non-listening port) fails closed with NO
//     client write.
func TestSerenaClientReconcile_DiscoversPortFromLivePidport_FailsClosedWhenAbsent(t *testing.T) {
	managedEntriesTestHelper(t) // RecordManagedEntry needs a hardened state dir

	// (1) Happy path: port comes from the pidport.
	t.Run("port_from_pidport", func(t *testing.T) {
		pp := seedPidport(t, 4242, 9137)
		cs := fakeReconcileClientMap()
		report, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
			PidportPath: pp,
			Ping:        okPing,
			Clients:     cs,
		})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(report.Applied) == 0 {
			t.Fatalf("expected applied rewrites, got none")
		}
		want := "http://127.0.0.1:9137/serena/mcp"
		for _, a := range report.Applied {
			if a.URL != want {
				t.Errorf("client %s URL = %q, want %q", a.Client, a.URL, want)
			}
		}
		// And the URL the adapter actually stored carries the pidport port.
		cc := cs["claude-code"].(*reconcileFakeClient)
		if got := cc.entries["serena"].URL; got != want {
			t.Errorf("stored claude-code serena URL = %q, want %q", got, want)
		}
	})

	// (2) Missing pidport: fail closed, no write.
	t.Run("missing_pidport_fails_closed", func(t *testing.T) {
		cs := fakeReconcileClientMap()
		_, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
			PidportPath: filepath.Join(t.TempDir(), "does-not-exist.pidport"),
			Ping:        okPing,
			Clients:     cs,
		})
		if !errors.Is(err, ErrSerenaReconcileGUINotLive) {
			t.Fatalf("expected ErrSerenaReconcileGUINotLive, got %v", err)
		}
		for name, c := range cs {
			if fc := c.(*reconcileFakeClient); fc.addCalls != 0 {
				t.Errorf("client %s: AddEntry called %d times on fail-closed path; want 0", name, fc.addCalls)
			}
		}
	})

	// (3) Ping failure (simulating a non-listening / stale bound port):
	//     fail closed, no write.
	t.Run("ping_failure_fails_closed", func(t *testing.T) {
		pp := seedPidport(t, 4242, 65530) // a real port number, but nothing is listening
		cs := fakeReconcileClientMap()
		failPing := func(context.Context, int) error { return errors.New("connection refused") }
		_, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
			PidportPath: pp,
			Ping:        failPing,
			Clients:     cs,
		})
		if !errors.Is(err, ErrSerenaReconcileGUINotLive) {
			t.Fatalf("expected ErrSerenaReconcileGUINotLive on ping failure, got %v", err)
		}
		for name, c := range cs {
			if fc := c.(*reconcileFakeClient); fc.addCalls != 0 {
				t.Errorf("client %s: AddEntry called %d times after ping failure; want 0", name, fc.addCalls)
			}
		}
	})

	// (4) Port==0 placeholder (GUI wrote pidport before binding): fail
	//     closed even before the ping.
	t.Run("zero_port_placeholder_fails_closed", func(t *testing.T) {
		pp := seedPidport(t, 4242, 0)
		cs := fakeReconcileClientMap()
		pingCalled := false
		_, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
			PidportPath: pp,
			Ping:        func(context.Context, int) error { pingCalled = true; return nil },
			Clients:     cs,
		})
		if !errors.Is(err, ErrSerenaReconcileGUINotLive) {
			t.Fatalf("expected ErrSerenaReconcileGUINotLive for port=0, got %v", err)
		}
		if pingCalled {
			t.Errorf("ping should not run for an unusable (0) port")
		}
	})
}

// TestDefaultRouterReadinessPing_RequiresSerenaRouterSignature is the bot PR #248
// P1 guard: the REAL readiness ping must REJECT a non-serena-router response (a
// stale pidport's port reused by another local HTTP server), not accept any HTTP
// status. The mcphub serena router answers a non-POST (our HEAD) with 405 +
// Allow: POST; anything else must fail closed so the reconcile never rewrites
// client configs to an unrelated service.
func TestDefaultRouterReadinessPing_RequiresSerenaRouterSignature(t *testing.T) {
	portOf := func(t *testing.T, ts *httptest.Server) int {
		t.Helper()
		return ts.Listener.Addr().(*net.TCPAddr).Port
	}
	// serenaRouterStub mimics the GUI serena router: a non-POST request gets
	// 405 + Allow: POST (the route signature); a POST (our MCP initialize probe)
	// gets either a valid JSON-RPC initialize result (serveLifecycle=true →
	// router-completion landed) or a params.name rejection (serveLifecycle=false →
	// the current tool-only router that has no MCP lifecycle).
	serenaRouterStub := func(serveLifecycle bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != SerenaRouterURLPath {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if serveLifecycle {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"serena"},"capabilities":{}}}`))
				return
			}
			http.Error(w, "missing required field: params.name", http.StatusBadRequest)
		}))
	}

	// (1) Full router (405 signature AND serves initialize) → ping passes.
	full := serenaRouterStub(true)
	defer full.Close()
	if err := defaultRouterReadinessPing(context.Background(), portOf(t, full)); err != nil {
		t.Errorf("serena router that serves initialize must pass; got %v", err)
	}

	// (2) Serena-router signature but NO MCP lifecycle (the current tool-only
	//     router) → fail closed (bot PR #248 P2): never point a client at a
	//     router that cannot complete initialize.
	noLifecycle := serenaRouterStub(false)
	defer noLifecycle.Close()
	if err := defaultRouterReadinessPing(context.Background(), portOf(t, noLifecycle)); err == nil {
		t.Errorf("a serena router that rejects initialize must fail closed; got nil")
	}

	// (3) Unrelated server (200 on everything) → fails the 405/Allow signature.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	if err := defaultRouterReadinessPing(context.Background(), portOf(t, other)); err == nil {
		t.Errorf("a non-router server (200 OK) must fail closed; got nil")
	}

	// (4) Unrelated server (404) → fails the 405/Allow signature.
	notfound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notfound.Close()
	if err := defaultRouterReadinessPing(context.Background(), portOf(t, notfound)); err == nil {
		t.Errorf("a non-router server (404) must fail closed; got nil")
	}
}

// TestSerenaClientReconcile_RewritesToRouterURL_PerClient exercises the REAL
// client adapter shapes (claude-code/codex-cli/cursor/vscode/gemini-cli/
// qwen-cli) via clients.AllClients() over a hermetic HOME, asserting each
// client's serena entry resolves to the constant router endpoint.
func TestSerenaClientReconcile_RewritesToRouterURL_PerClient(t *testing.T) {
	managedEntriesTestHelper(t)
	t.Cleanup(SetClientWriteFallbackForTest()) // %TEMP% HOME fails the prod DACL gate on Windows

	home := t.TempDir()
	// Redirect every adapter's path resolution into the hermetic home.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming")) // vscode (Windows)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))    // vscode (Linux)

	// Seed each URL client's config with a legacy serena entry so the
	// rewrite has something to overwrite and Exists() is true.
	seedFile := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	seedFile(".claude.json", `{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9121/mcp"}}}`)
	seedFile(filepath.Join(".codex", "config.toml"), "[mcp_servers.serena]\nurl = \"http://localhost:9121/mcp\"\n")
	seedFile(filepath.Join(".cursor", "mcp.json"), `{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp"}}}`)
	seedFile(filepath.Join(".gemini", "settings.json"), `{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp"}}}`)
	seedFile(filepath.Join(".qwen", "settings.json"), `{"mcpServers":{"serena":{"httpUrl":"http://localhost:9121/mcp"}}}`)
	// vscode resolves via APPDATA on Windows, XDG on Linux — seed both.
	seedFile(filepath.Join("AppData", "Roaming", "Code", "User", "mcp.json"), `{"servers":{"serena":{"url":"http://localhost:9121/mcp"}}}`)
	seedFile(filepath.Join(".config", "Code", "User", "mcp.json"), `{"servers":{"serena":{"url":"http://localhost:9121/mcp"}}}`)

	pp := seedPidport(t, 4242, 9140)

	// Narrow to the URL clients (Antigravity has its own dedicated test).
	urlClients := []string{"claude-code", "codex-cli", "cursor", "vscode", "gemini-cli", "qwen-cli"}
	report, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
		PidportPath:    pp,
		Ping:           okPing,
		ClientsInclude: urlClients,
		// Clients: nil → production AllClients() over the hermetic HOME.
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}

	want := "http://127.0.0.1:9140/serena/mcp"
	gotApplied := map[string]string{}
	for _, a := range report.Applied {
		gotApplied[a.Client] = a.URL
	}
	for _, c := range urlClients {
		if gotApplied[c] != want {
			t.Errorf("client %s applied URL = %q, want %q", c, gotApplied[c], want)
		}
	}

	// Verify the rewrite landed in each real adapter's config, reading back
	// through the adapter's own GetEntry (its format-specific parse).
	all := clients.AllClients()
	for _, c := range urlClients {
		entry, gerr := all[c].GetEntry("serena")
		if gerr != nil {
			t.Fatalf("GetEntry %s: %v", c, gerr)
		}
		if entry == nil {
			t.Fatalf("client %s: serena entry missing after reconcile", c)
		}
		if entry.URL != want {
			t.Errorf("client %s on-disk serena URL = %q, want %q", c, entry.URL, want)
		}
	}
}

// TestSerenaClientReconcile_Antigravity_RelayUpstreamIsRouter verifies the
// Antigravity stdio-relay entry's upstream points at the router endpoint
// (via the relay --url escape hatch) rather than the legacy 9121 daemon.
func TestSerenaClientReconcile_Antigravity_RelayUpstreamIsRouter(t *testing.T) {
	managedEntriesTestHelper(t)
	t.Cleanup(SetClientWriteFallbackForTest())

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Antigravity config: ~/.gemini/antigravity/mcp_config.json. Seed it so
	// Exists() is true.
	agPath := filepath.Join(home, ".gemini", "antigravity", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(agPath), 0o755); err != nil {
		t.Fatalf("mkdir antigravity dir: %v", err)
	}
	if err := os.WriteFile(agPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("seed antigravity config: %v", err)
	}

	pp := seedPidport(t, 4242, 9141)
	report, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
		PidportPath:    pp,
		Ping:           okPing,
		ClientsInclude: []string{"antigravity"},
		McphubExePath:  filepath.Join(home, "mcphub-test-bin"), // abs path required by the adapter
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Applied) != 1 || report.Applied[0].Client != "antigravity" {
		t.Fatalf("expected one applied antigravity row, got %+v", report.Applied)
	}

	// Read the raw config back and assert the relay args carry --url <router>
	// and do NOT carry the legacy --server/--daemon manifest-lookup form
	// (which would resolve to the dead 9121 daemon).
	raw, err := os.ReadFile(agPath)
	if err != nil {
		t.Fatalf("read antigravity config: %v", err)
	}
	s := string(raw)
	wantURL := "http://127.0.0.1:9141/serena/mcp"
	if !strings.Contains(s, "--url") || !strings.Contains(s, wantURL) {
		t.Errorf("antigravity relay args should carry --url %q; got:\n%s", wantURL, s)
	}
	if strings.Contains(s, "9121") {
		t.Errorf("antigravity relay must NOT point at legacy 9121; got:\n%s", s)
	}

	// And through the adapter's own GetEntry reconstruction.
	all := clients.AllClients()
	entry, gerr := all["antigravity"].GetEntry("serena")
	if gerr != nil {
		t.Fatalf("GetEntry antigravity: %v", gerr)
	}
	if entry == nil || entry.RelayURL != wantURL {
		t.Errorf("antigravity RelayURL = %v, want %q", entry, wantURL)
	}
}

// TestSerenaClientReconcile_RecordsManagedEntryMarker verifies each
// successfully rewritten (client, "serena") tuple is recorded in the
// managed-entries marker (demigrate symmetry).
func TestSerenaClientReconcile_RecordsManagedEntryMarker(t *testing.T) {
	managedEntriesTestHelper(t)

	pp := seedPidport(t, 4242, 9142)
	cs := fakeReconcileClientMap()
	report, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
		PidportPath: pp,
		Ping:        okPing,
		Clients:     cs,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.Applied) == 0 {
		t.Fatalf("expected applied rewrites")
	}
	for _, a := range report.Applied {
		managed, merr := IsManagedEntry(a.Client, "serena")
		if merr != nil {
			t.Fatalf("IsManagedEntry(%s, serena): %v", a.Client, merr)
		}
		if !managed {
			t.Errorf("client %s: (client, serena) tuple not recorded in managed-entries marker", a.Client)
		}
	}
}

// TestSerenaClientReconcile_LegacyEndpointRemovedOnlyAfterRewriteSuccess
// injects a rewrite failure for ONE client and asserts that client keeps its
// legacy entry (no removal) and the failure is reported, while the other
// clients succeed. This is claim #9: legacy removal is ordered AFTER a
// successful router rewrite for that client.
func TestSerenaClientReconcile_LegacyEndpointRemovedOnlyAfterRewriteSuccess(t *testing.T) {
	managedEntriesTestHelper(t)

	pp := seedPidport(t, 4242, 9143)

	// Build a client map where claude-code fails AddEntry and carries a
	// pre-existing legacy entry; the rest succeed.
	cs := fakeReconcileClientMap()
	broken := cs["claude-code"].(*reconcileFakeClient)
	broken.addErr = errors.New("simulated write failure")
	broken.entries["serena"] = clients.MCPEntry{Name: "serena", URL: "http://localhost:9143/mcp"}

	report, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
		PidportPath:  pp,
		Ping:         okPing,
		Clients:      cs,
		RemoveLegacy: true,
		LegacyPort:   9143,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// claude-code must appear in Failed, NOT Applied.
	var failedClaude bool
	for _, f := range report.Failed {
		if f.Client == "claude-code" {
			failedClaude = true
		}
	}
	if !failedClaude {
		t.Fatalf("expected claude-code in Failed rows; got %+v", report.Failed)
	}
	for _, a := range report.Applied {
		if a.Client == "claude-code" {
			t.Errorf("claude-code must not be in Applied after a rewrite failure")
		}
	}

	// The failed client must have made NO RemoveEntry call (legacy entry
	// retained) — legacy removal is ordered strictly after rewrite success.
	if broken.removeCalls != 0 {
		t.Errorf("claude-code RemoveEntry called %d times after a FAILED rewrite; legacy endpoint must be retained", broken.removeCalls)
	}
	// And its legacy entry is still present and unchanged.
	cur, _ := broken.GetEntry("serena")
	if cur == nil || cur.URL != "http://localhost:9143/mcp" {
		t.Errorf("claude-code legacy serena entry should be retained as-is; got %v", cur)
	}

	// A succeeding client should be Applied and carry the router URL.
	wantURL := "http://127.0.0.1:9143/serena/mcp"
	cursor := cs["cursor"].(*reconcileFakeClient)
	if got := cursor.entries["serena"].URL; got != wantURL {
		t.Errorf("cursor serena URL = %q, want %q", got, wantURL)
	}
}

// TestSerenaClientReconcile_AppliedRowCarriesBackupPath verifies the reconcile
// threads the per-client backup path onto each Applied row (the surface the
// migrate driver's partial-failure rollback restores from).
func TestSerenaClientReconcile_AppliedRowCarriesBackupPath(t *testing.T) {
	managedEntriesTestHelper(t)

	pp := seedPidport(t, 4242, 9151)
	cs := fakeReconcileClientMap()
	// Give each fake a distinct backup path so the row mapping is unambiguous.
	for name, c := range cs {
		c.(*reconcileFakeClient).backupPath = "/fake/bak-" + name
	}
	report, err := ReconcileSerenaClientsToRouter(context.Background(), SerenaReconcileOpts{
		PidportPath: pp,
		Ping:        okPing,
		Clients:     cs,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.Applied) == 0 {
		t.Fatal("expected applied rewrites")
	}
	for _, a := range report.Applied {
		if a.BackupPath != "/fake/bak-"+a.Client {
			t.Errorf("applied row %s BackupPath = %q, want %q", a.Client, a.BackupPath, "/fake/bak-"+a.Client)
		}
	}
}

// TestRestoreSerenaReconcileApplied verifies the partial-failure compensator
// restores every Applied client's serena entry from its recorded backup, skips
// rows without a backup path, reports a missing adapter, and joins per-client
// restore errors (best-effort: one failure does not strand the rest).
func TestRestoreSerenaReconcileApplied(t *testing.T) {
	cc := newReconcileFakeClient("claude-code")
	cursor := newReconcileFakeClient("cursor")
	cursor.restoreErr = errors.New("restore write denied")
	cs := map[string]clients.Client{"claude-code": cc, "cursor": cursor}

	report := &MigrateReport{Applied: []AppliedMigration{
		{Server: "serena", Client: "claude-code", BackupPath: "/bak/claude"},
		{Server: "serena", Client: "cursor", BackupPath: "/bak/cursor"},
		{Server: "serena", Client: "vscode", BackupPath: ""},           // no backup → skipped
		{Server: "serena", Client: "gemini-cli", BackupPath: "/bak/g"}, // no adapter → reported
	}}

	err := RestoreSerenaReconcileApplied(report, cs)
	if err == nil {
		t.Fatal("expected a joined error (cursor restore failure + gemini-cli missing adapter)")
	}
	// claude-code restored from its backup.
	if cc.restoreCalls != 1 || cc.restoreFrom != "/bak/claude" {
		t.Errorf("claude-code restore: calls=%d from=%q, want 1 from /bak/claude", cc.restoreCalls, cc.restoreFrom)
	}
	// cursor attempted (and failed).
	if cursor.restoreCalls != 1 {
		t.Errorf("cursor restore attempts = %d, want 1", cursor.restoreCalls)
	}
	if !strings.Contains(err.Error(), "restore write denied") {
		t.Errorf("joined error should carry the cursor restore failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "gemini-cli") || !strings.Contains(err.Error(), "no adapter") {
		t.Errorf("joined error should report the missing gemini-cli adapter; got %v", err)
	}
	// A nil report is a no-op.
	if e := RestoreSerenaReconcileApplied(nil, cs); e != nil {
		t.Errorf("nil report must be a no-op; got %v", e)
	}
}

// TestRestoreSerenaReconcileApplied_BypassesHubEntryGuard_RestoresLegacyHubBackup
// is the finding #3 regression guard. The migrate abort-rollback restores each
// already-rewritten client from the per-client backup captured BEFORE the
// reconcile rewrote it. For a normal pre-cutover serena client, that backup IS
// the legacy hub entry — a loopback http://localhost:9121/mcp URL for URL
// clients, or the `mcphub relay` form for Antigravity. The plain
// RestoreEntryFromBackup REFUSES those (ErrBackupEntryAlreadyMigrated, the
// demigrate guard), which would make the rollback FAIL and strand the rewritten
// clients on /serena/mcp even though the migration aborted. This test asserts
// RestoreSerenaReconcileApplied (which uses the guard-bypassing rollback
// variant) SUCCEEDS through the REAL adapters and the live configs are back on
// their legacy hub form.
func TestRestoreSerenaReconcileApplied_BypassesHubEntryGuard_RestoresLegacyHubBackup(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest()) // %TEMP% HOME fails the prod DACL gate on Windows

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	writeFile := func(rel, body string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}

	const routerURL = "http://127.0.0.1:9140/serena/mcp"
	const legacyURL = "http://localhost:9121/mcp"

	// claude-code: live config is post-reconcile (router URL); the backup
	// captured pre-reconcile is the LEGACY HUB entry (loopback 9121, no
	// command) — exactly the shape RestoreEntryFromBackup refuses.
	ccLive := writeFile(".claude.json",
		`{"mcpServers":{"serena":{"type":"http","url":"`+routerURL+`"}}}`)
	ccBackup := ccLive + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(ccBackup,
		[]byte(`{"mcpServers":{"serena":{"type":"http","url":"`+legacyURL+`"}}}`), 0o600); err != nil {
		t.Fatalf("write claude-code backup: %v", err)
	}

	// antigravity: live config is post-reconcile (relay --url <router>); the
	// backup captured pre-reconcile is the LEGACY RELAY entry (command=mcphub,
	// args[0]="relay", manifest-lookup --server/--daemon form pointing at the
	// legacy 9121 daemon) — hub-relay shaped, also refused by the guard.
	// The relay command path is a forward-slash literal (valid JSON, valid
	// mcphub basename for IsMcphubBinary); its actual value is irrelevant to
	// the guard, which keys off the basename + args[0]=="relay".
	const relayCmd = "C:/mcphub.exe"
	agLive := writeFile(filepath.Join(".gemini", "antigravity", "mcp_config.json"),
		`{"mcpServers":{"serena":{"command":"`+relayCmd+`","args":["relay","--url","`+routerURL+`"]}}}`)
	agBackup := agLive + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(agBackup,
		[]byte(`{"mcpServers":{"serena":{"command":"`+relayCmd+`","args":["relay","--server","serena","--daemon","claude"]}}}`), 0o600); err != nil {
		t.Fatalf("write antigravity backup: %v", err)
	}

	report := &MigrateReport{Applied: []AppliedMigration{
		{Server: "serena", Client: "claude-code", URL: routerURL, BackupPath: ccBackup},
		{Server: "serena", Client: "antigravity", URL: routerURL, BackupPath: agBackup},
	}}

	// Sanity guard: the PLAIN restore MUST refuse these backups — otherwise the
	// test would pass vacuously even if the rollback used the wrong (guarded)
	// path. This pins the bug the bypass exists to dodge.
	all := clients.AllClients()
	if err := all["claude-code"].RestoreEntryFromBackup(ccBackup, "serena"); !errors.Is(err, clients.ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("precondition: plain RestoreEntryFromBackup on a legacy-9121 hub backup must refuse with ErrBackupEntryAlreadyMigrated, got %v", err)
	}
	if err := all["antigravity"].RestoreEntryFromBackup(agBackup, "serena"); !errors.Is(err, clients.ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("precondition: plain RestoreEntryFromBackup on a legacy relay backup must refuse with ErrBackupEntryAlreadyMigrated, got %v", err)
	}
	// Re-write the live configs to the post-reconcile shape (the precondition's
	// refusal left them untouched, but be explicit so the assertions below test
	// the rollback in isolation).
	if err := os.WriteFile(ccLive, []byte(`{"mcpServers":{"serena":{"type":"http","url":"`+routerURL+`"}}}`), 0o600); err != nil {
		t.Fatalf("rewrite claude-code live: %v", err)
	}

	// The rollback restore MUST succeed (guard bypassed) and put both clients
	// back on their legacy hub form.
	if err := RestoreSerenaReconcileApplied(report, clients.AllClients()); err != nil {
		t.Fatalf("RestoreSerenaReconcileApplied must succeed via the guard-bypassing rollback restore, got: %v", err)
	}

	// claude-code is back on the legacy 9121 URL.
	ccEntry, err := clients.AllClients()["claude-code"].GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry claude-code: %v", err)
	}
	if ccEntry == nil || ccEntry.URL != legacyURL {
		t.Errorf("claude-code serena entry = %v, want legacy URL %q after rollback", ccEntry, legacyURL)
	}

	// antigravity is back on the legacy manifest-lookup relay (--server/--daemon),
	// NOT the router --url form.
	agRaw, err := os.ReadFile(agLive)
	if err != nil {
		t.Fatalf("read antigravity live: %v", err)
	}
	ags := string(agRaw)
	if !strings.Contains(ags, "--server") || !strings.Contains(ags, "--daemon") {
		t.Errorf("antigravity relay should be back on the legacy --server/--daemon form after rollback; got:\n%s", ags)
	}
	if strings.Contains(ags, "--url") {
		t.Errorf("antigravity relay should NOT carry the router --url form after rollback; got:\n%s", ags)
	}
}

// TestSerenaClientReconcile_DoesNotTouchG4Resolver structurally asserts the
// reconcile implementation does not invoke the G4 hub-resolver path — serena
// routing flows ONLY through the registry-driven /serena/mcp router (design
// claim #8). manifestHasScheduledDaemon lives in package cli (unexported, so
// package api literally cannot reference it); BuildResolverSnapshotFromManifests
// and ResolverSnapshot live in package api, so we parse this phase's source
// file and walk its AST, inspecting only real identifier references (NOT
// comment text) so the doc comment that explains "we do NOT use these" cannot
// trip the assertion.
func TestSerenaClientReconcile_DoesNotTouchG4Resolver(t *testing.T) {
	fset := token.NewFileSet()
	// Parse WITHOUT parser.ParseComments so comment text is not part of the
	// AST; only code identifiers are walked below.
	file, err := parser.ParseFile(fset, "serena_client_reconcile.go", nil, 0)
	if err != nil {
		t.Fatalf("parse reconcile source: %v", err)
	}
	banned := map[string]bool{
		"BuildResolverSnapshotFromManifests": true,
		"manifestHasScheduledDaemon":         true,
		"ResolverSnapshot":                   true,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if banned[id.Name] {
			t.Errorf("serena_client_reconcile.go references G4 resolver symbol %q in code; serena routing must flow only through the /serena/mcp router (design claim #8)", id.Name)
		}
		return true
	})
	// Belt-and-suspenders: the reconcile must not import the resolver's
	// surrounding file by name either. Imports are explicit in the AST; assert
	// no import path mentions the resolver. (All these symbols are in-package,
	// so there is no separate import to catch today — this guards a future
	// refactor that might move the resolver to a sub-package.)
	for _, imp := range file.Imports {
		if strings.Contains(imp.Path.Value, "hub_mcp_resolver") || strings.Contains(imp.Path.Value, "resolver") {
			t.Errorf("serena_client_reconcile.go imports a resolver path %s; serena routing must flow only through the /serena/mcp router", imp.Path.Value)
		}
	}
}
