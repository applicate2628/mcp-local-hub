// Package autostart manages the OS-level user-logon shim across
// Windows, Linux, and macOS (plan §2531-2541, spec §Q8).
//
// Per-OS subcommand divergence (PR #212):
//
//   - Windows  → `mcphub gui [--strict-mode]`. The GUI process owns
//     supervisor lifecycle ("tray icon = mcphub running" contract);
//     GUI adopts an existing supervisor via IPC probe or spawns one
//     as a detached child. Tray icon = mcphub running.
//
//   - Linux    → `mcphub supervise [--strict-mode]`. Linux is beta
//     tier with no functional tray surface, so the GUI ownership
//     pattern doesn't apply; the autostart entry launches the
//     supervisor directly. Revisit when a Linux tray ships.
//
//   - macOS    → `mcphub supervise [--strict-mode]`. macOS is preview
//     tier with build-only support (no kqueue child watcher); same
//     reasoning as Linux until a macOS tray ships.
//
// Per-OS shim locations:
//
//   - Windows  → Task Scheduler entry `\mcp-local-hub-supervisor` with
//     a LogonTrigger, owned by the current user.
//   - Linux    → systemd-user unit `mcphub-supervisor.service` under
//     `~/.config/systemd/user/`, enabled via `systemctl --user enable`.
//   - macOS    → LaunchAgent plist `com.applicate2628.mcphub-supervisor`
//     under `~/Library/LaunchAgents/`, bootstrapped via
//     `launchctl bootstrap gui/$(id -u)`.
//
// The Backend interface is the cross-platform contract; New() dispatches
// to a build-tag-selected platform constructor. State is a richer-than-
// boolean status enum so the CLI can distinguish "shim installed and
// running" from "shim installed but stopped" from "shim drifted from
// what mcphub would write now" — the last case matters because mcphub
// upgrades may relocate the binary or flip the strict-mode default.
//
// Imports allowed (per phase-11 task contract): standard library,
// `internal/api`, `internal/scheduler`, and (for the CLI wrapper)
// `github.com/spf13/cobra`. No `internal/gui` dependency.
package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

// WindowsTaskName is the Task Scheduler entry the Windows autostart
// shim installs (`\mcp-local-hub-supervisor`). Exported (capital) so
// tests can pin the contract without re-stringifying the literal across
// files. The leading backslash is intentional — Task Scheduler uses `\`
// as the root-folder prefix and schtasks accepts both forms, but storing
// it with the prefix keeps log output unambiguous when listing tasks via
// `schtasks /Query`.
//
// It lives in the NON-tagged autostart.go (not windows.go under
// `//go:build windows`) deliberately: it is a pure string literal with
// no Windows dependency, and cross-platform CLI code (the
// supervise --ensure-alive liveness relaunch in internal/cli) references
// it for log/error text on every OS. Keeping it here lets the whole
// module compile on Linux/macOS where the rest of windows.go does not —
// the relaunch fails loud at runtime on non-Windows (scheduler.New()
// returns "not implemented"), but the file must still COMPILE there.
const WindowsTaskName = `\mcp-local-hub-supervisor`

// State enumerates the lifecycle states the autostart backend can
// detect. The ordering of constants is load-bearing for any test or
// caller that converts State back into a numeric form; do NOT reorder
// without grepping for `iota` ordinals.
type State int

const (
	// StateAbsent — no shim installed at all. `Status()` returned
	// "task/unit/plist does not exist" from the OS scheduler.
	StateAbsent State = iota

	// StateEnabledRunning — shim installed AND the supervisor process
	// is currently alive. On Windows this means the Task Scheduler
	// state == "Running"; on Linux `systemctl --user is-active` ==
	// "active"; on macOS `launchctl print ... | grep state = running`.
	StateEnabledRunning

	// StateEnabledStopped — shim installed but the supervisor process
	// is NOT currently running. The shim will revive it on next logon
	// (or sooner, depending on the OS scheduler's restart policy).
	StateEnabledStopped

	// StateDrifted — shim installed but the recorded command-line
	// args (presence/absence of --strict-mode) or the recorded binary
	// path does not match what `mcphub autostart enable` would write
	// today. Operators should re-run `mcphub autostart enable` to
	// reconcile.
	StateDrifted

	// StateStaleResidue — remnants of a prior install (e.g. an old
	// plist with a different bundle id, a stale unit file from a
	// legacy install, or a Task Scheduler entry under an older name).
	// Reserved for future cleanup logic; current backends do not
	// proactively detect it.
	StateStaleResidue
)

// String returns the lower-case kebab-case label the CLI prints
// verbatim from `mcphub autostart status`. Tests pin every constant
// to its exact string so callers can grep against the output.
func (s State) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StateEnabledRunning:
		return "enabled-running"
	case StateEnabledStopped:
		return "enabled-stopped"
	case StateDrifted:
		return "drifted"
	case StateStaleResidue:
		return "stale-residue"
	default:
		// Non-empty sentinel so accidental new states show up as
		// something operators can grep for instead of a blank line.
		return "unknown"
	}
}

// Options carry per-call inputs the Backend honors during Enable/Status.
// Disable takes no options because the shim is identified solely by its
// canonical (per-OS) name.
type Options struct {
	// StrictMode controls whether the shim launches `mcphub supervise
	// --strict-mode` (true) or `mcphub supervise` (false). The flag
	// flows into the per-OS XML/unit/plist as an extra argv element.
	StrictMode bool

	// MCPHubPath is the absolute path to the mcphub binary the shim
	// should invoke. Empty means "resolve via os.Executable() at call
	// time" — used by the CLI default; tests pass an explicit value.
	MCPHubPath string
}

// Backend is the cross-platform contract implemented by the per-OS
// files (windows.go, linux.go, darwin.go). The same caller code in
// `internal/cli/autostart.go` drives all three.
type Backend interface {
	// Enable installs (or replaces) the autostart shim. Idempotent:
	// re-enabling with the same Options is a no-op apart from a
	// re-write of the on-disk artifact. Re-enabling with different
	// Options (strict-mode toggled, binary moved) MUST overwrite the
	// prior shim so a stale entry never lingers.
	Enable(opts Options) error

	// Disable removes the autostart shim. Idempotent: returns nil
	// when there is nothing installed. The shim's stop side-effects
	// (Task Scheduler /End, systemctl --user stop, launchctl bootout)
	// are best-effort — Disable does NOT fail when the supervisor
	// process is already dead.
	Disable() error

	// Status reports the current State. opts.StrictMode + opts.MCPHubPath
	// are inputs to drift detection: when the on-disk shim's recorded
	// command-line or binary path does not match what `Enable(opts)`
	// would write, Status returns StateDrifted. When opts.StrictMode
	// is false, drift detection ignores the strict-mode flag — i.e.
	// a shim that has --strict-mode but the caller didn't pass it
	// is still drift.
	Status(opts Options) (State, error)
}

// New returns the autostart Backend for the current OS. The dispatcher
// is split across build-tag-selected files (windows.go, linux.go,
// darwin.go) so each platform compiles only its own implementation.
func New() (Backend, error) {
	return newPlatformBackend()
}

// resolveMCPHubPath returns the absolute path the shim should invoke.
// Opts.MCPHubPath wins when non-empty; otherwise falls back to
// os.Executable() so a freshly-installed mcphub picks up its own
// install location without the operator hand-coding the path. Shared
// across all three per-OS backends so the resolution rule is
// identical regardless of platform.
func resolveMCPHubPath(opts Options) (string, error) {
	if opts.MCPHubPath != "" {
		return opts.MCPHubPath, nil
	}
	exe, err := osExecutableFn()
	if err != nil {
		return "", err
	}
	return exe, nil
}

// osExecutableFn wraps os.Executable so tests can stub it. Lives
// alongside the resolver so the seam is package-scoped and visible
// to every backend. Production points straight at os.Executable;
// tests assign a closure returning a deterministic path.
var osExecutableFn = os.Executable

// atomicWriteFile writes payload to path through a temp-file + rename
// pipeline. Mirrors the api.WriteStateFileAtomic pattern but for raw
// bytes (the JSON helper would base64-encode raw bytes otherwise).
// Kept local to the autostart package so the api surface stays narrow
// — Linux units + macOS plists need plain text, not JSON. Shared
// across the Linux and macOS backends; Windows uses the scheduler
// package's XML-handling path instead.
func atomicWriteFile(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
