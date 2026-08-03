// Package cli — tests for `mcphub workspace prune`.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// seedSerenaRow writes a serena (sentinel) registry row for the given canonical
// path under the hermetic state dir established by withStateDir.
func seedSerenaRow(t *testing.T, canonical string) {
	t.Helper()
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer assertRegistryReleased(t, unlock)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	key := api.WorkspaceKey(canonical)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: canonical,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9121,
		TaskName:      "mcp-local-hub-serena-" + key,
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
}

// canonicalForCleanup returns the cleanup-canonical key form (exists-tolerant)
// used for a possibly-deleted path, matching what PruneWorkspace computes.
func canonicalForCleanup(t *testing.T, raw string) string {
	t.Helper()
	c, err := api.CanonicalWorkspacePathForCleanup(raw)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathForCleanup(%s): %v", raw, err)
	}
	return c
}

// runWorkspacePruneCmd executes `workspace prune` with args, optionally piping
// stdin, and returns combined stdout+stderr plus the command error.
func runWorkspacePruneCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	c := newWorkspaceCmd()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs(append([]string{"prune"}, args...))
	if stdin != "" {
		c.SetIn(strings.NewReader(stdin))
	} else {
		c.SetIn(strings.NewReader("")) // non-*os.File → inputIsTerminal=false
	}
	err := c.Execute()
	return buf.String(), err
}

// runWorkspacePruneCmdSplit executes `workspace prune` with args, capturing
// STDOUT and STDERR into SEPARATE buffers (unlike runWorkspacePruneCmd, which
// merges them). Needed by the --json tests, which must prove STDOUT is a pure
// machine-readable JSON payload with the prompt/table/notes on STDERR. stdin is
// piped as a non-*os.File reader; pass a non-nil isTerminal to stub an
// interactive terminal (the prompt path is otherwise unreachable from a test
// because inputIsTerminal returns false for non-*os.File readers).
func runWorkspacePruneCmdSplit(t *testing.T, stdin string, isTerminal func() bool, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	if isTerminal != nil {
		prev := pruneInputIsTerminal
		pruneInputIsTerminal = func(io.Reader) bool { return isTerminal() }
		t.Cleanup(func() { pruneInputIsTerminal = prev })
	}
	c := newWorkspaceCmd()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	c.SetOut(outBuf)
	c.SetErr(errBuf)
	c.SilenceUsage = true
	c.SetArgs(append([]string{"prune"}, args...))
	c.SetIn(strings.NewReader(stdin)) // non-*os.File reader
	err = c.Execute()
	return outBuf.String(), errBuf.String(), err
}

// countSerenaRows returns the number of serena rows currently in the registry.
func countSerenaRows(t *testing.T) int {
	t.Helper()
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return len(reg.SerenaEntries())
}

// TestWorkspacePrune_DryRunListsWithoutMutation seeds a deleted-dir orphan and
// asserts --dry-run lists it, exits 0, and removes NOTHING.
func TestWorkspacePrune_DryRunListsWithoutMutation(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	out, err := runWorkspacePruneCmd(t, "", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "deleted-dir") {
		t.Errorf("dry-run output should classify the orphan as deleted-dir; got:\n%s", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run output should say dry-run; got:\n%s", out)
	}
	if n := countSerenaRows(t); n != 1 {
		t.Errorf("dry-run must NOT remove rows; got %d remaining, want 1", n)
	}
}

// TestWorkspacePrune_YesPrunesOrphans seeds a deleted-dir orphan and a live
// workspace, then `prune --yes` removes only the orphan.
func TestWorkspacePrune_YesPrunesOrphans(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	// A live workspace row that must NOT be pruned.
	liveDir := t.TempDir()
	liveCanon := canonicalForCleanup(t, liveDir)
	seedSerenaRow(t, liveCanon)

	if n := countSerenaRows(t); n != 2 {
		t.Fatalf("setup: want 2 serena rows, got %d", n)
	}

	out, err := runWorkspacePruneCmd(t, "", "--yes")
	if err != nil {
		t.Fatalf("prune --yes: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Pruned 1 workspace") {
		t.Errorf("output should report 1 pruned; got:\n%s", out)
	}
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reg.SerenaEntries()
	if len(got) != 1 {
		t.Fatalf("want 1 surviving serena row, got %d", len(got))
	}
	if got[0].WorkspacePath != liveCanon {
		t.Errorf("surviving row = %q, want the live workspace %q", got[0].WorkspacePath, liveCanon)
	}
}

// TestWorkspacePrune_NonInteractiveWithoutFlagsRefuses asserts a non-TTY shell
// with neither --dry-run nor --yes refuses and performs no teardown.
func TestWorkspacePrune_NonInteractiveWithoutFlagsRefuses(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	out, err := runWorkspacePruneCmd(t, "") // empty stdin → non-interactive
	if err == nil {
		t.Fatalf("non-interactive prune without --yes/--dry-run should refuse; output: %s", out)
	}
	if !strings.Contains(out, "non-interactive") {
		t.Errorf("refusal should mention non-interactive; got:\n%s", out)
	}
	if n := countSerenaRows(t); n != 1 {
		t.Errorf("refused prune must NOT remove rows; got %d, want 1", n)
	}
}

// TestWorkspacePrune_JSONShape asserts --json --dry-run emits an array of
// {PruneReport, reason} with the classification reason populated.
func TestWorkspacePrune_JSONShape(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	out, err := runWorkspacePruneCmd(t, "", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("prune --dry-run --json: %v\noutput: %s", err, out)
	}
	var arr []struct {
		Workspace    string `json:"workspace"`
		WorkspaceKey string `json:"workspace_key"`
		Backend      string `json:"backend"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &arr); err != nil {
		t.Fatalf("output is not a valid JSON array: %v\noutput: %s", err, out)
	}
	if len(arr) != 1 {
		t.Fatalf("want 1 JSON element, got %d (%s)", len(arr), out)
	}
	if arr[0].Reason != string(api.OrphanReasonDeletedDir) {
		t.Errorf("reason = %q, want %q", arr[0].Reason, api.OrphanReasonDeletedDir)
	}
	if arr[0].Workspace != gone {
		t.Errorf("workspace = %q, want %q", arr[0].Workspace, gone)
	}
	if arr[0].WorkspaceKey == "" {
		t.Errorf("workspace_key should be populated")
	}
}

// TestWorkspacePrune_IdleAddsCandidates asserts --idle adds idle workspaces as
// candidates that the structural-only run would not flag.
func TestWorkspacePrune_IdleAddsCandidates(t *testing.T) {
	withStateDir(t)

	// A live, present, non-worktree workspace with stale activity.
	const idleLeaf = "idle-workspace-leaf"
	idleDir := makeWorkspaceDirNamed(t, t.TempDir(), strings.Repeat("shared-parent-", 8)+idleLeaf, nil)
	idleCanon := canonicalForCleanup(t, idleDir)
	// Seed with an old LastToolsCallAt so the idle signal applies.
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	key := api.WorkspaceKey(idleCanon)
	if err := reg.Load(); err != nil {
		assertRegistryReleased(t, unlock)
		t.Fatalf("load: %v", err)
	}
	e := api.WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: idleCanon,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9121,
		TaskName:      "mcp-local-hub-serena-" + key,
	}
	e.LastToolsCallAt = time.Now().Add(-72 * time.Hour)
	if err := reg.PutSerena(e); err != nil {
		assertRegistryReleased(t, unlock)
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		assertRegistryReleased(t, unlock)
		t.Fatalf("save: %v", err)
	}
	assertRegistryReleased(t, unlock)

	// Without --idle, the structural-only run finds no orphan.
	out, err := runWorkspacePruneCmd(t, "", "--dry-run")
	if err != nil {
		t.Fatalf("structural dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No orphaned workspaces") {
		t.Errorf("structural-only run should find no orphans; got:\n%s", out)
	}

	// With --idle 48h, the stale workspace becomes an idle candidate.
	out, err = runWorkspacePruneCmd(t, "", "--idle", "48h", "--dry-run")
	if err != nil {
		t.Fatalf("idle dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, string(api.OrphanReasonIdle)) {
		t.Errorf("--idle run should flag the stale workspace as idle; got:\n%s", out)
	}
	if !strings.Contains(out, idleLeaf) {
		t.Errorf("--idle run should preserve the idle workspace identity leaf %q; got:\n%s", idleLeaf, out)
	}
}

// TestWorkspacePrune_NoOrphansReportsClean asserts a registry with only a live
// workspace reports no orphans and exits 0.
func TestWorkspacePrune_NoOrphansReportsClean(t *testing.T) {
	withStateDir(t)
	liveCanon := canonicalForCleanup(t, t.TempDir())
	seedSerenaRow(t, liveCanon)

	out, err := runWorkspacePruneCmd(t, "", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No orphaned workspaces") {
		t.Errorf("clean registry should report no orphans; got:\n%s", out)
	}
}

// TestWorkspacePrune_DefaultMarkerClearedOnPrune registers a default workspace,
// deletes its directory, then prunes — the default marker must be cleared.
func TestWorkspacePrune_DefaultMarkerClearedOnPrune(t *testing.T) {
	withStateDir(t)
	regPath, _ := api.DefaultRegistryPath()
	stateDir := filepath.Dir(regPath)

	// Create a real workspace dir, register it as a serena row + default marker,
	// then delete the dir so it classifies as deleted-dir.
	wsDir := filepath.Join(t.TempDir(), "defws")
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	canon, err := api.CanonicalWorkspacePath(wsDir)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	seedSerenaRow(t, canon)
	if err := api.WriteDefaultWorkspace(stateDir, canon); err != nil {
		t.Fatalf("write default: %v", err)
	}
	// Delete the directory so it becomes a deleted-dir orphan.
	if err := os.RemoveAll(wsDir); err != nil {
		t.Fatalf("rm: %v", err)
	}

	// The cleanup-canonical key of the now-deleted dir must match what we seeded
	// (abs+clean fallback == the resolved path for a path with no symlinks).
	cleanupCanon := canonicalForCleanup(t, wsDir)
	if cleanupCanon != canon {
		// If they differ (symlinked temp root), re-seed under the cleanup key so
		// the prune teardown can find + remove the row.
		seedSerenaRow(t, cleanupCanon)
		if err := api.WriteDefaultWorkspace(stateDir, cleanupCanon); err != nil {
			t.Fatalf("re-write default: %v", err)
		}
	}

	got, err := api.ReadDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default before prune: %v", err)
	}
	if got == "" {
		t.Fatalf("default marker should be set before prune")
	}

	out, err := runWorkspacePruneCmd(t, "", "--yes")
	if err != nil {
		t.Fatalf("prune --yes: %v\n%s", err, out)
	}

	cleared, err := api.ReadDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default after prune: %v", err)
	}
	if cleared != "" {
		t.Errorf("default marker should be cleared after pruning the default workspace; got %q", cleared)
	}
}

// TestWorkspacePrune_BackendNoOpNotCountedAsPruned (Finding 1) seeds a
// serena-ONLY deleted-dir orphan and prunes it with --backend lsp. The lsp
// backend has nothing to remove (the row is serena, not LSP), so PruneWorkspace
// returns an existence-tolerant zero-removal report. The command MUST NOT count
// it as pruned: the totals/exit reflect zero actual teardown, the candidate is
// surfaced as skipped, and the serena row remains (correct — --backend lsp does
// not touch serena rows).
func TestWorkspacePrune_BackendNoOpNotCountedAsPruned(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone) // serena-only row, no LSP rows

	out, err := runWorkspacePruneCmd(t, "", "--backend", "lsp", "--yes")
	if err != nil {
		t.Fatalf("prune --backend lsp --yes: %v\noutput: %s", err, out)
	}
	// The zero-removal candidate must report 0 pruned, 0 LSP, 0 serena.
	if !strings.Contains(out, "Pruned 0 workspace(s) (0 LSP rows, 0 serena rows)") {
		t.Errorf("a serena-only workspace pruned with --backend lsp must report 0 pruned / 0 rows; got:\n%s", out)
	}
	// And it must be surfaced as a skip, not silently dropped.
	if !strings.Contains(out, "skipped") {
		t.Errorf("the existence-tolerant no-op should be surfaced as skipped; got:\n%s", out)
	}
	// The serena row must remain — --backend lsp does not touch it.
	if n := countSerenaRows(t); n != 1 {
		t.Errorf("--backend lsp must NOT remove the serena row; got %d remaining, want 1", n)
	}
}

// TestWorkspacePrune_BackendNoOpJSONOmitsCandidate (Finding 1, --json variant)
// asserts the --json payload for a serena-only workspace pruned with
// --backend lsp is an EMPTY array (the zero-removal no-op is not a pruned row),
// and STDOUT stays clean JSON.
func TestWorkspacePrune_BackendNoOpJSONOmitsCandidate(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	stdout, _, err := runWorkspacePruneCmdSplit(t, "", nil, "--backend", "lsp", "--yes", "--json")
	if err != nil {
		t.Fatalf("prune --backend lsp --yes --json: %v\nstdout: %s", err, stdout)
	}
	var arr []prunePlanReport
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &arr); uerr != nil {
		t.Fatalf("stdout is not a valid JSON array: %v\nstdout: %s", uerr, stdout)
	}
	if len(arr) != 0 {
		t.Errorf("a zero-removal no-op must NOT appear in the JSON pruned list; got %d elements:\n%s", len(arr), stdout)
	}
	if n := countSerenaRows(t); n != 1 {
		t.Errorf("--backend lsp must NOT remove the serena row; got %d, want 1", n)
	}
}

// TestWorkspacePrune_PartialReportRecordedOnError (Finding 2) injects a
// PruneWorkspace partial-failure: the report carries LSP rows that WERE torn
// down ALONGSIDE an error from a later (serena) phase. The command MUST still
// record + display the partial removal (the LSP rows + the totals) AND surface
// the error, rather than dropping the report and claiming nothing was removed.
func TestWorkspacePrune_PartialReportRecordedOnError(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone) // an orphan so there is a candidate to apply against

	// Stub the shared teardown owner to mimic a phase failure AFTER the LSP phase
	// mutated state (e.g. --backend all removed LSP rows, then the serena
	// supervisor-intent teardown errored): a non-nil report with LSPRemoved
	// populated returned ALONGSIDE the error.
	prev := pruneWorkspaceFn
	pruneWorkspaceFn = func(workspacePath, backend string) (*api.PruneReport, error) {
		return &api.PruneReport{
			Workspace:    workspacePath,
			WorkspaceKey: api.WorkspaceKey(workspacePath),
			Backend:      backend,
			LSPRemoved:   []string{"go", "python"},
		}, errors.New("paired serena supervisor teardown for workspace: reconcile failed")
	}
	t.Cleanup(func() { pruneWorkspaceFn = prev })

	stdout, stderr, err := runWorkspacePruneCmdSplit(t, "", nil, "--backend", "all", "--yes")
	if err != nil {
		// The command returns nil (bulk best-effort) — the error is a per-candidate
		// warning, not a fatal command error.
		t.Fatalf("prune should not fail the whole command on a per-candidate error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	// The partial LSP removal must be reflected in the totals on STDOUT.
	if !strings.Contains(stdout, "Pruned 1 workspace(s) (2 LSP rows, 0 serena rows)") {
		t.Errorf("partial removal (2 LSP rows) must appear in the totals; got stdout:\n%s", stdout)
	}
	// The phase error must still be surfaced as a warning on STDERR.
	if !strings.Contains(stderr, "failed") || !strings.Contains(stderr, "reconcile failed") {
		t.Errorf("the phase error must be surfaced; got stderr:\n%s", stderr)
	}
}

// TestWorkspacePrune_PartialReportRecordedOnError_JSON (Finding 2, --json
// variant) asserts the injected partial report appears in the --json payload on
// STDOUT (so a scripted consumer sees the LSP rows that were actually removed),
// with the error warning isolated to STDERR.
func TestWorkspacePrune_PartialReportRecordedOnError_JSON(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	prev := pruneWorkspaceFn
	pruneWorkspaceFn = func(workspacePath, backend string) (*api.PruneReport, error) {
		return &api.PruneReport{
			Workspace:    workspacePath,
			WorkspaceKey: api.WorkspaceKey(workspacePath),
			Backend:      backend,
			LSPRemoved:   []string{"go"},
		}, errors.New("serena teardown errored")
	}
	t.Cleanup(func() { pruneWorkspaceFn = prev })

	stdout, stderr, err := runWorkspacePruneCmdSplit(t, "", nil, "--backend", "all", "--yes", "--json")
	if err != nil {
		t.Fatalf("prune --json should not fail the whole command: %v\nstdout: %s", err, stdout)
	}
	var arr []prunePlanReport
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &arr); uerr != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", uerr, stdout)
	}
	if len(arr) != 1 {
		t.Fatalf("partial report must be present in the JSON payload; got %d elements:\n%s", len(arr), stdout)
	}
	if len(arr[0].LSPRemoved) != 1 || arr[0].LSPRemoved[0] != "go" {
		t.Errorf("JSON must carry the partially-removed LSP rows; got %#v", arr[0].LSPRemoved)
	}
	if !strings.Contains(stderr, "serena teardown errored") {
		t.Errorf("the error must be surfaced on stderr; got:\n%s", stderr)
	}
}

// TestWorkspacePrune_InteractiveJSONPromptToStderr (Finding 3) drives the
// interactive --json prompt path (stubbed terminal, no --yes) with a "n"
// response. STDOUT must be a clean, parseable JSON array (the candidate table +
// "[y/N]" prompt + abort note all routed to STDERR), so a scripted --json
// consumer still parses STDOUT even when a prompt fired.
func TestWorkspacePrune_InteractiveJSONPromptToStderr(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	// "n" → abort; the abort branch under --json emits an empty JSON array.
	stdout, stderr, err := runWorkspacePruneCmdSplit(t, "n\n", func() bool { return true }, "--json")
	if err != nil {
		t.Fatalf("interactive --json prompt (answer n) should not error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	// STDOUT must be pure JSON — nothing else.
	var arr []prunePlanReport
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &arr); uerr != nil {
		t.Fatalf("STDOUT must be clean parseable JSON (prompt/table must go to stderr); got stdout:\n%s\n(unmarshal err: %v)", stdout, uerr)
	}
	// The prompt + table must have gone to STDERR, not STDOUT.
	if strings.Contains(stdout, "[y/N]") || strings.Contains(stdout, "WORKSPACE") {
		t.Errorf("the prompt/candidate table leaked into STDOUT; got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "[y/N]") {
		t.Errorf("the prompt should be on STDERR under --json; got stderr:\n%s", stderr)
	}
	// The row must remain — the operator answered "n".
	if n := countSerenaRows(t); n != 1 {
		t.Errorf("aborted prune must NOT remove rows; got %d, want 1", n)
	}
}

// TestWorkspacePrune_DryRunJSONCleanStdout (Finding 3 corollary) confirms the
// --dry-run --json path still emits clean JSON to STDOUT with nothing else (no
// prompt is ever shown on the dry-run path).
func TestWorkspacePrune_DryRunJSONCleanStdout(t *testing.T) {
	withStateDir(t)
	gone := canonicalForCleanup(t, filepath.Join(t.TempDir(), "no-such", "ws"))
	seedSerenaRow(t, gone)

	stdout, stderr, err := runWorkspacePruneCmdSplit(t, "", nil, "--dry-run", "--json")
	if err != nil {
		t.Fatalf("--dry-run --json: %v\nstdout: %s", err, stdout)
	}
	var arr []prunePlanReport
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &arr); uerr != nil {
		t.Fatalf("--dry-run --json STDOUT must be clean JSON; got stdout:\n%s\n(err: %v)", stdout, uerr)
	}
	if len(arr) != 1 {
		t.Fatalf("dry-run --json should list the 1 candidate; got %d:\n%s", len(arr), stdout)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("--dry-run --json should emit nothing on STDERR; got:\n%s", stderr)
	}
}

// TestWorkspacePrune_HelpMentionsDeadWorktreeInGateOnSet covers Finding 3 (r6):
// the `--idle` help paragraph previously said that without --idle only
// agent-worktree and deleted-dir run, but a bare/dry-run prune ALSO considers
// dead-worktree when daemons.prune_dead_worktrees is on (the default). The help
// text must enumerate dead-worktree alongside the other structural signals so it
// matches the implementation.
func TestWorkspacePrune_HelpMentionsDeadWorktreeInGateOnSet(t *testing.T) {
	long := newWorkspacePruneCmd().Long
	idleIdx := strings.Index(long, "--idle <dur>")
	if idleIdx < 0 {
		t.Fatalf("prune help is missing the --idle paragraph:\n%s", long)
	}
	// The --idle paragraph runs to the next flag bullet (--backend).
	para := long[idleIdx:]
	if end := strings.Index(para, "--backend"); end >= 0 {
		para = para[:end]
	}
	if !strings.Contains(para, "dead-worktree") {
		t.Fatalf("--idle help paragraph must mention dead-worktree (it is part of the gate-on structural set); got:\n%s", para)
	}
	if !strings.Contains(para, "prune_dead_worktrees") {
		t.Fatalf("--idle help paragraph should name the daemons.prune_dead_worktrees gate; got:\n%s", para)
	}
}

// redirectSettingsPath points api.SettingsPath() at a hermetic temp dir on every
// GOOS (Windows resolves via %LOCALAPPDATA%, Linux/macOS via $XDG_DATA_HOME, with
// HOME/USERPROFILE backstopped) so the test NEVER reads or writes the live
// gui-preferences.yaml. It returns the resolved settings-file path.
func redirectSettingsPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)  // Windows
	t.Setenv("XDG_DATA_HOME", dir) // Linux/macOS
	t.Setenv("HOME", dir)          // POSIX home backstop
	t.Setenv("USERPROFILE", dir)   // Windows home backstop
	p := api.SettingsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	return p
}

// seedSetting persists key=value at the redirected SettingsPath() THROUGH the
// hardened state-file writer (SettingsSetIn → WriteStateFileBytesAtomic), so the
// resulting gui-preferences.yaml carries the owner-only DACL the READ gate
// requires — a raw os.WriteFile on a corp/CI Windows t.TempDir() inherits an
// Authenticated-Users ACE that the state-file READ gate hard-rejects (it refuses
// any non-allowlisted SID granted WRITE/DAC/DELETE, regardless of relax env), so
// SettingsList would then fail with a DACL error instead of returning the value.
func seedSetting(t *testing.T, p, key, value string) {
	t.Helper()
	if err := api.NewAPI().SettingsSetIn(p, key, value); err != nil {
		t.Fatalf("SettingsSetIn(%s=%s): %v", key, value, err)
	}
}

// TestPruneDeadWorktreesEnabled_FailClosed covers Finding 2: the CLI's
// dead-worktree-gate resolution must FAIL CLOSED, exactly matching the GUI
// sweeper's defaultPruneDeadWorktrees. The dead-worktree prune is DESTRUCTIVE,
// so a settings-read error (malformed gui-preferences.yaml) MUST resolve the
// gate to false (signal disabled) — never enable it. A successful read of an
// ABSENT key applies the registry Default ("true"); a successful read of an
// explicit value is honored.
func TestPruneDeadWorktreesEnabled_FailClosed(t *testing.T) {
	t.Run("malformed settings file → read error → gate FALSE (fail closed)", func(t *testing.T) {
		p := redirectSettingsPath(t)
		// First create an owner-only file via the hardened writer, then overwrite
		// its CONTENT in place with malformed YAML. The in-place truncating write
		// preserves the owner-only DACL the writer installed, so SettingsList
		// reaches the YAML parse and fails THERE (the read error under test), not
		// at the file-DACL gate. A YAML SEQUENCE cannot unmarshal into the
		// map[string]string the reader expects → yaml.Unmarshal errors.
		seedSetting(t, p, api.PruneDeadWorktreesSettingKey, "true")
		if err := os.WriteFile(p, []byte("- not: a map\n- still: not\n"), 0o600); err != nil {
			t.Fatalf("overwrite with malformed settings: %v", err)
		}
		// Sanity-check the precondition: SettingsList genuinely errors here.
		if _, err := api.NewAPI().SettingsList(); err == nil {
			t.Fatalf("precondition: expected SettingsList to error on malformed YAML, got nil")
		}
		if pruneDeadWorktreesEnabled() {
			t.Fatalf("pruneDeadWorktreesEnabled() = true on a settings-read error; want false (fail closed — the destructive dead-worktree prune must NOT enable on an unreadable gate)")
		}
	})

	t.Run("absent settings file → registry default applies → gate TRUE", func(t *testing.T) {
		redirectSettingsPath(t) // no file written → SettingsList succeeds, key unset
		if !pruneDeadWorktreesEnabled() {
			t.Fatalf("pruneDeadWorktreesEnabled() = false with no persisted value; want true (registry Default \"true\" applies on a successful read of an unset key)")
		}
	})

	t.Run("explicit false persisted → gate FALSE", func(t *testing.T) {
		p := redirectSettingsPath(t)
		seedSetting(t, p, api.PruneDeadWorktreesSettingKey, "false")
		if pruneDeadWorktreesEnabled() {
			t.Fatalf("pruneDeadWorktreesEnabled() = true with the gate persisted \"false\"; want false (honor the operator's disable)")
		}
	})

	t.Run("explicit true persisted → gate TRUE", func(t *testing.T) {
		p := redirectSettingsPath(t)
		seedSetting(t, p, api.PruneDeadWorktreesSettingKey, "true")
		if !pruneDeadWorktreesEnabled() {
			t.Fatalf("pruneDeadWorktreesEnabled() = false with the gate persisted \"true\"; want true")
		}
	})
}
