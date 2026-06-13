//go:build windows

// install_migration_serena_pipe_sec_f1_windows_test.go — SEC-F1 regression.
//
// SEC-F1 (P2): the `mcphub install --upgrade` and `mcphub migrate serena`
// flows dialed the USERNAME-based IPC pipe
// (`\\.\pipe\mcphub-supervisor-<USERNAME>`) while the supervisor LISTENS on the
// SID-based pipe (`api.SupervisorIPCAddress` → `\\.\pipe\mcphub-supervisor-<SID>`,
// PR #212 r3). USERNAME ≠ SID, so every quiesce-timers/exit{graceful} handshake
// dialed a pipe no supervisor listened on → the graceful drain always timed out
// → the flow fell through to the force-kill fallback, bypassing the graceful
// path and opening the orphan-daemon window the quiesce sequence exists to
// avoid. It surfaced in the field as "supervisor won't restart after install
// --upgrade; recovery needs manual schtasks /Run".
//
// These tests pin that BOTH the upgrade deps (buildV5UpgradeDeps) and the
// migrate deps (migrateSerenaUpgradeDeps) construct their IPC dial path with the
// SAME SID-based canonical resolver the listener + status/exit clients use, and
// that the dialed path is NOT the old USERNAME form. The third test is the
// strongest negative control: under api.EnableSupervisorIPCTestPipeIsolation()
// (installed package-wide in settings_registry_test.go TestMain) + a fake
// MCPHUB_STATE_DIR_OVERRIDE, a fake winio listener bound on
// api.SupervisorIPCAddress(stateDir) IS actually REACHED by the upgrade flow's
// dial — pre-fix the dial targeted the USERNAME pipe and the listener was never
// reached.

package cli

import (
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"mcp-local-hub/internal/api"
)

// legacyUsernamePipePath reconstructs the now-DELETED USERNAME-based pipe form
// (`\\.\pipe\mcphub-supervisor-<USERNAME>`) so the regressions can assert the
// fixed dial path differs from it. Mirrors the old superviseIPCPipePath +
// currentWindowsUsername helpers that SEC-F1 removed.
func legacyUsernamePipePath(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return `\\.\pipe\mcphub-supervisor-` + name
}

// TestSECF1_UpgradeDepsDialSIDPipeNotUsername pins that buildV5UpgradeDeps
// (the `mcphub install --upgrade` deps builder) dials the SID-based pipe the
// supervisor LISTENS on, not the USERNAME pipe.
//
// Falsification on the unfixed code: buildV5UpgradeDeps did not exist; the
// inline construction used superviseIPCPipePath(currentWindowsUsername()) →
// pipePath == legacyUsernamePipePath → the first assertion (equality with the
// SID resolver) FAILS because USERNAME ≠ SID.
func TestSECF1_UpgradeDepsDialSIDPipeNotUsername(t *testing.T) {
	withNoopSchedulerEnv(t)
	stateDir := withTempStateDir(t)

	deps := buildV5UpgradeDeps(`C:\fake\mcphub.exe`, stateDir)

	want := api.SupervisorIPCAddress(stateDir)
	if deps.pipePath != want {
		t.Fatalf("upgrade deps pipePath = %q; want the listener's SID-based path %q (api.SupervisorIPCAddress)", deps.pipePath, want)
	}
	if legacy := legacyUsernamePipePath(t); deps.pipePath == legacy {
		t.Fatalf("upgrade deps pipePath = %q == the legacy USERNAME pipe; the supervisor never listens there (SEC-F1 regression)", legacy)
	}
}

// TestSECF1_MigrateSerenaDepsDialSIDPipeNotUsername pins the same property for
// the migrate-serena deps builder (migrateSerenaUpgradeDeps), which the reap +
// start seams share.
//
// Falsification on the unfixed code: migrateSerenaUpgradeDeps set
// pipePath = superviseIPCPipePath(currentWindowsUsername()) → the equality with
// the SID resolver FAILS.
func TestSECF1_MigrateSerenaDepsDialSIDPipeNotUsername(t *testing.T) {
	withNoopSchedulerEnv(t)
	stateDir := withTempStateDir(t)

	deps, gotStateDir, err := migrateSerenaUpgradeDeps()
	if err != nil {
		t.Fatalf("migrateSerenaUpgradeDeps: %v", err)
	}
	if gotStateDir != stateDir {
		t.Fatalf("migrateSerenaUpgradeDeps stateDir = %q; want the test state dir %q", gotStateDir, stateDir)
	}

	want := api.SupervisorIPCAddress(stateDir)
	if deps.pipePath != want {
		t.Fatalf("migrate deps pipePath = %q; want the listener's SID-based path %q (api.SupervisorIPCAddress)", deps.pipePath, want)
	}
	if legacy := legacyUsernamePipePath(t); deps.pipePath == legacy {
		t.Fatalf("migrate deps pipePath = %q == the legacy USERNAME pipe; the supervisor never listens there (SEC-F1 regression)", legacy)
	}
}

// TestSECF1_UpgradeDialReachesListenerBoundOnSupervisorIPCAddress is the
// strongest negative control: a fake supervisor listener bound on
// api.SupervisorIPCAddress(stateDir) (the listener's canonical path) is actually
// REACHABLE by the address the upgrade deps dial. Under
// api.EnableSupervisorIPCTestPipeIsolation() (package-wide via TestMain) plus a
// fake MCPHUB_STATE_DIR_OVERRIDE, SupervisorIPCAddress returns a per-test
// `-test-` pipe so this binds a TEST pipe — never the real supervisor pipe.
//
// Falsification on the unfixed code: the deps dialed the USERNAME pipe, which is
// NOT the `-test-` pipe the fake listener binds → DialPipe to deps.pipePath
// would fail with "file not found" (no listener on the USERNAME pipe) → the
// reach assertion FAILS. Post-fix deps.pipePath == the listener's bound address
// → the dial connects.
func TestSECF1_UpgradeDialReachesListenerBoundOnSupervisorIPCAddress(t *testing.T) {
	withNoopSchedulerEnv(t)
	stateDir := withTempStateDir(t)

	// Drive the test-pipe discriminator (installed in TestMain) to a non-empty
	// value so SupervisorIPCAddress returns the isolated `-test-` pipe — NEVER
	// the real `\\.\pipe\mcphub-supervisor-<SID>` the live fleet uses.
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	listenAddr := api.SupervisorIPCAddress(stateDir)
	if !strings.Contains(listenAddr, "mcphub-supervisor-test-") {
		t.Fatalf("STATE SAFETY: under EnableSupervisorIPCTestPipeIsolation + MCPHUB_STATE_DIR_OVERRIDE the address must be a -test- pipe, got %q; refusing to bind a non-test pipe", listenAddr)
	}

	// Bind a fake supervisor listener on the listener's canonical address.
	ln, err := winio.ListenPipe(listenAddr, &winio.PipeConfig{
		MessageMode:      false,
		InputBufferSize:  4096,
		OutputBufferSize: 4096,
	})
	if err != nil {
		t.Fatalf("bind fake supervisor listener on %q: %v", listenAddr, err)
	}
	defer ln.Close()

	// The upgrade deps' dial address must equal the listener's bound address.
	deps := buildV5UpgradeDeps(`C:\fake\mcphub.exe`, stateDir)
	if deps.pipePath != listenAddr {
		t.Fatalf("upgrade deps would dial %q but the supervisor listens on %q — the dial reaches NO listener (SEC-F1: pre-fix it dialed the USERNAME pipe)", deps.pipePath, listenAddr)
	}

	// Prove the dial actually CONNECTS to the fake listener (graceful path
	// reachable). A successful Accept on the server side confirms the client's
	// dialed address resolved to this listener.
	accepted := make(chan error, 1)
	go func() {
		conn, aerr := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		accepted <- aerr
	}()

	timeout := 3 * time.Second
	conn, err := winio.DialPipe(deps.pipePath, &timeout)
	if err != nil {
		t.Fatalf("DialPipe(%q) failed — the upgrade dial did NOT reach the supervisor listener: %v", deps.pipePath, err)
	}
	_ = conn.Close()

	select {
	case aerr := <-accepted:
		if aerr != nil {
			t.Fatalf("fake supervisor Accept on %q errored: %v", listenAddr, aerr)
		}
	case <-time.After(timeout):
		t.Fatalf("fake supervisor listener on %q never accepted the upgrade dial — handshake unreachable", listenAddr)
	}

	// Sanity: with the override set, the legacy USERNAME pipe form is provably
	// different from the address actually dialed, so the unfixed code could not
	// have reached this listener.
	if legacy := legacyUsernamePipePath(t); deps.pipePath == legacy {
		t.Fatalf("dialed path %q == legacy USERNAME pipe %q under test isolation; SEC-F1 not applied", deps.pipePath, legacy)
	}
}
