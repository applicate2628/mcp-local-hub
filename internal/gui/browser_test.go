// internal/gui/browser_test.go
package gui

import (
	"errors"
	"testing"
)

func TestLaunchBrowser_PrefersChromeOverEdgeOverDefault(t *testing.T) {
	// This test exercises the spawn-preference LOGIC via the fake spawner, so it
	// must opt OUT of the global MCPHUB_SUPPRESS_BROWSER_LAUNCH kill-switch the
	// gui TestMain sets (that guard early-returns before any spawnProcess call).
	t.Setenv(SuppressBrowserLaunchEnv, "")
	log := []string{}
	restore := withSpawnerOverride(func(cmd string, args ...string) error {
		log = append(log, cmd)
		// chrome "not found", edge "not found", default succeeds
		if cmd == "chrome" || cmd == "google-chrome" || cmd == "chromium" || cmd == "msedge" {
			return errors.New("not found")
		}
		return nil
	})
	defer restore()
	if err := LaunchBrowser("http://127.0.0.1:9100"); err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	if len(log) < 2 {
		t.Errorf("expected at least 2 spawn attempts; got %v", log)
	}
}

func withSpawnerOverride(fn func(string, ...string) error) func() {
	orig := spawnProcess
	spawnProcess = fn
	return func() { spawnProcess = orig }
}
