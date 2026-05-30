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

	"mcp-local-hub/internal/api"
)

// migrateSerenaUpgradeDeps builds the shared Windows v5UpgradeDeps used by both
// the reap and the start seams.
func migrateSerenaUpgradeDeps() (*v5UpgradeDeps, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("resolve executable: %w", err)
	}
	currentUser, err := currentWindowsUsername()
	if err != nil {
		return nil, "", fmt.Errorf("resolve current user: %w", err)
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, "", fmt.Errorf("resolve state-dir: %w", err)
	}
	deps := &v5UpgradeDeps{
		exePath:           exe,
		newBinaryPath:     exe, // current exe IS the new image (no rename in the reap path)
		supervisorLockDir: filepath.Join(stateDir, "supervisor.lock"),
		pipePath:          superviseIPCPipePath(currentUser),
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
			if d.Port > 0 {
				expectedPorts = append(expectedPorts, d.Port)
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

// defaultMigrateSerenaStart is the production §7.1 START driver on Windows. It
// starts a fresh supervisor (detached) that cold-reconciles whatever intent is
// on disk. The driver calls it AFTER its intent write commits (normal cutover)
// OR as the recovery step when an intent write fails after a reap (the
// still-on-disk OLD intent is restored).
func defaultMigrateSerenaStart(_ context.Context, w io.Writer) error {
	deps, _, err := migrateSerenaUpgradeDeps()
	if err != nil {
		return err
	}
	if err := deps.StartSupervisor(deps.exePath); err != nil {
		return err
	}
	fmt.Fprintln(w, "supervisor started; the current binary now reconciles the on-disk serena intent.")
	return nil
}
