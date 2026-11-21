package serena_routing

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// This package is a separate test binary: importing api does not run its TestMain.
// Keep the cached strict-mode posture and fallback audit writes in an owned root.
func TestMain(m *testing.M) {
	isolatedRoot, err := os.MkdirTemp("", "mcphub-serena-routing-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/api/serena_routing TestMain: create state root:", err)
		os.Exit(1)
	}
	if err := apitest.HardenedDirForTestMain(isolatedRoot); err != nil {
		fmt.Fprintln(os.Stderr, "internal/api/serena_routing TestMain: harden state root:",
			errors.Join(err, apitest.RemoveTestMainRoot(isolatedRoot)))
		os.Exit(1)
	}
	restore := api.SetDaemonStateRootForTest(isolatedRoot)
	api.ResetStrictModeIntentCacheForTest()

	code := m.Run()

	restore()
	api.ResetStrictModeIntentCacheForTest()
	if err := apitest.RemoveTestMainRoot(isolatedRoot); err != nil {
		fmt.Fprintln(os.Stderr, "internal/api/serena_routing TestMain cleanup:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
