package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

const spawnWorkingDirHelperOutputEnv = "MCPHUB_SPAWN_WORKING_DIR_HELPER_OUTPUT"

func TestSpawnWorkingDirHelper(t *testing.T) {
	output := os.Getenv(spawnWorkingDirHelperOutputEnv)
	if output == "" {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(output, []byte(dir), 0o600); err != nil {
		os.Exit(3)
	}
}

func TestProductionSpawnFnUsesDescriptorWorkingDirUnlessWorkspaceOverrides(t *testing.T) {
	root := apitest.HardenedTempDir(t)
	globalDir := filepath.Join(root, "global-working-dir")
	workspaceDir := filepath.Join(root, "workspace-working-dir")
	for _, dir := range []string{globalDir, workspaceDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	events, err := api.OpenSupervisorEventLog(filepath.Join(root, "supervisor-events.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	spawnFn := makeProductionSpawnFn(events, NewDaemonRuntimeTracker())

	for _, tc := range []struct {
		name      string
		workspace string
		want      string
	}{
		{name: "global descriptor working directory", want: globalDir},
		{name: "workspace overrides descriptor working directory", workspace: workspaceDir, want: workspaceDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(root, tc.name+".cwd")
			t.Setenv(spawnWorkingDirHelperOutputEnv, output)
			descriptor := api.SupervisorDaemon{
				TaskName: "\\mcp-local-hub-working-dir-" + tc.name,
				Server:   "working-dir", Daemon: "default",
				Command: os.Args[0], Args: []string{"-test.run=^TestSpawnWorkingDirHelper$"},
				WorkingDir: globalDir, Workspace: tc.workspace,
			}
			if err := spawnFn(descriptor); err != nil {
				t.Fatalf("spawn: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			var got []byte
			for time.Now().Before(deadline) {
				if data, readErr := os.ReadFile(output); readErr == nil {
					got = data
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if string(got) != tc.want {
				t.Fatalf("child working directory = %q, want %q", got, tc.want)
			}
		})
	}
}
