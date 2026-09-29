//go:build windows

package process

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestJob_KillsAssignedProcessOnClose proves the actual contract:
// when the last handle to a job with KILL_ON_JOB_CLOSE is closed, the
// kernel terminates every process still in the job. Without this, the
// fix is theater — the syscalls succeed but orphans still survive.
//
// Spawns `timeout /T 30` (Windows builtin), assigns it to a job,
// closes the job, asserts the process is dead within ~1s.
func TestJob_KillsAssignedProcessOnClose(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout.exe not on PATH: %v", err)
	}
	cmd := exec.Command("timeout", "/T", "30", "/NOBREAK")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		// Defensive: if the test failed before Close fired, kill the
		// child so we don't leak it across runs.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	if err := job.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// Verify process is alive immediately after assignment — orphan
	// protection should not pre-emptively kill anyone.
	if !processAlive(pid, t) {
		t.Fatalf("process pid=%d already dead before Close — assignment must not pre-kill", pid)
	}

	// Closing the job's last handle should cause the kernel to kill
	// the assigned process via KILL_ON_JOB_CLOSE.
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Poll for process death — kernel cleanup is fast but not instant.
	// 2s is generous; if it takes longer the contract is broken.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process pid=%d still alive 2s after Job.Close — KILL_ON_JOB_CLOSE did not fire", pid)
}

// TestJob_AssignNilCmdReturnsError guards against a misuse of the API
// where a caller forgets to Start() the cmd before assigning. Without
// this check, the underlying OpenProcess(0) syscall returns a
// confusing error from the kernel layer instead of an actionable one.
func TestJob_AssignNilCmdReturnsError(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	if err := job.Assign(nil); err == nil {
		t.Error("Assign(nil) returned nil error; want error")
	}
	cmd := exec.Command("notepad.exe")
	// cmd.Process is nil here — Start was not called.
	if err := job.Assign(cmd); err == nil {
		t.Error("Assign(cmd-not-started) returned nil error; want error")
	}
}

// TestJob_CloseIdempotent guards against a double-Close panic. The
// daemon Stop path closes the job after killProcessTree — if a future
// refactor accidentally calls Close twice, the second call must not
// panic or return a misleading "handle invalid" error.
func TestJob_CloseIdempotent(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Errorf("second Close: %v (must be no-op)", err)
	}
}

// TestJob_TerminateAllWaitsForExit is the load-bearing regression
// guard for ADR-#239 (per-task Job Object). Pre-ADR the TerminateAll
// stub returned immediately after TerminateJobObject, racing the
// kernel teardown — supervisor backoff respawn could rebind the
// port before the orphan socket was released. This test proves the
// post-ADR contract: TerminateAll(timeoutMs > 0) must NOT return
// until ActiveProcesses reaches zero OR the deadline expires AND
// every per-PID handle is signaled.
//
// Method: spawn ping (Windows builtin, console-free sleep proxy),
// assign to its own Job (mirrors the per-spawn allocation in
// runSupervise), call TerminateAll(5000), assert (a) call returned
// without timeout error, (b) wall-clock elapsed under the deadline,
// (c) the PID is no longer alive.
//
// Uses `ping -n 30 -w 1000 127.0.0.1` instead of `timeout.exe`
// because timeout.exe exits immediately with "Input redirection is
// not supported" when stdin is redirected/backgrounded (the
// default exec.Cmd behavior in `go test` runs with no controlling
// console attached). Closing bot P2 on PR #241. ping does not
// require console input and behaves consistently in CI.
func TestJob_TerminateAllWaitsForExit(t *testing.T) {
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skipf("ping.exe not on PATH: %v", err)
	}
	// `ping -n 30 -w 1000 127.0.0.1` sends 30 echo requests at
	// ~1s intervals = ~30s wall-clock. The Job's TerminateAll
	// must reap the process well before that natural exit.
	cmd := exec.Command("ping", "-n", "30", "-w", "1000", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	if err := job.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !processAlive(pid, t) {
		t.Fatalf("pid=%d already dead before TerminateAll — assignment must not pre-kill (or ping exited early — unexpected)", pid)
	}

	start := time.Now()
	if err := job.TerminateAll(5000); err != nil {
		t.Fatalf("TerminateAll: %v (regression: real polling broken OR ActiveProcesses query failed)", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("TerminateAll took %v; should return well within 5s for one ping child (regression: returned timeout when it should have completed)", elapsed)
	}
	if processAlive(pid, t) {
		t.Errorf("pid=%d still alive after TerminateAll returned; the wait-for-exit contract is broken (caller will race respawn against still-tearing-down orphan)", pid)
	}
}

// TestJob_TerminateAllZeroTimeoutSkipsWait guards the explicit
// opt-out-of-wait path documented in jobobject_windows.go. Callers
// that pass timeoutMs=0 want the pre-ADR fire-and-return semantic
// (e.g. shutdown paths where the kernel will reap on process exit
// anyway). A future refactor accidentally making zero-timeout wait
// forever would deadlock supervisor shutdown — this test catches that.
func TestJob_TerminateAllZeroTimeoutSkipsWait(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	start := time.Now()
	if err := job.TerminateAll(0); err != nil {
		t.Errorf("TerminateAll(0) on empty job: %v; want nil (opt-out-of-wait path)", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("TerminateAll(0) took %v; want near-instant return (regression: zero-timeout path is waiting when it should not)", elapsed)
	}
}

func TestJob_TerminateAllTimedNilOrClosedRejects(t *testing.T) {
	var nilJob *Job
	if err := nilJob.TerminateAll(1); err == nil {
		t.Fatal("timed nil Job falsely proved settlement")
	}
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	if err := job.TerminateAll(1); err == nil {
		t.Fatal("timed closed Job falsely proved settlement")
	}
}

func TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty(t *testing.T) {
	if os.Getenv("MCPHUB_JOB_PROOF_CHILD") == "1" {
		time.Sleep(time.Minute)
		return
	}
	queryReal := func(j *Job) (jobObjectBasicAccountingInformation, error) {
		var info jobObjectBasicAccountingInformation
		var used uint32
		err := windows.QueryInformationJobObject(j.Handle(), windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &used)
		return info, err
	}
	fault := errors.New("test-only Job observation fault")
	for _, tc := range []struct {
		name, stage string
		configure   func(*jobTerminationOps, uint32)
		wantTimeout bool
		timeoutMs   uint32
	}{
		{name: "enumeration_error", stage: "member_list", configure: func(o *jobTerminationOps, _ uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, fault }
		}},
		{name: "partial_list", stage: "member_list", configure: func(o *jobTerminationOps, p uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 2, PIDs: []uint32{p}}, nil }
		}},
		{name: "malformed_zero_pid", stage: "member_list", configure: func(o *jobTerminationOps, _ uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 1, PIDs: []uint32{0}}, nil }
		}},
		{name: "empty_list", stage: "member_list", configure: func(o *jobTerminationOps, _ uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, nil }
		}},
		{name: "duplicate_pid", stage: "member_list", configure: func(o *jobTerminationOps, p uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 2, PIDs: []uint32{p, p}}, nil }
		}},
		{name: "member_open", stage: "member_handle", configure: func(o *jobTerminationOps, _ uint32) {
			o.openProcess = func(uint32) (windows.Handle, error) { return 0, fault }
		}},
		{name: "member_identity_error", stage: "member_identity", configure: func(o *jobTerminationOps, _ uint32) {
			o.isMember = func(windows.Handle, *Job) (bool, error) { return false, fault }
		}},
		{name: "member_nonmember", stage: "member_identity", configure: func(o *jobTerminationOps, _ uint32) {
			o.isMember = func(windows.Handle, *Job) (bool, error) { return false, nil }
		}},
		{name: "wait_error", stage: "member_wait", configure: func(o *jobTerminationOps, _ uint32) {
			o.wait = func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_FAILED), fault }
		}},
		{name: "wait_unexpected", stage: "member_wait", configure: func(o *jobTerminationOps, _ uint32) {
			o.wait = func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_ABANDONED), nil }
		}},
		{name: "initial_accounting", stage: "snapshot", configure: func(o *jobTerminationOps, _ uint32) {
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				return jobObjectBasicAccountingInformation{}, fault
			}
		}},
		{name: "final_accounting", stage: "accounting", configure: func(o *jobTerminationOps, _ uint32) {
			calls := 0
			o.accounting = func(j *Job) (jobObjectBasicAccountingInformation, error) {
				calls++
				if calls > 1 {
					return jobObjectBasicAccountingInformation{}, fault
				}
				return queryReal(j)
			}
		}},
		{name: "lifetime_total_drift", stage: "membership_changed", configure: func(o *jobTerminationOps, _ uint32) {
			calls := 0
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				calls++
				if calls == 1 {
					return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
				}
				return jobObjectBasicAccountingInformation{TotalProcesses: 2, ActiveProcesses: 0}, nil
			}
		}},
		{name: "handle_close", stage: "handle_close", timeoutMs: 5000, configure: func(o *jobTerminationOps, _ uint32) {
			o.closeProcess = func(h windows.Handle) error { _ = windows.CloseHandle(h); return fault }
		}},
		{name: "zero_accounting_unsignaled_handle", stage: "member_wait", wantTimeout: true, configure: func(o *jobTerminationOps, _ uint32) {
			calls := 0
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				calls++
				if calls == 1 {
					return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
				}
				return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 0}, nil
			}
			o.terminate = func(*Job) error { return nil }
			o.wait = func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
			cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
			if _, err := StartWithJob(job, cmd); err != nil {
				_ = job.Close()
				t.Fatal(err)
			}
			defer func() {
				_ = job.Close()
				_ = cmd.Wait()
				if cmd.Process != nil && IsPidAlive(cmd.Process.Pid) {
					t.Errorf("test child %d survived", cmd.Process.Pid)
				}
			}()
			pids, err := job.processIDs()
			if err != nil || len(pids) != 1 {
				t.Fatalf("owned Job members=%v err=%v", pids, err)
			}
			ops := &jobTerminationOps{}
			kills, opens, closes := 0, 0, 0
			ops.terminate = func(j *Job) error { return windows.TerminateJobObject(j.Handle(), 1) }
			ops.openProcess = func(pid uint32) (windows.Handle, error) {
				opens++
				return windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
			}
			ops.closeProcess = func(h windows.Handle) error { return windows.CloseHandle(h) }
			tc.configure(ops, pids[0])
			terminate := ops.terminate
			ops.terminate = func(j *Job) error { kills++; return terminate(j) }
			closeProcess := ops.closeProcess
			ops.closeProcess = func(h windows.Handle) error { closes++; return closeProcess(h) }
			job.terminationOps = ops
			timeoutMs := tc.timeoutMs
			if timeoutMs == 0 {
				timeoutMs = 1
			}
			err = job.TerminateAll(timeoutMs)
			var proof *JobTerminationProofError
			if !errors.As(err, &proof) || proof.Stage != tc.stage {
				t.Fatalf("fault=%s error=%v want stage=%s", tc.name, err, tc.stage)
			}
			if tc.name == "handle_close" && !errors.Is(err, fault) {
				t.Fatalf("injected handle-close cause was lost: %v", err)
			}
			if tc.wantTimeout && !errors.Is(err, ErrCleanupTimeout) {
				t.Fatalf("lost cleanup-timeout identity: %v", err)
			}
			if kills != 1 {
				t.Fatalf("observation fault skipped Job termination attempt: kills=%d", kills)
			}
			if tc.name != "member_open" && tc.name != "initial_accounting" && tc.name != "partial_list" && tc.name != "malformed_zero_pid" && tc.name != "empty_list" && tc.name != "duplicate_pid" && opens != closes {
				t.Fatalf("owned member handles not closed: opens=%d closes=%d", opens, closes)
			}
		})
	}
}

func TestJob_ContainedCurrentTreeDoesNotGrantStrictLifetimeProof(t *testing.T) {
	if os.Getenv("MCPHUB_JOB_CURRENT_TREE_CHILD") == "root" {
		return
	} else if os.Getenv("MCPHUB_JOB_CURRENT_TREE_CHILD") == "member" {
		time.Sleep(time.Minute)
		return
	}
	for _, strict := range []bool{false, true} {
		name := "contained"
		if strict {
			name = "public_strict"
		}
		t.Run(name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			var member *exec.Cmd
			defer func() {
				_ = job.Close()
				if member != nil && member.Process != nil {
					_ = member.Wait()
					if IsPidAlive(member.Process.Pid) {
						t.Errorf("test member %d survived", member.Process.Pid)
					}
				}
			}()
			root := exec.Command(os.Args[0], "-test.run=^TestJob_ContainedCurrentTreeDoesNotGrantStrictLifetimeProof$")
			root.Env = append(os.Environ(), "MCPHUB_JOB_CURRENT_TREE_CHILD=root")
			if _, err := StartWithJob(job, root); err != nil {
				t.Fatal(err)
			}
			rootWaited := false
			defer func() {
				if !rootWaited {
					_ = root.Wait()
				}
			}()
			borrowed, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(root.Process.Pid))
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(borrowed)
			if err := root.Wait(); err != nil {
				t.Fatal(err)
			}
			rootWaited = true
			member = exec.Command(os.Args[0], "-test.run=^TestJob_ContainedCurrentTreeDoesNotGrantStrictLifetimeProof$")
			member.Env = append(os.Environ(), "MCPHUB_JOB_CURRENT_TREE_CHILD=member")
			if _, err := StartWithJob(job, member); err != nil {
				t.Fatal(err)
			}
			calls := 0
			job.terminationOps = &jobTerminationOps{
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					calls++
					active := uint32(0)
					if calls == 1 {
						active = 1
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 3, ActiveProcesses: active}, nil
				},
				listMembers: func(*Job) (jobProcessIDList, error) {
					return jobProcessIDList{Assigned: 1, PIDs: []uint32{uint32(member.Process.Pid)}}, nil
				},
			}
			if strict {
				err = job.TerminateAll(5000)
				var proof *JobTerminationProofError
				if !errors.As(err, &proof) || proof.Stage != "membership_changed" {
					t.Fatalf("public strict err=%v", err)
				}
			} else {
				err = job.terminateForContained(5000, containedJobOriginal{job: job, handle: borrowed, pid: uint32(root.Process.Pid), atCreate: true})
				if err != nil {
					t.Fatalf("contained current tree err=%v", err)
				}
			}
			if calls < 3 {
				t.Fatalf("missing final Job accounting: %d", calls)
			}
		})
	}
}

func TestJob_ContainedCurrentTreeFaultsDoNotProveExit(t *testing.T) {
	fault := errors.New("test-only current-tree observation fault")
	for _, tc := range []struct {
		name, stage string
		configure   func(*jobTerminationOps)
		noOriginal  bool
	}{
		{name: "missing_original", stage: "borrowed_handle", noOriginal: true},
		{name: "empty_without_original", stage: "borrowed_handle", noOriginal: true, configure: func(o *jobTerminationOps) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, nil }
		}},
		{name: "member_list", stage: "member_list", configure: func(o *jobTerminationOps) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, fault }
		}},
		{name: "member_open", stage: "member_handle", configure: func(o *jobTerminationOps) { o.openProcess = func(uint32) (windows.Handle, error) { return 0, fault } }},
		{name: "member_identity", stage: "member_identity", configure: func(o *jobTerminationOps) {
			o.isMember = func(windows.Handle, *Job) (bool, error) { return false, fault }
		}},
		{name: "member_wait", stage: "member_wait", configure: func(o *jobTerminationOps) { o.wait = func(windows.Handle) (uint32, error) { return 0, fault } }},
		{name: "handle_close", stage: "handle_close", configure: func(o *jobTerminationOps) {
			o.closeProcess = func(h windows.Handle) error { _ = windows.CloseHandle(h); return fault }
		}},
		{name: "active_stays_nonzero", stage: "member_wait", configure: func(o *jobTerminationOps) {
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				return jobObjectBasicAccountingInformation{TotalProcesses: 3, ActiveProcesses: 1}, nil
			}
		}},
		{name: "total_drifts", stage: "membership_changed", configure: func(o *jobTerminationOps) {
			n := 0
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				n++
				total := uint32(3)
				if n > 1 {
					total = 4
				}
				return jobObjectBasicAccountingInformation{TotalProcesses: total, ActiveProcesses: 0}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			defer job.Close()
			borrowed, err := windows.CreateEvent(nil, 1, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(borrowed)
			member, err := windows.CreateEvent(nil, 1, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			memberOwned := true
			defer func() {
				if memberOwned {
					_ = windows.CloseHandle(member)
				}
			}()
			calls, kills := 0, 0
			ops := &jobTerminationOps{
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					calls++
					active := uint32(0)
					if calls == 1 {
						active = 1
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 3, ActiveProcesses: active}, nil
				},
				listMembers: func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 1, PIDs: []uint32{42}}, nil },
				openProcess: func(uint32) (windows.Handle, error) { memberOwned = false; return member, nil },
				isMember:    func(windows.Handle, *Job) (bool, error) { return true, nil },
				terminate:   func(*Job) error { kills++; return nil },
			}
			if tc.configure != nil {
				tc.configure(ops)
			}
			job.terminationOps = ops
			original := containedJobOriginal{job: job, handle: borrowed, pid: 7, atCreate: true}
			if tc.noOriginal {
				original = containedJobOriginal{}
			}
			err = job.terminateForContained(20, original)
			var proof *JobTerminationProofError
			if !errors.As(err, &proof) || proof.Stage != tc.stage {
				t.Fatalf("err=%v want stage=%s", err, tc.stage)
			}
			if kills != 1 {
				t.Fatalf("Job kill attempts=%d", kills)
			}
		})
	}
}

func TestJob_TerminateAllLateRealHandleSignalNeverProves(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
	cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
	if _, err := StartWithJob(job, cmd); err != nil {
		_ = job.Close()
		t.Fatal(err)
	}
	defer func() { _ = job.Close(); _ = cmd.Wait() }()
	observedRealSignal := false
	job.terminationOps = &jobTerminationOps{wait: func(h windows.Handle) (uint32, error) {
		time.Sleep(5 * time.Millisecond)
		status, waitErr := windows.WaitForSingleObject(h, 2000)
		if waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
			observedRealSignal = true
		}
		return status, waitErr
	}}
	err = job.TerminateAll(1)
	var proof *JobTerminationProofError
	if !observedRealSignal {
		t.Fatalf("test never observed the real original handle signal: %v", err)
	}
	if !errors.As(err, &proof) || proof.Stage != "member_wait" || !errors.Is(err, ErrCleanupTimeout) {
		t.Fatalf("late real handle signal falsely certified timed termination: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("test child unexpectedly exited cleanly")
	}
	if cmd.Process == nil || IsPidAlive(cmd.Process.Pid) {
		t.Fatalf("test child survived: %v", cmd.Process)
	}
}

func TestJob_TerminateAllRechecksAccountingAfterOriginalHandleSignal(t *testing.T) {
	fault := errors.New("final accounting observation failed")
	for _, tc := range []struct {
		name, stage string
		finalErr    error
		finalTotal  uint32
	}{
		{name: "lifetime_drift", stage: "membership_changed", finalTotal: 2},
		{name: "query_error", stage: "accounting", finalErr: fault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
			cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
			if _, err := StartWithJob(job, cmd); err != nil {
				_ = job.Close()
				t.Fatal(err)
			}
			defer func() { _ = job.Close(); _ = cmd.Wait() }()
			queries, closed := 0, 0
			observedReal := false
			job.terminationOps = &jobTerminationOps{
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					queries++
					if queries == 1 {
						return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
					}
					if queries == 2 {
						return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 0}, nil
					}
					if tc.finalErr != nil {
						return jobObjectBasicAccountingInformation{}, tc.finalErr
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: tc.finalTotal, ActiveProcesses: 0}, nil
				},
				wait: func(h windows.Handle) (uint32, error) {
					status, waitErr := windows.WaitForSingleObject(h, 2000)
					if waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
						observedReal = true
					}
					return status, waitErr
				},
				closeProcess: func(h windows.Handle) error { closed++; return windows.CloseHandle(h) },
			}
			err = job.TerminateAll(5000)
			var proof *JobTerminationProofError
			if !observedReal || queries < 3 || !errors.As(err, &proof) || proof.Stage != tc.stage {
				t.Fatalf("post-signal final account not enforced: real=%v queries=%d err=%v", observedReal, queries, err)
			}
			if tc.finalErr != nil && !errors.Is(err, tc.finalErr) {
				t.Fatalf("final query cause lost: %v", err)
			}
			if closed != 1 {
				t.Fatalf("original handle close count=%d want=1", closed)
			}
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if cmd.Process == nil || IsPidAlive(cmd.Process.Pid) {
				t.Fatalf("owned child survived: %v", cmd.Process)
			}
		})
	}
}

func TestJob_TerminateAllAdaptiveShortWindowObservesRealHandle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		signalAt  time.Duration
		wantProof bool
	}{
		{name: "signal_by_10ms", signalAt: 10 * time.Millisecond, wantProof: true},
		{name: "signal_after_100ms", signalAt: 100 * time.Millisecond, wantProof: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
			cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
			if _, err := StartWithJob(job, cmd); err != nil {
				_ = job.Close()
				t.Fatal(err)
			}
			defer func() { _ = job.Close(); _ = cmd.Wait() }()
			virtual := time.Unix(0, 0)
			start := virtual
			queries, sleeps, closes := 0, 0, 0
			realSignal := false
			job.terminationOps = &jobTerminationOps{
				now:   func() time.Time { return virtual },
				sleep: func(d time.Duration) { virtual = virtual.Add(d); sleeps++ },
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					queries++
					if queries == 1 {
						return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 0}, nil
				},
				wait: func(h windows.Handle) (uint32, error) {
					if virtual.Sub(start) < tc.signalAt {
						return uint32(windows.WAIT_TIMEOUT), nil
					}
					status, waitErr := windows.WaitForSingleObject(h, 2000)
					if waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
						realSignal = true
					}
					return status, waitErr
				},
				closeProcess: func(h windows.Handle) error { closes++; return windows.CloseHandle(h) },
			}
			err = job.TerminateAll(50)
			if tc.wantProof {
				if err != nil || !realSignal || virtual.Sub(start) >= 50*time.Millisecond || queries < 4 {
					t.Fatalf("fast genuine signal lacked bounded proof: err=%v real=%v elapsed=%v queries=%d", err, realSignal, virtual.Sub(start), queries)
				}
			} else {
				var proof *JobTerminationProofError
				if !errors.As(err, &proof) || proof.Stage != "member_wait" || !errors.Is(err, ErrCleanupTimeout) || realSignal {
					t.Fatalf("slow signal falsely proved: %v real=%v", err, realSignal)
				}
			}
			if queries > 12 || sleeps > 10 || closes != 1 {
				t.Fatalf("poll/handle bounds queries=%d sleeps=%d closes=%d", queries, sleeps, closes)
			}
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if cmd.Process == nil || IsPidAlive(cmd.Process.Pid) {
				t.Fatalf("owned child survived: %v", cmd.Process)
			}
		})
	}
}

// processAlive returns true if a process with pid is still in the OS
// process table. Uses windows.OpenProcess with SYNCHRONIZE — sufficient
// for liveness probe and tolerates the fast post-exit window where the
// process object exists but is signaled.
func processAlive(pid int, t *testing.T) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Most likely "no such process" once the kernel has reaped it.
		return false
	}
	defer windows.CloseHandle(h)
	// WaitForSingleObject with timeout 0 returns WAIT_OBJECT_0 if the
	// process has exited (process handle is signaled on exit).
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return ev != uint32(windows.WAIT_OBJECT_0) // alive if NOT signaled
}

// silence unused-import on builds that strip the strconv usage.
var _ = strconv.Itoa
var _ = os.Stderr
