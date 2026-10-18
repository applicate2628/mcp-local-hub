//go:build windows && cst_vptr_probe

package process

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCSTObserverAtCreateRequiresOriginalRetention(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCSTObserverAtCreateTargetOriginalHandle$")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_OBSERVER_TARGET_HELPER=1")
	defer func() {
		if cmd.Process != nil {
			_ = job.Close()
			_ = cmd.Wait()
		}
	}()
	_, err = StartCSTObserverAtCreateForTest(job, cmd, CSTObserverAtCreateHooks{})
	var startErr *StartWithJobError
	if !errors.As(err, &startErr) || startErr.Phase != StartWithJobInvalid || cmd.Process != nil {
		t.Fatalf("missing original callback launched child or lacked invalid-input error: err=%v process=%v", err, cmd.Process)
	}
}

func TestCSTObserverTargetFilesRequireOriginalAndCompleteStreams(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCSTObserverAtCreateTargetOriginalHandle$")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_OBSERVER_TARGET_HELPER=1")
	_, err = StartCSTObserverTargetAtCreateForTest(job, cmd, os.Stdin, os.Stdout, os.Stderr, CSTObserverAtCreateHooks{})
	var startErr *StartWithJobError
	if !errors.As(err, &startErr) || startErr.Phase != StartWithJobInvalid || cmd.Process != nil {
		t.Fatalf("missing original callback launched target: err=%v process=%v", err, cmd.Process)
	}
	_, err = StartCSTObserverTargetAtCreateForTest(job, cmd, nil, os.Stdout, os.Stderr, CSTObserverAtCreateHooks{
		RetainOriginal: func(windows.Handle, uint32) error { return nil },
	})
	if !errors.As(err, &startErr) || startErr.Phase != StartWithJobInvalid || cmd.Process != nil {
		t.Fatalf("partial target streams launched target: err=%v process=%v", err, cmd.Process)
	}
}

func TestCSTObserverAtCreateTargetOriginalHandle(t *testing.T) {
	if os.Getenv("MCPHUB_CST_OBSERVER_TARGET_HELPER") == "1" {
		time.Sleep(time.Hour)
		return
	}
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCSTObserverAtCreateTargetOriginalHandle$")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_OBSERVER_TARGET_HELPER=1")
	var original windows.Handle
	var originalPID uint32
	defer func() {
		if err := job.Close(); err != nil {
			t.Errorf("inner target Job close: %v", err)
		}
		if original != 0 {
			result, err := windows.WaitForSingleObject(original, 500)
			if err != nil || result != windows.WAIT_OBJECT_0 {
				t.Errorf("retained original target did not signal after Job close: result=%d err=%v", result, err)
				return // preserve the uncertain original handle until this test host exits
			}
			if err := windows.CloseHandle(original); err != nil {
				t.Errorf("retained original target close: %v", err)
			}
		}
		if cmd.Process != nil {
			_ = cmd.Wait()
		}
	}()
	_, err = StartCSTObserverAtCreateForTest(job, cmd, CSTObserverAtCreateHooks{
		RetainOriginal: func(handle windows.Handle, pid uint32) error {
			original, originalPID = handle, pid
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if original == 0 || cmd.Process == nil || uint32(cmd.Process.Pid) != originalPID {
		t.Fatalf("target original was not retained at-create: handle=%v pid=%d cmd=%v", original, originalPID, cmd.Process)
	}
	retainedPID, err := windows.GetProcessId(original)
	if err != nil || retainedPID != originalPID {
		t.Fatalf("retained target handle identity mismatch: handle pid=%d expected=%d err=%v", retainedPID, originalPID, err)
	}
	member, active, pids, err := CSTObserverOriginalMemberForTest(job, original)
	if err != nil || !member || active == 0 || active != uint32(len(pids)) {
		t.Fatalf("target original not in inner Job: member=%v active=%d pids=%v err=%v", member, active, pids, err)
	}
	t.Logf("original target retained before ordinary close; inner Job members=%d", active)
}
