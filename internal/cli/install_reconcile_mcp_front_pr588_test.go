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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func TestMCPFrontGuardPinnedBackupCapSymmetry(t *testing.T) {
	backupPath := filepath.Join(t.TempDir(), "claude-code.backup")
	before := bytes.Repeat([]byte("x"), api.MaxClientConfigBackupBytes+1)
	if err := os.WriteFile(backupPath, before, 0o600); err != nil {
		t.Fatalf("seed oversized client-config backup: %v", err)
	}
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")
	pinWriteCalled := false
	journal := &mcpFrontReconcileJournal{
		reportPath: filepath.Join(t.TempDir(), "mcp-front-reconcile.json"),
		record:     mcpFrontReconcileReport{Rows: map[string]mcpFrontReconcileRow{}},
		writeClientConfigPin: func(string, []byte) error {
			pinWriteCalled = true
			return errors.New("unexpected pin write")
		},
	}

	pin, err := journal.pinBackup(context.Background(), "claude-code", backupPath)
	var readErr *api.StateFileReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("oversized pin preparation err=%v, want *api.StateFileReadError", err)
	}
	if readErr.Category != api.StateFileReadErrorTooLarge {
		t.Fatalf("oversized pin preparation category=%q, want %q", readErr.Category, api.StateFileReadErrorTooLarge)
	}
	if pinWriteCalled {
		t.Fatal("oversized pin preparation invoked writeClientConfigPin")
	}
	if pin != (mcpFrontSerenaPin{}) {
		t.Fatalf("oversized pin preparation returned a durable pin: %+v", pin)
	}
	after, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read client-config backup after failed preparation: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("oversized pin preparation mutated the client-config backup")
	}
	if row, found := journal.record.Rows[key]; found {
		t.Fatalf("oversized pin preparation created a Serena journal row: %+v", row)
	}
}

// mcpFrontPR588Env redirects every path resolution this command family
// performs to one throwaway directory and returns it.
//
// HOME/USERPROFILE/LOCALAPPDATA alone are NOT enough. This command drives a
// REAL reconcile over clients.AllClients(), and the remaining adapters resolve
// their config path from %APPDATA%, $XDG_CONFIG_HOME, $MIMOCODE_*,
// %ProgramData%, $COPILOT_HOME and $KIMI_CODE_HOME. Before
// neutralizeClientConfigPathEnv was added here, the un-redirected %APPDATA%
// admitted the developer's REAL vscode adapter
// (%APPDATA%\Code\User\mcp.json) and the forward pass rewrote their live MCP
// config to the test's ephemeral port. neutralizeClientConfigPathEnv is the one
// owner of that list — extend it there, never inline here.
func mcpFrontPR588Env(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	neutralizeClientConfigPathEnv(t, tmp)
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
	serenaKey := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")
	firstRow, ok := first.Rows[serenaKey]
	if !ok || firstRow.Pin == nil || firstRow.Pin.Path == "" {
		t.Fatalf("forward #1 must record a row-owned serena pin; row=%+v", firstRow)
	}
	firstBackup := firstRow.Pin.Path

	// --- forward run #2 (the documented retry) --------------------------
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile #2: %v", err)
	}
	second := readPersistedMCPFrontReport(t, reportPath)
	secondRow := second.Rows[serenaKey]
	secondBackup := secondRow.Pin.Path
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
	// A NEWER artifact and an OLDER one need OPPOSITE remedies, and the
	// operator cannot tell which they have from the version number alone. A
	// higher version means they downgraded (or ran a different install), so the
	// binary that can read this file is the newer one — sending them to "the
	// older mcphub that wrote this file" points at the one binary guaranteed
	// not to understand it.
	if !strings.Contains(err.Error(), "NEWER mcphub") {
		t.Fatalf("a higher-than-supported version must send the operator to the NEWER binary; got %v", err)
	}
	if strings.Contains(err.Error(), "OLDER mcphub") {
		t.Fatalf("a higher-than-supported version must NOT offer the legacy downgrade remedy; got %v", err)
	}
}

// TestMCPFrontPR588_MergePreservesRecordedRowsAndAddsNewOnes is the unit-level
// statement of the merge invariant, independent of any client on disk: a row an
// earlier generation recorded is never replaced, and a row for a key not yet
// recorded IS added (that is the retry of a previously-failed client, whose
// captured state genuinely is its original pre-state).
func TestMCPFrontPR588_MergePreservesRecordedRowsAndAddsNewOnes(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	inheritedName := api.LSPRouterEntryName("rust")
	gen1 := v3LSPSnapshot("claude-code", "go", name, true, "GEN1-URL")
	inherited := v3LSPSnapshot("claude-code", "rust", inheritedName, true, "RUST-GEN1-URL")
	first := v3Journal(t, 9137, nil,
		v3LSPAdd("claude-code", "go", name, gen1,
			v3LSPSnapshot("claude-code", "go", name, true, "FRONT-A")),
		v3LSPAdd("claude-code", "rust", inheritedName, inherited,
			v3LSPSnapshot("claude-code", "rust", inheritedName, true, "RUST-FRONT-A")),
	)
	gen2 := v3LSPSnapshot("claude-code", "go", name, true, "FRONT-A")
	cursor := v3LSPSnapshot("cursor", "go", name, true, "CURSOR-URL")
	retry := v3Journal(t, 9137, &first.record,
		v3LSPAdd("claude-code", "go", name, gen2,
			v3LSPSnapshot("claude-code", "go", name, true, "FRONT-B")),
		v3LSPAdd("cursor", "go", name, cursor,
			v3LSPSnapshot("cursor", "go", name, true, "FRONT-B")),
	)
	claudeKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "go", name)
	inheritedKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "claude-code", "rust", inheritedName)
	cursorKey := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, "cursor", "go", name)
	if got := retry.record.Rows[claudeKey].Baseline.LSP.URL; got != "GEN1-URL" {
		t.Fatalf("retry overwrote first immutable baseline: %q", got)
	}
	if got := retry.record.Rows[cursorKey].Baseline.LSP.URL; got != "CURSOR-URL" {
		t.Fatalf("retry omitted new row baseline: %q", got)
	}
	if retry.record.ActivePlan.Port != 9137 {
		t.Fatalf("active generation port=%d, want 9137", retry.record.ActivePlan.Port)
	}
	if len(retry.record.ActivePlan.Rows) != 3 || len(retry.record.ActivePlan.Operations) != 3 {
		t.Fatalf("merged active plan rows=%d operations=%d, want one retained, one replaced, and one new",
			len(retry.record.ActivePlan.Rows), len(retry.record.ActivePlan.Operations))
	}
	current, ok := activeMCPFrontPlanOperation(retry.record.ActivePlan, claudeKey)
	if !ok || current.PreState.LSP == nil || current.IntendedState.LSP == nil ||
		current.PreState.LSP.URL != "FRONT-A" || current.IntendedState.LSP.URL != "FRONT-B" {
		t.Fatalf("current-generation operation did not replace inherited state: %+v", current)
	}
	retained, ok := activeMCPFrontPlanOperation(retry.record.ActivePlan, inheritedKey)
	if !ok || retained.PreState.LSP == nil || retained.IntendedState.LSP == nil ||
		retained.PreState.LSP.URL != "RUST-GEN1-URL" || retained.IntendedState.LSP.URL != "RUST-FRONT-A" {
		t.Fatalf("settled inherited operation was not preserved: %+v", retained)
	}
	if _, ok := activeMCPFrontPlanOperation(retry.record.ActivePlan, cursorKey); !ok {
		t.Fatalf("new row %q has no active-plan operation", cursorKey)
	}
}

func TestMCPFrontPR588_MergeRefusesAnInheritedRowWithoutPlanAuthority(t *testing.T) {
	name := api.LSPRouterEntryName("go")
	first := v3Journal(t, 9137, nil, v3LSPAdd(
		"claude-code", "go", name,
		v3LSPSnapshot("claude-code", "go", name, true, "GEN1-URL"),
		v3LSPSnapshot("claude-code", "go", name, true, "FRONT-A"),
	))
	first.record.ActivePlan.Rows = nil
	first.record.ActivePlan.Operations = nil

	_, err := newMCPFrontV3Journal(first.reportPath, &first.record, first.record.Version, first.record.Generation, 9137, &api.LSPRouterClientPlan{Port: 9137})
	if err == nil || !strings.Contains(err.Error(), "has no inherited active-plan operation") {
		t.Fatalf("merge accepted an inherited row whose persisted plan carried no authority: %v", err)
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
