// Package cli — tests for `mcphub workspace prune`.
package cli

import (
	"bytes"
	"encoding/json"
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
	defer unlock()
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
	idleDir := t.TempDir()
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
		unlock()
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
		unlock()
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		unlock()
		t.Fatalf("save: %v", err)
	}
	unlock()

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
	if !strings.Contains(out, idleCanon) {
		t.Errorf("--idle run should name the idle workspace; got:\n%s", out)
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
