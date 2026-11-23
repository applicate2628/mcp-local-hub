package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// These fixtures have at most two distinct, simultaneously open handles. A
// failed close is injected only AFTER closing the owned native handle once.
// A repeated attempt records the defect without passing a consumed number
// back to the OS, where it might already refer to an unrelated resource.
type stateReadNativeFaults struct {
	statErr      error
	closeErrors  []error
	statCalls    int
	closeOrder   []uintptr
	closeCounts  map[uintptr]int
	nativeErrors []error
}

func (f *stateReadNativeFaults) close(handle uintptr, nativeClose func() error) error {
	if f.closeCounts == nil {
		f.closeCounts = make(map[uintptr]int)
	}
	f.closeOrder = append(f.closeOrder, handle)
	f.closeCounts[handle]++
	if f.closeCounts[handle] != 1 {
		return fmt.Errorf("fixture refused a second native close of consumed handle %d", handle)
	}
	if err := nativeClose(); err != nil {
		f.nativeErrors = append(f.nativeErrors, err)
		return err
	}
	index := len(f.closeOrder) - 1
	if index < len(f.closeErrors) {
		return f.closeErrors[index]
	}
	return nil
}

func (f *stateReadNativeFaults) check(t *testing.T, wantCloses, wantStats int) {
	t.Helper()
	if len(f.closeOrder) != wantCloses || f.statCalls != wantStats {
		t.Errorf("native operation attempts: closes=%v stat=%d; want %d closes and %d stat", f.closeOrder, f.statCalls, wantCloses, wantStats)
	}
	for handle, count := range f.closeCounts {
		if count != 1 {
			t.Errorf("numeric ownership %d received %d close attempts; want exactly one", handle, count)
		}
	}
	if len(f.nativeErrors) != 0 {
		t.Errorf("fixture's real native close failed before fault injection: %v", f.nativeErrors)
	}
}

func TestReadStateBeneathRootRetainsPrimaryAndCloseFailures(t *testing.T) {
	for _, test := range []struct {
		name, primary, operation string
		finalFail, parentFail    bool
		category                 StateFileReadErrorCategory
	}{
		{"final-close", "", "close-final", true, false, StateFileReadErrorUnsafeObjectOrIO},
		{"parent-close", "", "close-parent", false, true, StateFileReadErrorUnsafeObjectOrIO},
		{"both-closes", "", "close-final", true, true, StateFileReadErrorUnsafeObjectOrIO},
		{"stat-and-both-closes", "stat", "stat-final", true, true, StateFileReadErrorUnsafeObjectOrIO},
		{"step-and-both-closes", "step", "before-read", true, true, StateFileReadErrorUnsafeObjectOrIO},
		{"cancel-and-both-closes", "cancel", "before-read", true, true, StateFileReadErrorCanceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			payload := bytes.Repeat([]byte("x"), stateReadChunkSize+1)
			if err := os.WriteFile(filepath.Join(root, "file.json"), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			statErr := errors.New("injected final stat failure")
			stepErr := errors.New("injected read-step failure")
			finalErr := errors.New("injected final close failure")
			parentErr := errors.New("injected parent close failure")
			faults := &stateReadNativeFaults{closeErrors: make([]error, 2)}
			var causes []error
			if test.finalFail {
				faults.closeErrors[0] = finalErr
				causes = append(causes, finalErr)
			}
			if test.parentFail {
				faults.closeErrors[1] = parentErr
				causes = append(causes, parentErr)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.primary == "stat" {
				faults.statErr = statErr
				causes = append(causes, statErr)
			}
			if test.primary == "step" {
				causes = append(causes, stepErr)
			}
			if test.primary == "cancel" {
				causes = append(causes, context.Canceled)
			}
			stepReached := false
			step := func(event stateReadBeneathRootStep) error {
				if event.Event == stateReadBeneathRootBeforeRead {
					stepReached = true
					if test.primary == "step" {
						return stepErr
					}
					if test.primary == "cancel" {
						cancel()
					}
				}
				return nil
			}
			result, err := readStateBeneathRootWithNativeFaults(ctx, root, []string{"file.json"}, stateReadBeneathRootPolicy{
				maxBytes: maxStateFileBytes, expectedSHA256: stateReadDigest(payload),
			}, step, faults)
			if result != nil {
				t.Errorf("failed read released %d bytes", len(result))
			}
			requireStateReadCategory(t, err, test.category)
			var primary *StateFileReadError
			if !errors.As(err, &primary) || primary.Operation != test.operation {
				t.Errorf("first typed failure=%+v, want operation %s", primary, test.operation)
			}
			for _, cause := range causes {
				if !errors.Is(err, cause) {
					t.Errorf("returned error lost cause %q: %v", cause, err)
				}
			}
			if (test.primary == "step" || test.primary == "cancel") && !stepReached {
				t.Error("fixture did not reach its primary-failure step")
			}
			faults.check(t, 2, 1)
		})
	}
}

func TestReadStateBeneathRootParentCloseConsumedOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousErr := errors.New("injected previous-parent close failure")
	nextErr := errors.New("injected next-parent close failure")
	faults := &stateReadNativeFaults{closeErrors: []error{previousErr, nextErr}}
	result, err := readStateBeneathRootWithNativeFaults(context.Background(), root, []string{"child", "missing.json"}, stateReadBeneathRootPolicy{
		maxBytes: maxStateFileBytes, expectedSHA256: stateReadDigest(nil),
	}, nil, faults)
	if result != nil || !errors.Is(err, previousErr) || !errors.Is(err, nextErr) {
		t.Errorf("parent handoff result=%v error=%v; want no bytes and both close causes", result, err)
	}
	requireStateReadCategory(t, err, StateFileReadErrorUnsafeObjectOrIO)
	faults.check(t, 2, 0)
}

func TestStateReadBeneathRootAbsenceRequiresOnlyMissing(t *testing.T) {
	_, missing := ReadClientConfigBackupBeneathRootNoFollow(context.Background(), t.TempDir(), []string{"missing.json"}, "")
	if !errors.Is(missing, os.ErrNotExist) {
		t.Fatalf("fixture did not receive a native open-missing result: %v", missing)
	}
	var original *StateFileReadError
	if !errors.As(missing, &original) {
		t.Fatalf("native missing did not retain the reader's typed error: %v", missing)
	}
	closeErr := newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-parent", "", errors.New("close failed"))
	joined := errors.Join(missing, closeErr)
	if !errors.Is(joined, os.ErrNotExist) {
		t.Fatal("joined error must retain its missing cause for diagnostics")
	}
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"original-missing", missing, true},
		{"wrapped-missing-is-not-direct", fmt.Errorf("wrapped: %w", missing), false},
		{"joined-missing-and-close", joined, false},
		{"joined-single-missing-is-not-direct", errors.Join(missing), false},
		{"open-with-compound-cause", newStateFileReadError(original.Category, original.Operation, original.Component, errors.Join(original.Cause, closeErr)), false},
		{"close-caused-not-exist", newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-final", "file.json", os.ErrNotExist), false},
		{"untyped-not-exist", os.ErrNotExist, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := stateReadBeneathRootAbsent(test.err); got != test.want {
				t.Errorf("absence=%v, want %v for %T %v", got, test.want, test.err, test.err)
			}
		})
	}
}

func TestReadStateBeneathRootMissingRetainsParentCloseFailure(t *testing.T) {
	closeErr := errors.New("injected missing-read parent close failure")
	faults := &stateReadNativeFaults{closeErrors: []error{closeErr}}
	result, err := readStateBeneathRootWithNativeFaults(context.Background(), t.TempDir(), []string{"missing.json"}, stateReadBeneathRootPolicy{
		maxBytes: MaxClientConfigBackupBytes, allowEmptyExpectedSHA256: true,
	}, nil, faults)
	if result != nil || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, closeErr) {
		t.Errorf("missing read result=%v error=%v; want no bytes and both causes", result, err)
	}
	if stateReadBeneathRootAbsent(err) {
		t.Error("failed parent close was promoted to a complete absence")
	}
	faults.check(t, 1, 0)
}

func TestCompleteStateReadCloseZeroesBytes(t *testing.T) {
	primary := errors.New("primary failure")
	closeErr := errors.New("close failure")
	for _, test := range []struct {
		name    string
		primary error
	}{
		{"successful-read", nil},
		{"prior-failure", primary},
	} {
		t.Run(test.name, func(t *testing.T) {
			backing := []byte("private test fixture")
			result, err := completeStateReadClose(backing, test.primary, "close-final", "file.json", closeErr)
			if result != nil || !bytes.Equal(backing, make([]byte, len(backing))) {
				t.Error("close failure retained readable result bytes")
			}
			if !errors.Is(err, closeErr) || (test.primary != nil && !errors.Is(err, primary)) {
				t.Errorf("close finalization lost a cause: %v", err)
			}
		})
	}
}
