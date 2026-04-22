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
	pid, port, err := ReadPidport(pidportPath)
	if err != nil {
		return fmt.Errorf("read pidport: %w", err)
	}
	deadline := time.Now().Add(totalTimeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		var body struct {
			OK  bool `json:"ok"`
			PID int  `json:"pid"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
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
