//go:build linux

// Linux autostart backend: writes a per-user systemd unit under
// `~/.config/systemd/user/mcphub-supervisor.service`, then enables and
// starts it via `systemctl --user enable --now`. Disable reverses the
// flow.
//
// Status uses `systemctl --user is-active` + on-disk unit comparison
// for drift detection.
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemctlFn is the test seam — production paths leave it pointing at
// realSystemctl, but autostart tests inject a recording fake. Returns
// (stdout, stderr, err); err is nil for exit 0, non-nil otherwise.
var systemctlFn = realSystemctl

// unitPathFn is the test seam for the unit-file path. Production
// resolves to `~/.config/systemd/user/mcphub-supervisor.service`.
// Tests redirect into t.TempDir().
var unitPathFn = realUnitPath

// linuxBackend is the per-OS Backend implementation. Stateless; every
// call resolves the seam state at call time so tests can swap fakes.
type linuxBackend struct{}

func newPlatformBackend() (Backend, error) {
	return &linuxBackend{}, nil
}

// realSystemctl runs `systemctl <args...>` and returns its
// (stdout, stderr, err). Non-zero exit codes surface as a non-nil
// err. `systemctl` is canonical Linux distro tooling; absence is
// rare but possible (Alpine container without systemd, WSL1 without
// the host's PID-1 service manager) — Enable downgrades to a
// best-effort unit-file write and emits a warning to stderr.
func realSystemctl(args []string) (string, string, error) {
	bin, err := exec.LookPath("systemctl")
	if err != nil {
		return "", "", fmt.Errorf("systemctl not on PATH: %w", err)
	}
	cmd := exec.Command(bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	return stdout.String(), stderr.String(), runErr
}

// realUnitPath returns the canonical per-user systemd unit path:
// `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/mcphub-supervisor.service`.
// XDG_CONFIG_HOME is honored if set (per the XDG basedir spec); otherwise
// `$HOME/.config` is the documented default.
func realUnitPath() (string, error) {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve HOME: %w", err)
		}
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "systemd", "user", "mcphub-supervisor.service"), nil
}

// linuxUnitName is the bare systemd unit name (without path) used in
// every systemctl invocation. Exported (capital) for cross-file
// reference in tests.
const linuxUnitName = "mcphub-supervisor.service"

// renderLinuxUnit produces the systemd unit body. Pure function so
// tests can call it directly to seed an on-disk shim that exactly
// matches what Enable would write.
//
// `Restart=on-failure` + `RestartSec=5` mirror the watchdog's
// 5-second retry cadence (plan §2531) so a supervisor crash is
// revived without spamming systemd's restart backoff.
func renderLinuxUnit(mcphubPath string, strictMode bool) string {
	flag := ""
	if strictMode {
		flag = " --strict-mode"
	}
	return strings.Join([]string{
		"[Unit]",
		"Description=MCP Local Hub supervisor (autostart shim, plan §2531-2541)",
		"After=default.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + mcphubPath + " supervise" + flag,
		"Restart=on-failure",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

// Enable writes the unit file and runs `systemctl --user daemon-reload`
// followed by `systemctl --user enable --now mcphub-supervisor.service`.
//
// When systemctl is not on PATH (rare embedded distros, WSL1 without a
// systemd userland), the unit is still written but the systemctl
// shell-out is skipped with a stderr warning — the operator can
// hand-enable via a third-party init system or schedule a manual
// re-run after migrating to a systemd-bearing distro.
func (l *linuxBackend) Enable(opts Options) error {
	cmd, err := resolveMCPHubPath(opts)
	if err != nil {
		return err
	}
	unitPath, err := unitPathFn()
	if err != nil {
		return fmt.Errorf("resolve unit path: %w", err)
	}
	body := renderLinuxUnit(cmd, opts.StrictMode)
	if err := atomicWriteFile(unitPath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	// daemon-reload first so the kernel of systemd picks up the new
	// unit body before enable --now references it.
	if _, _, err := systemctlFn([]string{"--user", "daemon-reload"}); err != nil {
		if isSystemctlMissing(err) {
			fmt.Fprintln(os.Stderr, "warning: systemctl not available; unit written but not enabled. Re-run `systemctl --user enable --now `"+linuxUnitName+"` once systemd is available.")
			return nil
		}
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, _, err := systemctlFn([]string{"--user", "enable", "--now", linuxUnitName}); err != nil {
		if isSystemctlMissing(err) {
			fmt.Fprintln(os.Stderr, "warning: systemctl missing mid-call; unit written but not enabled.")
			return nil
		}
		return fmt.Errorf("systemctl enable --now: %w", err)
	}
	return nil
}

// Disable removes the unit file and runs `systemctl --user disable --now`.
// Idempotent: missing unit + non-existent service both surface as nil.
func (l *linuxBackend) Disable() error {
	unitPath, err := unitPathFn()
	if err != nil {
		return fmt.Errorf("resolve unit path: %w", err)
	}
	// Stop + disable first so the running process is killed before we
	// delete the unit definition (otherwise systemd loses the link).
	if _, _, err := systemctlFn([]string{"--user", "disable", "--now", linuxUnitName}); err != nil {
		if !isSystemctlMissing(err) && !isSystemctlAbsent(err) {
			// Non-fatal: report through stderr and continue with
			// unit-file removal so Disable remains best-effort.
			fmt.Fprintf(os.Stderr, "warning: systemctl disable --now %s: %v\n", linuxUnitName, err)
		}
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	// daemon-reload after removal so systemd drops the stale link.
	if _, _, err := systemctlFn([]string{"--user", "daemon-reload"}); err != nil {
		if !isSystemctlMissing(err) {
			// Non-fatal: removing the unit file is the load-bearing
			// part; daemon-reload is housekeeping.
			fmt.Fprintf(os.Stderr, "warning: systemctl daemon-reload: %v\n", err)
		}
	}
	return nil
}

// Status reports the autostart shim's State by inspecting the on-disk
// unit + querying `systemctl --user is-active` for liveness.
//
//   - Unit file absent → StateAbsent.
//   - Unit present + is-active=="active" + body matches opts → StateEnabledRunning.
//   - Unit present + is-active!="active" + body matches → StateEnabledStopped.
//   - Unit present + body mismatch → StateDrifted regardless of liveness.
func (l *linuxBackend) Status(opts Options) (State, error) {
	unitPath, err := unitPathFn()
	if err != nil {
		return StateAbsent, fmt.Errorf("resolve unit path: %w", err)
	}
	body, err := os.ReadFile(unitPath)
	if err != nil {
		if os.IsNotExist(err) {
			return StateAbsent, nil
		}
		return StateAbsent, fmt.Errorf("read unit: %w", err)
	}
	want, err := resolveMCPHubPath(opts)
	if err != nil {
		return StateAbsent, err
	}
	expected := renderLinuxUnit(want, opts.StrictMode)
	if string(body) != expected {
		return StateDrifted, nil
	}
	// is-active returns exit 0 + "active" when running, exit 3 +
	// "inactive"/"failed"/"activating" otherwise. We only need the
	// stdout token to decide.
	stdout, _, _ := systemctlFn([]string{"--user", "is-active", linuxUnitName})
	if strings.TrimSpace(stdout) == "active" {
		return StateEnabledRunning, nil
	}
	return StateEnabledStopped, nil
}

// isSystemctlMissing returns true when realSystemctl's exec.LookPath
// reported the binary as absent from PATH. Lets Enable downgrade to
// the "unit written, not enabled" warning path.
func isSystemctlMissing(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "systemctl not on PATH") || errors.Is(err, exec.ErrNotFound)
}

// isSystemctlAbsent returns true when `systemctl --user disable` is
// reporting "Unit ... does not exist" — the idempotent-disable
// signal. We swallow that case so re-running Disable is safe.
func isSystemctlAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "not loaded") || strings.Contains(msg, "no such")
}
