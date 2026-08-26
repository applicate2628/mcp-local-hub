package reversedepgraph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessTreeHelper(t *testing.T) {
	if os.Getenv("VRDG_PROCESS_TREE_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode == "grandchild" {
		for {
			time.Sleep(time.Second)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		os.Exit(91)
	}
	grandchild := exec.Command(executable, "-test.run=^TestProcessTreeHelper$", "--", "grandchild")
	grandchild.Env = os.Environ()
	if err := grandchild.Start(); err != nil {
		os.Exit(92)
	}
	pidFile := os.Getenv("VRDG_PROCESS_TREE_PID_FILE")
	body := fmt.Sprintf("%d\n%d\n", os.Getpid(), grandchild.Process.Pid)
	if err := os.WriteFile(pidFile, []byte(body), 0o600); err != nil {
		os.Exit(93)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestCancellationReapsProcessTree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runProcessTreeSettlement(t, ctx, cancel, context.Canceled)
}

func TestTimeoutReapsProcessTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	runProcessTreeSettlement(t, ctx, func() {}, context.DeadlineExceeded)
}

func runProcessTreeSettlement(t *testing.T, ctx context.Context, settle func(), wantError error) {
	t.Helper()
	scratch := t.TempDir()
	pidFile := filepath.Join(scratch, "pids.txt")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := append(allowedEnvironment(scratch), "VRDG_PROCESS_TREE_HELPER=1", "VRDG_PROCESS_TREE_PID_FILE="+pidFile)
	resultChannel := make(chan RunOutput, 1)
	go func() {
		resultChannel <- DefaultRunner().Run(ctx, Command{
			Executable: executable,
			Args:       []string{"-test.run=^TestProcessTreeHelper$", "--", "child"},
			Dir:        scratch,
			Env:        environment,
			Stage:      "process_tree_test",
		})
	}()
	pids := waitForPIDFile(t, pidFile)
	settle()
	var output RunOutput
	select {
	case output = <-resultChannel:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not settle after cancellation/timeout")
	}
	if !errors.Is(output.Err, wantError) || !output.Started || !output.Reaped {
		t.Fatalf("runner settlement = %#v, want error %v and started/reaped", output, wantError)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for processExistsForTest(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if processExistsForTest(pid) {
			t.Fatalf("process survived settlement: pid=%d", pid)
		}
	}
}

func waitForPIDFile(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Fields(string(body))
			if len(lines) == 2 {
				pids := make([]int, 0, 2)
				for _, line := range lines {
					pid, err := strconv.Atoi(line)
					if err != nil {
						t.Fatal(err)
					}
					pids = append(pids, pid)
				}
				return pids
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper did not publish process identifiers: %s", path)
	return nil
}
