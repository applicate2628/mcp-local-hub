//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Partial duplication exercises the real at-create builder before execution,
// using actual native duplicates and explicit test-owned cleanup on all exits.
func TestOriginalInheritedPartialDuplicationRetainsFirstOutcomes(t *testing.T) {
	for _, disposition := range []string{"consumed-reported-fault", "known-owned", "uncertain"} {
		t.Run(disposition, func(t *testing.T) {
			job, err := NewJobObserved(JobOptions{KillOnClose: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := job.Close(); err != nil {
					t.Error(err)
				}
			}()
			stdin, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			duplicateFailure, closeFailure := errors.New("injected second duplication refusal"), errors.New("injected close report")
			var duplicate windows.Handle
			duplicateCalls, closeCalls := 0, 0
			options := &startWithJobObserverOptions{
				duplicateHandle: func(file *os.File) (windows.Handle, error) {
					duplicateCalls++
					if duplicateCalls == 2 {
						return 0, duplicateFailure
					}
					duplicate, err = duplicateInheritableHandle(file)
					return duplicate, err
				},
				closeChildHandle: func(handle windows.Handle) (originalInheritedCloseOutcome, error) {
					closeCalls++
					if disposition == "consumed-reported-fault" {
						nativeErr := windows.CloseHandle(handle)
						return originalInheritedCloseOutcome{NativeError: nativeErr, ClosedKnown: nativeErr == nil}, closeFailure
					}
					return originalInheritedCloseOutcome{NativeError: closeFailure, OwnedKnown: disposition == "known-owned"}, closeFailure
				},
			}
			owner, err := startWithJobFilesOriginalWithOptions(job, exec.Command("cmd.exe", "/c", "exit", "0"), stdin, stdout, stdout, options)
			if owner == nil {
				t.Fatalf("partial duplication lost original owner: %v", err)
			}
			// Test-created duplicates are known unconsumed in these two cases;
			// recovery uses that actual creator identity, never guessed PID state.
			if disposition != "consumed-reported-fault" && duplicate != 0 {
				defer func() {
					if err := windows.CloseHandle(duplicate); err != nil {
						t.Error(err)
					}
				}()
			}
			if owner.Created() || !errors.Is(err, duplicateFailure) || !errors.Is(err, closeFailure) {
				t.Fatalf("custody refusal lost causes: %v", err)
			}
			child := owner.child.(*windowsContainedChild)
			if len(child.inheritedHandles) != 1 {
				t.Fatalf("acquired slots=%d", len(child.inheritedHandles))
			}
			slot := child.inheritedHandles[0]
			if slot.handle != duplicate || !slot.closeAttempted || slot.actual.ClosedKnown != (disposition == "consumed-reported-fault") || slot.actual.OwnedKnown != (disposition == "known-owned") {
				t.Fatalf("first duplicate disposition was changed: %+v", slot)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			snapshot, observeErr := owner.ObserveOriginal(ctx, false)
			if observeErr != nil || !snapshot.NoChildKnown {
				t.Fatalf("actual no-child accounting: %+v %v", snapshot, observeErr)
			}
			settlement, releaseErr := owner.ReleaseOriginal()
			if settlement.Complete() || !errors.Is(releaseErr, closeFailure) {
				t.Fatalf("partial inherited-close fault became clean: %v", releaseErr)
			}
			_, againErr := owner.ReleaseOriginal()
			if !errors.Is(againErr, closeFailure) || closeCalls != 1 || child.inheritedHandles[0].actual != slot.actual {
				t.Fatalf("repeat lost first result or retried raw handle: %v closes=%d", againErr, closeCalls)
			}
		})
	}
}

func TestOriginalCreatorEnrollmentRefusesAndWrapperFailureRetains(t *testing.T) {
	for _, phase := range []string{"enrollment", "post-create-wrapper"} {
		t.Run(phase, func(t *testing.T) {
			job, err := NewJobObserved(JobOptions{KillOnClose: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := job.Close(); err != nil {
					t.Error(err)
				}
			}()
			stdin, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			fault := errors.New("injected creator boundary refusal")
			options := &startWithJobObserverOptions{}
			if phase == "enrollment" {
				options.newAttributeList = func(count uint32) (*procThreadAttributeListObserved, error) {
					al, err := newProcThreadAttributeListObserved(count)
					if al != nil {
						al.native.update = func(unsafe.Pointer, uintptr, unsafe.Pointer, uintptr) error { return fault }
					}
					return al, err
				}
			} else {
				options.findProcess = func(int) (*os.Process, error) { return nil, fault }
			}
			owner, startErr := startWithJobFilesOriginalWithOptions(job, exec.Command("cmd.exe", "/c", "exit", "17"), stdin, output, output, options)
			if owner == nil || startErr == nil {
				t.Fatalf("creator refusal lost original owner: %v", startErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if phase == "enrollment" {
				if owner.Created() || !errors.Is(startErr, fault) {
					t.Fatalf("enrollment failed after execution: %v", startErr)
				}
				view, err := owner.ObserveOriginal(ctx, false)
				if err != nil || !view.NoChildKnown {
					t.Fatalf("precreate accounting: %+v %v", view, err)
				}
			} else {
				if !owner.Created() || !errors.Is(startErr, ErrSpawnPostCreate) {
					t.Fatalf("postcreate failure masqueraded as no-child: %v", startErr)
				}
				waited := make(chan OriginalRootWait, 1)
				go func() { waited <- owner.WaitOriginal() }()
				select {
				case root := <-waited:
					if !root.Known || root.ExitCode != 17 || root.Error != nil {
						t.Fatalf("actual original wait: %+v", root)
					}
				case <-ctx.Done():
					t.Fatal("actual postcreate original waiter pending")
				}
				if _, err := owner.ObserveOriginal(ctx, true); err != nil {
					t.Fatal(err)
				}
			}
			settlement, err := owner.ReleaseOriginal()
			if err != nil || !settlement.Complete() {
				t.Fatalf("creator refusal custody not settled: %v", err)
			}
		})
	}
}
