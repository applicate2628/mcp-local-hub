//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

// These immutable build inputs come from the original registry-driven build.
// An ordinary unconfigured build refuses dispatch.
var productRelativePath string
var publicEntrySetBuildID string

func main() { os.Exit(launcherExit()) }

func launcherExit() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	result, err := runLauncher(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	return receiveForeground(result, err, os.Stderr).ExitCode
}

type dispatchEntrySet interface {
	Snapshot() (binaryadmission.WindowsCanonicalEntrySetSnapshotV1, error)
	VerifyUnchanged() error
	VerifyOriginalProductProcessHandle(uintptr) error
	Close() error
}

type dispatchFence interface{ Release() error }

// dispatchAdmission owns the acquired original set and the borrowed upgrade
// fence lease. Close is exactly once, with both first close outcomes preserved.
// It holds no mutex over child execution or native wait.
type dispatchAdmission struct {
	set        dispatchEntrySet
	fence      dispatchFence
	closeOnce  sync.Once
	closeError error
}

func (a *dispatchAdmission) AfterCreateBeforeResume(original *process.AtCreateJobProcess) error {
	if a == nil || a.set == nil || a.fence == nil || original == nil {
		return errors.New("original dispatch admission unavailable")
	}
	// This synchronously borrows the actual creator's process handle while all
	// source/name guards and the same upgrade fence are still held.
	if err := original.BorrowOriginalProcessHandle(a.set.VerifyOriginalProductProcessHandle); err != nil {
		return fmt.Errorf("original Product image admission: %w", err)
	}
	if err := a.set.VerifyUnchanged(); err != nil {
		return fmt.Errorf("original entry set changed before resume: %w", err)
	}
	// The real child may itself perform setup/upgrade and acquire this fence.
	// All failures here forbid ResumeThread; foreground settles its originals.
	return a.Close()
}

func (a *dispatchAdmission) Close() error {
	if a == nil {
		return errors.New("dispatch admission unavailable")
	}
	a.closeOnce.Do(func() {
		var setError, fenceError error
		if a.set != nil {
			setError = a.set.Close()
		}
		if a.fence != nil {
			fenceError = a.fence.Release()
		}
		a.closeError = errors.Join(setError, fenceError)
	})
	return a.closeError
}

type launcherDependencies struct {
	stateDir func() (string, error)
	tryFence func(context.Context, string) (dispatchFence, bool, error)
	acquire  func(context.Context, string) (dispatchEntrySet, error)
	run      func(context.Context, *exec.Cmd, *os.File, *os.File, *os.File, process.ForegroundCreateAdmission) (process.ForegroundResult, error)
}

func runLauncher(ctx context.Context, args []string, stdin, stdout, stderr *os.File) (process.ForegroundResult, error) {
	return runLauncherWithDependencies(ctx, args, stdin, stdout, stderr, productRelativePath, publicEntrySetBuildID, launcherDependencies{
		stateDir: api.DaemonStateDir,
		tryFence: func(ctx context.Context, stateDir string) (dispatchFence, bool, error) {
			lease, acquired, err := api.TryAcquireUpgradeFence(ctx, stateDir)
			if lease == nil {
				return nil, acquired, err
			}
			return lease, acquired, err
		},
		acquire: func(ctx context.Context, buildID string) (dispatchEntrySet, error) {
			set, err := binaryadmission.AcquireDispatchWindowsCanonicalEntrySetV1(ctx, buildID)
			if set == nil {
				return nil, err
			}
			return set, err
		},
		run: process.RunForegroundAdmitted,
	})
}

func runLauncherWithDependencies(ctx context.Context, args []string, stdin, stdout, stderr *os.File, projection, buildID string, deps launcherDependencies) (result process.ForegroundResult, resultErr error) {
	if ctx == nil || stdin == nil || stdout == nil || stderr == nil {
		return result, errors.New("launcher requires context and three borrowed standard files")
	}
	if err := ctx.Err(); err != nil {
		result.Cancelled = true
		return result, err
	}
	relative := filepath.FromSlash(projection)
	if !filepath.IsLocal(relative) || relative == "." || relative != filepath.Clean(relative) || buildID == "" {
		return result, errors.New("missing or invalid original entry-set build inputs")
	}
	stateDir, err := deps.stateDir()
	if err != nil {
		return result, fmt.Errorf("dispatch state directory: %w", err)
	}
	fence, acquired, err := deps.tryFence(ctx, stateDir)
	admission := &dispatchAdmission{fence: fence}
	// Until RunForegroundAdmitted takes receiving ownership, this composition
	// retains every partial acquisition and its exact cached close result.
	transferred := false
	defer func() {
		if !transferred {
			result.Admission = admission
			result.AdmissionCloseAttempted = true
			result.AdmissionCloseError = admission.Close()
			resultErr = errors.Join(resultErr, result.AdmissionCloseError)
		}
	}()
	if err != nil {
		return result, fmt.Errorf("dispatch upgrade fence: %w", err)
	}
	if !acquired || fence == nil {
		return result, errors.New("dispatch upgrade fence busy or unavailable")
	}
	admission.set, err = deps.acquire(ctx, buildID)
	if err != nil {
		return result, fmt.Errorf("original dispatch entry set: %w", err)
	}
	if admission.set == nil {
		return result, errors.New("original dispatch entry set unavailable")
	}
	snapshot, err := admission.set.Snapshot()
	if err != nil {
		return result, err
	}
	// The path is data from the held original owner, never authority by itself.
	// The before-resume hook revalidates the actual created image's native ID.
	projected := filepath.Join(filepath.Dir(snapshot.CanonicalEntry.Path), relative)
	if !filepath.IsAbs(snapshot.Product.Path) || !strings.EqualFold(filepath.Clean(projected), filepath.Clean(snapshot.Product.Path)) {
		return result, errors.New("build Product projection differs from original entry set")
	}
	if err := ctx.Err(); err != nil {
		result.Cancelled = true
		return result, err
	}
	transferred = true
	return deps.run(ctx, exec.Command(snapshot.Product.Path, args...), stdin, stdout, stderr, admission)
}

func foregroundExitCode(result process.ForegroundResult, err error) int {
	if result.Root.Known && result.Root.ExitCode != 0 {
		return result.Root.ExitCode
	}
	if err != nil || result.Cancelled || !result.Root.Known || !result.Settlement.Complete() || result.AdmissionCloseError != nil || len(result.Outcome.Pending) != 0 {
		return 1
	}
	return 0
}
