//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

type launcherTestSet struct {
	snapshot                  binaryadmission.WindowsCanonicalEntrySetSnapshotV1
	snapshotError, closeError error
	closes                    int
	order                     *[]string
}

func (s *launcherTestSet) Snapshot() (binaryadmission.WindowsCanonicalEntrySetSnapshotV1, error) {
	return s.snapshot, s.snapshotError
}
func (*launcherTestSet) VerifyUnchanged() error { return nil }
func (*launcherTestSet) VerifyOriginalProductProcessHandle(uintptr) error {
	return errors.New("no synthetic handle is native authority")
}
func (s *launcherTestSet) Close() error {
	s.closes++
	if s.order != nil {
		*s.order = append(*s.order, "set-close")
	}
	return s.closeError
}

type launcherTestFence struct {
	releases int
	err      error
	order    *[]string
}

func (f *launcherTestFence) Release() error {
	f.releases++
	if f.order != nil {
		*f.order = append(*f.order, "fence-release")
	}
	return f.err
}

func TestLauncherAcquisitionFailureClosesOriginalSetThenFenceOnce(t *testing.T) {
	for _, phase := range []string{"busy", "fence-error", "partial-set", "snapshot", "projection"} {
		t.Run(phase, func(t *testing.T) {
			failure, setFailure, fenceFailure := errors.New("acquisition cause"), errors.New("set close cause"), errors.New("fence release cause")
			var order []string
			set := &launcherTestSet{closeError: setFailure, order: &order}
			fence := &launcherTestFence{err: fenceFailure, order: &order}
			acquires, runs := 0, 0
			deps := launcherDependencies{
				stateDir: func() (string, error) { return "synthetic-state", nil },
				tryFence: func(context.Context, string) (dispatchFence, bool, error) {
					if phase == "busy" {
						return nil, false, nil
					}
					if phase == "fence-error" {
						return fence, false, failure
					}
					return fence, true, nil
				},
				acquire: func(context.Context, string) (dispatchEntrySet, error) {
					acquires++
					if phase == "partial-set" {
						return set, failure
					}
					if phase == "snapshot" {
						set.snapshotError = failure
					}
					return set, nil
				},
				run: func(context.Context, *exec.Cmd, *os.File, *os.File, *os.File, process.ForegroundCreateAdmission) (process.ForegroundResult, error) {
					runs++
					return process.ForegroundResult{}, nil
				},
			}
			result, err := runLauncherWithDependencies(context.Background(), nil, os.Stdin, os.Stdout, os.Stderr, "mcphub-product/mcphub.exe", "00112233445566778899aabbccddeeff", deps)
			if err == nil || runs != 0 || foregroundExitCode(result, err) != 1 || !result.AdmissionCloseAttempted {
				t.Fatal("failed acquisition became dispatch/success")
			}
			if phase == "busy" {
				if acquires != 0 || fence.releases != 0 || set.closes != 0 {
					t.Fatal("busy fence touched image set")
				}
				return
			}
			if !errors.Is(err, fenceFailure) || fence.releases != 1 {
				t.Fatal("fence release failure lost")
			}
			if phase == "fence-error" {
				if acquires != 0 || !errors.Is(err, failure) {
					t.Fatal("partial fence facts lost")
				}
				return
			}
			if !errors.Is(err, setFailure) || set.closes != 1 || !reflect.DeepEqual(order, []string{"set-close", "fence-release"}) {
				t.Fatalf("source/fence release order lost: %v %v", order, err)
			}
			if err2 := result.Admission.Close(); !errors.Is(err2, setFailure) || set.closes != 1 || fence.releases != 1 {
				t.Fatal("cached admission close retried originals")
			}
		})
	}
}

func TestLauncherUsesHeldProductAndBorrowedStandards(t *testing.T) {
	root := t.TempDir()
	set := &launcherTestSet{}
	set.snapshot.CanonicalEntry.Path = filepath.Join(root, "mcphub.exe")
	set.snapshot.Product.Path = filepath.Join(root, "mcphub-product", "mcphub.exe")
	fence := &launcherTestFence{}
	args := []string{"manifest", "create", "../synthetic-probe"}
	runs := 0
	deps := launcherDependencies{
		stateDir: func() (string, error) { return "synthetic-state", nil },
		tryFence: func(context.Context, string) (dispatchFence, bool, error) { return fence, true, nil },
		acquire: func(context.Context, string) (dispatchEntrySet, error) {
			if fence.releases != 0 {
				t.Fatal("fence released before image acquisition")
			}
			return set, nil
		},
		run: func(ctx context.Context, cmd *exec.Cmd, in, out, errout *os.File, admission process.ForegroundCreateAdmission) (process.ForegroundResult, error) {
			runs++
			if cmd.Path != set.snapshot.Product.Path || !reflect.DeepEqual(cmd.Args[1:], args) || in != os.Stdin || out != os.Stdout || errout != os.Stderr || set.closes != 0 || fence.releases != 0 {
				t.Fatal("original held selection/arguments/standards lost")
			}
			// This is a composition test only, not a native creation receipt.
			err := admission.Close()
			return process.ForegroundResult{Root: process.OriginalRootWait{Known: true, ExitCode: 37}, Admission: admission, AdmissionCloseAttempted: true, AdmissionCloseError: err}, err
		},
	}
	result, err := runLauncherWithDependencies(context.Background(), args, os.Stdin, os.Stdout, os.Stderr, "mcphub-product/mcphub.exe", "00112233445566778899aabbccddeeff", deps)
	if err != nil || runs != 1 || set.closes != 1 || fence.releases != 1 || foregroundExitCode(result, err) != 37 || result.Settlement.Complete() {
		t.Fatal("composition outcome/ownership changed or synthetic native settlement invented")
	}
}

func TestLauncherMissingBuildAndCancellationAcquireNothing(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		ctx               context.Context
		projection, build string
	}{{context.Background(), "", ""}, {context.Background(), "../mcphub.exe", "id"}, {cancelled, "mcphub-product/mcphub.exe", "id"}} {
		deps := launcherDependencies{stateDir: func() (string, error) { t.Fatal("invalid invocation acquired state"); return "", nil }}
		result, err := runLauncherWithDependencies(tc.ctx, nil, os.Stdin, os.Stdout, os.Stderr, tc.projection, tc.build, deps)
		if err == nil || result.Job != nil || result.Original != nil || foregroundExitCode(result, err) != 1 {
			t.Fatal("invalid invocation became success")
		}
	}
}
