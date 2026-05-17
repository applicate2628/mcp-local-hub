//go:build darwin

// macOS autostart backend: writes a LaunchAgent plist under
// `~/Library/LaunchAgents/com.applicate2628.mcphub-supervisor.plist`,
// then bootstraps it via `launchctl bootstrap gui/$(id -u) <plist>`.
// Disable reverses the flow with `launchctl bootout`.
//
// Status uses `launchctl print gui/<uid>/<label>` and greps for
// `state = running` to detect the live process.
package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DarwinLabel is the LaunchAgent label / bundle identifier the
// autostart shim installs under. Exported (capital) so tests can
// reference the contract without re-stringifying the literal. The
// `applicate2628.` segment matches the install-time bundle ID
// convention the rest of the repo uses (see CLAUDE.md G5 mention
// of the marketplace registry under the same org name).
const DarwinLabel = "com.applicate2628.mcphub-supervisor"

// DarwinPlistFileName is the basename of the on-disk plist —
// `<Label>.plist`. Kept separate from `DarwinLabel` because the
// label appears in plist BODY content too, where the `.plist`
// suffix would be incorrect.
const DarwinPlistFileName = DarwinLabel + ".plist"

// launchctlFn is the test seam for the launchctl shell-out. Returns
// (stdout, stderr, err). t.Cleanup-restored by the per-test helper.
var launchctlFn = realLaunchctl

// plistPathFn is the test seam for the plist on-disk path. Tests
// redirect into t.TempDir().
var plistPathFn = realPlistPath

// currentUIDFn is the test seam for the per-user UID used in
// `gui/<uid>/<label>` launchctl targets. Production uses os.Getuid;
// tests stub to a deterministic UID like 501.
var currentUIDFn = realCurrentUID

// darwinBackend is the per-OS Backend implementation. Stateless.
type darwinBackend struct{}

func newPlatformBackend() (Backend, error) {
	return &darwinBackend{}, nil
}

// realLaunchctl runs `launchctl <args...>` and returns its output.
func realLaunchctl(args []string) (string, string, error) {
	bin, err := exec.LookPath("launchctl")
	if err != nil {
		return "", "", fmt.Errorf("launchctl not on PATH: %w", err)
	}
	cmd := exec.Command(bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	return stdout.String(), stderr.String(), runErr
}

// realPlistPath returns `$HOME/Library/LaunchAgents/<plist-name>`.
func realPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve HOME: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", DarwinPlistFileName), nil
}

// realCurrentUID returns the current process's UID as a string.
// launchctl uses `gui/<uid>/<label>` as its target syntax, so we
// pre-format it for the call sites.
func realCurrentUID() string {
	return strconv.Itoa(os.Getuid())
}

// renderDarwinPlist produces the LaunchAgent plist body. Pure
// function — tests use it to seed an on-disk shim that exactly
// matches what Enable would write.
//
// Key=>value semantics:
//   - Label: agent identity used in launchctl print/bootout targets.
//   - ProgramArguments: argv passed to the launched process; each
//     element is one <string> so launchctl avoids shell-splitting.
//   - RunAtLoad: start at login + after bootstrap.
//   - KeepAlive: restart on unexpected exit (the launchctl analog of
//     systemd's Restart=on-failure).
func renderDarwinPlist(mcphubPath string, strictMode bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	b.WriteString("    <key>Label</key>\n")
	b.WriteString("    <string>" + DarwinLabel + "</string>\n")
	b.WriteString("    <key>ProgramArguments</key>\n")
	b.WriteString("    <array>\n")
	b.WriteString("        <string>" + xmlEscape(mcphubPath) + "</string>\n")
	b.WriteString("        <string>supervise</string>\n")
	if strictMode {
		b.WriteString("        <string>--strict-mode</string>\n")
	}
	b.WriteString("    </array>\n")
	b.WriteString("    <key>RunAtLoad</key>\n")
	b.WriteString("    <true/>\n")
	b.WriteString("    <key>KeepAlive</key>\n")
	b.WriteString("    <true/>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String()
}

// xmlEscape replaces the five XML-special characters in a string
// so plist bodies survive paths with `&` or `<` (rare but valid on
// macOS).
func xmlEscape(s string) string {
	repl := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return repl.Replace(s)
}

// Enable writes the plist and runs `launchctl bootstrap gui/<uid> <plist>`.
//
// `bootstrap` is the modern (10.10+) loading idiom; `load -w` is the
// deprecated equivalent that still works but emits deprecation
// warnings. We pick the supported path.
func (d *darwinBackend) Enable(opts Options) error {
	cmd, err := resolveMCPHubPath(opts)
	if err != nil {
		return err
	}
	plistPath, err := plistPathFn()
	if err != nil {
		return fmt.Errorf("resolve plist path: %w", err)
	}
	body := renderDarwinPlist(cmd, opts.StrictMode)
	if err := atomicWriteFile(plistPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	uid := currentUIDFn()
	target := "gui/" + uid
	// Bootstrap may fail if the agent was previously bootstrapped under
	// a different plist; bootout-then-bootstrap is the safe replacement
	// pattern.
	_, _, _ = launchctlFn([]string{"bootout", target + "/" + DarwinLabel})
	if _, _, err := launchctlFn([]string{"bootstrap", target, plistPath}); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	return nil
}

// Disable runs `launchctl bootout gui/<uid>/<label>` then removes the
// plist file. Idempotent: bootout on an absent label and remove on an
// absent file both surface as nil.
func (d *darwinBackend) Disable() error {
	plistPath, err := plistPathFn()
	if err != nil {
		return fmt.Errorf("resolve plist path: %w", err)
	}
	uid := currentUIDFn()
	target := "gui/" + uid + "/" + DarwinLabel
	// bootout failures are non-fatal — agent may already be gone.
	_, _, _ = launchctlFn([]string{"bootout", target})
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	return nil
}

// Status reports the autostart shim's State by inspecting the on-disk
// plist + `launchctl print` for liveness.
//
//   - Plist absent → StateAbsent.
//   - Plist present + `launchctl print` stdout contains "state = running"
//     + body matches opts → StateEnabledRunning.
//   - Plist present + body matches + not running → StateEnabledStopped.
//   - Plist present + body mismatch → StateDrifted regardless of liveness.
func (d *darwinBackend) Status(opts Options) (State, error) {
	plistPath, err := plistPathFn()
	if err != nil {
		return StateAbsent, fmt.Errorf("resolve plist path: %w", err)
	}
	body, err := os.ReadFile(plistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return StateAbsent, nil
		}
		return StateAbsent, fmt.Errorf("read plist: %w", err)
	}
	want, err := resolveMCPHubPath(opts)
	if err != nil {
		return StateAbsent, err
	}
	expected := renderDarwinPlist(want, opts.StrictMode)
	if string(body) != expected {
		return StateDrifted, nil
	}
	uid := currentUIDFn()
	stdout, _, _ := launchctlFn([]string{"print", "gui/" + uid + "/" + DarwinLabel})
	// `launchctl print` returns a multi-line block; we look for the
	// `state = running` substring (other states are `not running`,
	// `waiting`, `stopping`, etc.). Substring match keeps us robust
	// against indentation/whitespace differences across macOS
	// releases.
	if strings.Contains(stdout, "state = running") {
		return StateEnabledRunning, nil
	}
	return StateEnabledStopped, nil
}
