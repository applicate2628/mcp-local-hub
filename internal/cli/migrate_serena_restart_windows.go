//go:build windows

// Package cli — Windows production wiring for the serena migrate §7.1
// supervisor REAP-FIRST restart gate (bot PR #250).
//
// The migrate is a SAME-binary cutover, so it does NOT use RunInstallUpgrade
// (whose rename-aside step would abort replacing a binary with itself) and it
// reaps BEFORE writing the spec-bearing intent (the InstallParsedManifest §7.1
// gate refuses a spec-bearing write while a supervisor is running). The flow is
// split into two seams the driver calls around its own intent write:
//
//   - defaultMigrateSerenaReap  → ReapSupervisorForRestart (IPC quiesce-timers →
//     exit{graceful} → force-kill fallback → verify ports unbound), NO binary
//     swap, NO successor start. Runs BEFORE the intent write; expected ports come
//     from the still-on-disk OLD supervisor-intent.json (the daemons the prior
//     supervisor is bound to).
//   - defaultMigrateSerenaStart → v5UpgradeDeps.StartSupervisor (detached per-OS
//     supervisor spawn). Runs AFTER the intent write commits so the fresh
//     supervisor cold-reconciles the new runtime_spec intent.
//
// Both are the production binding on Windows (the supervisor cold-restart IPC +
// spawn primitives are Windows-only in v0.5.0 — release scope Windows GA / Linux
// beta / macOS preview); non-Windows builds use the stubs in
// migrate_serena_restart_other.go.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"mcp-local-hub/internal/api"
)

// migrateSerenaReconcileReadyTimeout (the 60s reconcile-ready poll bound this
// START driver consumes via waitReconcileReadyViaIPC) is defined cross-platform
// in migrate_serena.go — NOT here under //go:build windows — because the driver's
// POST-COMMIT downgrade messaging (step 10) references it and must compile on
// every GOOS (Linux is shipping beta scope). See its doc comment there.

// migrateSerenaUpgradeDeps builds the shared Windows v5UpgradeDeps used by both
// the reap and the start seams.
func migrateSerenaUpgradeDeps() (*v5UpgradeDeps, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("resolve executable: %w", err)
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, "", fmt.Errorf("resolve state-dir: %w", err)
	}
	deps := &v5UpgradeDeps{
		exePath:           exe,
		supervisorLockDir: filepath.Join(stateDir, "supervisor.lock"),
		// SEC-F1: dial the SID-based pipe the supervisor LISTENS on, NOT the
		// USERNAME-based one. See install_migration_wiring_windows.go for the
		// full rationale (PR #212 r3 SID-consistency propagation gap).
		pipePath: api.SupervisorIPCAddress(stateDir),
	}
	return deps, stateDir, nil
}

// defaultMigrateSerenaReap is the production §7.1 REAP driver on Windows. It
// reaps the OLD supervisor WITHOUT replacing the binary and WITHOUT starting a
// successor (the driver writes the intent + starts the successor itself, after
// this returns).
func defaultMigrateSerenaReap(ctx context.Context, w io.Writer) error {
	deps, stateDir, err := migrateSerenaUpgradeDeps()
	if err != nil {
		return err
	}

	// Expected ports come from the still-on-disk OLD supervisor-intent.json —
	// the ports the prior supervisor's daemon children are bound to — so the
	// post-force-kill verification proves no zombie children survived BEFORE the
	// driver writes the new intent. A missing intent file (the prior supervisor
	// ran a never-persisted/transient set) is benign: no ports to verify.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	var expectedPorts []int
	if intent, rerr := api.ReadSupervisorIntent(intentPath); rerr != nil {
		if !os.IsNotExist(rerr) {
			return fmt.Errorf("read prior supervisor-intent.json at %s for reap port verification: %w", intentPath, rerr)
		}
	} else if intent != nil {
		for _, d := range intent.Daemons {
			// Resolve the EFFECTIVE port through the owner, not the raw field: a
			// legacy Port=0 row still declares a manifest port its daemon binds, and
			// dropping it here would skip the post-reap unbound verification, letting a
			// surviving transient hold the port and the successor spawn a duplicate
			// (commission fable-F1; same class as the upgrade path).
			if port, ok := api.EffectiveDaemonPort(d); ok && port > 0 {
				expectedPorts = append(expectedPorts, port)
			}
		}
	}

	if err := ReapSupervisorForRestart(ctx, ReapOpts{
		PipePath:           deps.pipePath,
		Deps:               deps,
		ExpectedPorts:      expectedPorts,
		VerifyPortsUnbound: verifyPortsUnboundForUpgrade,
	}); err != nil {
		return err
	}
	fmt.Fprintln(w, "prior supervisor reaped; ready to write the new serena dynamic-pool intent.")
	return nil
}

// defaultMigrateSerenaStartSupported is the Windows production binding for
// migrateSerenaStartSupportedFn: the detached supervisor spawn primitive
// (defaultMigrateSerenaStart) IS wired on Windows, so a cutover that requires a
// start can proceed to the intent write. Returns true unconditionally.
func defaultMigrateSerenaStartSupported() bool {
	return true
}

// defaultAcquireSupervisorInterlock is the Windows production binding for
// acquireSupervisorInterlockFn. It acquires supervisor.lock on the EXACT leaf the
// §7.1 install gate probes — filepath.Join(api.DaemonStateDir(), "supervisor.lock")
// (install_parsed_manifest.go:342's gateLockPath resolver) — so the held-lock path
// equals the gate-probed path and the typed bypass token (AllowSpecBearingWriteBypass)
// passes the gate's IDENTITY check.
//
// It uses the QUIET acquire (api.AcquireSupervisorLockQuiet — flock only, NO
// owner-sidecar write), NOT api.AcquireSupervisorLock (bot PR #276 finding 1). The
// full acquire overwrites supervisor.lock.owner.json with THIS CLI process's PID;
// the reap primitive (ForceKillSupervisor / QuiesceTimers / ExitGraceful) reads
// that sidecar to choose the PID it taskkills / IPC-handshakes against, so a
// sidecar-overwriting acquire makes a concurrent serena auto-register reap target
// the migrate process instead of the old supervisor. The quiet acquire leaves the
// sidecar pointing at the OLD supervisor (or absent), so every reap targets the
// correct PID, while the flock still provides the reap→write→start mutual
// exclusion. The quiet handle still has .fl + .path set, so it mints a valid §7.1
// bypass token (the gate identity check needs only those, not the sidecar).
//
// HIGHEST-PRIORITY invariant: the path MUST be api.DaemonStateDir(), NOT
// stateDirFunc(). stateDirFunc honors MCPHUB_STATE_DIR_OVERRIDE for the cli's own
// audit-log path, but the §7.1 gate inside InstallParsedManifest resolves its
// stateDir via api.DaemonStateDir(); a mismatch would acquire a DIFFERENT lock
// leaf than the gate probes, so the gate's identity check would REJECT the bypass
// (probe runs → fail-closed) and the migrate would re-open the very split-brain
// this interlock prevents. (The Phase-1 in-gate path-mismatch guard is the
// backstop, but the call site must still hard-code the right resolver.)
//
// It returns the live lock HANDLE (so the driver mints the bypass token) and an
// IDEMPOTENT release closure (the driver calls release at multiple start sites; a
// second call is a harmless no-op because (*SupervisorLock).Release() nils its
// flock). A non-nil error means a foreign supervisor — or a concurrent serena
// cutover — already holds the lock.
func defaultAcquireSupervisorInterlock() (*api.SupervisorLock, func(), error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, func() {}, fmt.Errorf("resolve state-dir for the supervisor.lock interlock: %w", err)
	}
	lock, err := api.AcquireSupervisorLockQuiet(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		return nil, func() {}, err
	}
	var once sync.Once
	release := func() { once.Do(lock.Release) }
	return lock, release, nil
}

// defaultMigrateSerenaSupervisorHealthy is the Windows production health probe
// for Fix 5's idempotency-recovery branch. It reports (true, nil) ONLY when a
// supervisor is both running (holds supervisor.lock) AND reconcile-ready (IPC
// `status` reports reconcile_ready=true). A not-running supervisor returns
// (false, nil) → recovery. A running-but-not-ready or IPC-probe-failure returns
// (false, err) → the caller treats it as a recovery situation (the redundant
// start is benign: the supervisor singleton lock makes a duplicate `mcphub
// supervise` exit, and the start's own readiness poll then confirms the live
// supervisor).
func defaultMigrateSerenaSupervisorHealthy() (bool, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return false, fmt.Errorf("resolve state-dir for supervisor health probe: %w", err)
	}
	running, _, err := api.SupervisorRunningUnderStateDir(stateDir)
	if err != nil {
		// Liveness undeterminable (e.g. lock-probe error on a hardened host) →
		// not confirmed healthy. Surface so the caller recovers.
		return false, fmt.Errorf("probe supervisor liveness: %w", err)
	}
	if !running {
		return false, nil
	}
	// Running — now require reconcile-ready via a single IPC `status` probe.
	// SEC-F1: probe the SID-based pipe the supervisor LISTENS on (the same
	// path api.SupervisorIPCAddress resolves for the listener + status/exit
	// clients), NOT the USERNAME-based superviseIPCPipePath. Dialing the
	// USERNAME pipe here reached no listener, so a HEALTHY running supervisor
	// was misreported as unhealthy → an unnecessary recovery restart.
	ready, err := probeReconcileReadyOnce(api.SupervisorIPCAddress(stateDir))
	if err != nil {
		return false, fmt.Errorf("probe supervisor reconcile-ready: %w", err)
	}
	return ready, nil
}

// defaultMigrateSerenaStart is the production §7.1 START driver on Windows. It
// starts a fresh supervisor (detached) that cold-reconciles whatever intent is
// on disk, then SELF-VERIFIES the cutover by polling supervisor IPC `status`
// until reconcile_ready=true (Fix 4, PR #250 deeper review — consultant Q4/Q5).
// The driver calls it AFTER its intent write commits (normal cutover) OR as the
// recovery step when an intent write fails after a reap (the still-on-disk OLD
// intent is restored).
//
// Without the readiness poll the start was fire-and-forget: a detached spawn
// that returned success the instant CreateProcess succeeded, declaring the
// cutover done before the new supervisor had read the intent or bound a single
// daemon port. The poll makes the migrate fail loud (with operator guidance) if
// the supervisor does not reconcile within the bounded window, so a hung or
// crash-looping start surfaces as a migrate error rather than a silent
// "complete" with clients pointed at a router that resolves to nothing.
func defaultMigrateSerenaStart(_ context.Context, w io.Writer) error {
	deps, _, err := migrateSerenaUpgradeDeps()
	if err != nil {
		return err
	}
	if err := deps.StartSupervisor(deps.exePath); err != nil {
		// HARD start failure — the detached spawn itself failed. This stays
		// fail-loud at EVERY call site (pre-commit, recovery, AND post-commit
		// step 10): it is NOT wrapped with ErrMigrateSerenaReconcileReadyTimeout.
		return err
	}
	fmt.Fprintln(w, "supervisor started; waiting for it to reconcile the on-disk serena intent…")

	// Revision 4 hand-off-window observation: the migrate released supervisor.lock
	// immediately before this start, so the fresh supervisor must acquire the lock
	// and bind its IPC pipe before `status` answers. If the FIRST reconcile probe
	// is not ready-or-errors, the benign release→child-acquire pre-bind pipe race
	// materialized (it then resolves under the bounded poll below) — signal the
	// observer so the driver emits the named event, letting an operator distinguish
	// this known-benign window from a recurrence of the original bare-IPC-timeout
	// bug. Best-effort and non-blocking; a probe error here is NOT fatal (the
	// bounded poll is the authoritative readiness gate).
	if ready, perr := probeReconcileReadyOnce(deps.pipePath); perr != nil || !ready {
		migrateSerenaHandoffWindowFn("reconcile-ready-retry")
	}

	// Self-verify: poll IPC `status` until reconcile_ready=true (reuse the
	// existing forward-migration readiness primitive). Bounded so a hung start
	// cannot block forever.
	if err := waitReconcileReadyViaIPC(deps.pipePath)(migrateSerenaReconcileReadyTimeout); err != nil {
		// The spawn SUCCEEDED but the supervisor did not report reconcile-ready
		// in the window. Wrap the TYPED sentinel so the POST-COMMIT start (step
		// 10) can recognize this as the benign release→child-acquire hand-off
		// timeout and downgrade it to a warning (the intent is committed; the
		// supervisor reconciles eventually). EVERY OTHER call site still fails
		// loud on this — they return on any non-nil start error. The detailed
		// operator guidance is preserved in the wrapped message; errors.Is finds
		// the sentinel through the %w chain.
		return fmt.Errorf(
			"supervisor started but did not reach reconcile-ready within %s: %w: %w; "+
				"the binary spawned a detached supervisor but it never reported reconcile_ready=true over IPC — "+
				"check `mcphub status` and the supervisor-events.log, then run `mcphub supervise` from a shell to see startup diagnostics",
			migrateSerenaReconcileReadyTimeout, ErrMigrateSerenaReconcileReadyTimeout, err)
	}
	fmt.Fprintln(w, "supervisor reconcile-ready; the serena dynamic-pool daemons are reconciling on their allocated ports.")
	return nil
}
