package mcproute

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
)

func TestMain(m *testing.M) {
	api.EnableSupervisorIPCTestPipeIsolation()
	tmp, err := os.MkdirTemp("", "mcphub-workspace-route-test-*")
	if err != nil {
		panic(err)
	}
	stateRoot := filepath.Join(tmp, "AppData", "Local", "mcp-local-hub")
	if err := apitest.HardenedDirForTestMain(stateRoot); err != nil {
		panic(errors.Join(err, apitest.RemoveTestMainRoot(tmp)))
	}
	restoreState := api.SetDaemonStateRootForTest(stateRoot)
	restoreClientEnv, err := clients.ApplyClientConfigSandboxEnvironment(tmp)
	if err != nil {
		restoreState()
		panic(errors.Join(err, apitest.RemoveTestMainRoot(tmp)))
	}
	if err := os.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateRoot); err != nil {
		restoreClientEnv()
		restoreState()
		panic(errors.Join(err, apitest.RemoveTestMainRoot(tmp)))
	}
	auditRestore := clients.EnforceSandboxedConfigPaths(tmp)

	code := m.Run()
	if escapes := auditRestore(); escapes > 0 && code == 0 {
		code = 1
	}
	restoreClientEnv()
	restoreState()
	if err := apitest.RemoveTestMainRoot(tmp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
