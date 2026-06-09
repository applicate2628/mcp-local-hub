package cli

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestOverlayKeySetSortedDeterministic pins the supervisor-side key-set
// helper: keys are returned in sorted order (so the injected
// MCPHUB_DAEMON_ENV_OVERLAY_KEYS value is stable across map-iteration order),
// the original key spelling is preserved (so the wrapper's case-fold reader
// matches the supervisor-written os.Environ() entry on Windows), and an
// empty/nil map yields nil.
func TestOverlayKeySetSortedDeterministic(t *testing.T) {
	if got := overlayKeySet(nil); got != nil {
		t.Fatalf("overlayKeySet(nil) = %v, want nil", got)
	}
	if got := overlayKeySet(map[string]string{}); got != nil {
		t.Fatalf("overlayKeySet(empty) = %v, want nil", got)
	}
	got := overlayKeySet(map[string]string{
		"ZEBRA":            "z",
		"ALPHA":            "a",
		"MEMORY_FILE_PATH": "m",
	})
	want := []string{"ALPHA", "MEMORY_FILE_PATH", "ZEBRA"}
	if len(got) != len(want) {
		t.Fatalf("overlayKeySet = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("overlayKeySet[%d] = %q, want %q (sorted)", i, got[i], want[i])
		}
	}
}

// TestSupervisorInjectsOverlayKeySetReconstructsAcrossUnreadableReload is the
// supervisor↔wrapper round-trip for the Codex bot #268 daemon.go:380 P2 fix.
// It reproduces the supervisor's spawn-time env construction (the same pure
// helper chain TestMakeProductionSpawnFnAppliesOverlayEnv pins), captures the
// resulting child environment, and then drives the WRAPPER-side reconstruction
// (daemonOverlayKeysFromEnv → overlayMapFromInjectedKeys) against that
// environment to prove a key present in BOTH the manifest and the overlay
// resolves to the OVERLAY value even when the overlay file would be
// unreadable. Covers both GOOS for the comma join/split + case-fold readback.
func TestSupervisorInjectsOverlayKeySetReconstructsAcrossUnreadableReload(t *testing.T) {
	// Overlay key spelled platform-appropriately so the round-trip exercises
	// the real PATH-family case-fold path on Windows and the exact path on
	// POSIX. MEMORY_FILE_PATH is a non-PATH key common to both.
	pathKey := "PATH"
	if runtime.GOOS == "windows" {
		pathKey = "Path"
	}
	overlayEnv := map[string]string{
		pathKey:            "C:/overlay/bin",
		"MEMORY_FILE_PATH": "D:/overlay/memory.json",
	}
	manifest := map[string]string{
		// Same keys as the overlay, different values — the clobber the fix
		// must prevent.
		pathKey:            "C:/manifest/bin",
		"MEMORY_FILE_PATH": "C:/manifest/memory.json",
	}

	// --- Supervisor side: build the child env exactly as the spawn fn does. ---
	parent := os.Environ()
	merged := mergeDaemonEnv(parent, manifest, overlayEnv)
	if merged == nil {
		t.Fatalf("mergeDaemonEnv returned nil; expected non-empty with overlay+manifest")
	}
	merged = appendDaemonOverlayAppliedMarker(merged)
	merged = appendDaemonOverlayKeys(merged, overlayKeySet(overlayEnv))

	// The injected key set must carry both keys, comma-joined and sorted.
	if got := lookupEnvValue(merged, daemonOverlayKeysEnvVar); got == "" {
		t.Fatalf("%s not injected into child env", daemonOverlayKeysEnvVar)
	}

	// --- Wrapper side: reconstruct from the injected keys (file unreadable). ---
	injectedKeys := daemonOverlayKeysFromEnv(merged)
	if len(injectedKeys) != 2 {
		t.Fatalf("injected keys = %v, want 2 (overlay key set round-tripped)", injectedKeys)
	}
	reconstructed := overlayMapFromInjectedKeys(injectedKeys, merged)

	// The reconstructed overlay map is what daemonEnvWithOverlay merges over
	// the manifest (overlay-wins). Model that final merge to get cfg.Env.
	cfgEnv := overlayWinsMergeForTest(manifest, reconstructed)

	// Effective child value = append(parent==merged child env, cfg.Env...);
	// last duplicate wins. The overlay value must win for BOTH keys.
	if got := effectiveChildValueFromEnvMap(merged, cfgEnv, "MEMORY_FILE_PATH"); got != "D:/overlay/memory.json" {
		t.Fatalf("effective child MEMORY_FILE_PATH = %q, want overlay value (overlay wins on unreadable file)", got)
	}
	if got := effectiveChildValueFromEnvMap(merged, cfgEnv, "PATH"); got != "C:/overlay/bin" {
		t.Fatalf("effective child PATH = %q, want overlay value (case-fold readback, overlay wins)", got)
	}
}

// overlayWinsMergeForTest models mergeDaemonEnvMaps (manifest < overlay) as a
// plain map so the supervisor↔wrapper round-trip test can compute cfg.Env
// without depending on the unexported daemon.go merge surface signature.
func overlayWinsMergeForTest(manifest, overlay map[string]string) map[string]string {
	merged := mergeDaemonEnv(nil, manifest, overlay)
	out := make(map[string]string, len(merged))
	for _, kv := range merged {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}
