//go:build windows

// Package cli — Windows production wiring for the serena migrate §7.1
// supervisor upgrade/restart gate.
//
// defaultMigrateSerenaRestart drives the existing cold-restart upgrade flow
// (RunInstallUpgrade: rename-aside → IPC quiesce-timers → IPC exit{graceful} →
// force-kill fallback → start-new-supervisor) after the migrate driver has
// written the spec-bearing supervisor-intent.json. This is the production
// binding of migrateSerenaRestartFn on Windows (the supervisor cold-restart IPC
// + spawn primitives are Windows-only in v0.5.0 — release scope is Windows GA /
// Linux beta / macOS preview); non-Windows builds use the stub in
// migrate_serena_restart_other.go.
//
// The wiring mirrors runV5UpgradeWindows (install_migration_wiring_windows.go)
// but omits its supervisor-intent-present routing discriminator: the migrate
// driver has just written the intent, so the file is known to exist. The
// rename-aside step is a no-op-equivalent in-place swap of the running binary
// onto itself (current exe IS the new image), matching the same-version upgrade
// path; its purpose here is solely to drive the supervisor reaping + restart so
// the binary that next reads the runtime_spec intent is the current one.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"
)

// defaultMigrateSerenaRestart is the production §7.1 restart driver on Windows.
func defaultMigrateSerenaRestart(ctx context.Context, w io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("resolve canonical target: %w", err)
	}
	currentUser, err := currentWindowsUsername()
	if err != nil {
		return fmt.Errorf("resolve current user: %w", err)
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("resolve state-dir: %w", err)
	}

	deps := &v5UpgradeDeps{
		exePath:           exe,
		newBinaryPath:     exe, // current exe IS the new image (in-place swap)
		supervisorLockDir: filepath.Join(stateDir, "supervisor.lock"),
		pipePath:          superviseIPCPipePath(currentUser),
	}

	// Resolve expected daemon ports from the just-written supervisor-intent.json
	// so the post-force-kill verification proves no zombie children survived.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		return fmt.Errorf("supervisor-intent.json unreadable at %s: %w (refusing to skip post-force-kill port verification)", intentPath, err)
	}
	if intent == nil {
		return fmt.Errorf("supervisor-intent.json at %s decoded to nil (corrupt envelope); refusing to skip post-force-kill port verification", intentPath)
	}
	var expectedPorts []int
	for _, d := range intent.Daemons {
		if d.Port > 0 {
			expectedPorts = append(expectedPorts, d.Port)
		}
	}

	if err := RunInstallUpgrade(ctx, UpgradeOpts{
		BinaryPath:         target,
		NewBinary:          exe,
		PipePath:           deps.pipePath,
		Deps:               deps,
		ExpectedPorts:      expectedPorts,
		VerifyPortsUnbound: verifyPortsUnboundForUpgrade,
	}); err != nil {
		return err
	}
	fmt.Fprintln(w, "supervisor restarted; the current binary now reconciles the new serena dynamic-pool intent.")
	return nil
}
