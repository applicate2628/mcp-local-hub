//go:build windows && test_state_path_env

package tray

import (
	"os"
	"strings"
	"time"
)

const testTrayQuitSignalEnv = "MCPHUB_TEST_TRAY_QUIT_SIGNAL"

// startTestTrayControl exposes no production command surface. In an explicitly
// tagged state-isolated binary, a caller-owned signal file drives the existing
// tray protocol event and message-pump shutdown used by the real Quit menu item.
func startTestTrayControl(tc *trayChild) func() {
	signalPath := strings.TrimSpace(os.Getenv(testTrayQuitSignalEnv))
	stop := make(chan struct{})
	done := make(chan struct{})
	if signalPath == "" {
		close(done)
		return func() {}
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(signalPath); err == nil {
				tc.emitEvent("quit")
				postMessageW(tc.hwnd, wmShutdown, 0, 0)
				return
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
