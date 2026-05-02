// Package gui — POST /api/force-kill/probe + /api/force-kill handlers.
//
// Memo D12 + D13. Wraps C1's Probe + KillRecordedHolder semantics in
// HTTP. Probe is read-only and returns the C1 Verdict struct as JSON.
// Kill returns the post-kill Verdict on success and maps Verdict.Class
// onto HTTP status on failure.
//
// Status mapping (memo D12):
//   - VerdictKilledRecovered / VerdictHealthy  -> 200 + Verdict JSON
//   - VerdictKillRefused (identity gate fail)  -> 403 + verdict body
//   - VerdictRaceLost   (lock changed mid)     -> 412 + verdict body
//   - VerdictKillFailed / VerdictMalformed     -> 500 + verdict body
//   - any other err != nil                     -> 500 + verdict body
//
// macOS short-circuit (memo D13): both endpoints return 501 with
// product-neutral copy. The message MUST NOT reference CLAUDE.md per
// the design memo's D13 resolution.
//
// Test seams Server.probeForRoute / Server.killForRoute on Server let
// tests drive deterministic outcomes without touching real file locks
// or processes. Production callers go through the package-level Probe
// + KillRecordedHolder against the canonical PidportPath().
package gui

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
)

func registerForceKillRoutes(s *Server) {
	s.mux.HandleFunc("/api/force-kill/probe",
		s.requireSameOrigin(s.forceKillProbeHandler))
	s.mux.HandleFunc("/api/force-kill",
		s.requireSameOrigin(s.forceKillHandler))
}

// macosNotSupportedMsg is the product-neutral copy returned on macOS
// for both force-kill endpoints. Memo D13 explicitly forbids any
// CLAUDE.md reference here — the message must read identically to the
// frontend's macOS-fallback string.
const macosNotSupportedMsg = "Lock recovery is not yet supported on macOS. As a workaround, run `mcphub gui --force` from Terminal for a diagnostic, or restart the system to clear stuck file handles."

// forceKillProbeHandler implements POST /api/force-kill/probe. It runs
// C1's Probe (read-only) against the canonical pidport path and
// returns the resulting Verdict as JSON. macOS short-circuits to 501.
func (s *Server) forceKillProbeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if runtime.GOOS == "darwin" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":  "not_supported_on_macos",
			"detail": macosNotSupportedMsg,
		})
		return
	}
	probe := s.probeForRoute
	if probe == nil {
		probe = defaultProbeForRoute
	}
	v := probe()
	writeJSON(w, http.StatusOK, v)
}

// forceKillHandler implements POST /api/force-kill. It runs C1's
// KillRecordedHolder against the canonical pidport path and maps the
// resulting Verdict.Class + error onto HTTP status. The verdict body
// is always returned (success or failure) so the GUI can refresh its
// post-kill probe state from one round-trip.
func (s *Server) forceKillHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if runtime.GOOS == "darwin" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":  "not_supported_on_macos",
			"detail": macosNotSupportedMsg,
		})
		return
	}
	kill := s.killForRoute
	if kill == nil {
		kill = defaultKillForRoute
	}
	v, err := kill()
	if err == nil {
		// Success path covers VerdictKilledRecovered (we did kill +
		// re-acquire) and VerdictHealthy (incumbent recovered to
		// healthy between Probe and the second-click kill — caller
		// should activate-window instead of forcing a restart).
		writeJSON(w, http.StatusOK, v)
		return
	}
	// Map Verdict.Class onto HTTP status per memo D12. The C1 contract
	// uses Verdict.Class as the canonical signal; the err returned
	// alongside it carries the diagnostic string but is NOT a typed
	// sentinel. Switching on Class keeps the mapping stable even when
	// future C1 work refines err wording.
	switch v.Class {
	case VerdictKillRefused:
		// Three-part identity gate (image/argv/start-time) refused
		// the kill — the recorded PID is not a kill-eligible mcphub
		// gui process. 403 mirrors the design memo's "identity gate
		// refuses" mapping.
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "identity_gate_refused",
			"detail":  err.Error(),
			"verdict": v,
		})
	case VerdictRaceLost:
		// Pidport changed between the GUI's probe-click and the
		// kill-click (a competitor rewrote {pid, port, mtime}), or a
		// new incumbent won the post-kill flock acquire. The GUI must
		// re-probe before retrying. 412 Precondition Failed is the
		// closest semantic match (memo D12: "lock file changed
		// mid-flight").
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error":   "lock_changed_mid_flight",
			"detail":  err.Error(),
			"verdict": v,
		})
	default:
		// VerdictKillFailed (SIGKILL/TerminateProcess returned
		// error), VerdictMalformed (pidport unreadable), or any
		// future class without a dedicated status arm. Surface as
		// 500 so callers see the diagnostic without ambiguity.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "kill_failed",
			"detail":  err.Error(),
			"verdict": v,
		})
	}
}

// defaultProbeForRoute is the production fall-through invoked when
// Server.probeForRoute is nil. It resolves the canonical pidport path
// and runs the read-only C1 Probe against it. Errors resolving the
// path collapse to a VerdictMalformed-shaped body so the GUI gets a
// well-formed Verdict struct on every probe round-trip; the path
// resolution failure itself is rare (XDG_STATE_HOME / LOCALAPPDATA
// missing in production is already a startup-blocking condition).
func defaultProbeForRoute() Verdict {
	path, err := PidportPath()
	if err != nil {
		return Verdict{
			Class:    VerdictMalformed,
			Diagnose: fmt.Sprintf("resolve pidport path: %v", err),
		}
	}
	return Probe(context.Background(), path)
}

// defaultKillForRoute is the production fall-through invoked when
// Server.killForRoute is nil. It resolves the canonical pidport path
// and runs C1 KillRecordedHolder against it with default KillOpts.
// The returned SingleInstanceLock is intentionally discarded: the
// HTTP wrapper does not take ownership of the new lock — the
// post-kill incumbent is whichever process the operator launches
// next via `mcphub gui`. KillRecordedHolder leaves the OS-level
// flock free for that subsequent acquire.
//
// The function returns (Verdict, error) so the handler can map
// Verdict.Class onto HTTP status without caring about the lock
// pointer.
func defaultKillForRoute() (Verdict, error) {
	path, err := PidportPath()
	if err != nil {
		return Verdict{
			Class:    VerdictMalformed,
			Diagnose: fmt.Sprintf("resolve pidport path: %v", err),
		}, fmt.Errorf("resolve pidport path: %w", err)
	}
	lock, v, err := KillRecordedHolder(context.Background(), path, KillOpts{})
	if lock != nil {
		// Defensive: the GUI process is already a single-instance
		// holder, so KillRecordedHolder should never return a usable
		// lock here. If it does (e.g. the wrapper is exercised from
		// a non-GUI host in some future test path), release it
		// immediately so we don't dangle a flock the wrapper cannot
		// own.
		lock.Release()
	}
	return v, err
}
