package cli

import (
	"os"
	"testing"
)

// guiSpawnTestsEnv opts a test into spawning REAL `mcphub gui` / `mcphub supervise`
// OS processes. Those are visible on the developer's desktop (and can trip the
// activate-window path into opening a browser window), so they are OFF by default:
// a routine `go test ./internal/cli/` must not litter the machine. CI sets this.
const guiSpawnTestsEnv = "MCPHUB_GUI_SPAWN_TESTS"

// requireGuiSpawnTests skips unless the opt-in env var is set.
func requireGuiSpawnTests(t *testing.T) {
	t.Helper()
	if os.Getenv(guiSpawnTestsEnv) == "" {
		t.Skipf("skipping test that spawns real mcphub GUI/supervisor processes; set %s=1 to run", guiSpawnTestsEnv)
	}
}
