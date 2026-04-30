// internal/gui/headless.go
package gui

import (
	"errors"
	"os"
	"runtime"
)

// ErrActivationNoTarget is returned by an OnActivateWindow callback when
// the request to bring the dashboard to front is impossible in this
// session — currently only the headless-Linux case (no $DISPLAY /
// $WAYLAND_DISPLAY, no browser to launch). The /api/activate-window
// handler maps this to 503 Service Unavailable so the client of
// TryActivateIncumbent can distinguish "incumbent reached and
// activated" from "incumbent reached but cannot activate". Codex bot
// review on PR #26 P2.
var ErrActivationNoTarget = errors.New("no activation target (headless session — no display server, no browser to launch)")

// HeadlessSession reports whether the current process is running in a
// session without an attached display server. Used by the GUI command
// to decide whether auto-launching a browser is meaningful (Phase 3B-II
// §F4: Linux-server readiness).
//
// Detection rules per OS:
//   - linux: $DISPLAY (X11) AND $WAYLAND_DISPLAY (Wayland) both empty
//     means no graphical session. SSH-without-X11-forwarding, systemd
//     services, and bare server installs all fall in this bucket.
//   - darwin: macOS always has Aqua when a real user session is logged
//     in. Headless mac-minis exist but they're rare — default to false
//     and let the LaunchBrowser fallback path log the inevitable error.
//     We can revisit if F4 use cases on macOS materialize.
//   - windows: detection is non-trivial (Session 0 isolation, RDP
//     without redirected display, scheduled tasks running as SYSTEM).
//     Default to false until a concrete headless-Windows use case
//     drives a more nuanced check.
//   - other: default false; unknown platforms get the same behavior
//     they already have today (a failed browser launch is logged but
//     does not crash mcphub gui).
//
// Test seam: tests can override this via the headlessSessionOverride
// hook below to assert callers' headless code paths without mutating
// process env.
func HeadlessSession() bool {
	if headlessSessionOverride != nil {
		return *headlessSessionOverride
	}
	switch runtime.GOOS {
	case "linux":
		return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
	default:
		return false
	}
}

// headlessSessionOverride is the test seam used by gui_test / cli_test
// helpers. Production callers leave it nil. Stored as a *bool so a test
// can explicitly assert "this run is headless" or "is NOT headless"
// without depending on the actual host environment.
var headlessSessionOverride *bool

// SetHeadlessSessionForTest overrides HeadlessSession's return value for
// the duration of a test. Callers must defer the restore func.
func SetHeadlessSessionForTest(v bool) (restore func()) {
	orig := headlessSessionOverride
	headlessSessionOverride = &v
	return func() {
		headlessSessionOverride = orig
	}
}
