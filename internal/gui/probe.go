// internal/gui/probe.go
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ProcessIdentity is the cross-platform result of inspecting an OS
// process. Used by single_instance.go's Probe + KillRecordedHolder
// to run the three-part identity gate before any destructive action.
//
// Codex r5 #1 + r6 #5 + r7 #6: image basename + (argv[1]=="gui" OR
// len(argv)==1) + start-time vs pidport mtime. All three checks live
// in single_instance.go; this struct just exposes the raw signals.
type ProcessIdentity struct {
	Alive     bool      // process is alive (cross-platform: Kill(0) on Unix; OpenProcess on Windows)
	Denied    bool      // privilege rejected the query — treat as alive (refuse take-over)
	ImagePath string    // canonical executable path, empty on Denied
	Cmdline   []string  // argv split honoring CommandLineToArgvW (Win) / NUL-delimited (Linux)
	StartTime time.Time // process creation time; opaque token for identity equality
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
// probe_windows.go and probe_unix.go. Called by single_instance.go
// only — keep callers narrow.
func processID(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{Alive: false}, nil
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
