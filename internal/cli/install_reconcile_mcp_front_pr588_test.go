// internal/cli/install_reconcile_mcp_front_pr588_test.go
//
// Regression coverage for the codex bot PR #588 findings against
// `mcphub install --reconcile-mcp-front[/--rollback]`:
//
//   - P1: a forward RE-RUN (the retry the command's own per-client-failure
//     message recommends) must not destroy the pre-reconcile record rollback
//     restores from.
//   - P1: rollback must restore the PRIOR LSP router URL, not demote the
//     shared router entry out of existence.
//   - P2: the shared gate must refuse a mcp_front.port a different listener
//     already owns, even when that listener satisfies the /serena/mcp
//     protocol probe (the GUI does).
//
// SAFETY. These tests seed REAL client-config files and drive the REAL
// reconcile (whose serena leg calls api.RecordManagedEntry, a state-dir
// WRITE), so they must never reach the operator's live fleet. Two layers,
// both active under a plain `go test`:
//
//   - HOME/USERPROFILE/LOCALAPPDATA are redirected to a throwaway t.TempDir(),
//     so every client adapter and the settings file resolve inside it;
//   - api.SetDaemonStateRootForTest pins the daemon state dir to that same
//     temp dir (composing with, and restoring to, this package's TestMain
//     global fleet-safety redirect — see
//     fleet_safety_state_redirect_test.go).
//
// assertRedirectedStateDir then FAILS the test if the state dir did not land
// under the temp dir, so a future regression in either layer surfaces as a
// test failure rather than as writes to the live fleet.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// mcpFrontPR588Env redirects every path resolution this command family
// performs to one throwaway directory and returns it.
func mcpFrontPR588Env(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	t.Cleanup(api.SetDaemonStateRootForTest(tmp))
	return tmp
}

// assertRedirectedStateDir is the fleet-safety net: it FAILS (does not skip)
// when the daemon state dir did not resolve under tmp. See the SAFETY note in
// the file header — these tests drive a state-dir write path, so an
// un-redirected state dir must stop the run loudly rather than silently
// touching the operator's live managed-entries.json.
func assertRedirectedStateDir(t *testing.T, tmp string) {
	t.Helper()
	dir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("state dir unresolvable (%v); refusing to run a state-dir write path with no proven redirect", err)
	}
	norm := func(p string) string { return strings.ToLower(filepath.Clean(p)) }
	if !strings.HasPrefix(norm(dir), norm(tmp)) {
		t.Fatalf("FLEET SAFETY: state dir %s is NOT redirected under %s — this test seeds real client configs and the serena reconcile writes managed-entries.json; the redirect in mcpFrontPR588Env is broken", dir, tmp)
	}
}

func claudeCodeConfigPath(t *testing.T, tmp string) string {
	t.Helper()
	return filepath.Join(tmp, ".claude.json")
}

// seedClaudeCodeConfig writes ~/.claude.json with the given mcpServers map so
// clients.AllClients()["claude-code"].Exists() is true and the reconcile has
// something real to rewrite.
func seedClaudeCodeConfig(t *testing.T, tmp string, servers map[string]any) string {
	t.Helper()
	path := claudeCodeConfigPath(t, tmp)
	raw, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	return path
}

// claudeCodeEntryURL returns (url, present) for one mcpServers entry.
func claudeCodeEntryURL(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse %s: %v (body=%s)", path, err, string(raw))
	}
	servers, _ := m["mcpServers"].(map[string]any)
	entry, ok := servers[name].(map[string]any)
	if !ok {
		return "", false
	}
	url, _ := entry["url"].(string)
	return url, true
}

func readPersistedMCPFrontReport(t *testing.T, path string) mcpFrontReconcileReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted report %s: %v", path, err)
	}
	var out mcpFrontReconcileReport
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse persisted report %s: %v", path, err)
	}
	return out
}

func newMCPFrontTestCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(new(testWriter))
	cmd.SetErr(new(testWriter))
	return cmd
}

// TestMCPFrontPR588_ForwardRerunPreservesFirstGenerationRecord is the P1-a
// guard.
//
// The forward command tells the operator to RE-RUN it to retry per-client
// failures. Before the fix, every re-run re-captured a fresh per-client backup
// and overwrote the persisted record wholesale — so on the second run the
// recorded "pre-reconcile" backup for an already-migrated client contained the
// FRONT-port URL, and `--rollback` then restored the front URL. The original
// GUI-port entry was destroyed by the very retry the command recommends.
//
// The assertion is end-to-end and behavioral: after forward, forward, rollback
// the client's serena entry must be back at its ORIGINAL URL.
func TestMCPFrontPR588_ForwardRerunPreservesFirstGenerationRecord(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	const originalSerenaURL = "http://127.0.0.1:9125/serena/mcp"
	cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": originalSerenaURL},
	})

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	frontSerenaURL := fmt.Sprintf("http://127.0.0.1:%d/serena/mcp", port)

	// --- forward run #1 -------------------------------------------------
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile #1: %v", err)
	}
	if got, ok := claudeCodeEntryURL(t, cfgPath, "serena"); !ok || got != frontSerenaURL {
		t.Fatalf("after forward #1 the serena entry should point at the front port: got %q (present=%v), want %q", got, ok, frontSerenaURL)
	}
	first := readPersistedMCPFrontReport(t, reportPath)
	if first.Serena == nil || len(first.Serena.Applied) == 0 {
		t.Fatalf("forward #1 must record an applied serena row; got %+v", first.Serena)
	}
	firstBackup := ""
	for _, row := range first.Serena.Applied {
		if row.Client == "claude-code" {
			firstBackup = row.BackupPath
		}
	}
	if firstBackup == "" {
		t.Fatalf("forward #1 must record a backup path for claude-code; applied=%+v", first.Serena.Applied)
	}

	// --- forward run #2 (the documented retry) --------------------------
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile #2: %v", err)
	}
	second := readPersistedMCPFrontReport(t, reportPath)
	secondBackup := ""
	for _, row := range second.Serena.Applied {
		if row.Client == "claude-code" {
			secondBackup = row.BackupPath
		}
	}
	if secondBackup != firstBackup {
		t.Fatalf("forward re-run REPLACED the first generation's recorded backup for claude-code: first=%q second=%q; the second-run backup captures the ALREADY-REWRITTEN front-port config, so rollback would restore the front URL and the original state would be unrecoverable", firstBackup, secondBackup)
	}

	// --- rollback -------------------------------------------------------
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), true); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, ok := claudeCodeEntryURL(t, cfgPath, "serena")
	if !ok {
		t.Fatalf("rollback must leave the serena entry present; it is gone from %s", cfgPath)
	}
	if got != originalSerenaURL {
		t.Fatalf("rollback restored the WRONG serena URL after a forward re-run: got %q, want the pre-reconcile %q (a rollback that leaves the host on the front port is worse than no rollback)", got, originalSerenaURL)
	}
}

// TestMCPFrontPR588_RollbackRestoresPriorLSPRouterURL is the P1-b guard.
//
// On an already-migrated host the canonical `mcp-language-server-<language>`
// entry already exists and the forward pass only changes its PORT. The
// pre-fix rollback called api.RollbackLSPRouterClientEntries — the
// router-to-LEGACY demotion routine — which REMOVES that canonical entry
// instead of putting its previous port back, so `--rollback` silently deleted
// the client's shared LSP route.
//
// Two assertions, both of which the pre-fix code fails: the pre-existing
// language's entry must SURVIVE rollback and carry its ORIGINAL URL; and a
// language whose entry the forward run CREATED must be removed again (the
// inverse of "absent" is "absent").
func TestMCPFrontPR588_RollbackRestoresPriorLSPRouterURL(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	withMCPFrontReportPathSeam(t)

	const preexistingLanguage = "go"
	preexistingEntry := api.LSPRouterEntryName(preexistingLanguage)
	originalLSPURL := api.LSPRouterURL(9125, preexistingLanguage)

	cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena":         map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
		preexistingEntry: map[string]any{"url": originalLSPURL},
	})

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile: %v", err)
	}
	wantFrontLSPURL := api.LSPRouterURL(port, preexistingLanguage)
	if got, ok := claudeCodeEntryURL(t, cfgPath, preexistingEntry); !ok || got != wantFrontLSPURL {
		t.Fatalf("after forward the %s entry should point at the front port: got %q (present=%v), want %q", preexistingEntry, got, ok, wantFrontLSPURL)
	}
	// Identify one language the forward run CREATED (absent pre-state) so the
	// removal half of the inverse is exercised too.
	createdEntry := ""
	for _, language := range []string{"python", "rust", "typescript", "javascript"} {
		name := api.LSPRouterEntryName(language)
		if _, ok := claudeCodeEntryURL(t, cfgPath, name); ok {
			createdEntry = name
			break
		}
	}

	if err := runReconcileMCPFront(newMCPFrontTestCmd(), true); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	got, ok := claudeCodeEntryURL(t, cfgPath, preexistingEntry)
	if !ok {
		t.Fatalf("rollback REMOVED the pre-existing %s entry instead of restoring its previous URL — the client is now left with no LSP route at all (this is the router-to-legacy demotion routine running in place of the port-rewrite inverse)", preexistingEntry)
	}
	if got != originalLSPURL {
		t.Fatalf("rollback did not restore the pre-reconcile %s URL: got %q, want %q", preexistingEntry, got, originalLSPURL)
	}
	if createdEntry != "" {
		if _, stillThere := claudeCodeEntryURL(t, cfgPath, createdEntry); stillThere {
			t.Fatalf("rollback left behind %s, an entry the forward run CREATED (its recorded pre-state is absent, so its inverse is removal)", createdEntry)
		}
	}
}

// TestMCPFrontPR588_ForwardRefusesPortOwnedByGUI is the P2 shared-gate guard.
//
// The liveness gate only proves "something here speaks the /serena/mcp
// protocol shape" — and the GUI speaks it. A mcp_front.port set to the GUI's
// own port therefore SATISFIED the probe while the `mcphub route` child could
// never bind it, and the command happily reconciled every client onto an
// endpoint that dies with the GUI.
//
// The test makes the collision indistinguishable to the probe on purpose: a
// REAL route-shaped listener answers on the port, so the ONLY thing that can
// refuse the run is the ownership check.
func TestMCPFrontPR588_ForwardRefusesPortOwnedByGUI(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	const originalSerenaURL = "http://127.0.0.1:9125/serena/mcp"
	cfgPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": originalSerenaURL},
	})

	port, cleanup := startTestRouteServer(t)
	defer cleanup()

	a := api.NewAPI()
	// gui_server.port and mcp_front.port deliberately collide.
	if err := a.SettingsSet("gui_server.port", strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet gui_server.port: %v", err)
	}
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet mcp_front.port: %v", err)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if err == nil {
		t.Fatalf("forward reconcile must REFUSE when mcp_front.port equals gui_server.port; it returned nil (a live route-shaped listener satisfied the protocol probe, which is exactly why the probe alone is insufficient)")
	}
	if !strings.Contains(err.Error(), "gui_server.port") {
		t.Fatalf("refusal must name the colliding owner so the operator can fix it; got %v", err)
	}
	if got, _ := claudeCodeEntryURL(t, cfgPath, "serena"); got != originalSerenaURL {
		t.Fatalf("a refused run must write NOTHING; the serena entry changed to %q", got)
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("a refused run must not persist a reconcile report; stat err = %v", statErr)
	}
}

// TestMCPFrontPR588_ForwardRefusesUnreadablePriorReport pins the fail-closed
// half of the preserve-the-first-generation contract: an existing record that
// cannot be parsed must stop the run, never be silently replaced. Overwriting
// it would destroy the only copy of the pre-reconcile state.
func TestMCPFrontPR588_ForwardRefusesUnreadablePriorReport(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	// Written through the hardened state-file pipeline so the failure this
	// test observes is the PARSE failure it names, not an owner-only DACL
	// refusal from a hand-rolled os.WriteFile.
	const corrupt = "{ this is not json"
	if err := api.WriteStateFileBytesAtomic(reportPath, []byte(corrupt)); err != nil {
		t.Fatalf("seed corrupt report: %v", err)
	}

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)
	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if err == nil {
		t.Fatalf("forward reconcile must refuse when a prior report exists but cannot be read")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("refusal must explain that the prior record is unreadable; got %v", err)
	}
	raw, rerr := os.ReadFile(reportPath)
	if rerr != nil || string(raw) != corrupt {
		t.Fatalf("the unreadable prior report must be left untouched: err=%v body=%q", rerr, string(raw))
	}
}

// TestMCPFrontPR588_RollbackRefusesUnknownArtifactVersion pins the rollback's
// own fail-closed gate: a record whose schema this build does not understand
// must never drive client-config writes.
func TestMCPFrontPR588_RollbackRefusesUnknownArtifactVersion(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	// Hardened writer for the same reason as above: the refusal under test is
	// the artifact-version gate, not a DACL rejection.
	if err := api.WriteStateFileAtomic(reportPath, map[string]any{
		"version": mcpFrontReconcileReportVersion + 1,
		"port":    9137,
		"serena":  map[string]any{"applied": []any{}},
	}); err != nil {
		t.Fatalf("seed future-version report: %v", err)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
	if err == nil {
		t.Fatalf("rollback must refuse an artifact version it does not understand")
	}
	if !strings.Contains(err.Error(), "artifact version") {
		t.Fatalf("refusal must name the version mismatch; got %v", err)
	}
}

// TestMCPFrontPR588_MergePreservesRecordedRowsAndAddsNewOnes is the unit-level
// statement of the merge invariant, independent of any client on disk: a row an
// earlier generation recorded is never replaced, and a row for a key not yet
// recorded IS added (that is the retry of a previously-failed client, whose
// captured state genuinely is its original pre-state).
func TestMCPFrontPR588_MergePreservesRecordedRowsAndAddsNewOnes(t *testing.T) {
	prior := &mcpFrontReconcileReport{
		Version: mcpFrontReconcileReportVersion,
		Port:    9137,
		Serena: &api.MigrateReport{Applied: []api.AppliedMigration{
			{Server: "serena", Client: "claude-code", URL: "front", BackupPath: "GEN1-BACKUP"},
		}},
		LSP: &[]api.LSPRouterEntrySnapshot{
			{Client: "claude-code", Language: "go", EntryName: "mcp-language-server-go", Present: true, URL: "GEN1-URL"},
		},
		Pins: []mcpFrontSerenaPin{
			{Client: "claude-code", Origin: "GEN1-ROLLING", Path: "GEN1-BACKUP", SHA256: "GEN1-SUM"},
		},
	}
	// Generation 2 re-reports claude-code (now carrying POST-generation-1
	// state) and additionally reports cursor, which generation 1 failed on.
	serena := &api.MigrateReport{Applied: []api.AppliedMigration{
		{Server: "serena", Client: "claude-code", URL: "front", BackupPath: "GEN2-BACKUP"},
		{Server: "serena", Client: "cursor", URL: "front", BackupPath: "CURSOR-BACKUP"},
	}}
	lsp := []api.LSPRouterEntrySnapshot{
		{Client: "claude-code", Language: "go", EntryName: "mcp-language-server-go", Present: true, URL: "GEN2-URL"},
		{Client: "cursor", Language: "go", EntryName: "mcp-language-server-go", Present: true, URL: "CURSOR-URL"},
	}

	pins := []mcpFrontSerenaPin{
		{Client: "claude-code", Origin: "GEN2-ROLLING", Path: "GEN2-BACKUP", SHA256: "GEN2-SUM"},
		{Client: "cursor", Origin: "CURSOR-ROLLING", Path: "CURSOR-BACKUP", SHA256: "CURSOR-SUM"},
	}

	merged := mergeMCPFrontReconcileReport(prior, 9200, serena, lsp, pins)

	// The port polarity is the OPPOSITE of the pre-state fields below, and
	// this assertion was inverted when the record was first written (it
	// demanded the FIRST generation's 9137). Port does not describe the
	// pre-reconcile state — it describes what the most recent forward run
	// WROTE into the live client configs, and generation 2 really did move
	// them to 9200. Keeping 9137 made the rollback judge the live entries
	// against a port the command had stopped writing, so its absent-row
	// removal guard stopped matching and the entries the cutover created were
	// left behind pointing at a retired port (codex bot PR #588).
	if merged.Port != 9200 {
		t.Fatalf("merge must adopt the LATEST generation's port (that is the port now written into the live client configs): got %d, want 9200", merged.Port)
	}
	if !merged.SnapshotComplete {
		t.Fatalf("every record this merge produces carries both recovery sections, so it must be marked snapshot-complete; the rollback refuses one that is not")
	}
	if merged.LSP == nil {
		t.Fatalf("the merged record's lsp section must be non-nil (an explicit empty section, never a missing one) — a nil slice marshals to `null`, which the rollback cannot tell apart from a lost section")
	}
	pinPaths := map[string]string{}
	for _, pin := range merged.Pins {
		if _, dup := pinPaths[pin.Client]; dup {
			t.Fatalf("merge produced a duplicate pin for %s: %+v", pin.Client, merged.Pins)
		}
		pinPaths[pin.Client] = pin.Path
	}
	if pinPaths["claude-code"] != "GEN1-BACKUP" {
		t.Fatalf("merge overwrote the recorded pin for claude-code: got %q, want GEN1-BACKUP (the generation-2 pin copies the already-rewritten config)", pinPaths["claude-code"])
	}
	if pinPaths["cursor"] != "CURSOR-BACKUP" {
		t.Fatalf("merge dropped the pin for a client generation 1 never migrated: got %q, want CURSOR-BACKUP", pinPaths["cursor"])
	}
	backups := map[string]string{}
	for _, row := range merged.Serena.Applied {
		if _, dup := backups[row.Client]; dup {
			t.Fatalf("merge produced a duplicate serena row for %s: %+v", row.Client, merged.Serena.Applied)
		}
		backups[row.Client] = row.BackupPath
	}
	if backups["claude-code"] != "GEN1-BACKUP" {
		t.Fatalf("merge overwrote a recorded serena row: claude-code backup = %q, want GEN1-BACKUP (the generation-2 backup captures the already-rewritten config)", backups["claude-code"])
	}
	if backups["cursor"] != "CURSOR-BACKUP" {
		t.Fatalf("merge dropped the retry row for a client generation 1 never migrated: cursor backup = %q, want CURSOR-BACKUP", backups["cursor"])
	}

	urls := map[string]string{}
	for _, row := range *merged.LSP {
		key := lspSnapshotKey(row)
		if _, dup := urls[key]; dup {
			t.Fatalf("merge produced a duplicate lsp row for %s/%s", row.Client, row.Language)
		}
		urls[key] = row.URL
	}
	if urls["claude-code\x00go"] != "GEN1-URL" {
		t.Fatalf("merge overwrote a recorded lsp pre-state row: got %q, want GEN1-URL", urls["claude-code\x00go"])
	}
	if urls["cursor\x00go"] != "CURSOR-URL" {
		t.Fatalf("merge dropped a new lsp pre-state row: got %q, want CURSOR-URL", urls["cursor\x00go"])
	}
}

// TestMCPFrontPR588_RollbackRequiresReconcileBeforeAnyModeDispatch is the P2
// flag-ordering guard.
//
// `--rollback` is a MODIFIER of `--reconcile-mcp-front`. Before the fix its
// dependency was validated only AFTER the per-mode dispatch, and the
// `--upgrade` / `--reconcile-hub-mode` exclusivity lists did not enumerate it
// — so `mcphub install --upgrade --rollback` ran a REAL binary upgrade with
// `--rollback` silently ignored.
//
// The assertion is the identity of the error: it must be the top-of-RunE
// dependency error, which can only be produced BEFORE any mode dispatch.
func TestMCPFrontPR588_RollbackRequiresReconcileBeforeAnyModeDispatch(t *testing.T) {
	for _, mode := range []string{"--upgrade", "--reconcile-hub-mode"} {
		t.Run(mode, func(t *testing.T) {
			c := newInstallCmdReal()
			c.SetArgs([]string{mode, "--rollback"})
			c.SetOut(new(testWriter))
			c.SetErr(new(testWriter))
			err := c.Execute()
			if err == nil {
				t.Fatalf("%s --rollback must be rejected, not silently dispatched as %s", mode, mode)
			}
			if err.Error() != "--rollback requires --reconcile-mcp-front" {
				t.Fatalf("%s --rollback must fail the top-of-RunE dependency gate BEFORE any mode dispatch; got a different error, which means the mode was entered first: %v", mode, err)
			}
		})
	}
}
