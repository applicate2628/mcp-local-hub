package pinstatus

import (
	"context"
	"encoding/json"
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

const remoteProcessHelperEnv = "MCPHUB_PINSTATUS_REMOTE_PROCESS_HELPER"

func TestRemoteRefsProcessHelper(t *testing.T) {
	mode := os.Getenv(remoteProcessHelperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "success":
		fmt.Printf("%s\tHEAD\n", commitA)
		ready := os.Getenv("MCPHUB_REMOTE_READY")
		release := os.Getenv("MCPHUB_REMOTE_RELEASE")
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForRemoteHelperFile(t, release)
		fmt.Printf("%s\trefs/heads/main\n", commitA)
	case "limit-tree", "cancel-tree":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		grandchild := exec.Command(exe, "-test.run=^TestRemoteRefsProcessHelper$")
		grandchild.Env = append(os.Environ(), remoteProcessHelperEnv+"=hold")
		grandchild.Stdout = os.Stdout
		grandchild.Stderr = os.Stderr
		if err := grandchild.Start(); err != nil {
			t.Fatal(err)
		}
		pids := fmt.Sprintf("%d\n%d\n", os.Getpid(), grandchild.Process.Pid)
		if err := os.WriteFile(os.Getenv("MCPHUB_REMOTE_PIDS"), []byte(pids), 0o600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stderr, strings.Repeat("stderr-secret-", 500))
		if mode == "limit-tree" {
			fmt.Println(strings.Repeat("x", MaxRemoteRefLineBytes+1))
		} else {
			fmt.Printf("%s\tHEAD\n", commitA)
			waitForRemoteHelperFile(t, os.Getenv("MCPHUB_REMOTE_RELEASE"))
		}
		_ = grandchild.Wait()
	case "hold":
		time.Sleep(30 * time.Second)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func waitForRemoteHelperFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}

func remoteProcessHelperCommand(t *testing.T, mode string, env ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRemoteRefsProcessHelper$")
	cmd.Env = append(os.Environ(), remoteProcessHelperEnv+"="+mode)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func TestDefaultRemoteRefsRealCommandStreamsAndSettles(t *testing.T) {
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	cmd := remoteProcessHelperCommand(
		t,
		"success",
		"MCPHUB_REMOTE_READY="+ready,
		"MCPHUB_REMOTE_RELEASE="+release,
	)
	type outcome struct {
		refs map[string]string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		refs, err := remoteRefsFromCommand(context.Background(), "https://example.invalid/repo.git", cmd)
		done <- outcome{refs: refs, err: err}
	}()
	waitForRemoteHelperFile(t, ready)
	select {
	case got := <-done:
		t.Fatalf("remote command returned before helper exit: %+v", got)
	default:
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("remoteRefsFromCommand: %v", got.err)
	}
	if got.refs["HEAD"] != commitA || got.refs["refs/heads/main"] != commitA {
		t.Fatalf("refs=%v, want both streamed refs", got.refs)
	}
}

func TestDefaultRemoteRefsReadFailureTerminatesChildAndGrandchild(t *testing.T) {
	root := t.TempDir()
	pidsPath := filepath.Join(root, "pids")
	cmd := remoteProcessHelperCommand(t, "limit-tree", "MCPHUB_REMOTE_PIDS="+pidsPath)
	refs, err := remoteRefsFromCommand(context.Background(), "https://example.invalid/repo.git", cmd)
	if !errors.Is(err, ErrRemoteRefLimit) {
		t.Fatalf("err=%v, want ErrRemoteRefLimit", err)
	}
	if refs != nil {
		t.Fatalf("refs=%v, want nil after parser limit", refs)
	}
	for _, pid := range readRemoteHelperPIDs(t, pidsPath) {
		waitForRemoteProcessGone(t, pid)
	}
	assertRemoteFailureSecretFree(t, err, "stderr-secret-")
}

func TestDefaultRemoteRefsCancellationTerminatesChildAndGrandchild(t *testing.T) {
	root := t.TempDir()
	pidsPath := filepath.Join(root, "pids")
	release := filepath.Join(root, "release")
	cmd := remoteProcessHelperCommand(
		t,
		"cancel-tree",
		"MCPHUB_REMOTE_PIDS="+pidsPath,
		"MCPHUB_REMOTE_RELEASE="+release,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := remoteRefsFromCommand(ctx, "https://example.invalid/repo.git", cmd)
		done <- err
	}()
	pids := readRemoteHelperPIDs(t, pidsPath)
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	for _, pid := range pids {
		waitForRemoteProcessGone(t, pid)
	}
	assertRemoteFailureSecretFree(t, err, "stderr-secret-")
}

func readRemoteHelperPIDs(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Fields(string(data))
			if len(lines) == 2 {
				pids := make([]int, 0, 2)
				for _, line := range lines {
					pid, convErr := strconv.Atoi(line)
					if convErr != nil || pid <= 0 {
						t.Fatalf("invalid PID file %q", data)
					}
					pids = append(pids, pid)
				}
				return pids
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for PID coordination file")
	return nil
}

func assertRemoteFailureSecretFree(t *testing.T, remoteErr error, secret string) {
	t.Helper()
	dir := newPort(t, "remote-process-secret", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:  DefaultFS(),
		Now: fixedNow(),
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			return nil, remoteErr
		},
	})
	whole, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	minimal, err := json.Marshal(result.PublicResultProjection())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(whole), secret) || strings.Contains(string(minimal), secret) {
		t.Fatalf("secret leaked: whole=%s minimal=%s", whole, minimal)
	}
}
