package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// TestIntentCollapseCmd_CheckPrintsReport verifies the `mcphub
// intent-collapse --check` command prints the header summary + per-task
// decision table from the DaemonIntentCollapseResult. Uses the check-fn seam
// so the test never touches a real state directory.
func TestIntentCollapseCmd_CheckPrintsReport(t *testing.T) {
	fakeRes := api.DaemonIntentCollapseResult{
		Changed: true,
		MergedStops: map[string]api.DaemonIntent{
			`\mcp-local-hub-foo-default`: {Desired: api.IntentDesiredStopped},
		},
		Entries: []api.MergeStopsEntry{
			{TaskName: `\mcp-local-hub-foo-default`, Action: api.MergeStopAdded, Reason: api.IntentReasonUserStop},
			{TaskName: `\mcp-local-hub-bar-default`, Action: api.MergeStopDroppedExpired},
		},
	}
	uninstallCheck := setIntentCollapseCheckFnForTest(func(stateDir string, now time.Time) (api.DaemonIntentCollapseResult, error) {
		if stateDir != "/fake/state" {
			t.Errorf("check called with stateDir=%q, want /fake/state", stateDir)
		}
		if !now.IsZero() {
			t.Errorf("check called with now=%v, want zero (let CheckDaemonIntentCollapse default it)", now)
		}
		return fakeRes, nil
	})
	defer uninstallCheck()
	uninstallDir := setIntentCollapseStateDirFnForTest(func() (string, error) { return "/fake/state", nil })
	defer uninstallDir()

	cmd := newIntentCollapseCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --check: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"state-dir: /fake/state",
		"mode: check (dry-run, no write)",
		"merge changes: true",
		"per-task decisions: 2",
		"TASK", "ACTION", "REASON",
		`\mcp-local-hub-foo-default`,
		`\mcp-local-hub-bar-default`,
		"add",
		"drop-expired",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; full:\n%s", want, out)
		}
	}
}

func TestIntentCollapseCmd_CheckExplainsBookkeepingCompactionDelta(t *testing.T) {
	fakeRes := api.DaemonIntentCollapseResult{
		Changed: true,
		MergedStops: map[string]api.DaemonIntent{
			`\mcp-local-hub-foo-default`: {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserStop},
		},
	}
	uninstallCheck := setIntentCollapseCheckFnForTest(func(stateDir string, now time.Time) (api.DaemonIntentCollapseResult, error) {
		if stateDir != "/fake/state" {
			t.Errorf("check called with stateDir=%q, want /fake/state", stateDir)
		}
		return fakeRes, nil
	})
	defer uninstallCheck()
	uninstallDir := setIntentCollapseStateDirFnForTest(func() (string, error) { return "/fake/state", nil })
	defer uninstallDir()

	cmd := newIntentCollapseCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --check: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "merge changes: true") {
		t.Fatalf("output missing changed=true header; full:\n%s", out)
	}
	if strings.Contains(out, "no per-task changes — daemon-intent.json stops already reflected in supervisor-intent.json") {
		t.Fatalf("bookkeeping changed result printed bare no-delta line; full:\n%s", out)
	}
	if !strings.Contains(out, "bookkeeping compaction: pruning redundant legacy-stop watermarks (no daemon lifecycle change)") {
		t.Fatalf("bookkeeping changed result did not explain compaction write; full:\n%s", out)
	}
}

// TestIntentCollapseCmd_RequiresCheckFlag verifies the command refuses to run
// without --check (the destructive merge is NOT exposed via the CLI in this
// PR — only the read-only preview is).
func TestIntentCollapseCmd_RequiresCheckFlag(t *testing.T) {
	// If the guard regresses, fail loudly rather than silently invoking the
	// state-dir resolver.
	uninstallDir := setIntentCollapseStateDirFnForTest(func() (string, error) {
		t.Fatalf("state-dir resolver called without --check; the --check guard regressed")
		return "", nil
	})
	defer uninstallDir()

	cmd := newIntentCollapseCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("Execute without --check: want error, got nil")
	}
	if !strings.Contains(err.Error(), "--check") {
		t.Fatalf("error %q does not mention --check guidance", err)
	}
}

// TestIntentCollapseCmd_CheckEndToEndIsPureRead drives the REAL
// CheckDaemonIntentCollapse against a t.TempDir-seeded state dir (no seam on
// the check fn) and asserts the command (1) succeeds, (2) reports the seeded
// active stop, and (3) leaves the state dir BYTE-UNCHANGED — no write, no
// pre-collapse-backup directory. This proves the operator-facing --check is a
// genuine dry-run against live state, the property the E-deploy plan relies on.
func TestIntentCollapseCmd_CheckEndToEndIsPureRead(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-foo-default`

	// Seed supervisor-intent.json (no stops) + daemon-intent.json with an
	// active user stop. The merge would ADD that stop — Changed=true — but
	// --check must not persist it.
	supPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(supPath, &api.SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	daemonPath := filepath.Join(stateDir, "daemon-intent.json")
	freshUpdatedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	daemonRaw := []byte(fmt.Sprintf(`{"tasks":{"\\mcp-local-hub-foo-default":{"desired":"stopped","reason":"user-stop","updated_at":%q}}}`, freshUpdatedAt))
	if err := os.WriteFile(daemonPath, daemonRaw, 0o600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
	}

	supBefore, err := os.ReadFile(supPath)
	if err != nil {
		t.Fatalf("read supervisor-intent.json before: %v", err)
	}

	uninstallDir := setIntentCollapseStateDirFnForTest(func() (string, error) { return stateDir, nil })
	defer uninstallDir()

	cmd := newIntentCollapseCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --check (real): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "merge changes: true") {
		t.Errorf("expected merge changes: true for a seeded new stop; full:\n%s", out)
	}
	if !strings.Contains(out, taskName) {
		t.Errorf("expected report to name %q; full:\n%s", taskName, out)
	}

	// (1) supervisor-intent.json byte-unchanged.
	supAfter, err := os.ReadFile(supPath)
	if err != nil {
		t.Fatalf("read supervisor-intent.json after: %v", err)
	}
	if !bytes.Equal(supBefore, supAfter) {
		t.Fatalf("--check mutated supervisor-intent.json; before=%s after=%s", supBefore, supAfter)
	}

	// (2) no pre-collapse-backup-* directory created.
	backups, err := filepath.Glob(filepath.Join(stateDir, "pre-collapse-backup-*"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 0 {
		t.Fatalf("--check created %d pre-collapse-backup dirs; want 0: %v", len(backups), backups)
	}
}

func TestIntentCollapseCmd_CheckDoesNotCreateStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "missing-state")
	defer api.SetDaemonStateRootForTest(stateDir)()

	cmd := newIntentCollapseCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --check against missing state dir: %v; output=%s", err, buf.String())
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("--check created state dir %s; stat err=%v", stateDir, err)
	}
}
