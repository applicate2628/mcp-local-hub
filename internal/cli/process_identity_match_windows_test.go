//go:build windows

package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestPidMatchesMcphub_WindowsBasename(t *testing.T) {
	if pidMatchesMcphub(os.Getpid()) {
		t.Fatalf("current test binary pid %d must not match mcphub.exe basename", os.Getpid())
	}

	helperPath := copyCurrentTestBinaryAs(t, "mcphub.exe")
	cmd := exec.Command(helperPath, "-test.run=TestPidMatchesMcphub_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PID_MATCH_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mcphub.exe-named helper: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper started without Process")
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pidMatchesMcphub(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pid %d for mcphub.exe-named helper did not match mcphub.exe basename", pid)
}

func TestLoadSupervisorCurrentRunning_SkipsLiveNonMcphubPID(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestPidMatchesMcphub_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PID_MATCH_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start non-mcphub helper: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper started without Process")
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	pid := cmd.Process.Pid
	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			reconcileWiringTestTaskName: {
				State:      "running",
				CurrentPID: pid,
			},
		},
	}
	if err := api.WriteSupervisorState(filepath.Join(tmpHome, "supervisor-state.json"), state); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	got, gotPIDs := loadSupervisorCurrentRunning(tmpHome)
	if got[reconcileWiringTestTaskName] {
		t.Fatalf("live non-mcphub pid %d must not suppress startup spawn; currentRunning=%v", pid, got)
	}
	if len(got) != 0 || len(gotPIDs) != 0 {
		t.Fatalf("expected no current-running entries for non-mcphub pid, got currentRunning=%v pids=%v", got, gotPIDs)
	}
}

func TestPidMatchesMcphub_HelperSleep(t *testing.T) {
	if os.Getenv("MCPHUB_PID_MATCH_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

func copyCurrentTestBinaryAs(t *testing.T, name string) string {
	t.Helper()

	srcPath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dstPath := filepath.Join(t.TempDir(), name)
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("open test binary: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create helper binary: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatalf("copy helper binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close helper binary: %v", err)
	}
	return dstPath
}
