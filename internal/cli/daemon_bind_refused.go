package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"
)

// exitBindRefused is the reserved process exit code a supervised daemon proxy
// (`mcphub daemon serena-proxy` / `mcphub daemon workspace-proxy`) returns when
// it cannot bind its 127.0.0.1 port because another process already holds it
// (api.IsPortBindRefusedErr — Winsock WSAEADDRINUSE/WSAEACCES). The supervisor's
// crash handler keys the ephemeral-collision self-heal (dynamic-pool port
// reallocation) on this EXACT code, so it must be distinct from every other exit
// path a daemon proxy can take:
//
//	0  success / clean shutdown
//	1  cobra's default "RunE returned a non-nil error" (every OTHER failure:
//	   manifest load, runtime-spec mismatch, secrets, spawn, unexpected serve
//	   error) — the code the supervisor treats as a genuine crash today
//	2  Go runtime (panic, flag-parse) — reserved by the runtime
//	3  exitBindRefused (THIS) — loopback bind refused; a fresh pool port frees it
//
// Exit codes are command-scoped (CLAUDE.md "Exit codes"): the gui / setup /
// strict-mode commands reuse small integers like 3 for their OWN meanings, but a
// daemon-proxy child process never runs those code paths, so there is no
// collision on the daemon-proxy process the supervisor spawns and waits on.
//
// KNOWN COLLISION (documented, bounded): on Windows the C runtime's abort()
// terminates a process with exit code 3, so a daemon-proxy child that aborts via
// a CRT path (a cgo/native dependency, not the Go proxy code itself, which never
// calls abort) would exit 3 and be MIS-classified as a bind refusal. The blast
// radius is bounded, not a brick: the self-heal only fires from StRunning on a
// DYNAMIC-pool proxy, and AllocatePort OS-probes real bindability before moving —
// so a spurious exit-3 costs at most reallocationCap moves to actually-bindable
// ports, then falls through to the normal crash → quarantine path. Go daemon
// proxies do not use the CRT abort path, so this is a theoretical edge, noted here
// so a future reader does not treat exit 3 as unambiguously "bind refused".
const exitBindRefused = 3

// daemonBindRefusedExitError wraps a loopback bind refusal so
// cmd/mcphub/main.go routes it to os.Exit(exitBindRefused). It implements the
// SAME combined marker interface (`ExitCode() int` + `IsMcphubForceExit() bool`)
// main.go matches for the gui/force sentinels, so the distinct code survives
// instead of collapsing to cobra's exit 1. It Unwraps the original bind error so
// the deferred writeLaunchFailure in each daemon RunE still records the concrete
// Winsock failure to the per-workspace log — the supervisor sees the code, the
// operator's log keeps the cause.
type daemonBindRefusedExitError struct{ err error }

func (e *daemonBindRefusedExitError) Error() string {
	return fmt.Sprintf("daemon bind refused (exit %d): %v", exitBindRefused, e.err)
}
func (e *daemonBindRefusedExitError) Unwrap() error           { return e.err }
func (e *daemonBindRefusedExitError) ExitCode() int           { return exitBindRefused }
func (e *daemonBindRefusedExitError) IsMcphubForceExit() bool { return true }

// bindRefusedExit returns a daemonBindRefusedExitError (→ exit 3) iff err is a
// loopback bind refusal (api.IsPortBindRefusedErr). Otherwise it returns err
// unchanged so every non-bind failure keeps its existing cobra exit-1 behavior.
// The daemon bind sites call this at their single serve/bind error return so the
// classification lives in ONE helper, never duplicated per proxy. A nil err is
// passed through as nil (the caller's clean-shutdown path is unaffected).
func bindRefusedExit(err error) error {
	if err == nil {
		return nil
	}
	if api.IsPortBindRefusedErr(err) {
		return &daemonBindRefusedExitError{err: err}
	}
	return err
}
