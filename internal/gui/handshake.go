package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrIncumbentNoActivationTarget signals that the incumbent GUI process
// was reachable but reported it cannot bring a dashboard window to front.
// Callers of TryActivateIncumbent (cli/gui.go) use errors.Is to distinguish
// this non-fatal "incumbent alive but no activation target" outcome from
// "incumbent unreachable" and print useful guidance to the operator.
// Codex bot review on PR #26 P2.
var ErrIncumbentNoActivationTarget = errors.New("incumbent reachable but has no dashboard window to focus")

// IncumbentNoActivationTargetError is the typed error returned when the
// 503 path fires. It carries the port `TryActivateIncumbent` already
// verified via /api/ping plus the incumbent's refusal reason, so callers can
// choose correct guidance without re-reading the pidport (which races a
// successor's pre-bind port=0 write). Implements Is so
// `errors.Is(err, ErrIncumbentNoActivationTarget)` keeps working.
// Codex CLI xhigh review on PR #26 P3.
type IncumbentNoActivationTargetError struct {
	// Port is the port the incumbent was successfully ping'd on before
	// the activate-window POST returned 503.
	Port int
	// Reason distinguishes a genuinely headless incumbent from a local
	// --no-browser instance with no window. Older incumbents omit the
	// wire header, which defaults to ReasonNoBrowserWindow.
	Reason ActivationNoTargetReason
}

func (e *IncumbentNoActivationTargetError) Error() string {
	return fmt.Sprintf("activate-window on port %d: %s: %s", e.Port, ErrIncumbentNoActivationTarget.Error(), e.Reason)
}

func (e *IncumbentNoActivationTargetError) Is(target error) bool {
	return target == ErrIncumbentNoActivationTarget
}

// TryActivateIncumbent is called by a second `mcphub gui` invocation when
// AcquireSingleInstance returned ErrSingleInstanceBusy. It reads the
// pidport file to locate the running instance, probes /api/ping with a
// short total deadline, and if that succeeds posts /api/activate-window.
// Returns nil if the incumbent was reached and signaled, a typed
// IncumbentNoActivationTargetError if it was reached but could not activate,
// or another error when the second instance should escalate or abort.
func TryActivateIncumbent(pidportPath string, totalTimeout time.Duration) error {
	deadline := time.Now().Add(totalTimeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	var lastErr error
	var pid, port int
	var err error
	for time.Now().Before(deadline) {
		// Re-read pidport on each iteration: the incumbent writes the
		// pidport with the configured port (often 0) BEFORE bind, then
		// rewrites it to the OS-assigned port after Server.Start resolves
		// the ephemeral port (see gui.RewritePidportPort). Polling lets a
		// second instance launched during that startup window catch up to
		// the update instead of forever probing 127.0.0.1:0.
		pid, port, err = ReadPidport(pidportPath)
		if err != nil {
			lastErr = fmt.Errorf("read pidport: %w", err)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if port == 0 {
			lastErr = fmt.Errorf("incumbent still binding (pidport port=0)")
			time.Sleep(250 * time.Millisecond)
			continue
		}
		resp, perr := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
		if perr != nil {
			lastErr = perr
			time.Sleep(250 * time.Millisecond)
			continue
		}
		var body struct {
			OK  bool `json:"ok"`
			PID int  `json:"pid"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if decErr != nil {
			// Non-JSON or malformed response. This does not
			// definitively mean "the incumbent said not-ok": the
			// pidport's port briefly points at the configured port
			// (often the default 17842) before Server.Start binds
			// and RewritePidportPort overwrites it with the actual
			// OS-assigned port. During that race window a probe can
			// land on an unrelated transient listener (another dev
			// server, a browser extension, a stale socket) that
			// returns HTML, an empty body, or any other non-JSON
			// content on 200. Bailing with "not-ok" after the first
			// such reply would kill the handshake even though the
			// real mcphub gui incumbent is about to become reachable.
			// Treat it as transient and keep retrying until
			// totalTimeout; a truly unreachable incumbent falls out
			// of the loop naturally and returns the aggregated
			// "incumbent unreachable" error.
			//
			// Contrast with the body.OK==false branch below: that
			// case is a STRUCTURED JSON reply from something
			// fluent in the mcphub ping schema explicitly saying
			// it is unhealthy — terminal, no retry helps.
			lastErr = fmt.Errorf("decode ping: %w", decErr)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if !body.OK {
			return fmt.Errorf("incumbent ping replied not-ok")
		}
		if body.PID != pid {
			return fmt.Errorf("pidport PID %d does not match running /api/ping PID %d", pid, body.PID)
		}
		// Ping OK — activate.
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/activate-window", port), nil)
		resp2, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("activate-window: %w", err)
		}
		// Reason classification, including version skew. An incumbent that
		// predates this header only ever returned 503/no-target from ONE
		// branch — the headless-session check — so a MISSING header
		// unambiguously means headless, and defaulting it to
		// no-browser-window would strip the SSH-tunnel guidance a remote
		// operator needs (the local-URL wording points them at their own
		// machine). An UNKNOWN (present but unrecognized) value comes from a
		// NEWER incumbent whose reasons we do not model yet: fall back to the
		// generic no-window wording rather than inventing a headless story.
		reason := ReasonHeadless
		switch v := resp2.Header.Get(activationNoTargetReasonHeader); v {
		case "":
			reason = ReasonHeadless // legacy incumbent: headless was the only 503 source
		case string(ReasonHeadless):
			reason = ReasonHeadless
		default:
			reason = ReasonNoBrowserWindow
		}
		resp2.Body.Close()
		switch resp2.StatusCode {
		case http.StatusNoContent:
			return nil
		case http.StatusServiceUnavailable:
			// Incumbent reachable but cannot focus / launch a window
			// (no activation target). Return a typed error carrying the
			// verified port so the cli caller can print no-window guidance
			// without re-reading the pidport (which races a
			// successor's pre-bind port=0 write). errors.Is against
			// ErrIncumbentNoActivationTarget keeps working via the
			// Is method on the typed error. Codex CLI xhigh review
			// on PR #26 P3.
			return &IncumbentNoActivationTargetError{Port: port, Reason: reason}
		default:
			return fmt.Errorf("activate-window status %d", resp2.StatusCode)
		}
	}
	return fmt.Errorf("incumbent unreachable: %w", lastErr)
}
