// internal/gui/probe.go
package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// errMacOSProbeUnsupported is the cross-platform sentinel returned by
// processIDImpl on darwin (see probe_darwin.go). It lives here in
// probe.go (not probe_darwin.go) so single_instance.go can compare
// against it via errors.Is on every platform without breaking the
// linux/windows builds. The literal value is darwin-specific; on
// non-darwin platforms processIDImpl never returns it.
var errMacOSProbeUnsupported = errors.New("--force --kill identity probe not supported on macOS (reboot is the recovery path; tracked as backlog: macOS libproc/sysctl-based identity)")

// errWindowsArchUnsupported is the cross-platform sentinel for Windows
// builds whose architecture lacks PEB-offset support. Only the
// `windows && !amd64` build tag's processIDImpl returns it (see
// probe_windows_unsupported_arch.go); on all other platforms
// processIDImpl never returns this value. Defined here so probeOnce
// can errors.Is against it on every platform without splitting the
// classifier across build-tagged files. Codex bot review on PR #23 P2
// (probeOnce arch sentinel handling).
var errWindowsArchUnsupported = errors.New("--force --kill identity probe is amd64-only on Windows; this build (other arch) cannot enumerate cmdline/start-time")

// ProcessIdentity is the cross-platform result of inspecting an OS
// process. Used by single_instance.go's Probe + KillRecordedHolder
// to run the three-part identity gate before any destructive action.
//
// Codex r5 #1 + r6 #5 + r7 #6: image basename + (argv[1]=="gui" OR
// len(argv)==1) + start-time vs pidport mtime. All three checks live
// in single_instance.go; this struct just exposes the raw signals.
//
// Codex CLI xhigh review on PR #23 P2: Handle pins the kernel
// PID-table entry between probe and kill so a successful kill
// can't land on a recycled successor PID.
type ProcessIdentity struct {
	Alive     bool      // process is alive (cross-platform: Kill(0) on Unix; OpenProcess on Windows)
	Denied    bool      // privilege rejected the query — treat as alive (refuse take-over)
	ImagePath string    // canonical executable path, empty on Denied
	Cmdline   []string  // argv split honoring CommandLineToArgvW (Win) / NUL-delimited (Linux)
	StartTime time.Time // process creation time; opaque token for identity equality

	// Handle is the kernel reference taken at probe time. While held,
	// the OS pins the PID-table entry (Windows: PROCESS handle reserves
	// the PID; Linux: pidfd reserves analogous state), preventing
	// PID-reuse between gate-pass and kill. Zero when probe couldn't
	// open the process (Denied=true) or the platform has no
	// handle-reservation primitive (macOS today; Linux <5.3 fallback).
	// killProcess uses it via killProcessByIdentity when non-zero,
	// falls back to PID-based signal otherwise.
	Handle uintptr
}

// Close releases any kernel handle held by Handle. Safe to call
// multiple times; safe to call on a zero ProcessIdentity (no-op).
// Callers MUST invoke Close once the handle is no longer needed.
func (id *ProcessIdentity) Close() {
	if id == nil || id.Handle == 0 {
		return
	}
	closeProcessHandle(id.Handle)
	id.Handle = 0
}

// pingIncumbent issues GET http://127.0.0.1:<port>/api/ping and
// returns the PID the incumbent reports. Returns an error on
// connection failure, non-200 status, malformed body, or ok:false.
//
// Lifted from TryActivateIncumbent's inner block so Probe and the
// existing handshake share the same probe semantic. Callers should
// pass a 500ms-or-shorter timeout to keep the diagnostic path snappy.
func pingIncumbent(ctx context.Context, port int, timeout time.Duration) (int, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", port), nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err // connection refused / timeout / etc.
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("ping status %d", resp.StatusCode)
	}
	var body struct {
		OK  bool `json:"ok"`
		PID int  `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode ping body: %w", err)
	}
	if !body.OK {
		return 0, fmt.Errorf("ping returned ok=false")
	}
	return body.PID, nil
}

// processID is the platform-specific process inspector; defined in
// probe_windows.go, probe_linux.go, and probe_darwin.go. Called by
// single_instance.go only — keep callers narrow.
//
// processIDOverride (test seam in test_seams.go), when non-nil,
// replaces the platform implementation. Production code path is
// unchanged when the override is nil. Tests use it to inject
// specific ProcessIdentity payloads or to simulate the darwin
// errMacOSProbeUnsupported sentinel on linux/windows runners.
func processID(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{Alive: false}, nil
	}
	if processIDOverride != nil {
		return processIDOverride(pid)
	}
	return processIDImpl(pid)
}

// killProcess sends SIGKILL / TerminateProcess to the given PID.
// Defined in probe_windows.go and probe_unix.go. Returns nil on
// successful signal delivery; "process not found" errors map to
// non-nil so callers can distinguish "kill succeeded" from "PID
// already gone".
func killProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return killProcessImpl(pid)
}
