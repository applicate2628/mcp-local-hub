package cli

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"mcp-local-hub/internal/api"
)

// These tests exercise the creator's receiving protocol, not kernel identity or
// IPC authentication. Observation and settlement are modeled in the existing
// operations seam; no process is started, killed or waited by these fixtures.
func newSupervisorControlTestOwner() *supervisorOwner {
	return &supervisorOwner{
		proc:              &os.Process{Pid: 42},
		createdPID:        42,
		createdGeneration: api.SupervisorLockOwner{PID: 42, StartedAt: "original"},
		exitDone:          make(chan struct{}),
		stderrBuf:         newBoundedBuffer(256),
		attempt:           &supervisorCreationAttempt{},
	}
}

type supervisorControlTestResult struct {
	owner *supervisorOwner
	err   error
}

func TestGUISupervisorNativeControlHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An already-canceled operation must not enter native discovery or WMI.
	if err := validateCreatedSupervisorNativeControl(ctx, SupervisorControlExpectation{}, api.SupervisorLockOwner{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("native control discarded the caller's context: %v", err)
	}
}

func TestGUISupervisorControlWaitsForOriginalPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newSupervisorControlTestOwner()
		originalProcess := owner.proc
		published, observedStale := make(chan struct{}), make(chan struct{}, 1)
		var settlements atomic.Int32
		operations := supervisorCreatorOperations{
			observeControl: func(context.Context, SupervisorControlExpectation, api.SupervisorLockOwner, api.SupervisorProcessIdentityV1) (api.SupervisorLockOwner, error) {
				select {
				case <-published:
					return owner.createdGeneration, nil
				default:
				}
				select {
				case observedStale <- struct{}{}:
				default:
				}
				// An old sidecar remains before the fresh child publishes its
				// own owner. No live competing peer has accepted control.
				return api.SupervisorLockOwner{}, errors.Join(errSupervisorControlGenerationChanged, api.ErrSupervisorIPCUnavailable)
			},
			settleUnbound: func(*supervisorOwner) error {
				settlements.Add(1)
				return errors.New("premature original child settlement")
			},
		}
		finished := make(chan supervisorControlTestResult, 1)
		go func() {
			got, err := owner.waitForControl(context.Background(), SupervisorControlExpectation{}, api.SupervisorProcessIdentityV1{}, time.Second, operations)
			finished <- supervisorControlTestResult{got, err}
		}()
		<-observedStale
		synctest.Wait() // The original publication is still held at this barrier.
		if settlements.Load() != 0 || owner.proc != originalProcess {
			t.Errorf("stale publication consumed the original creator: settlements=%d retained=%v", settlements.Load(), owner.proc == originalProcess)
		}
		select {
		case <-owner.exitDone:
			t.Error("original exit was fabricated before publication")
		default:
		}
		close(published)
		result := <-finished
		if result.err != nil || result.owner != owner || !owner.spawned || owner.proc != originalProcess || settlements.Load() != 0 {
			t.Fatalf("original child did not become the bound owner: result=%+v spawned=%v settlements=%d", result, owner.spawned, settlements.Load())
		}
		if owner.control.proof == nil || owner.control.proof.creator != originalProcess || owner.control.proof.owner != owner.createdGeneration {
			t.Fatal("control binding replaced the original creator/generation")
		}
	})
}

func TestGUISupervisorControlSettlesOnlyAfterCompetingProof(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		name := "same-competitor"
		if replaced {
			name = "competitor-changes-during-settlement"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner := newSupervisorControlTestOwner()
				originalProcess := owner.proc
				winner := api.SupervisorLockOwner{PID: 84, StartedAt: "competitor"}
				entered, authenticated := make(chan struct{}), make(chan struct{})
				var observations, settlements atomic.Int32
				operations := supervisorCreatorOperations{
					observeControl: func(context.Context, SupervisorControlExpectation, api.SupervisorLockOwner, api.SupervisorProcessIdentityV1) (api.SupervisorLockOwner, error) {
						if observations.Add(1) == 1 {
							close(entered)
							<-authenticated
							return winner, nil
						}
						select {
						case <-owner.exitDone:
						default:
							return api.SupervisorLockOwner{}, errors.New("adoption preceded original exit receiving")
						}
						if replaced {
							return api.SupervisorLockOwner{PID: 126, StartedAt: "replacement"}, nil
						}
						return winner, nil
					},
					settleUnbound: func(actual *supervisorOwner) error {
						settlements.Add(1)
						if actual != owner || actual.proc != originalProcess {
							return errors.New("settlement addressed a different creator")
						}
						select {
						case <-authenticated:
						default:
							return errors.New("settlement preceded competing proof")
						}
						close(owner.exitDone)
						return nil
					},
				}
				finished := make(chan supervisorControlTestResult, 1)
				go func() {
					got, err := owner.waitForControl(context.Background(), SupervisorControlExpectation{}, api.SupervisorProcessIdentityV1{}, time.Second, operations)
					finished <- supervisorControlTestResult{got, err}
				}()
				<-entered
				synctest.Wait()
				if settlements.Load() != 0 || owner.proc != originalProcess {
					t.Error("unconfirmed competitor consumed original custody")
				}
				close(authenticated)
				result := <-finished
				if settlements.Load() != 1 || observations.Load() != 2 || owner.spawned || owner.control.proof != nil {
					t.Fatalf("competing observation gained custody or original outcome was lost: result=%+v settlements=%d observations=%d", result, settlements.Load(), observations.Load())
				}
				if replaced {
					if result.owner != nil || !errors.Is(result.err, errSupervisorControlGenerationChanged) {
						t.Fatalf("changed competitor was adopted: %+v", result)
					}
				} else if result.err != nil || result.owner != owner || owner.proc != nil {
					t.Fatalf("proved competitor was not adopted after original exit: %+v", result)
				}
			})
		})
	}
}

func TestGUISupervisorControlPendingPublicationIsBounded(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "overall-deadline"
		if cancelled {
			name = "parent-cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner := newSupervisorControlTestOwner()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered := make(chan struct{})
				var settlements atomic.Int32
				cleanupFailure := errors.New("original cleanup outcome")
				operations := supervisorCreatorOperations{
					observeControl: func(probe context.Context, _ SupervisorControlExpectation, _ api.SupervisorLockOwner, _ api.SupervisorProcessIdentityV1) (api.SupervisorLockOwner, error) {
						close(entered)
						<-probe.Done()
						return api.SupervisorLockOwner{}, errors.Join(errSupervisorControlGenerationChanged, probe.Err())
					},
					settleUnbound: func(actual *supervisorOwner) error {
						settlements.Add(1)
						if actual != owner {
							return errors.New("different creator settled")
						}
						close(owner.exitDone)
						return cleanupFailure
					},
				}
				const waitFor = 50 * time.Millisecond
				started := time.Now()
				finished := make(chan supervisorControlTestResult, 1)
				go func() {
					got, err := owner.waitForControl(ctx, SupervisorControlExpectation{}, api.SupervisorProcessIdentityV1{}, waitFor, operations)
					finished <- supervisorControlTestResult{got, err}
				}()
				<-entered
				synctest.Wait()
				if settlements.Load() != 0 {
					t.Error("pending publication settled before cancellation/deadline")
				}
				want := error(context.DeadlineExceeded)
				if cancelled {
					want = context.Canceled
					cancel()
				}
				result := <-finished
				if result.owner != nil || !errors.Is(result.err, want) || !errors.Is(result.err, cleanupFailure) || settlements.Load() != 1 {
					t.Fatalf("terminal cause or original settlement lost: result=%+v settlements=%d", result, settlements.Load())
				}
				if elapsed := time.Since(started); elapsed > waitFor {
					t.Fatalf("readiness deadline refreshed by competing probe: elapsed=%s limit=%s", elapsed, waitFor)
				}
			})
		})
	}
}

func TestGUISupervisorControlOriginalExitBeforePublication(t *testing.T) {
	owner := newSupervisorControlTestOwner()
	exitFailure := errors.New("original child exit outcome")
	owner.exitResult = exitInfo{exitErr: exitFailure}
	close(owner.exitDone)
	settlements := 0
	operations := supervisorCreatorOperations{
		observeControl: func(context.Context, SupervisorControlExpectation, api.SupervisorLockOwner, api.SupervisorProcessIdentityV1) (api.SupervisorLockOwner, error) {
			return api.SupervisorLockOwner{}, errSupervisorControlGenerationChanged
		},
		settleUnbound: func(*supervisorOwner) error { settlements++; return nil },
	}
	got, err := owner.waitForControl(context.Background(), SupervisorControlExpectation{}, api.SupervisorProcessIdentityV1{}, time.Second, operations)
	if got != nil || !errors.Is(err, exitFailure) || settlements != 0 {
		t.Fatalf("received original exit was replaced: owner=%v err=%v settlements=%d", got, err, settlements)
	}
}

func TestGUISupervisorControlCancellationPrecedesSuccessfulObservation(t *testing.T) {
	for _, when := range []string{"before-success", "during-original-settlement", "during-original-settlement-error"} {
		t.Run(when, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner := newSupervisorControlTestOwner()
				originalProcess := owner.proc
				incumbent := api.SupervisorLockOwner{PID: 125, StartedAt: "incumbent"}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered := make(chan struct{})
				reportSuccess := make(chan struct{})
				settling := make(chan struct{})
				completeOriginal := make(chan struct{})
				done := make(chan supervisorControlTestResult, 1)
				var observations, settlements atomic.Int32
				cleanupFailure := errors.Join(errSupervisorLaunchSettlement, errors.New("original settlement failure"))
				operations := supervisorCreatorOperations{
					observeControl: func(_ context.Context, _ SupervisorControlExpectation, _ api.SupervisorLockOwner, _ api.SupervisorProcessIdentityV1) (api.SupervisorLockOwner, error) {
						if observations.Add(1) == 1 {
							close(entered)
							<-reportSuccess
						}
						return incumbent, nil
					},
					settleUnbound: func(actual *supervisorOwner) error {
						if settlements.Add(1) != 1 || actual != owner || actual.proc != originalProcess {
							return errors.New("settlement did not address only the original creator")
						}
						close(settling)
						<-completeOriginal
						actual.receiveExitOutcome(nil)
						if when == "during-original-settlement-error" {
							return cleanupFailure
						}
						return nil
					},
				}
				go func() {
					got, err := owner.waitForControl(ctx, SupervisorControlExpectation{}, api.SupervisorProcessIdentityV1{}, time.Second, operations)
					done <- supervisorControlTestResult{got, err}
				}()
				<-entered
				if when == "before-success" {
					cancel()
				}
				close(reportSuccess)
				synctest.Wait()
				select {
				case <-settling:
				default:
					t.Error("cancellation did not settle the original")
				}
				select {
				case <-done:
					t.Fatal("returned before original completion")
				default:
				}
				if when != "before-success" {
					cancel()
				}
				close(completeOriginal)
				result := <-done
				if when == "during-original-settlement-error" && !errors.Is(result.err, cleanupFailure) {
					t.Fatalf("actual settlement cause lost: %v", result.err)
				}
				if result.owner != nil || !errors.Is(result.err, context.Canceled) || settlements.Load() != 1 || observations.Load() != 1 || owner.spawned || owner.control.proof != nil || owner.proc != originalProcess {
					t.Fatalf("cancellation lost original custody or rechecked/adopted the incumbent: result=%+v settlements=%d observations=%d", result, settlements.Load(), observations.Load())
				}
			})
		})
	}
}
