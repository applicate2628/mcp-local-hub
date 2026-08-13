package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// Env vars the env-dump helper child reads. The sentinel gates the helper so
// it no-ops under a normal `go test` run; the dump-path tells it where to
// write its own os.Environ() so the parent can assert what env the production
// spawn closure actually composed.
const (
	overlayMarkerHelperSentinelEnv = "MCPHUB_OVERLAY_MARKER_DUMP_HELPER"
	overlayMarkerHelperDumpPathEnv = "MCPHUB_OVERLAY_MARKER_DUMP_PATH"
)

// TestSpawnEnvDumpHelper is the env-dump helper subprocess. Under a normal
// `go test` run the sentinel is unset and it no-ops (mirrors
// TestProductionTerminateFn_HelperSleep). When the production spawn closure
// launches THIS test binary as a child (Command=os.Args[0],
// Args=-test.run=^TestSpawnEnvDumpHelper$), the closure's composed cmd.Env
// becomes this process's os.Environ(); the helper writes that verbatim to the
// dump path so the parent can assert on the marker / keys / inherited-env
// presence.
func TestSpawnEnvDumpHelper(t *testing.T) {
	if os.Getenv(overlayMarkerHelperSentinelEnv) != "1" {
		return
	}
	dumpPath := os.Getenv(overlayMarkerHelperDumpPathEnv)
	if dumpPath == "" {
		// No dump path means the closure dropped the inherited parent env
		// entirely (the exact regression the nil-seed guards). Exit non-zero
		// so the parent's spawn-side wait observes a failure rather than a
		// silently-empty dump.
		os.Exit(3)
	}
	_ = os.WriteFile(dumpPath, []byte(strings.Join(os.Environ(), "\n")), 0o600)
}

// TestProductionSpawnFn_NoOverlayRowStillEmitsAppliedMarker is the PR #403
// bot-edge regression guard. The supervisor used to append the
// MCPHUB_DAEMON_ENV_OVERLAY_APPLIED marker only INSIDE `if overlayApplied`,
// so a daemon with NO overlay row got NO marker. In the spawned child,
// daemonOverlayEnv then took the no-marker branch, which is FATAL on a
// malformed/unreadable overlay file — bricking a no-row daemon's launch when
// an UNRELATED overlay edit corrupts daemon-env-overrides.yaml. The fix
// appends the marker UNCONDITIONALLY for every supervised daemon (keeping the
// KEYS injection gated on overlayApplied).
//
// This drives the REAL production spawn closure (makeProductionSpawnFnWithStatePath)
// with a no-overlay-row global descriptor, launching an env-dump child that
// writes the closure's composed cmd.Env to a file. It asserts:
//   - the marker IS present (so the child takes the non-fatal degrade branch),
//   - the KEYS var is ABSENT (no overlay → no injected key set),
//   - the inherited parent env (PATH) is PRESERVED (guards the nil-seed: a
//     nil cmd.Env must be seeded from os.Environ() before the marker append,
//     else the child loses its whole environment).
//
// STATE-SAFE: HardenedTempDir + MCPHUB_STATE_DIR_OVERRIDE redirect the state
// dir; no overlay file is seeded, so daemonOverlayEnv's LookupOverlay returns
// no row and overlayApplied stays false — exactly the no-row case.
func TestProductionSpawnFn_NoOverlayRowStillEmitsAppliedMarker(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)
	// Clear any inherited marker/keys so the closure's append is the only
	// source of a non-empty value. (appendDaemonOverlayAppliedMarker strips
	// the empty marker entry before appending the trusted "supervisor" value;
	// the keys path leaves the empty entry untouched when overlayApplied is
	// false, which assertion (b) below accounts for.)
	t.Setenv(daemonOverlayAppliedEnvVar, "")
	t.Setenv(daemonOverlayKeysEnvVar, "")

	// The sentinel + dump-path live in the PARENT os.Environ() (via t.Setenv).
	// With a no-d.Env, no-overlay descriptor the closure's mergeDaemonEnv
	// returns nil; the nil-seed then copies os.Environ() (carrying the
	// sentinel + dump-path) before appending the marker. The child can only
	// FIND its dump path if that inheritance survived — so a passing dump is
	// itself the inherited-env-preserved assertion.
	dumpPath := filepath.Join(tmpHome, "child-env-dump.txt")
	t.Setenv(overlayMarkerHelperSentinelEnv, "1")
	t.Setenv(overlayMarkerHelperDumpPathEnv, dumpPath)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	crashCh := make(chan crashEvent, 8)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, NewDaemonRuntimeTracker(), "", nil, "", crashCh, shutdown, nil, false,
	)

	// Global-shape descriptor with NO d.Env (manifest env empty) and NO
	// overlay row → overlayApplied=false, mergeDaemonEnv returns nil. This is
	// the exact no-row case the fix protects.
	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  os.Args[0],
		Args:     []string{"-test.run=^TestSpawnEnvDumpHelper$"},
	}
	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on env-dump helper: %v", err)
	}

	// Wait for the helper child to write the dump (it exits right after).
	var body []byte
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, rerr := os.ReadFile(dumpPath); rerr == nil && len(b) > 0 {
			body = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(body) == 0 {
		t.Fatalf("env-dump child never wrote %s within 20s (closure may have dropped the inherited env, so the child could not find its dump path)", dumpPath)
	}
	childEnv := strings.Split(string(body), "\n")

	// (a) The APPLIED marker MUST be present even though there is no overlay
	// row — this is the fix. Without it the child's daemonOverlayEnv takes the
	// no-marker FATAL branch on a malformed overlay file.
	if got, ok := envValueFromDump(childEnv, daemonOverlayAppliedEnvVar); !ok || got != daemonOverlayAppliedEnvValue {
		t.Fatalf("child %s = %q (present=%v), want %q — marker must be appended for a NO-ROW supervised daemon",
			daemonOverlayAppliedEnvVar, got, ok, daemonOverlayAppliedEnvValue)
	}
	// (b) No NON-EMPTY KEYS value may be injected: no overlay row → empty key
	// set → appendDaemonOverlayKeys (which requires a non-empty set, and is
	// gated on overlayApplied) is NOT called. The closure only ever appends a
	// non-empty comma-joined value, so the gate holds iff the child sees no
	// non-empty keys value. (An empty value is the inherited parent entry the
	// test cleared below, never a closure injection.)
	if got, ok := envValueFromDump(childEnv, daemonOverlayKeysEnvVar); ok && got != "" {
		t.Fatalf("child %s = %q present, want no injected key set — keys injection must stay gated on overlayApplied", daemonOverlayKeysEnvVar, got)
	}
	// (c) The inherited parent env MUST be preserved (the nil-seed guard).
	// PATH is the canonical always-present inherited var; its presence proves
	// the nil cmd.Env was seeded from os.Environ() rather than replaced by a
	// 1-element marker-only env.
	if _, ok := envValueFromDump(childEnv, "PATH"); !ok {
		t.Fatalf("child env has no PATH — the nil cmd.Env was NOT seeded from os.Environ() before the marker append (inherited env dropped); child env = %v", childEnv)
	}
}

// TestProductionSpawnFn_NoOverlayRowNeutralizesInheritedKeys is the PR #403
// r2 security-edge regression guard. Now that the APPLIED marker is set for
// EVERY supervised daemon (the unconditional-marker fix above), a NO-ROW spawn
// whose supervisor os.Environ() carries a STALE / SPOOFED
// MCPHUB_DAEMON_ENV_OVERLAY_KEYS would let the spawned child's marker-present
// reload-failure branch reconstruct + apply UNRELATED env keys from
// os.Environ() instead of falling back to manifest-only env. The fix strips
// the reserved keys var in the no-row branch so the child sees no injected
// key set.
//
// This drives the REAL production spawn closure with a no-overlay-row global
// descriptor whose PARENT os.Environ() carries a stale keys value, then asserts
// the closure's composed cmd.Env CLEARED it (absent OR empty — either way the
// child's daemonOverlayKeysFromEnv returns nil → no reconstruction).
//
// STATE-SAFE: HardenedTempDir + MCPHUB_STATE_DIR_OVERRIDE redirect the state
// dir; no overlay file is seeded → overlayApplied stays false → the no-row
// branch runs.
func TestProductionSpawnFn_NoOverlayRowNeutralizesInheritedKeys(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)
	t.Setenv(daemonOverlayAppliedEnvVar, "")
	// A STALE / SPOOFED keys value in the supervisor's inherited environment.
	// Without the strip, the closure would carry this verbatim into the
	// no-row child's cmd.Env (the marker is set), and the child would
	// reconstruct STALE_SPOOFED_KEY as a trusted overlay key.
	t.Setenv(daemonOverlayKeysEnvVar, "STALE_SPOOFED_KEY")
	// Seed a value for that key too, so a leaked reconstruction would
	// actually surface a (wrong) overlay value rather than an empty one.
	t.Setenv("STALE_SPOOFED_KEY", "attacker-value")

	dumpPath := filepath.Join(tmpHome, "child-env-dump.txt")
	t.Setenv(overlayMarkerHelperSentinelEnv, "1")
	t.Setenv(overlayMarkerHelperDumpPathEnv, dumpPath)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	crashCh := make(chan crashEvent, 8)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, NewDaemonRuntimeTracker(), "", nil, "", crashCh, shutdown, nil, false,
	)

	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  os.Args[0],
		Args:     []string{"-test.run=^TestSpawnEnvDumpHelper$"},
	}
	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on env-dump helper: %v", err)
	}

	var body []byte
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, rerr := os.ReadFile(dumpPath); rerr == nil && len(b) > 0 {
			body = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(body) == 0 {
		t.Fatalf("env-dump child never wrote %s within 20s", dumpPath)
	}
	childEnv := strings.Split(string(body), "\n")

	// (a) The marker is present (unconditional-marker fix) — so the child WOULD
	// take the reconstruction branch on an unreadable overlay file.
	if got, ok := envValueFromDump(childEnv, daemonOverlayAppliedEnvVar); !ok || got != daemonOverlayAppliedEnvValue {
		t.Fatalf("child %s = %q (present=%v), want %q", daemonOverlayAppliedEnvVar, got, ok, daemonOverlayAppliedEnvValue)
	}
	// (b) THE FIX: the inherited keys var is CLEARED in the composed cmd.Env.
	// daemonOverlayKeysFromEnv treats an absent OR empty value as nil → no
	// reconstruction. Assert via the SAME reader the child uses so the test
	// proves the exact behavior the child depends on.
	if keys := daemonOverlayKeysFromEnv(childEnv); len(keys) != 0 {
		t.Fatalf("child %s reconstructs %v from a no-row spawn; want empty — the supervisor must strip the inherited/spoofed key set in the no-row branch", daemonOverlayKeysEnvVar, keys)
	}
	// (c) The inherited parent env (PATH) is still preserved.
	if _, ok := envValueFromDump(childEnv, "PATH"); !ok {
		t.Fatalf("child env has no PATH — the strip must not drop the rest of the inherited env")
	}
}

// TestProductionSpawnFn_NoOverlayRowImmuneToInheritedOverlayControlVars is the
// PR #403 r3 whole-class inheritance-immunity guard. The r3 fix added a THIRD
// reserved overlay control var (MCPHUB_DAEMON_ENV_OVERLAY_PATH) the spawned
// wrapper TRUSTS in its os.Environ() (resolveDaemonOverlayPath reads it FIRST to
// locate daemon-env-overrides.yaml). Stripping each reserved var individually
// (KEYS in r2, then PATH) is whack-a-mole; the general fix strips the WHOLE
// class (stripAllDaemonOverlayControlVars over the canonical
// reservedDaemonOverlayControlVars list) before re-appending only the trusted
// vars THIS spawn injects.
//
// This drives the REAL production spawn closure with a NON-serena, no-overlay-row
// global descriptor whose PARENT os.Environ() carries a STALE/SPOOFED
// MCPHUB_DAEMON_ENV_OVERLAY_PATH=/spoofed AND MCPHUB_DAEMON_ENV_OVERLAY_KEYS=SPOOF.
// It asserts the closure's composed cmd.Env carries NEITHER stale value — only
// the unconditional APPLIED marker. A non-serena daemon never gets a trusted
// PATH channel (that append is serena-scoped), so the only correct outcome is
// "_PATH absent/empty".
//
// FALSIFICATION: removing the stripAllDaemonOverlayControlVars call makes the
// inherited /spoofed _PATH (and SPOOF _KEYS) survive into cmd.Env → this test
// goes red. (A no-row spawn re-appends neither KEYS nor PATH, so without the
// strip the inherited values leak verbatim.)
//
// STATE-SAFE: HardenedTempDir + MCPHUB_STATE_DIR_OVERRIDE redirect the state
// dir; no overlay file is seeded → overlayApplied stays false → the no-row path
// runs.
func TestProductionSpawnFn_NoOverlayRowImmuneToInheritedOverlayControlVars(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)
	t.Setenv(daemonOverlayAppliedEnvVar, "")
	// STALE/SPOOFED control vars in the supervisor's inherited environment.
	// Without the whole-class strip, the no-row closure would carry these
	// verbatim into the child's cmd.Env (the marker is unconditionally set),
	// and the child would resolve its overlay file from /spoofed and reconstruct
	// SPOOF as a trusted overlay key.
	t.Setenv(daemonOverlayPathEnvVar, "/spoofed/daemon-env-overrides.yaml")
	t.Setenv(daemonOverlayKeysEnvVar, "SPOOF")
	t.Setenv("SPOOF", "attacker-value")

	dumpPath := filepath.Join(tmpHome, "child-env-dump.txt")
	t.Setenv(overlayMarkerHelperSentinelEnv, "1")
	t.Setenv(overlayMarkerHelperDumpPathEnv, dumpPath)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	crashCh := make(chan crashEvent, 8)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, NewDaemonRuntimeTracker(), "", nil, "", crashCh, shutdown, nil, false,
	)

	// Global-shape (NON-serena) descriptor, NO d.Env, NO overlay row.
	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  os.Args[0],
		Args:     []string{"-test.run=^TestSpawnEnvDumpHelper$"},
	}
	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on env-dump helper: %v", err)
	}

	childEnv := waitForEnvDump(t, dumpPath)

	// (a) The APPLIED marker IS present (unconditional — the only trusted
	// control var a no-row, non-serena daemon gets).
	if got, ok := envValueFromDump(childEnv, daemonOverlayAppliedEnvVar); !ok || got != daemonOverlayAppliedEnvValue {
		t.Fatalf("child %s = %q (present=%v), want %q", daemonOverlayAppliedEnvVar, got, ok, daemonOverlayAppliedEnvValue)
	}
	// (b) THE FIX: the inherited _PATH channel is NEUTRALIZED. A non-serena
	// daemon never gets a trusted PATH channel, so any value here is the leaked
	// inherited spoof. resolveDaemonOverlayPath treats absent OR empty as "fall
	// back to the state dir", so either is safe; a non-empty /spoofed value is
	// the bug.
	if got, ok := envValueFromDump(childEnv, daemonOverlayPathEnvVar); ok && got != "" {
		t.Fatalf("child %s = %q present, want neutralized — a NON-serena no-row spawn must strip the inherited/spoofed overlay-path channel", daemonOverlayPathEnvVar, got)
	}
	// (c) The inherited _KEYS set is ALSO neutralized (the r2 property, now
	// subsumed by the whole-class strip). Assert via the SAME reader the child
	// uses.
	if keys := daemonOverlayKeysFromEnv(childEnv); len(keys) != 0 {
		t.Fatalf("child %s reconstructs %v from a no-row spawn; want empty — the whole-class strip must neutralize the inherited/spoofed key set too", daemonOverlayKeysEnvVar, keys)
	}
	// (d) The inherited parent env (PATH) is still preserved (the strip must not
	// drop the rest of the env).
	if _, ok := envValueFromDump(childEnv, "PATH"); !ok {
		t.Fatalf("child env has no PATH — the whole-class strip must not drop the rest of the inherited env")
	}
}

// TestProductionSpawnFn_SerenaProxyTrustedOverlayPathBeatsInheritedSpoof is the
// PR #403 r3 trusted-injection guard. The whole-class strip
// (stripAllDaemonOverlayControlVars) removes the inherited _PATH; the
// serena-scoped channel append (resolveSpawnOverlayChannelPath →
// appendDaemonOverlayPathChannel) then re-injects the SUPERVISOR's resolved
// overlay path. So a serena-proxy descriptor whose parent env carries a STALE
// inherited _PATH must end up with the TRUSTED supervisor path in cmd.Env, not
// the inherited spoof.
//
// This drives the REAL production spawn closure with a serena-proxy-shaped
// descriptor (Args[0..1] = `daemon serena-proxy`, so isSerenaProxyDescriptor is
// true) launching the env-dump child via os.Args[0]. The child is gated by the
// sentinel env (TestMain fast-path), so its serena-shaped argv does not need a
// leading -test.run flag. With a non-empty statePath the trusted overlay path is
// the deterministic sibling of statePath.
//
// STATE-SAFE: HardenedTempDir for the state dir; statePath is a path INSIDE it.
func TestProductionSpawnFn_SerenaProxyTrustedOverlayPathBeatsInheritedSpoof(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)
	t.Setenv(daemonOverlayAppliedEnvVar, "")
	// A STALE inherited overlay-path channel in the supervisor's environment.
	// The trusted serena channel append must WIN over this.
	t.Setenv(daemonOverlayPathEnvVar, "/spoofed/daemon-env-overrides.yaml")

	dumpPath := filepath.Join(tmpHome, "child-env-dump.txt")
	t.Setenv(overlayMarkerHelperSentinelEnv, "1")
	t.Setenv(overlayMarkerHelperDumpPathEnv, dumpPath)
	t.Setenv(cliProductionArgvHelperEnv, "1")

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	// statePath is a real path inside tmpHome; the trusted overlay path is its
	// sibling daemon-env-overrides.yaml (resolveSpawnOverlayChannelPath with a
	// non-empty statePath). The file need not exist — the channel only NAMES it.
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	wantTrustedPath := filepath.Join(tmpHome, overlayBaseName)

	crashCh := make(chan crashEvent, 8)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, NewDaemonRuntimeTracker(), statePath, nil, "", crashCh, shutdown, nil, false,
	)

	// Serena-proxy-shaped descriptor: Args[0..1] = `daemon serena-proxy` so
	// isSerenaProxyDescriptor(d) is true and the closure injects the trusted
	// overlay-path channel. Command=os.Args[0] launches the env-dump child (the
	// TestMain sentinel fast-path dumps regardless of the serena-shaped argv).
	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-serena-deadbeef`,
		Server:   "serena",
		Daemon:   "deadbeef",
		Command:  os.Args[0],
		Args:     []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", "testdata/workspaces/alpha", "--port", "9121", "--task-name", `\mcp-local-hub-serena-deadbeef`},
	}
	if !isSerenaProxyDescriptor(descriptor) {
		t.Fatalf("test fixture is not recognized as a serena-proxy descriptor; the trusted-channel append would not fire")
	}
	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on serena env-dump helper: %v", err)
	}

	childEnv := waitForEnvDump(t, dumpPath)

	// THE FIX: the trusted supervisor overlay path WON over the inherited spoof.
	// resolveDaemonOverlayPath reads the LAST duplicate (Go exec semantics), and
	// the strip-then-trusted-append leaves exactly one entry — the trusted one.
	got, ok := envValueFromDump(childEnv, daemonOverlayPathEnvVar)
	if !ok {
		t.Fatalf("child env has no %s — the serena channel append must inject the trusted overlay path", daemonOverlayPathEnvVar)
	}
	if got != wantTrustedPath {
		t.Fatalf("child %s = %q, want trusted supervisor path %q (NOT the inherited /spoofed value)", daemonOverlayPathEnvVar, got, wantTrustedPath)
	}
	if strings.Contains(got, "spoofed") {
		t.Fatalf("child %s = %q still carries the inherited spoof; the strip-before-trusted-append failed", daemonOverlayPathEnvVar, got)
	}
}

// waitForEnvDump blocks until the env-dump child writes path (then exits) and
// returns its newline-split contents, or fails the test after 20s. Shared by
// the env-dump-driven spawn closure tests.
func waitForEnvDump(t *testing.T, path string) []string {
	t.Helper()
	var body []byte
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, rerr := os.ReadFile(path); rerr == nil && len(b) > 0 {
			body = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(body) == 0 {
		t.Fatalf("env-dump child never wrote %s within 20s", path)
	}
	return strings.Split(string(body), "\n")
}

// envValueFromDump returns the value of key from a newline-split child env
// dump. Matching is case-insensitive on Windows (PATH/Path) and exact on
// POSIX, mirroring mergeDaemonEnv's PATH-family normalizer. The last matching
// entry wins (Go exec duplicate-key semantics).
func envValueFromDump(env []string, key string) (string, bool) {
	return lookupEnvValueCaseFold(env, key)
}
