package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProvisionalNoSpawnReviewInvalidCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *exec.Cmd
	}{
		{name: "nil"},
		{name: "empty", cmd: &exec.Cmd{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child, err := StartProvisional(tc.cmd)
			if child != nil || err == nil || (tc.cmd != nil && tc.cmd.Process != nil) {
				t.Fatalf("invalid command produced child=%v err=%v", child, err)
			}
			// Check the complete returned error, never a nested matching cause.
			if _, ok := err.(*ProvisionalNoProcessError); !ok {
				t.Fatalf("owner refused before spawn without no-process evidence: type=%T err=%v", err, err)
			}
		})
	}
}

func TestProvisionalNoSpawnReviewContainmentFailure(t *testing.T) {
	cause := errors.New("injected job creation refusal")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "creation_refused", err: cause},
		{name: "wrapped_creation_refused", err: fmt.Errorf("create job: %w", cause)},
		{name: "nil_job_without_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "spawned")
			cmd := exec.Command(os.Args[0], "-test.run=^TestProvisionalNoSpawnReviewHelper$", "--", marker)
			calls := 0
			child, err := startProvisional(cmd, func() (*Job, error) {
				calls++
				return nil, tc.err
			})
			if child != nil {
				_ = child.TerminateAndWait(time.Second)
				t.Fatal("containment refusal returned a running child")
			}
			if calls != 1 || err == nil || cmd.Process != nil {
				t.Fatalf("containment refusal: calls=%d process=%v err=%v", calls, cmd.Process, err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("containment refusal ran helper: %v", statErr)
			}
			if tc.err != nil && !errors.Is(err, cause) {
				t.Fatalf("containment cause was lost: %v", err)
			}
			if tc.err == nil && !strings.Contains(err.Error(), "job creation returned nil job") {
				t.Fatalf("missing containment diagnostic: %v", err)
			}
			if _, ok := err.(*ProvisionalNoProcessError); !ok {
				t.Fatalf("containment owner failed before spawn without no-process evidence: type=%T err=%v", err, err)
			}
		})
	}
}

func TestProvisionalNoSpawnReviewLaunchFailureRemainsUncertain(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "absent-executable"), "invalid\x00command"} {
		cmd := exec.Command(path)
		child, err := StartProvisional(cmd)
		if child != nil || err == nil || cmd.Process != nil {
			t.Fatalf("bad executable: child=%v process=%v err=%v", child, cmd.Process, err)
		}
		if _, ok := err.(*ProvisionalNoProcessError); ok {
			t.Fatalf("StartWithJob failure was certified without owner proof: %v", err)
		}
	}
}

func TestProvisionalNoSpawnReviewHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			if err := os.WriteFile(os.Args[i+1], []byte("spawned"), 0o600); err != nil {
				os.Exit(2)
			}
			// An owned fixture remains alive until the provisional owner reaps it.
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
	}
}

func TestProvisionalNoSpawnReviewSuccessfulCleanup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProvisionalNoSpawnReviewHelper$", "--", marker)
	child, err := StartProvisional(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if err := child.TerminateAndWait(5 * time.Second); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("owned helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.TerminateAndWait(5 * time.Second); err != nil {
		t.Fatalf("successful owned cleanup: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("successful cleanup did not reap the child")
	}
}
