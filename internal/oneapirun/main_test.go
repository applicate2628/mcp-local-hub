package oneapirun

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// TestMain keeps implicit API policy reads and audit writes inside an owned
// state root. Windows production resolution uses KnownFolderPath, so redirected
// profile environment variables alone do not isolate these in-process calls.
func TestMain(m *testing.M) {
	stateRoot, err := os.MkdirTemp("", "mcphub-oneapirun-test-state-")
	if err != nil {
		panic("internal/oneapirun TestMain: create state root: " + err.Error())
	}
	if err := apitest.HardenedDirForTestMain(stateRoot); err != nil {
		panic(errors.Join(
			fmt.Errorf("internal/oneapirun TestMain: harden state root: %w", err),
			apitest.RemoveTestMainRoot(stateRoot),
		))
	}

	restoreState := api.SetDaemonStateRootForTest(stateRoot)
	api.ResetStrictModeIntentCacheForTest()

	code := m.Run()

	restoreState()
	api.ResetStrictModeIntentCacheForTest()
	if err := apitest.RemoveTestMainRoot(stateRoot); err != nil {
		if code == 0 {
			code = 1
		}
		_, _ = fmt.Fprintln(os.Stderr, "internal/oneapirun TestMain cleanup:", err)
	}
	os.Exit(code)
}
