package sonnet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestOutputSettlementDoesNotHoldServerMutex(t *testing.T) {
	root := t.TempDir()
	s := NewServer(testConfig(root, &fakeDriver{}))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	s.beforeOutput = func() { close(entered); <-release }
	defer func() { unblock(); _ = s.Close(context.Background()) }()
	started, err := s.Start(context.Background(), StartRequest{
		ProjectRoot: writeProject(t), ProjectFile: "sample.son", TimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter output settlement")
	}
	statusDone := make(chan error, 1)
	go func() {
		status, err := s.Status(started.RunID)
		if err == nil && terminal(status.State) {
			err = errors.New("terminal state exposed before output settlement")
		}
		statusDone <- err
	}()
	select {
	case err := <-statusDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Status blocked behind output settlement")
	}
	cancelDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := s.Cancel(ctx, started.RunID, "during-output", true)
		cancelDone <- err
	}()
	select {
	case err := <-cancelDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Cancel = %v, want bounded wait", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel could not reach its context-aware wait")
	}
	status, err := s.Status(started.RunID)
	if err != nil || status.State != StateRunning || status.Phase != "force_cancel_requested" {
		t.Fatalf("cancellation request during output status=%+v err=%v", status, err)
	}
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		closeDone <- s.Close(ctx)
	}()
	select {
	case err := <-closeDone:
		if !errors.Is(err, ErrShutdownIncomplete) {
			t.Fatalf("Close = %v, want incomplete shutdown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind output settlement")
	}
	next := NewServer(testConfig(root, &fakeDriver{}))
	if !errors.Is(next.initErr, ErrRunStoreOwned) {
		_ = next.Close(context.Background())
		t.Fatalf("store lease released before worker settled: %v", next.initErr)
	}
	unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(started.RunID)
	if err != nil || status.State != StateCompleted || status.ExitCode == nil || *status.ExitCode != 0 || !status.Reportable || status.TimedOut || status.Cancellation != "" {
		t.Fatalf("settled status=%+v err=%v", status, err)
	}
}

type outputDeadlineDriver struct {
	fixedStrictDriverOutcome
	context chan context.Context
}

func (d outputDeadlineDriver) Run(ctx context.Context, em, project string) (DriverResult, error) {
	d.context <- ctx
	return d.fixedStrictDriverOutcome.Run(ctx, em, project)
}

func TestOutputSettlementDeadlinePreservesExecutionOutcome(t *testing.T) {
	exitThree := 3
	for _, tc := range []struct {
		name       string
		outcome    fixedStrictDriverOutcome
		state      State
		exitCode   int
		failureID  string
		reportable bool
	}{
		{name: "completed", state: StateCompleted, reportable: true},
		{name: "nonzero", outcome: fixedStrictDriverOutcome{result: DriverResult{ExitCode: 3}}, state: StateCompletedNonzero, exitCode: 3, failureID: "SONNET_EM_EXIT_NONZERO"},
		{name: "cleanup_failed", outcome: fixedStrictDriverOutcome{err: &strictDriverFailure{FailureID: "SONNET_DRIVER_CLEANUP_FAILED", ExitCode: &exitThree, Cause: errors.New("injected cleanup failure")}}, state: StateFailed, exitCode: 3, failureID: "SONNET_DRIVER_CLEANUP_FAILED"},
	} {
		runCase := func(t *testing.T) {
			storeRoot := t.TempDir()
			tc.outcome.result.Stdout = []byte("acquired stdout")
			tc.outcome.result.Stderr = []byte("acquired stderr")
			driver := outputDeadlineDriver{fixedStrictDriverOutcome: tc.outcome, context: make(chan context.Context, 1)}
			s := NewServer(testConfig(storeRoot, driver))
			defer s.Close(context.Background())
			var beforeOutputContextErr, outputContextErr error
			s.beforeOutput = func() {
				ctx := <-driver.context
				beforeOutputContextErr = ctx.Err()
				<-ctx.Done()
				outputContextErr = ctx.Err()
			}
			started, err := s.Start(context.Background(), StartRequest{
				ProjectRoot: writeProject(t), ProjectFile: "sample.son", TimeoutSeconds: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			done := s.runs[started.RunID].done
			s.mu.Unlock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not settle after its deadline")
			}
			if beforeOutputContextErr != nil {
				t.Fatalf("execution context expired before output settlement: %v", beforeOutputContextErr)
			}
			if !errors.Is(outputContextErr, context.DeadlineExceeded) {
				t.Fatalf("output context error=%v, want exact worker deadline", outputContextErr)
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			restarted := NewServer(testConfig(storeRoot, &fakeDriver{}))
			defer restarted.Close(context.Background())
			for _, owner := range []*Server{s, restarted} {
				status, err := owner.Status(started.RunID)
				if err != nil || status.State != tc.state || status.ExitCode == nil || *status.ExitCode != tc.exitCode || status.FailureID != tc.failureID || status.Reportable != tc.reportable || status.TimedOut || status.Cancellation != "" || status.Phase != "settled" {
					t.Errorf("status=%+v err=%v; want %s exit=%d failure=%q reportable=%v", status, err, tc.state, tc.exitCode, tc.failureID, tc.reportable)
				}
				owner.mu.Lock()
				r := owner.runs[started.RunID]
				stdout, stderr, stdoutBytes, stderrBytes := r.Stdout, r.Stderr, r.StdoutBytes, r.StderrBytes
				owner.mu.Unlock()
				if stdout != string(tc.outcome.result.Stdout) || stderr != string(tc.outcome.result.Stderr) || stdoutBytes != len(tc.outcome.result.Stdout) || stderrBytes != len(tc.outcome.result.Stderr) {
					t.Errorf("acquired streams=(%q/%d, %q/%d)", stdout, stdoutBytes, stderr, stderrBytes)
				}
			}
		}
		// Advance the worker deadline only after output settlement blocks.
		// Admission and durable writes must not consume this test's execution budget.
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, runCase)
		})
	}
}
