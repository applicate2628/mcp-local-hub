package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

// SetWeeklyScheduleUnlockFailuresForTest injects only release failures for the
// two weekly-schedule lock leaves in an already installed private test root.
// Callers must derive root from t.TempDir, bind both settings and daemon state
// to it, and remain serialized. Nil causes leave the corresponding release
// untouched. Real lock handles are retained until test cleanup confirms their
// release; this hook never fabricates a committed mutation or a ledger error.
func SetWeeklyScheduleUnlockFailuresForTest(t *testing.T, root string, weeklyCause, settingsCause error) {
	if !testing.Testing() {
		panic("api.SetWeeklyScheduleUnlockFailuresForTest called outside a test binary")
	}
	t.Helper()
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root ||
		daemonStateRootOverride != root || SettingsPath() != filepath.Join(root, "gui-preferences.yaml") {
		t.Fatal("weekly release fixture requires the same exact private settings and daemon state root")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("weekly release fixture requires an existing private directory: %v", err)
	}
	if weeklyCause == nil && settingsCause == nil {
		t.Fatal("weekly release fixture requires at least one release failure")
	}
	failures := map[string]error{
		filepath.Join(root, WeeklyRefreshTaskName+weeklyRefreshLockSuffix): weeklyCause,
		SettingsPath() + ".lock": settingsCause,
	}
	for path, cause := range failures {
		if cause != nil && unconfirmedLockRelease(path) != nil {
			t.Fatalf("weekly release fixture cannot own a previously poisoned lock: %s", path)
		}
	}

	previous := flockUnlockFn
	stranded := map[string]*flock.Flock{}
	// Register owned cleanup before replacing the process-wide fault seam.
	t.Cleanup(func() {
		flockUnlockFn = previous
		for path, fl := range stranded {
			if err := fl.Unlock(); err != nil {
				t.Errorf("cleanup weekly release fixture lock %s: %v", path, err)
				continue
			}
			unconfirmedLockReleasesMu.Lock()
			delete(unconfirmedLockReleases, path)
			unconfirmedLockReleasesMu.Unlock()
		}
	})
	flockUnlockFn = func(fl *flock.Flock) error {
		if cause := failures[fl.Path()]; cause != nil {
			stranded[fl.Path()] = fl
			return cause
		}
		return previous(fl)
	}
}
