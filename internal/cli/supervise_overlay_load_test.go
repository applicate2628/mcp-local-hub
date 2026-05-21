package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

// TestLoadOverlayAtStartupMissingFileIsBenign verifies the missing-file
// path returns an empty Overlay + nil error and does NOT emit a
// fail-LOUD startup-failed event. This is the fresh-install case.
func TestLoadOverlayAtStartupMissingFileIsBenign(t *testing.T) {
	stateDir := t.TempDir()
	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	defer events.Close()

	ov, loadErr := loadOverlayAtStartup(stateDir, events, nil)
	if loadErr != nil {
		t.Fatalf("loadOverlayAtStartup on missing overlay returned error: %v", loadErr)
	}
	if ov == nil {
		t.Fatalf("missing-file path should still return an empty *Overlay, not nil")
	}
	if len(ov.Daemons) != 0 {
		t.Fatalf("missing-file overlay should have no daemons; got %d", len(ov.Daemons))
	}

	logBytes, _ := os.ReadFile(filepath.Join(stateDir, "supervisor-events.log"))
	logStr := string(logBytes)
	if strings.Contains(logStr, "supervise-startup-failed") {
		t.Fatalf("missing-file path must NOT emit supervise-startup-failed; got log:\n%s", logStr)
	}
	if strings.Contains(logStr, "daemon-env-overlay-load-failed") {
		t.Fatalf("missing-file path must NOT emit load-failed; got log:\n%s", logStr)
	}
}

// TestLoadOverlayAtStartupEmitsLoadedEvent verifies the success path
// emits `daemon-env-overlay-loaded` with the row count.
func TestLoadOverlayAtStartupEmitsLoadedEvent(t *testing.T) {
	stateDir := t.TempDir()
	overlayYAML := `version: 1
daemons:
  \mcp-local-hub-foo-default:
    env:
      Path: "C:/foo/bin"
    source: operator
`
	if err := os.WriteFile(filepath.Join(stateDir, "daemon-env-overrides.yaml"), []byte(overlayYAML), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	defer events.Close()

	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-foo-default`, Server: "foo", Daemon: "default"},
		},
	}
	ov, loadErr := loadOverlayAtStartup(stateDir, events, intent)
	if loadErr != nil {
		t.Fatalf("loadOverlayAtStartup: %v", loadErr)
	}
	if ov == nil || len(ov.Daemons) != 1 {
		t.Fatalf("expected 1 overlay row; got %v", ov)
	}

	logBytes, _ := os.ReadFile(filepath.Join(stateDir, "supervisor-events.log"))
	logStr := string(logBytes)
	if !strings.Contains(logStr, "daemon-env-overlay-loaded") {
		t.Fatalf("expected daemon-env-overlay-loaded in log; got:\n%s", logStr)
	}
	// No orphan: the single overlay row matches the single intent daemon.
	if strings.Contains(logStr, "daemon-env-overlay-orphan-row") {
		t.Fatalf("matched overlay+intent should NOT emit orphan-row; got:\n%s", logStr)
	}
}

// TestLoadOverlayAtStartupEmitsOrphanForUnknownTask verifies an overlay
// row whose taskName is NOT in the intent triggers the
// `daemon-env-overlay-orphan-row` event.
func TestLoadOverlayAtStartupEmitsOrphanForUnknownTask(t *testing.T) {
	stateDir := t.TempDir()
	overlayYAML := `version: 1
daemons:
  \mcp-local-hub-zombie-default:
    env:
      Path: "C:/zombie/bin"
    source: operator
`
	if err := os.WriteFile(filepath.Join(stateDir, "daemon-env-overrides.yaml"), []byte(overlayYAML), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	defer events.Close()

	// Empty intent: every overlay row is an orphan.
	intent := &api.SupervisorIntentFile{Daemons: nil}

	ov, loadErr := loadOverlayAtStartup(stateDir, events, intent)
	if loadErr != nil {
		t.Fatalf("loadOverlayAtStartup: %v", loadErr)
	}
	if ov == nil || len(ov.Daemons) != 1 {
		t.Fatalf("expected 1 overlay row; got %v", ov)
	}

	logBytes, _ := os.ReadFile(filepath.Join(stateDir, "supervisor-events.log"))
	logStr := string(logBytes)
	if !strings.Contains(logStr, "daemon-env-overlay-orphan-row") {
		t.Fatalf("unmatched overlay row should emit orphan-row; got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "zombie") {
		t.Fatalf("orphan-row event should name the zombie task; got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "prune-orphan-overlay-rows") {
		t.Fatalf("orphan-row event should suggest prune-orphan-overlay-rows remedy; got:\n%s", logStr)
	}
}

// TestLoadOverlayAtStartupBareKeyInOverlayMatchesCanonicalIntent verifies
// the normalization symmetry: an operator who hand-edited the overlay
// without a leading backslash should still match a canonical-form
// intent daemon (so the row is NOT classified orphan).
func TestLoadOverlayAtStartupBareKeyInOverlayMatchesCanonicalIntent(t *testing.T) {
	stateDir := t.TempDir()
	// Note: bare key (no leading backslash).
	overlayYAML := `version: 1
daemons:
  mcp-local-hub-foo-default:
    env:
      Path: "C:/foo/bin"
    source: operator
`
	if err := os.WriteFile(filepath.Join(stateDir, "daemon-env-overrides.yaml"), []byte(overlayYAML), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	defer events.Close()

	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			// Canonical (leading-backslash) intent task name.
			{TaskName: `\mcp-local-hub-foo-default`, Server: "foo", Daemon: "default"},
		},
	}

	_, loadErr := loadOverlayAtStartup(stateDir, events, intent)
	if loadErr != nil {
		t.Fatalf("loadOverlayAtStartup: %v", loadErr)
	}

	logBytes, _ := os.ReadFile(filepath.Join(stateDir, "supervisor-events.log"))
	logStr := string(logBytes)
	if strings.Contains(logStr, "daemon-env-overlay-orphan-row") {
		t.Fatalf("bare-form overlay key matching canonical intent should NOT be orphan; got:\n%s", logStr)
	}
}

// TestLoadOverlayAtStartupFailLoudOnNonRegularFile verifies the
// fail-LOUD path: when Load returns an error (here: overlay path is a
// directory, so Load's IsRegular check fails), loadOverlayAtStartup
// wraps the error with the overlay-quarantine remedy hint and emits
// both `daemon-env-overlay-load-failed` (warn) and
// `supervise-startup-failed` (error) audit rows.
func TestLoadOverlayAtStartupFailLoudOnNonRegularFile(t *testing.T) {
	stateDir := t.TempDir()
	// Create a DIRECTORY at the overlay path so Load's IsRegular check
	// rejects it.
	if err := os.Mkdir(filepath.Join(stateDir, "daemon-env-overrides.yaml"), 0o700); err != nil {
		t.Fatalf("mkdir as overlay: %v", err)
	}

	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	defer events.Close()

	ov, loadErr := loadOverlayAtStartup(stateDir, events, nil)
	if loadErr == nil {
		t.Fatalf("expected fail-LOUD error on non-regular overlay; got nil + %v", ov)
	}
	if !strings.Contains(loadErr.Error(), "overlay-quarantine") {
		t.Fatalf("expected error to suggest 'overlay-quarantine'; got %q", loadErr.Error())
	}

	logBytes, _ := os.ReadFile(filepath.Join(stateDir, "supervisor-events.log"))
	logStr := string(logBytes)
	if !strings.Contains(logStr, "daemon-env-overlay-load-failed") {
		t.Fatalf("expected daemon-env-overlay-load-failed event; got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "supervise-startup-failed") {
		t.Fatalf("expected supervise-startup-failed event; got:\n%s", logStr)
	}
}

// TestMakeProductionSpawnFnAppliesOverlayEnv verifies that the spawn
// factory plumbs LookupOverlay → ExpandParentPath → mergeDaemonEnv
// correctly. It constructs an overlay with a Path containing the
// ${parent_path} token, builds a spawn closure, and asserts that the
// SupervisorDaemon (after going through the closure's env-merge path)
// would receive a cmd.Env containing the expanded substring.
//
// Because cmd.Env is set inside the closure but the closure also
// invokes process.NoConsole + cmd.Start, we cannot intercept cmd.Env
// from outside without launching a real child. Instead this test
// builds the same merge chain directly to pin the contract — the
// production closure's env-merge logic is intentionally pure helper
// calls so this direct-merge test reflects what the closure does.
func TestMakeProductionSpawnFnAppliesOverlayEnv(t *testing.T) {
	overlay := &daemon_env_overlay.Overlay{
		Version: 1,
		Daemons: map[string]daemon_env_overlay.DaemonRow{
			`\mcp-local-hub-foo-default`: {
				Env: map[string]string{"Path": "C:/extra/bin;${parent_path}"},
			},
		},
	}

	d := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-foo-default`,
		Server:   "foo",
		Daemon:   "default",
		Command:  "echo",
		Args:     []string{"hello"},
	}

	// Recreate the closure's merge chain. The production code path:
	//   1. LookupOverlay returns map[string]string for the daemon.
	//   2. ExpandParentPath substitutes ${parent_path}.
	//   3. mergeDaemonEnv folds parent + manifest + overlay.
	overlayEnv := daemon_env_overlay.LookupOverlay(overlay, d.TaskName)
	if overlayEnv == nil {
		t.Fatalf("LookupOverlay returned nil; expected map with Path")
	}
	parent := []string{"PATH=/system/parent"}
	expanded, err := daemon_env_overlay.ExpandParentPath(overlayEnv, parent)
	if err != nil {
		t.Fatalf("ExpandParentPath: %v", err)
	}
	if strings.Contains(expanded["Path"], "${parent_path}") {
		t.Fatalf("ExpandParentPath did not substitute the token; got %q", expanded["Path"])
	}
	if !strings.Contains(expanded["Path"], "/system/parent") {
		t.Fatalf("ExpandParentPath did not interpolate parent; got %q", expanded["Path"])
	}

	merged := mergeDaemonEnv(parent, d.Env, expanded)
	if merged == nil {
		t.Fatalf("mergeDaemonEnv returned nil; expected non-empty slice when overlay env is set")
	}
	var pathLine string
	for _, kv := range merged {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			pathLine = kv
		}
	}
	if pathLine == "" {
		t.Fatalf("merged env missing PATH; got %v", merged)
	}
	if !strings.Contains(pathLine, "C:/extra/bin") {
		t.Fatalf("merged PATH should include overlay-prepended C:/extra/bin; got %q", pathLine)
	}
	if !strings.Contains(pathLine, "/system/parent") {
		t.Fatalf("merged PATH should preserve parent via ${parent_path}; got %q", pathLine)
	}
}
