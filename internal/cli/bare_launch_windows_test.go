//go:build windows

package cli

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mcp-local-hub/internal/gui"
	processowner "mcp-local-hub/internal/process"
)

type bareLaunchProcessFake struct {
	pid          int
	terminateErr error
	releaseErr   error
	terminated   int
	released     int
}

func (p *bareLaunchProcessFake) PID() int { return p.pid }

func (p *bareLaunchProcessFake) TerminateBeforeRelease(context.Context) error {
	p.terminated++
	return p.terminateErr
}

func (p *bareLaunchProcessFake) DetachAtRelease() error {
	p.released++
	return p.releaseErr
}

func TestWindowsBareLaunchActivatesIncumbentWithoutAdmissionOrSpawn(t *testing.T) {
	admitted := 0
	spawned := 0
	deps := windowsBareLaunchDeps{
		pidportPath:  func() (string, error) { return `C:\isolated\gui.pidport`, nil },
		activate:     func(string, time.Duration) error { return nil },
		admitProduct: func(string) error { admitted++; return nil },
		start: func(string, []string, []string, processowner.BreakawayPolicy) (gui.RetainedGUIProcess, error) {
			spawned++
			return nil, nil
		},
	}

	if err := launchWindowsBareGUI(context.Background(), deps); err != nil {
		t.Fatalf("launchWindowsBareGUI: %v", err)
	}
	if admitted != 0 || spawned != 0 {
		t.Fatalf("incumbent path admitted=%d spawned=%d, want 0/0", admitted, spawned)
	}
}

func TestWindowsBareLaunchReachableNoTargetDoesNotSpawnDuplicate(t *testing.T) {
	spawned := 0
	deps := windowsBareLaunchDeps{
		pidportPath: func() (string, error) { return `C:\isolated\gui.pidport`, nil },
		activate: func(string, time.Duration) error {
			return &gui.IncumbentNoActivationTargetError{Port: 19125, Reason: gui.ReasonNoBrowserWindow}
		},
		start: func(string, []string, []string, processowner.BreakawayPolicy) (gui.RetainedGUIProcess, error) {
			spawned++
			return nil, nil
		},
	}

	if err := launchWindowsBareGUI(context.Background(), deps); err != nil {
		t.Fatalf("launchWindowsBareGUI: %v", err)
	}
	if spawned != 0 {
		t.Fatalf("reachable incumbent spawned %d duplicate process(es)", spawned)
	}
}

func TestWindowsBareLaunchStartsSameProductImageAndReturnsAfterReadiness(t *testing.T) {
	launcher := filepath.Join(`C:\isolated`, "mcphub.exe")
	process := &bareLaunchProcessFake{pid: 4242}
	activationCalls := 0
	var admittedProduct, startedPath string
	var startedArgs, startedEnv []string
	var startedPolicy processowner.BreakawayPolicy
	deps := windowsBareLaunchDeps{
		executable:  func() (string, error) { return launcher, nil },
		pidportPath: func() (string, error) { return `C:\isolated\gui.pidport`, nil },
		environment: func() []string { return []string{"ISOLATED=1"} },
		admitProduct: func(productPath string) error {
			admittedProduct = productPath
			return nil
		},
		activate: func(string, time.Duration) error {
			activationCalls++
			if activationCalls == 1 {
				return errors.New("no incumbent")
			}
			return nil
		},
		start: func(path string, args, env []string, policy processowner.BreakawayPolicy) (gui.RetainedGUIProcess, error) {
			startedPath, startedArgs, startedEnv, startedPolicy = path, append([]string(nil), args...), append([]string(nil), env...), policy
			return process, nil
		},
	}

	if err := launchWindowsBareGUI(context.Background(), deps); err != nil {
		t.Fatalf("launchWindowsBareGUI: %v", err)
	}
	if admittedProduct != launcher {
		t.Fatalf("admitted product=%q, want current product image %q", admittedProduct, launcher)
	}
	if startedPath != launcher || !reflect.DeepEqual(startedArgs, []string{"gui"}) || !reflect.DeepEqual(startedEnv, []string{"ISOLATED=1"}) {
		t.Fatalf("start=(%q,%q,%q), want same-product explicit gui with inherited env", startedPath, startedArgs, startedEnv)
	}
	if startedPolicy != processowner.BreakawayRequired {
		t.Fatalf("start policy=%v, want BreakawayRequired", startedPolicy)
	}
	if activationCalls != 2 || process.released != 1 || process.terminated != 0 {
		t.Fatalf("activation=%d release=%d terminate=%d, want 2/1/0", activationCalls, process.released, process.terminated)
	}
}

func TestWindowsBareLaunchFailureSettlesOnlyOwnedChild(t *testing.T) {
	readinessErr := errors.New("readiness refused")
	cleanupErr := errors.New("tree cleanup refused")
	process := &bareLaunchProcessFake{pid: 4243, terminateErr: cleanupErr}
	activationCalls := 0
	deps := windowsBareLaunchDeps{
		executable:   func() (string, error) { return `C:\isolated\mcphub.exe`, nil },
		pidportPath:  func() (string, error) { return `C:\isolated\gui.pidport`, nil },
		environment:  func() []string { return nil },
		admitProduct: func(string) error { return nil },
		activate: func(string, time.Duration) error {
			activationCalls++
			if activationCalls == 1 {
				return errors.New("no incumbent")
			}
			return readinessErr
		},
		start: func(string, []string, []string, processowner.BreakawayPolicy) (gui.RetainedGUIProcess, error) {
			return process, nil
		},
	}

	err := launchWindowsBareGUI(context.Background(), deps)
	if !errors.Is(err, readinessErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("error=%v, want readiness and exact-tree cleanup causes", err)
	}
	var typed interface{ FailureID() string }
	if !errors.As(err, &typed) || typed.FailureID() != WindowsBareLaunchFailureID {
		t.Fatalf("error=%v, want typed %s", err, WindowsBareLaunchFailureID)
	}
	if process.terminated != 1 || process.released != 0 {
		t.Fatalf("terminate=%d release=%d, want 1/0", process.terminated, process.released)
	}
}

func TestWindowsBareLaunchReleaseFailureRetainsThenSettlesOwnedChild(t *testing.T) {
	releaseErr := errors.New("release retained handle")
	process := &bareLaunchProcessFake{pid: 4244, releaseErr: releaseErr}
	activationCalls := 0
	deps := windowsBareLaunchDeps{
		executable:   func() (string, error) { return `C:\isolated\mcphub.exe`, nil },
		pidportPath:  func() (string, error) { return `C:\isolated\gui.pidport`, nil },
		environment:  func() []string { return nil },
		admitProduct: func(string) error { return nil },
		activate: func(string, time.Duration) error {
			activationCalls++
			if activationCalls == 1 {
				return errors.New("no incumbent")
			}
			return nil
		},
		start: func(string, []string, []string, processowner.BreakawayPolicy) (gui.RetainedGUIProcess, error) {
			return process, nil
		},
	}

	err := launchWindowsBareGUI(context.Background(), deps)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("error=%v, want release failure", err)
	}
	if process.released != 1 || process.terminated != 1 {
		t.Fatalf("release=%d terminate=%d, want 1/1 because failed release retains ownership", process.released, process.terminated)
	}
}
