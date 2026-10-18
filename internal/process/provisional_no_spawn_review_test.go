package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type creatorClosureUnitChild struct {
	metadata                                  creatorExitObservation
	binding                                   creatorClosureBinding
	bindingErr                                error
	waitResult                                containedWaitResult
	waitFn                                    func() containedWaitResult
	observeFn                                 func(context.Context) error
	closeFn                                   func() error
	waits, observations, closes, terminations atomic.Int32
}

type creatorTerminalUnitChild struct {
	*creatorClosureUnitChild
	terminalFn func(context.Context) (creatorTerminalObservation, error)
	actions    atomic.Int32
}

func (c *creatorTerminalUnitChild) completeCreatorMembers(ctx context.Context) (creatorTerminalObservation, error) {
	c.actions.Add(1)
	return c.terminalFn(ctx)
}

func TestCreatorRootSoleWaitClaimDoesNotHoldLifetime(t *testing.T) {
	p, child := creatorClosureUnitFixture(creatorExitObservation{known: true})
	entered, release := make(chan struct{}), make(chan struct{})
	child.waitFn = func() containedWaitResult { close(entered); <-release; return containedWaitResult{exited: true} }
	rootDone := make(chan error, 1)
	go func() {
		root, err := p.WaitCreatorRoot()
		if err == nil && !root.Known {
			err = errors.New("root exit not known")
		}
		rootDone <- err
	}()
	<-entered
	claimReturned := make(chan struct{})
	go func() { p.wait(); close(claimReturned) }()
	select {
	case <-claimReturned:
	case <-time.After(time.Second):
		close(release)
		<-rootDone
		t.Fatal("wait-start claim held over the original lifetime")
	}
	ctx, cancel := context.WithCancel(context.Background())
	finite, finiteCancel := context.WithTimeout(ctx, time.Second)
	cancel()
	proof, err := p.WaitCreatorClosure(finite)
	finiteCancel()
	if proof != nil || !errors.Is(err, context.Canceled) {
		close(release)
		<-rootDone
		t.Fatalf("finite observer did not retain cancellation: proof=%v error=%v", proof, err)
	}
	close(release)
	if err := <-rootDone; err != nil {
		t.Fatal(err)
	}
	if child.waits.Load() != 1 || child.closes.Load() != 0 || child.terminations.Load() != 0 {
		t.Fatal("root wait duplicated wait or mutated resource lifetime")
	}
}

// These owner fakes prove sequencing and omission safety only. They never
// establish original Windows creation, native action or physical closure.
func TestCreatorTerminalPositivePolicySeparateCache(t *testing.T) {
	for _, name := range []string{"apply-omitted", "root-pending", "nil-context", "no-deadline", "expired", "action-failure", "close-failure", "success"} {
		t.Run(name, func(t *testing.T) {
			p, original := creatorClosureUnitFixture(creatorExitObservation{known: true, code: 0})
			cause := errors.New("synthetic native action failure")
			child := &creatorTerminalUnitChild{creatorClosureUnitChild: original, terminalFn: func(context.Context) (creatorTerminalObservation, error) {
				if name == "action-failure" {
					return creatorTerminalObservation{action: creatorTerminalRequestFailed, actionErr: cause}, cause
				}
				return creatorTerminalObservation{action: creatorTerminalRequestSucceeded, membersExited: true}, nil
			}}
			p.owner = child
			if name != "root-pending" {
				if _, err := p.WaitCreatorRoot(); err != nil {
					t.Fatal(err)
				}
			}
			if name == "close-failure" {
				original.closeFn = func() error { return cause }
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var supplied context.Context = ctx
			apply := name != "apply-omitted"
			switch name {
			case "nil-context":
				supplied = nil
			case "no-deadline":
				supplied = context.Background()
			case "expired":
				cancel()
			}
			result, err := p.CompleteCreatorAfterExit(supplied, apply)
			switch name {
			case "success":
				if err != nil || result.Closure() == nil || !result.Snapshot().HandlesClosed {
					t.Fatalf("owner sequence failed: result=%v error=%v", result, err)
				}
				if proof, err := p.WaitCreatorClosure(ctx); proof != nil || err == nil {
					t.Fatal("terminal result populated the natural cache")
				}
				if original.observations.Load() != 0 {
					t.Fatal("natural observer called after terminal policy")
				}
			case "action-failure", "close-failure":
				if result == nil || result.Closure() != nil || !errors.Is(err, cause) {
					t.Fatalf("terminal failure lost cause or exposed proof: %v", err)
				}
				if again, err := p.CompleteCreatorAfterExit(ctx, true); again != result || !errors.Is(err, cause) {
					t.Fatal("remembered terminal failure changed")
				}
				if child.actions.Load() != 1 {
					t.Fatal("terminal policy repeated native request")
				}
				if name == "action-failure" && original.closes.Load() != 0 {
					t.Fatal("failed action released original custody")
				}
			default:
				if result != nil || err == nil || child.actions.Load() != 0 || original.closes.Load() != 0 {
					t.Fatalf("refusal mutated original owner: result=%v error=%v", result, err)
				}
			}
		})
	}
}

func TestCreatorStartupCachedCloseCannotAuthorizeRetry(t *testing.T) {
	p, child := creatorClosureUnitFixture(creatorExitObservation{known: true})
	p.closeAttempted = true
	cause := errors.New("synthetic startup failure")
	err := p.closeStartupFailure("pre-create", cause)
	projection, ok := err.(interface {
		Snapshot() CreatorTerminalSnapshot
	})
	if !ok || !errors.Is(err, cause) {
		t.Fatal("startup failure lost owning outer projection/cause")
	}
	view := projection.Snapshot()
	if view.StartupReleaseKnown || view.StartupResourcesReleased || view.StartupReleaseError == nil || child.closes.Load() != 0 {
		t.Fatal("cached nil close became positive startup release")
	}
	observed, err := p.terminateAndWaitObserved(time.Second)
	if observed || err != nil || child.waits.Load() != 0 || child.terminations.Load() != 0 {
		t.Fatal("legacy cached cleanup became a newly observed resource result")
	}
	if err := p.TerminateAndWait(time.Second); err != nil {
		t.Fatal("legacy cached return contract changed")
	}
}

type creatorClosureAdmissionCancelContext struct {
	context.Context
	calls  int
	cancel context.CancelFunc
}

func (c *creatorClosureAdmissionCancelContext) Err() error {
	c.calls++
	if c.calls == 3 {
		c.cancel()
	}
	return c.Context.Err()
}
func (c *creatorClosureUnitChild) start(*exec.Cmd, containedInputFile, containedWriteFile, containedWriteFile) error {
	return nil
}
func (c *creatorClosureUnitChild) wait() containedWaitResult {
	c.waits.Add(1)
	if c.waitFn != nil {
		return c.waitFn()
	}
	return c.waitResult
}
func (c *creatorClosureUnitChild) terminate(uint32) error { c.terminations.Add(1); return nil }
func (c *creatorClosureUnitChild) close() error {
	c.closes.Add(1)
	if c.closeFn != nil {
		return c.closeFn()
	}
	return nil
}
func (c *creatorClosureUnitChild) creatorBinding() (creatorClosureBinding, error) {
	return c.binding, c.bindingErr
}
func (c *creatorClosureUnitChild) creatorExitObservation() creatorExitObservation { return c.metadata }
func (c *creatorClosureUnitChild) observeCreatorMembers(ctx context.Context) error {
	c.observations.Add(1)
	if c.observeFn != nil {
		return c.observeFn(ctx)
	}
	return nil
}

func creatorClosureUnitFixture(metadata creatorExitObservation) (*ProvisionalProcess, *creatorClosureUnitChild) {
	c := &creatorClosureUnitChild{metadata: metadata, binding: creatorClosureBinding{pid: 4242, startedAt: "2026-10-07T09:00:00Z"}, waitResult: containedWaitResult{exited: true}}
	return &ProvisionalProcess{cmd: &exec.Cmd{Path: "synthetic-owner"}, job: &Job{}, owner: c, waitDone: make(chan struct{})}, c
}

// Windows calls this helper once with its one actual auxiliary exit observation;
// these fakes prove sequencing only, never original kernel custody/closure.
func runCreatorClosureSourceUnit(t *testing.T, nonzero creatorExitObservation) {
	zero := creatorExitObservation{known: true, code: 0}
	for _, name := range []string{"nil-context", "no-deadline", "expired", "nil-process", "missing-cmd", "missing-job", "missing-owner", "missing-wait-channel", "unsupported", "bad-binding", "zero-pid", "empty-started"} {
		t.Run(name, func(t *testing.T) {
			p, c := creatorClosureUnitFixture(zero)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var supplied context.Context = ctx
			switch name {
			case "nil-context":
				supplied = nil
			case "no-deadline":
				supplied = context.Background()
			case "expired":
				cancel()
			case "nil-process":
				p = nil
			case "missing-cmd":
				p.cmd = nil
			case "missing-job":
				p.job = nil
			case "missing-owner":
				p.owner = nil
			case "missing-wait-channel":
				p.waitDone = nil
			case "unsupported":
				p.owner = &creatorClosureUnsupportedUnitChild{creatorClosureUnitChild: c}
			case "bad-binding":
				c.bindingErr = errors.New("synthetic creator custody failure")
			case "zero-pid":
				c.binding.pid = 0
			case "empty-started":
				c.binding.startedAt = ""
			}
			proof, err := p.WaitCreatorClosure(supplied)
			if proof != nil || err == nil || c.waits.Load() != 0 || c.observations.Load() != 0 || c.closes.Load() != 0 || c.terminations.Load() != 0 {
				t.Fatalf("invalid admission performed activity: proof=%v err=%v", proof, err)
			}
		})
	}
	for _, name := range []string{"zero-closed", "nonzero-closed", "os-wait-error", "missing-observation", "unproved-exit", "member-error", "close-error", "cancel-observation", "cancel-close-success", "cancel-close-error", "legacy-first", "new-first"} {
		t.Run(name, func(t *testing.T) {
			metadata := zero
			if name == "nonzero-closed" {
				metadata = nonzero
			}
			p, c := creatorClosureUnitFixture(metadata)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cause := errors.New("synthetic distinct OS/observation/close failure")
			switch name {
			case "os-wait-error":
				c.waitResult.err = cause
			case "missing-observation":
				c.metadata.known = false
			case "unproved-exit":
				c.waitResult.exited = false
			case "member-error":
				c.observeFn = func(context.Context) error { return cause }
			case "close-error":
				c.closeFn = func() error { return cause }
			case "cancel-observation":
				c.observeFn = func(ctx context.Context) error { cancel(); return ctx.Err() }
			case "cancel-close-success":
				c.closeFn = func() error { cancel(); return nil }
			case "cancel-close-error":
				c.closeFn = func() error { cancel(); return cause }
			case "legacy-first":
				if err := p.TerminateAndWait(time.Second); err != nil {
					t.Fatal(err)
				}
			}
			proof, err := p.WaitCreatorClosure(ctx)
			success := name == "zero-closed" || name == "nonzero-closed" || name == "new-first"
			if success {
				if proof == nil || err != nil {
					t.Fatalf("confirmed closure: %v", err)
				}
				s := proof.Snapshot()
				if !s.DirectWaitComplete || !s.CurrentMembersExited || !s.HandlesClosed || !s.OriginalExitCodeKnown || s.OriginalExitCode != metadata.code || s.OriginalPID != c.binding.pid || s.OriginalStartedAt != c.binding.startedAt || len(s.EvidenceSHA256) != 64 {
					t.Fatal("closure observation incomplete")
				}
				if name == "nonzero-closed" {
					if s.RawExitError == nil || s.RawExitError.ProcessState != nonzero.raw.ProcessState || s.RawExitError.ProcessState.ExitCode() != 17 || s.RawExitError.Error() != nonzero.raw.Error() {
						t.Fatal("actual raw nonzero exit was replaced")
					}
					s.RawExitError.Stderr = append(s.RawExitError.Stderr, []byte("caller poison")...)
					s.RawExitError.ProcessState = nil
					fresh := proof.Snapshot()
					if fresh.RawExitError.ProcessState != nonzero.raw.ProcessState || string(fresh.RawExitError.Stderr) != string(nonzero.raw.Stderr) {
						t.Fatal("snapshot aliases cached exit observation")
					}
				}
				s.OriginalExitCode = 99
				if again, err := p.WaitCreatorClosure(ctx); err != nil || again != proof || again.Snapshot().OriginalExitCode != metadata.code {
					t.Fatal("repeat mutated/waited/closed cached result")
				}
				if name == "new-first" {
					if err := p.TerminateAndWait(time.Second); err != nil {
						t.Fatal(err)
					}
				}
				if c.waits.Load() != 1 || c.observations.Load() != 1 || c.closes.Load() != 1 || c.terminations.Load() != 0 {
					t.Fatal("new-first duplicate lifecycle activity")
				}
			} else {
				if proof != nil || err == nil {
					t.Fatal("failure produced closure proof")
				}
				if err.Error() == "" {
					t.Fatal("failure lost its visible diagnostic")
				}
				if name == "os-wait-error" || name == "member-error" || name == "close-error" || name == "cancel-close-error" {
					if !errors.Is(err, cause) {
						t.Fatal("original distinct cause lost")
					}
				}
				if strings.HasPrefix(name, "cancel-") {
					if !errors.Is(err, context.Canceled) {
						t.Fatal("cancellation cause lost")
					}
				}
				if name == "close-error" || name == "cancel-close-error" || name == "cancel-close-success" {
					fresh, freshCancel := context.WithTimeout(context.Background(), time.Second)
					defer freshCancel()
					again, nextErr := p.WaitCreatorClosure(fresh)
					if name == "cancel-close-success" {
						if again == nil || nextErr != nil {
							t.Fatal("cancelled completed closure did not preserve true cache")
						}
					} else {
						if again != nil || !errors.Is(nextErr, cause) {
							t.Fatal("remembered close failure erased")
						}
					}
					if c.waits.Load() != 1 || c.observations.Load() != 1 || c.closes.Load() != 1 {
						t.Fatal("repeat performed another wait/census/close")
					}
				}
				if name == "cancel-observation" {
					if c.closes.Load() != 0 || c.terminations.Load() != 0 {
						t.Fatal("cancelled observation released custody")
					}
					c.observeFn = nil
					fresh, freshCancel := context.WithTimeout(context.Background(), time.Second)
					defer freshCancel()
					if again, nextErr := p.WaitCreatorClosure(fresh); again == nil || nextErr != nil || c.waits.Load() != 1 {
						t.Fatal("same owner could not resume observation")
					}
				}
			}
		})
	}
	t.Run("legacy-admission-deadline", func(t *testing.T) {
		p, c := creatorClosureUnitFixture(zero)
		entered, release, legacyDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		c.waitFn = func() containedWaitResult { close(entered); <-release; return containedWaitResult{exited: true} }
		go func() { legacyDone <- p.TerminateAndWait(time.Second) }()
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		proof, err := p.WaitCreatorClosure(ctx)
		if proof != nil || !errors.Is(err, context.DeadlineExceeded) || c.waits.Load() != 1 || c.observations.Load() != 0 || c.closes.Load() != 0 || c.terminations.Load() != 1 {
			close(release)
			<-legacyDone
			t.Fatal("admission did not respect finite deadline or changed legacy custody")
		}
		close(release)
		if err := <-legacyDone; err != nil {
			t.Fatal(err)
		}
		fresh, freshCancel := context.WithTimeout(context.Background(), time.Second)
		defer freshCancel()
		if proof, err := p.WaitCreatorClosure(fresh); proof != nil || err == nil || c.waits.Load() != 1 || c.closes.Load() != 1 || c.terminations.Load() != 1 {
			t.Fatal("late lock waiter or inferred legacy proof")
		}
	})
	t.Run("concurrent-new-calls", func(t *testing.T) {
		p, c := creatorClosureUnitFixture(zero)
		entered, release := make(chan struct{}), make(chan struct{})
		c.observeFn = func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan *CreatorSettlement, 2)
		errs := make(chan error, 2)
		go func() { s, e := p.WaitCreatorClosure(ctx); done <- s; errs <- e }()
		<-entered
		go func() { s, e := p.WaitCreatorClosure(ctx); done <- s; errs <- e }()
		close(release)
		first, second := <-done, <-done
		if <-errs != nil || <-errs != nil || first == nil || second != first || c.waits.Load() != 1 || c.observations.Load() != 1 || c.closes.Load() != 1 {
			t.Fatal("concurrent observation duplicated ownership")
		}
	})
	t.Run("pending-wait-cancel-retains-owner", func(t *testing.T) {
		p, c := creatorClosureUnitFixture(zero)
		entered, release := make(chan struct{}), make(chan struct{})
		c.waitFn = func() containedWaitResult { close(entered); <-release; return containedWaitResult{exited: true} }
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		done := make(chan error, 1)
		go func() { _, err := p.WaitCreatorClosure(ctx); done <- err }()
		<-entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || c.closes.Load() != 0 || c.terminations.Load() != 0 {
			close(release)
			t.Fatal("wait cancellation changed creator custody")
		}
		close(release)
		fresh, freshCancel := context.WithTimeout(context.Background(), time.Second)
		defer freshCancel()
		if proof, err := p.WaitCreatorClosure(fresh); proof == nil || err != nil || c.waits.Load() != 1 {
			t.Fatal("pending original wait was duplicated or lost")
		}
	})
	t.Run("zero-proof-snapshot", func(t *testing.T) {
		if s := (&CreatorSettlement{}).Snapshot(); s.HandlesClosed || s.EvidenceSHA256 != "" {
			t.Fatal("zero proof manufactured authority")
		}
	})
	t.Run("post-admission-cancel", func(t *testing.T) {
		p, c := creatorClosureUnitFixture(zero)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		supplied := &creatorClosureAdmissionCancelContext{Context: ctx, cancel: cancel}
		if proof, err := p.WaitCreatorClosure(supplied); proof != nil || !errors.Is(err, context.Canceled) || c.waits.Load() != 0 || c.observations.Load() != 0 || c.closes.Load() != 0 || c.terminations.Load() != 0 {
			t.Fatal("cancellation after TryLock performed activity")
		}
	})
	t.Run("descendant-barrier-cancel", func(t *testing.T) {
		p, c := creatorClosureUnitFixture(zero)
		entered := make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c.observeFn = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
		done := make(chan error, 1)
		go func() { _, err := p.WaitCreatorClosure(ctx); done <- err }()
		<-entered
		if c.waits.Load() != 1 || c.closes.Load() != 0 || c.terminations.Load() != 0 {
			cancel()
			<-done
			t.Fatal("root exit released descendant custody")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("held descendant cancellation lost")
		}
		c.observeFn = nil
		fresh, freshCancel := context.WithTimeout(context.Background(), time.Second)
		defer freshCancel()
		if proof, err := p.WaitCreatorClosure(fresh); proof == nil || err != nil || c.waits.Load() != 1 {
			t.Fatal("descendant timeout lost original owner")
		}
	})
	t.Run("legacy-nonzero-normalization", func(t *testing.T) {
		p, c := creatorClosureUnitFixture(nonzero)
		c.waitResult.err = nonzero.raw
		if err := p.TerminateAndWait(time.Second); err != nil || c.waits.Load() != 1 || c.terminations.Load() != 1 || c.closes.Load() != 1 || c.observations.Load() != 0 {
			t.Fatalf("legacy expected exit behavior changed: %v", err)
		}
	})
}

// Deliberately exposes only the old containedChild capability.
type creatorClosureUnsupportedUnitChild struct{ creatorClosureUnitChild *creatorClosureUnitChild }

func (c *creatorClosureUnsupportedUnitChild) start(*exec.Cmd, containedInputFile, containedWriteFile, containedWriteFile) error {
	return nil
}
func (c *creatorClosureUnsupportedUnitChild) wait() containedWaitResult {
	return c.creatorClosureUnitChild.wait()
}
func (c *creatorClosureUnsupportedUnitChild) terminate(ms uint32) error {
	return c.creatorClosureUnitChild.terminate(ms)
}
func (c *creatorClosureUnsupportedUnitChild) close() error { return c.creatorClosureUnitChild.close() }

func TestProvisionalCreatorClosureExitMetadataHelper(t *testing.T) {
	for _, arg := range os.Args {
		if arg == "--creator-closure-exit17" {
			fmt.Fprintln(os.Stdout, "NONZERO_EXIT_METADATA_ONLY")
			fmt.Fprintln(os.Stderr, "SYNTHETIC_EXIT17")
			os.Exit(17)
		}
	}
}

func TestProvisionalNoSpawnReviewInvalidCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *exec.Cmd
	}{
		{name: "nil"},
		{name: "empty", cmd: &exec.Cmd{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child, err := StartProvisional(tc.cmd)
			if child != nil || err == nil || (tc.cmd != nil && tc.cmd.Process != nil) {
				t.Fatalf("invalid command produced child=%v err=%v", child, err)
			}
			// Check the complete returned error, never a nested matching cause.
			if _, ok := err.(*ProvisionalNoProcessError); !ok {
				t.Fatalf("owner refused before spawn without no-process evidence: type=%T err=%v", err, err)
			}
		})
	}
}

func TestProvisionalNoSpawnReviewContainmentFailure(t *testing.T) {
	cause := errors.New("injected job creation refusal")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "creation_refused", err: cause},
		{name: "wrapped_creation_refused", err: fmt.Errorf("create job: %w", cause)},
		{name: "nil_job_without_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "spawned")
			cmd := exec.Command(os.Args[0], "-test.run=^TestProvisionalNoSpawnReviewHelper$", "--", marker)
			calls := 0
			child, err := startProvisional(cmd, func() (*Job, error) {
				calls++
				return nil, tc.err
			})
			if child != nil {
				_ = child.TerminateAndWait(time.Second)
				t.Fatal("containment refusal returned a running child")
			}
			if calls != 1 || err == nil || cmd.Process != nil {
				t.Fatalf("containment refusal: calls=%d process=%v err=%v", calls, cmd.Process, err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("containment refusal ran helper: %v", statErr)
			}
			if tc.err != nil && !errors.Is(err, cause) {
				t.Fatalf("containment cause was lost: %v", err)
			}
			if tc.err == nil && !strings.Contains(err.Error(), "job creation returned nil job") {
				t.Fatalf("missing containment diagnostic: %v", err)
			}
			if _, ok := err.(*ProvisionalNoProcessError); !ok {
				t.Fatalf("containment owner failed before spawn without no-process evidence: type=%T err=%v", err, err)
			}
		})
	}
}

func TestProvisionalNoSpawnReviewLaunchFailureRemainsUncertain(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "absent-executable"), "invalid\x00command"} {
		cmd := exec.Command(path)
		child, err := StartProvisional(cmd)
		if child != nil || err == nil || cmd.Process != nil {
			t.Fatalf("bad executable: child=%v process=%v err=%v", child, cmd.Process, err)
		}
		if _, ok := err.(*ProvisionalNoProcessError); ok {
			t.Fatalf("StartWithJob failure was certified without owner proof: %v", err)
		}
	}
}

func TestProvisionalNoSpawnReviewHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			if err := os.WriteFile(os.Args[i+1], []byte("spawned"), 0o600); err != nil {
				os.Exit(2)
			}
			// An owned fixture remains alive until the provisional owner reaps it.
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
	}
}

func TestProvisionalNoSpawnReviewSuccessfulCleanup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProvisionalNoSpawnReviewHelper$", "--", marker)
	child, err := StartProvisional(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if err := child.TerminateAndWait(5 * time.Second); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("owned helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.TerminateAndWait(5 * time.Second); err != nil {
		t.Fatalf("successful owned cleanup: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("successful cleanup did not reap the child")
	}
}
