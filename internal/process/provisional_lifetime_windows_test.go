//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestProvisionalCallerNativeStartupFailureCustody(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsettled=%v", uncertain), func(t *testing.T) {
			root := t.TempDir()
			cmd := provisionalLifetimeCommand("root", root)
			startCause := errors.New("owned post-create Go wrapper refusal")
			snapshotCause := errors.New("owned initial accounting observation refusal")
			var captured *provisionalWindowsChild
			child, err := startProvisionalWithStarter(cmd, NewKillOnCloseJob, func(job *Job, cmd *exec.Cmd) (containedChild, error) {
				native := job.terminationOperations()
				if uncertain {
					job.terminationOps = &jobTerminationOps{accounting: func(j *Job) (jobObjectBasicAccountingInformation, error) {
						info, err := native.accounting(j)
						if info.TotalProcesses != 0 {
							return info, errors.Join(err, snapshotCause)
						}
						return info, err
					}}
				}
				owner, err := startProvisionalWithJobObserver(job, cmd, func(int) (*os.Process, error) {
					return nil, startCause
				})
				captured = owner.(*provisionalWindowsChild)
				return owner, err
			})
			defer func() {
				if child != nil && child.job.Handle() != 0 {
					child.job.terminationOps = nil
					if err := child.TerminateAndWait(5 * time.Second); err != nil {
						t.Errorf("retained startup owner cleanup: %v", err)
					}
				}
			}()
			if !errors.Is(err, startCause) {
				t.Fatalf("startup cause lost: %v", err)
			}
			if uncertain {
				if child == nil || !errors.Is(err, snapshotCause) || captured.process == 0 || captured.thread == 0 || captured.job.Handle() == 0 {
					t.Fatalf("unsettled owner custody discarded: child=%v error=%v", child, err)
				}
				// This is a per-case injected observation refusal, not an OS or
				// host denial; recovery uses the same retained creator owner.
				captured.job.terminationOps = nil
				if err := child.TerminateAndWait(5 * time.Second); err != nil {
					t.Fatalf("owned same-binding recovery: %v", err)
				}
			} else if child != nil {
				t.Fatalf("confirmed startup cleanup retained an owner: %v", err)
			}
			if captured.process != 0 || captured.thread != 0 || captured.job.Handle() != 0 {
				t.Fatal("creator startup handles were not closed")
			}
			t.Logf("PROVISIONAL_START_ERROR unsettled_initially=%v cause_preserved=true same_owner_recovered=true process_thread_job_closed=true", uncertain)
		})
	}
}

func TestProvisionalCallerNativeMemberCloseFailureRemainsVisible(t *testing.T) {
	root := t.TempDir()
	cmd := provisionalLifetimeCommand("root", root)
	child, err := StartProvisional(cmd)
	defer func() {
		if child != nil && child.job.Handle() != 0 {
			child.job.terminationOps = nil
			if err := child.TerminateAndWait(5 * time.Second); err != nil {
				t.Errorf("owned close-failure cleanup: %v", err)
			}
		}
	}()
	if err != nil {
		t.Fatal(err)
	}
	provisionalLifetimeWait(t, root, "root-ready")
	provisionalLifetimeMark(t, root, "release")
	provisionalLifetimeWait(t, root, "churn-ready")
	native := child.job.terminationOperations()
	closeCause := errors.New("owned member-close receipt refusal")
	child.job.terminationOps = &jobTerminationOps{closeProcess: func(handle windows.Handle) error {
		return errors.Join(native.closeProcess(handle), closeCause)
	}}
	err = child.TerminateAndWait(5 * time.Second)
	if !errors.Is(err, closeCause) || child.job.Handle() == 0 {
		t.Fatalf("close refusal became success or lost custody: %v", err)
	}
	child.job.terminationOps = nil
	if err := child.TerminateAndWait(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	t.Log("PROVISIONAL_CLOSE_ERROR cause_preserved=true no_false_success=true retained_owner_recovery=true")
}

func TestProvisionalCallerNativeCleanupTimeoutRetainsCustody(t *testing.T) {
	root := t.TempDir()
	cmd := provisionalLifetimeCommand("root", root)
	child, err := StartProvisional(cmd)
	defer func() {
		if child != nil && child.job.Handle() != 0 {
			child.job.terminationOps = nil
			if err := child.TerminateAndWait(5 * time.Second); err != nil {
				t.Errorf("owned timeout fixture cleanup: %v", err)
			}
		}
	}()
	if err != nil {
		t.Fatal(err)
	}
	provisionalLifetimeWait(t, root, "root-ready")
	provisionalLifetimeMark(t, root, "release")
	provisionalLifetimeWait(t, root, "churn-ready")
	// Native Job termination and creator Wait remain real; only the current
	// member wait observation is degraded through the existing per-Job seam.
	child.job.terminationOps = &jobTerminationOps{wait: func(windows.Handle) (uint32, error) {
		return uint32(windows.WAIT_TIMEOUT), nil
	}}
	err = child.TerminateAndWait(20 * time.Millisecond)
	if !errors.Is(err, ErrCleanupTimeout) || child.job.Handle() == 0 {
		t.Fatalf("unsettled cleanup timeout lost its cause/custody: %v", err)
	}
	if child.owner.(*provisionalWindowsChild).process == 0 || child.owner.(*provisionalWindowsChild).thread == 0 {
		t.Fatal("timeout discarded creator handles")
	}
	child.job.terminationOps = nil
	if err := child.TerminateAndWait(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	t.Log("PROVISIONAL_TIMEOUT cause_preserved=true no_false_success=true creator_handles_retained=true same_binding_recovered=true")
}

const provisionalLifetimeRole = "MCPHUB_TEST_PROVISIONAL_LIFETIME_ROLE"
const provisionalLifetimeRoot = "MCPHUB_TEST_PROVISIONAL_LIFETIME_ROOT"

func provisionalLifetimeCommand(role, root string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProvisionalCallerNativeAdmissionLifecycle$")
	cmd.Env = []string{provisionalLifetimeRole + "=" + role, provisionalLifetimeRoot + "=" + root}
	for _, key := range []string{"SYSTEMROOT", "WINDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	NoConsole(cmd)
	return cmd
}

func provisionalLifetimeMark(t *testing.T, root, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func provisionalLifetimeWait(t *testing.T, root, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("owned helper gate %s timed out", name)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProvisionalCallerNativeAdmissionLifecycle(t *testing.T) {
	if role := os.Getenv(provisionalLifetimeRole); role != "" {
		root := os.Getenv(provisionalLifetimeRoot)
		switch role {
		case "root":
			provisionalLifetimeMark(t, root, "root-ready")
			provisionalLifetimeWait(t, root, "release")
			for _, middle := range []string{"middle-one", "middle-two"} {
				cmd := provisionalLifetimeCommand(middle, root)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			provisionalLifetimeWait(t, root, "leaf-one-ready")
			provisionalLifetimeWait(t, root, "leaf-two-ready")
			provisionalLifetimeWait(t, root, "leaf-three-ready")
			provisionalLifetimeMark(t, root, "churn-ready")
			provisionalLifetimeWait(t, root, "never-release")
		case "middle-one", "middle-two":
			leaves := []string{"leaf-one", "leaf-two"}
			if role == "middle-two" {
				leaves = []string{"leaf-three"}
			}
			for _, leaf := range leaves {
				cmd := provisionalLifetimeCommand(leaf, root)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Process.Release(); err != nil {
					t.Fatal(err)
				}
			}
		case "leaf-one", "leaf-two", "leaf-three":
			provisionalLifetimeMark(t, root, role+"-ready")
			provisionalLifetimeWait(t, root, "never-release")
		default:
			t.Fatal("unknown owned helper role")
		}
		return
	}
	for _, mode := range []string{"canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cmd := provisionalLifetimeCommand("root", root)
			child, err := StartProvisional(cmd)
			var held []containedJobOriginal
			var native jobTerminationOps
			defer func() {
				if child != nil && child.job.Handle() != 0 {
					if err := child.TerminateAndWait(5 * time.Second); err != nil {
						t.Errorf("owning fixture cleanup: %v", err)
					}
				}
				for _, entry := range held {
					status, err := native.wait(entry.handle)
					if err != nil || status != 0 {
						t.Errorf("owned member wait: status=%d error=%v", status, err)
					}
					if err := native.closeProcess(entry.handle); err != nil {
						t.Errorf("owned member handle close: %v", err)
					}
				}
			}()
			if err != nil {
				t.Fatal(err)
			}
			job := child.job
			native = job.terminationOperations()
			provisionalLifetimeWait(t, root, "root-ready")
			provisionalLifetimeMark(t, root, "release")
			provisionalLifetimeWait(t, root, "churn-ready")
			accounting, err := job.queryAccounting()
			if err != nil {
				t.Fatal(err)
			}
			members, err := job.queryProcessIDList()
			if err != nil {
				t.Fatal(err)
			}
			for _, pid := range members.PIDs {
				handle, err := native.openProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, containedJobOriginal{job: job, handle: handle, pid: pid})
				member, err := native.isMember(handle, job)
				if err != nil || !member {
					t.Fatalf("same creator Job membership=%v error=%v", member, err)
				}
			}
			if accounting.TotalProcesses <= members.Assigned || len(held) != int(accounting.ActiveProcesses) {
				t.Fatal("owned native fixture did not produce incomplete lifetime history")
			}
			zeroBeforeClose := false
			job.terminationOps = &jobTerminationOps{accounting: func(j *Job) (jobObjectBasicAccountingInformation, error) {
				info, err := native.accounting(j)
				if err == nil && info.ActiveProcesses == 0 {
					zeroBeforeClose = true
				}
				return info, err
			}}
			primary := errors.New("owned admission failure")
			var ctx context.Context
			var cancel context.CancelFunc
			if mode == "canceled" {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			} else {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
			}
			<-ctx.Done()
			cleanup := child.TerminateAndWait(5 * time.Second)
			joined := errors.Join(primary, ctx.Err(), cleanup)
			if !errors.Is(joined, primary) || !errors.Is(joined, ctx.Err()) {
				t.Fatal("admission failure/cancellation cause was lost")
			}
			if cleanup != nil {
				t.Errorf("provisional caller falsely rejected owned current settlement: %v", cleanup)
			}
			if !zeroBeforeClose || job.Handle() != 0 || cmd.ProcessState == nil || !cmd.ProcessState.Exited() || cmd.ProcessState.ExitCode() != 1 {
				t.Fatalf("owned current-zero/direct wait/Job closure incomplete: zero=%v job_closed=%v state=%v", zeroBeforeClose, job.Handle() == 0, cmd.ProcessState)
			}
			if cleanup == nil {
				if err := child.TerminateAndWait(5 * time.Second); err != nil {
					t.Errorf("settled owner cleanup was not idempotent: %v", err)
				}
			}
			t.Logf("PROVISIONAL_NATIVE mode=%s lifetime=%d current_before=%d current_zero_before_close=%v direct_creator_exit=1 job_closed=%v joined_primary=%v", mode, accounting.TotalProcesses, accounting.ActiveProcesses, zeroBeforeClose, job.Handle() == 0, errors.Is(joined, primary))
			if strings.Contains(fmt.Sprint(cleanup), "membership_changed") {
				t.Log("CURRENT_OWNER_ERROR: strict-original coverage was used for provisional current settlement")
			}
		})
	}
}
