package api

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
)

// This catches discarding the original creator child on cleanup refusal. The
// API install gate and native starter stay real; only one settlement is refused.
func TestStdioBridgeAdmissionCustodyNativeInstall(t *testing.T) {
	for _, startupFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(startupFailure), func(t *testing.T) {
			a, manifest, plan, port, deps := custodyAdmissionFixture(t)
			refusal := errors.New("owned settlement refused once")
			primary := errors.New("post-create startup failure")
			restore := RefuseNextStdioBridgeSettlementForTest(refusal)
			t.Cleanup(restore)
			previousStart := startStdioBridgeProvisionalFn
			t.Cleanup(func() { startStdioBridgeProvisionalFn = previousStart })
			var command *exec.Cmd
			var original *process.ProvisionalProcess
			startStdioBridgeProvisionalFn = func(cmd *exec.Cmd) (*process.ProvisionalProcess, error) {
				command = cmd
				child, err := process.StartProvisional(cmd)
				original = child
				if child != nil && err == nil && startupFailure {
					return child, primary
				}
				return child, err
			}
			err := a.installFrozenPlanCoreWithManagedDeps(context.Background(), manifest, plan, InstallOpts{Server: manifest.Name, Writer: io.Discard}, CanonicalAtAdmission(), io.Discard, nil, deps)
			if !errors.Is(err, refusal) || startupFailure && !errors.Is(err, primary) {
				t.Fatalf("install failure lost admission/cleanup cause: %v", err)
			}
			a.stdioBridgeAdmissions.mu.Lock()
			entry := a.stdioBridgeAdmissions.entries["mcp-local-hub-custody-probe-default"]
			retained := entry != nil && entry.child == original && original != nil
			a.stdioBridgeAdmissions.mu.Unlock()
			if !retained {
				t.Fatal("install returned without retaining its original provisional child")
			}
			if err := a.CloseStdioBridgeAdmissions(context.Background()); err != nil {
				t.Fatalf("terminal same-child settlement: %v", err)
			}
			if command.ProcessState == nil || command.ProcessState.ExitCode() != 1 || portInUse(port) {
				t.Fatal("native original command did not complete expected termination/port release")
			}
			if err := a.CloseStdioBridgeAdmissions(context.Background()); err != nil {
				t.Fatalf("idempotent close: %v", err)
			}
			t.Log("API_CUSTODY original_child=true real_terminal_settlement=true creator_exit=1 port_released=true")
		})
	}
}

// This catches a descriptor change escaping an unsettled logical task, or a
// terminal close reopening admission. The same real native child is recovered.
func TestStdioBridgeAdmissionCustodyRefusesReentryUntilSettled(t *testing.T) {
	a, manifest, plan, port, deps := custodyAdmissionFixture(t)
	refusal := errors.New("owned settlement remains refused")
	previousSettle := settleStdioBridgeProvisionalFn
	previousStart := startStdioBridgeProvisionalFn
	t.Cleanup(func() {
		settleStdioBridgeProvisionalFn = previousSettle
		startStdioBridgeProvisionalFn = previousStart
	})
	settleStdioBridgeProvisionalFn = func(*process.ProvisionalProcess, time.Duration) error { return refusal }
	starts := 0
	startStdioBridgeProvisionalFn = func(cmd *exec.Cmd) (*process.ProvisionalProcess, error) {
		starts++
		return process.StartProvisional(cmd)
	}
	apply := func() error {
		return a.installFrozenPlanCoreWithManagedDeps(context.Background(), manifest, plan, InstallOpts{Server: manifest.Name, Writer: io.Discard}, CanonicalAtAdmission(), io.Discard, nil, deps)
	}
	if err := apply(); !errors.Is(err, refusal) {
		t.Fatalf("initial refusal: %v", err)
	}
	plan.SupervisorIntent[0].Command = "changed-descriptor-must-not-start"
	if err := apply(); !errors.Is(err, refusal) || starts != 1 {
		t.Fatalf("descriptor drift escaped custody: starts=%d error=%v", starts, err)
	}
	if err := a.CloseStdioBridgeAdmissions(context.Background()); !errors.Is(err, refusal) {
		t.Fatalf("permanent terminal refusal hidden: %v", err)
	}
	if err := apply(); err == nil || starts != 1 {
		t.Fatalf("closed API admitted a new generation: starts=%d error=%v", starts, err)
	}
	settleStdioBridgeProvisionalFn = previousSettle
	if err := a.CloseStdioBridgeAdmissions(context.Background()); err != nil || portInUse(port) {
		t.Fatalf("retained original recovery: %v", err)
	}
}

// This catches Close declaring absence while an already reserved starter has
// not yet returned. A deterministic gate keeps acquisition outside the budget.
func TestStdioBridgeAdmissionCloseRetainsLateAcquisition(t *testing.T) {
	a, manifest, plan, _, deps := custodyAdmissionFixture(t)
	previousStart := startStdioBridgeProvisionalFn
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var command *exec.Cmd
	startStdioBridgeProvisionalFn = func(cmd *exec.Cmd) (*process.ProvisionalProcess, error) {
		close(entered)
		<-release
		command = cmd
		return process.StartProvisional(cmd)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("owned admission invocation did not return")
		}
		startStdioBridgeProvisionalFn = previousStart
		if err := a.CloseStdioBridgeAdmissions(context.Background()); err != nil {
			t.Errorf("late owner cleanup: %v", err)
		}
	})
	go func() {
		done <- a.installFrozenPlanCoreWithManagedDeps(context.Background(), manifest, plan, InstallOpts{Server: manifest.Name, Writer: io.Discard}, CanonicalAtAdmission(), io.Discard, nil, deps)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("starter reservation was not reached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := a.CloseStdioBridgeAdmissions(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("in-flight acquisition was declared settled: %v", err)
	}
	close(release)
	released = true
	select {
	case err := <-done:
		if err == nil || command == nil || command.ProcessState == nil {
			t.Fatalf("late creator was not attached and settled: %v", err)
		}
		done <- err // retain terminal observation for unconditional cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("late creator invocation did not settle")
	}
}

// This catches losing a known admission cause while another caller owns the
// first settlement. The wait timeout and the admission defer must retain it.
func TestStdioBridgeAdmissionCustodyPendingTimeoutPreservesPrimary(t *testing.T) {
	a, manifest, plan, _, deps := custodyAdmissionFixture(t)
	primary := errors.New("original post-create admission cause")
	refusal := errors.New("first owned settlement refusal")
	previousStart, previousSettle := startStdioBridgeProvisionalFn, settleStdioBridgeProvisionalFn
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var first sync.Once
	var releaseOnce sync.Once
	var command *exec.Cmd
	startStdioBridgeProvisionalFn = func(cmd *exec.Cmd) (*process.ProvisionalProcess, error) {
		command = cmd
		child, err := process.StartProvisional(cmd)
		if child != nil && err == nil {
			return child, primary
		}
		return child, err
	}
	settleStdioBridgeProvisionalFn = func(child *process.ProvisionalProcess, timeout time.Duration) error {
		held := false
		first.Do(func() {
			held = true
			close(entered)
		})
		if held {
			<-release
			return refusal
		}
		return child.TerminateAndWait(timeout)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("owned admission did not return after releasing settlement")
		}
		startStdioBridgeProvisionalFn, settleStdioBridgeProvisionalFn = previousStart, previousSettle
		if err := a.CloseStdioBridgeAdmissions(context.Background()); err != nil {
			t.Errorf("original owner terminal cleanup: %v", err)
		}
	})
	go func() {
		done <- a.installFrozenPlanCoreWithManagedDeps(context.Background(), manifest, plan, InstallOpts{Server: manifest.Name, Writer: io.Discard}, CanonicalAtAdmission(), io.Discard, nil, deps)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("held original settlement was not reached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := a.CloseStdioBridgeAdmissions(ctx)
	cancel()
	if !errors.Is(err, primary) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending settlement lost original cause or timeout: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case admissionErr := <-done:
		done <- admissionErr
		if !errors.Is(admissionErr, primary) || !errors.Is(admissionErr, refusal) {
			t.Fatalf("admission defer lost primary or cleanup refusal: %v", admissionErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("admission did not return after first settlement refusal")
	}
	settleStdioBridgeProvisionalFn = previousSettle
	if err := a.CloseStdioBridgeAdmissions(context.Background()); err != nil || command == nil || command.ProcessState == nil {
		t.Fatalf("same original creator did not settle: %v", err)
	}
}

func custodyAdmissionFixture(t *testing.T) (*API, *config.ServerManifest, *Plan, int, managedRegistrationInstallCompositionDeps) {
	t.Helper()
	previousAdmission := frozenStdioBridgeAdmissionFn
	t.Cleanup(func() { frozenStdioBridgeAdmissionFn = previousAdmission })
	frozenStdioBridgeAdmissionFn = nil
	port := reserveAdmissionPort(t)
	t.Setenv(stdioBridgeAdmissionHelperEnv, "ready")
	t.Setenv(stdioBridgeAdmissionHelperPortEnv, strconv.Itoa(port))
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))
	a := NewAPI()
	t.Cleanup(func() {
		if err := a.CloseStdioBridgeAdmissions(context.Background()); err != nil {
			t.Errorf("fixture retained unsettled custody: %v", err)
		}
	})
	m := &config.ServerManifest{Name: "custody-probe", Kind: config.KindGlobal, Transport: config.TransportStdioBridge, Daemons: []config.DaemonSpec{{Name: "default", Port: port}}}
	plan := &Plan{SupervisorIntent: []SupervisorIntentEntry{{Name: "mcp-local-hub-custody-probe-default", Command: os.Args[0], Args: []string{"-test.run=^TestFrozenStdioBridgeAdmissionHelper$"}, WorkingDir: t.TempDir(), StartupBindDeadlineSeconds: 2}}}
	deps := managedRegistrationInstallCompositionDeps{
		Materialize: func(context.Context, *API, *config.ServerManifest, *Plan, InstallOpts, ClientRoutingAuthorityRequest, io.Writer) error {
			err := errors.New("custody admission reached install materialization")
			t.Error(err)
			return err
		},
	}
	return a, m, plan, port, deps
}
