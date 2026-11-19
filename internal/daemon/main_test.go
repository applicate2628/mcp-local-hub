package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

const stdioOriginalHelperMode = "MCPHUB_STDIO_ORIGINAL_HELPER_MODE"
const stdioOriginalHelperRoot = "MCPHUB_STDIO_ORIGINAL_HELPER_ROOT"

// The existing Windows helper supplies the native reparse-point check.
var stdioOriginalHelperDirectoryCheck func(string) error

func validateStdioOriginalHelperInvocation(args []string, mode, root string) error {
	if len(args) != 2 || args[1] != "-test.run=^TestStdioOriginalHelper$" {
		return errors.New("original stdio helper requires its exact test selector")
	}
	switch mode {
	case "echo", "final", "wrapper", "descendant", "sentinel":
	default:
		return errors.New("original stdio helper mode is invalid")
	}
	if !filepath.IsAbs(root) || stdioOriginalHelperDirectoryCheck == nil {
		return errors.New("original stdio helper requires an absolute native directory")
	}
	return stdioOriginalHelperDirectoryCheck(root)
}

// This package is a separate test binary: importing api does not run its TestMain.
// Keep the cached strict-mode posture and fallback audit writes in an owned root.
func TestMain(m *testing.M) {
	mode, modeSet := os.LookupEnv(stdioOriginalHelperMode)
	root, rootSet := os.LookupEnv(stdioOriginalHelperRoot)
	if modeSet || rootSet {
		if err := validateStdioOriginalHelperInvocation(os.Args, mode, root); err != nil {
			fmt.Fprintln(os.Stderr, "internal/daemon TestMain:", err)
			os.Exit(2)
		}
		// These children use only their parent-owned marker directory and may
		// exit or be killed before m.Run returns. Do not allocate a state root.
		os.Exit(m.Run())
	}

	isolatedRoot, err := os.MkdirTemp("", "mcphub-daemon-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/daemon TestMain: create state root:", err)
		os.Exit(1)
	}
	if err := apitest.HardenedDirForTestMain(isolatedRoot); err != nil {
		fmt.Fprintln(os.Stderr, "internal/daemon TestMain: harden state root:",
			errors.Join(err, apitest.RemoveTestMainRoot(isolatedRoot)))
		os.Exit(1)
	}
	restore := api.SetDaemonStateRootForTest(isolatedRoot)
	api.ResetStrictModeIntentCacheForTest()

	code := m.Run()

	restore()
	api.ResetStrictModeIntentCacheForTest()
	if err := apitest.RemoveTestMainRoot(isolatedRoot); err != nil {
		fmt.Fprintln(os.Stderr, "internal/daemon TestMain cleanup:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
