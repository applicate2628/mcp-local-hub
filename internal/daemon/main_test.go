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

// Keep admission independent of the private fixture implementation. Some
// source compositions contain TestOwnedIsolatedStdioFixture in a separate file.
const stdioPrivateHelperMode = "MCPHUB_TEST_PRIVATE_STDIO_FIXTURE"
const stdioPrivateHelperGate = "MCPHUB_TEST_PRIVATE_STDIO_GATE"

type daemonTestRole uint8

const (
	daemonTestNormal daemonTestRole = iota
	daemonTestOriginalHelper
	daemonTestPrivateHelper
)

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

func validateStdioPrivateHelperInvocation(args []string, mode, gate string, gateSet bool) error {
	if len(args) != 2 || args[1] != "-test.run=^TestOwnedIsolatedStdioFixture$" {
		return errors.New("private stdio helper requires its exact test selector")
	}
	if stdioOriginalHelperDirectoryCheck == nil {
		return errors.New("private stdio helper requires native Windows support")
	}
	switch mode {
	case "cooperative", "ignore-eof":
		if gateSet {
			return errors.New("private stdio helper mode does not accept a gate directory")
		}
		return nil
	case "grace-wrapper", "grace-member":
		if !gateSet || !filepath.IsAbs(gate) {
			return errors.New("private stdio grace helper requires an absolute native gate directory")
		}
		return stdioOriginalHelperDirectoryCheck(gate)
	default:
		return errors.New("private stdio helper mode is invalid")
	}
}

func daemonTestHelperDispatch(args []string, lookupEnv func(string) (string, bool)) (daemonTestRole, error) {
	mode, modeSet := lookupEnv(stdioOriginalHelperMode)
	root, rootSet := lookupEnv(stdioOriginalHelperRoot)
	privateMode, privateModeSet := lookupEnv(stdioPrivateHelperMode)
	gate, gateSet := lookupEnv(stdioPrivateHelperGate)
	original := modeSet || rootSet
	private := privateModeSet || gateSet
	if original && private {
		return daemonTestNormal, errors.New("original and private stdio helper environments must not be mixed")
	}
	if original {
		return daemonTestOriginalHelper, validateStdioOriginalHelperInvocation(args, mode, root)
	}
	if private {
		return daemonTestPrivateHelper, validateStdioPrivateHelperInvocation(args, privateMode, gate, gateSet)
	}
	return daemonTestNormal, nil
}

// This package is a separate test binary: importing api does not run its TestMain.
// Keep the cached strict-mode posture and fallback audit writes in an owned root.
func TestMain(m *testing.M) {
	role, err := daemonTestHelperDispatch(os.Args, os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/daemon TestMain:", err)
		os.Exit(2)
	}
	if role != daemonTestNormal {
		// These children use only parent-owned marker directories, when needed,
		// and may exit or be killed before m.Run returns. Do not allocate a root.
		code := m.Run()
		if role == daemonTestPrivateHelper {
			// Every admitted private mode exits the process itself or is killed.
			// A return also catches source compositions that lack the fixture.
			fmt.Fprintln(os.Stderr, "internal/daemon TestMain: private stdio fixture returned from m.Run; helper must exit its process")
			if code == 0 {
				code = 2
			}
		}
		os.Exit(code)
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
