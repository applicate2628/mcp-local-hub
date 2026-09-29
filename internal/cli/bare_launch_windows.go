//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/gui"
	processowner "mcp-local-hub/internal/process"
)

const (
	WindowsBareLaunchFailureID = "E_WINDOWS_BARE_GUI_LAUNCH"
	bareIncumbentProbeTimeout  = 500 * time.Millisecond
	bareLaunchReadinessTimeout = 15 * time.Second
	bareLaunchCleanupTimeout   = 10 * time.Second
)

type WindowsBareLaunchError struct {
	Stage string
	Cause error
}

func (e *WindowsBareLaunchError) Error() string {
	return fmt.Sprintf("%s: %s: %v", WindowsBareLaunchFailureID, e.Stage, e.Cause)
}

func (e *WindowsBareLaunchError) Unwrap() error     { return e.Cause }
func (e *WindowsBareLaunchError) FailureID() string { return WindowsBareLaunchFailureID }

type windowsBareLaunchDeps struct {
	executable   func() (string, error)
	pidportPath  func() (string, error)
	environment  func() []string
	admitProduct func(string) error
	activate     func(string, time.Duration) error
	start        func(string, []string, []string, processowner.BreakawayPolicy) (gui.RetainedGUIProcess, error)
}

func defaultWindowsBareLaunchDeps() windowsBareLaunchDeps {
	return windowsBareLaunchDeps{
		executable:  os.Executable,
		pidportPath: gui.PidportPathNoCreate,
		environment: os.Environ,
		admitProduct: func(productPath string) error {
			_, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{
				Path: productPath,
				Role: binaryadmission.WindowsArtifactRoleProduct,
			})
			return err
		},
		activate: gui.TryActivateIncumbent,
		start:    gui.StartDetachedGUI,
	}
}

// LaunchBareGUI owns the Windows-only bare invocation handoff. A non-Windows
// sibling returns handled=false so the existing inline Cobra route is preserved.
func LaunchBareGUI() (bool, error) {
	return true, launchWindowsBareGUI(context.Background(), defaultWindowsBareLaunchDeps())
}

func launchWindowsBareGUI(ctx context.Context, deps windowsBareLaunchDeps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if deps.pidportPath == nil || deps.activate == nil {
		return bareLaunchFailure("composition", errors.New("bare activation dependencies are incomplete"))
	}
	if err := ctx.Err(); err != nil {
		return bareLaunchFailure("canceled", err)
	}

	pidportPath, err := deps.pidportPath()
	if err != nil {
		return bareLaunchFailure("resolve GUI owner", err)
	}
	if activationErr := deps.activate(pidportPath, bareIncumbentProbeTimeout); activationHandled(activationErr) {
		return nil
	}
	if deps.executable == nil || deps.environment == nil || deps.admitProduct == nil || deps.start == nil {
		return bareLaunchFailure("composition", errors.New("bare launch dependencies are incomplete"))
	}

	productPath, err := deps.executable()
	if err != nil {
		return bareLaunchFailure("resolve current product", err)
	}
	productPath, err = filepath.Abs(productPath)
	if err != nil {
		return bareLaunchFailure("resolve current product absolute path", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(productPath); resolveErr == nil {
		productPath = resolved
	}
	if err := deps.admitProduct(productPath); err != nil {
		return bareLaunchFailure("admit current Windows product", err)
	}
	if err := ctx.Err(); err != nil {
		return bareLaunchFailure("canceled", err)
	}

	child, err := deps.start(productPath, []string{"gui"}, deps.environment(), processowner.BreakawayRequired)
	if err != nil {
		return bareLaunchFailure("start detached product GUI", err)
	}
	if child == nil || child.PID() <= 0 {
		cleanupErr := settleBareLaunchChild(child)
		return bareLaunchFailure("start detached product GUI", errors.Join(errors.New("start returned no retained process"), cleanupErr))
	}

	activationErr := deps.activate(pidportPath, bareLaunchReadinessTimeout)
	if !activationHandled(activationErr) {
		cleanupErr := settleBareLaunchChild(child)
		return bareLaunchFailure("verify GUI ownership and activation", errors.Join(activationErr, cleanupErr))
	}
	if err := child.DetachAtRelease(); err != nil {
		cleanupErr := settleBareLaunchChild(child)
		return bareLaunchFailure("release ready GUI process handle", errors.Join(err, cleanupErr))
	}
	return nil
}

func activationHandled(err error) bool {
	return err == nil || errors.Is(err, gui.ErrIncumbentNoActivationTarget)
}

func settleBareLaunchChild(child gui.RetainedGUIProcess) error {
	if child == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), bareLaunchCleanupTimeout)
	defer cancel()
	return child.TerminateBeforeRelease(ctx)
}

func bareLaunchFailure(stage string, cause error) error {
	if cause == nil {
		cause = errors.New("unknown bare launch failure")
	}
	return &WindowsBareLaunchError{Stage: stage, Cause: cause}
}
