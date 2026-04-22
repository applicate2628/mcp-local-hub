package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TryActivateIncumbent is called by a second `mcphub gui` invocation when
// AcquireSingleInstance returned ErrSingleInstanceBusy. It reads the
// pidport file to locate the running instance, probes /api/ping with a
// short total deadline, and if that succeeds posts /api/activate-window.
// Returns nil if the incumbent was reached and signaled; any non-nil
// error means the second instance should either escalate (--force) or
// abort with a human-readable message.
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
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusNoContent {
			return fmt.Errorf("activate-window status %d", resp2.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("incumbent unreachable: %w", lastErr)
}
