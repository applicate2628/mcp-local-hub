package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"mcp-local-hub/internal/config"
)

// This assembly only refuses. It cannot create preparation/source evidence;
// the counters prove that rejected private publishers never reach its probes.
type privatePublisherRefusingAssembly struct {
	inputs, qualifier, consumer int
}

func (a *privatePublisherRefusingAssembly) PrepareInputs(context.Context, *config.ServerManifest, string, []string, *SerenaPreparationPolicyV2) (SerenaPreparationInputs, error) {
	a.inputs++
	return SerenaPreparationInputs{}, errors.New("unexpected private publisher input probes")
}
func (a *privatePublisherRefusingAssembly) QualificationPort() PreparedSerenaQualificationPort {
	a.qualifier++
	return nil
}
func (a *privatePublisherRefusingAssembly) ObserveConsumerEnvironment(context.Context, *config.ServerManifest, string, string, *SerenaPreparationPolicyV2) (*SerenaConsumerEnvironmentObservation, error) {
	a.consumer++
	return nil, errors.New("unexpected private publisher consumer observation")
}
func (a *privatePublisherRefusingAssembly) assertUnused(t *testing.T) {
	t.Helper()
	if a.inputs != 0 || a.qualifier != 0 || a.consumer != 0 {
		t.Fatalf("assembly reached before admission: inputs=%d qualifier=%d consumer=%d", a.inputs, a.qualifier, a.consumer)
	}
}

func TestPrivateSerenaPublisherConstructorRejectsBeforeProbes(t *testing.T) {
	finite, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	canceled, stop := context.WithCancel(finite)
	stop()
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		noAssembly bool
		code       string
		cause      error
	}{
		{name: "nil_context", code: "SERENA_PRIVATE_PUBLISHER_CONTEXT_MISSING"},
		{name: "unbounded", ctx: context.Background(), code: "SERENA_PRIVATE_PUBLISHER_DEADLINE_MISSING"},
		{name: "caller_canceled", ctx: canceled, cause: context.Canceled},
		{name: "caller_expired", ctx: expired, cause: context.DeadlineExceeded},
		{name: "assembly_missing", ctx: finite, noAssembly: true, code: "SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE"},
		// This actual api.test process cannot become cli.test by supplying data.
		{name: "actual_non_cli_entrypoint", ctx: finite, code: "SERENA_PUBLISHER_IMAGE_ENTRYPOINT_UNQUALIFIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assembly := &privatePublisherRefusingAssembly{}
			var supplied SerenaPreparationAssembly = assembly
			if tc.noAssembly {
				supplied = nil
			}
			a, err := NewAPIWithPrivateSerenaIntegrationPublisher(tc.ctx, supplied)
			if a != nil || err == nil {
				t.Fatalf("constructor returned API=%v error=%v", a != nil, err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("original context cause lost: %v", err)
			}
			if tc.code != "" && !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("wrong refusal: %v", err)
			}
			assembly.assertUnused(t)
		})
	}
}

func TestPrivateSerenaPublisherOriginalBoundCannotBeRenewed(t *testing.T) {
	end := time.Now().Add(time.Minute)
	original, cancel := context.WithDeadline(context.Background(), end)
	defer cancel()
	owner := &privateSerenaPublisherOwner{original: original, deadline: end}
	later, releaseLater := context.WithDeadline(context.Background(), end.Add(time.Minute))
	defer releaseLater()
	earlier, releaseEarlier := context.WithDeadline(context.Background(), end.Add(-time.Second))
	defer releaseEarlier()
	for _, ctx := range []context.Context{context.Background(), later} {
		if err := owner.validateBounds(ctx); err == nil || !strings.Contains(err.Error(), "CONTEXT_OUT_OF_BOUND") {
			t.Fatalf("new caller renewed original bound: %v", err)
		}
	}
	// This checks only a bound; it is deliberately not a positive owner proof.
	if err := owner.validateBounds(earlier); err != nil {
		t.Fatalf("narrower bound rejected: %v", err)
	}
	if err := owner.validateOrigin(earlier); err == nil {
		t.Fatal("a deadline-only struct acquired publisher authority")
	}
	cancel()
	if err := owner.validateBounds(earlier); !errors.Is(err, context.Canceled) {
		t.Fatalf("fresh caller context hid original cancellation: %v", err)
	}
}

func TestPrivateSerenaPublisherRefusesPartialAndForeignAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	assembly := &privatePublisherRefusingAssembly{}
	a := NewAPIWithSerenaPreparation(assembly)
	end, _ := ctx.Deadline()
	// Retention of a partial factory result must never make it admitted.
	a.serenaPrivatePublisher = &privateSerenaPublisherOwner{
		api: a, publication: a.serenaPreparationPublication, original: ctx, deadline: end,
	}
	_, _, _, err := a.prepareSerenaInstall(ctx, nil, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "OWNER_UNAVAILABLE") {
		t.Fatalf("partial publisher reached preparation: %v", err)
	}
	foreign := NewAPIWithSerenaPreparation(assembly)
	foreign.serenaPrivatePublisher = a.serenaPrivatePublisher
	_, _, _, err = foreign.prepareSerenaInstall(ctx, nil, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "API_MISMATCH") {
		t.Fatalf("foreign API borrowed the original publisher: %v", err)
	}
	assembly.assertUnused(t)
}

func TestPrivateSerenaPublisherConsumerEntriesRetainOriginalCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	end, _ := ctx.Deadline()
	original, cancelOriginal := context.WithDeadline(context.Background(), end)
	cancelOriginal()
	assembly := &privatePublisherRefusingAssembly{}
	a := NewAPIWithSerenaPreparation(assembly)
	private := &privateSerenaPublisherOwner{
		api: a, publication: a.serenaPreparationPublication, original: original, deadline: end,
	}
	a.serenaPrivatePublisher = private
	// Deliberately unqualified original objects: cancellation must be received
	// before a namespace, environment assembly or consumer callback is entered.
	warm := &PreparedSerenaRuntimeOwner{effects: &preparedSerenaRuntimeEffects{
		image: &PublisherImageObservation{privatePublisher: private},
	}}
	cold := &PreparedSerenaRuntimeOwner{consumer: &preparedSerenaConsumerCoordinator{
		privatePublisher: private,
	}}
	request := PreparedSerenaConsumerInvocationRequest{
		OperationID:         "private-cancellation",
		ExpectedDescriptors: []SupervisorDaemon{{TaskName: "private-cancellation"}},
	}
	for name, owner := range map[string]*PreparedSerenaRuntimeOwner{"warm": warm, "recovered": cold} {
		t.Run(name, func(t *testing.T) {
			scope, err := owner.AcquireConsumerInvocation(ctx, request)
			if scope != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("consumer owner renewed canceled publisher: scope=%v error=%v", scope != nil, err)
			}
			invocation := &PreparedSerenaConsumerInvocation{owner: owner, request: request, deadline: end}
			if err := invocation.Validate(ctx, owner, request); !errors.Is(err, context.Canceled) {
				t.Fatalf("validation renewed canceled publisher: %v", err)
			}
			called := false
			err = invocation.receiveBody(ctx, nil, func(context.Context, preparedSerenaIntentConsumerEffects) error {
				called = true
				return nil
			})
			if called || !errors.Is(err, context.Canceled) {
				t.Fatalf("receiving entered after original cancellation: called=%v error=%v", called, err)
			}
			// The empty lease is deliberately unqualified. Both entry points
			// must reject origin cancellation before touching it or a callback.
			held := &StateFileObservedLease{}
			err = invocation.withIntentLockHeldContext(ctx, held, func(context.Context) error {
				called = true
				return nil
			})
			if called || !errors.Is(err, context.Canceled) {
				t.Fatalf("internal held effect renewed canceled publisher: called=%v error=%v", called, err)
			}
			err = invocation.WithIntentLockHeld(ctx, held, func() error {
				called = true
				return nil
			})
			if called || !errors.Is(err, context.Canceled) {
				t.Fatalf("opaque held callback renewed canceled publisher: called=%v error=%v", called, err)
			}
		})
	}
	t.Run("parsed_callback", func(t *testing.T) {
		called := false
		prepared := &PreparedSerenaParsedInstall{api: a, owner: warm}
		runErr, cleanupErr := prepared.WithConsumerInvocation(ctx, nil, InstallParsedManifestBypass{}, func(context.Context, *PreparedSerenaConsumerInvocation) error {
			called = true
			return nil
		})
		if called || cleanupErr != nil || !errors.Is(runErr, context.Canceled) {
			t.Fatalf("parsed entry renewed canceled publisher: called=%v run=%v cleanup=%v", called, runErr, cleanupErr)
		}
	})
	t.Run("launch_observation", func(t *testing.T) {
		launch, err := a.ObservePreparedSerenaLaunch(ctx, "private-cancellation")
		if launch != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("launch observation renewed canceled publisher: launch=%v error=%v", launch != nil, err)
		}
	})
	t.Run("retire_unused", func(t *testing.T) {
		if err := warm.RetireUnused(ctx, PreparedSerenaRuntimeRecord{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("retirement entered intent lock after original cancellation: %v", err)
		}
	})
	t.Run("observed_owner", func(t *testing.T) {
		owner, err := a.PreparedSerenaConsumerOwnerObserved(ctx, &PreparedSerenaLaunchObservation{api: a})
		if owner != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("observed owner renewed canceled publisher: owner=%v error=%v", owner != nil, err)
		}
	})
	assembly.assertUnused(t)
}

func TestPrivateSerenaPublisherEffectClaimIsTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	end, _ := ctx.Deadline()
	for _, tc := range []struct {
		name                       string
		activity                   privateSerenaPublisherActivity
		probeFailure, closeFailure bool
	}{
		{name: "namespace", activity: privateSerenaNamespaceActivity},
		{name: "pending_probe", activity: privateSerenaPreparationActivity, probeFailure: true},
		{name: "failed_close", activity: privateSerenaPreparationActivity, closeFailure: true},
		{name: "no_reported_failure", activity: privateSerenaPreparationActivity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assembly := &privatePublisherRefusingAssembly{}
			a := NewAPIWithSerenaPreparation(assembly)
			owner := &privateSerenaPublisherOwner{
				api: a, publication: a.serenaPreparationPublication, original: ctx, deadline: end,
			}
			a.serenaPrivatePublisher = owner
			// A scalar claim only consumes an opportunity. No image/native
			// authority or positive close proof is manufactured by this test.
			if err := owner.claimActivity(a, tc.activity); err != nil {
				t.Fatal(err)
			}
			if err := owner.validateOrigin(ctx); err == nil {
				t.Fatal("activity claim issued publisher authority")
			}
			failure := errors.New("retained unqualified fixture failure")
			if tc.probeFailure {
				owner.retainFailure(failure)
			}
			var retained *preparedSerenaRuntimeEffects
			if tc.closeFailure {
				retained = &preparedSerenaRuntimeEffects{localCloseErr: failure}
				owner.retainEffects(retained)
			}
			_, _, _, err := a.prepareSerenaInstall(ctx, nil, "", nil, nil)
			if err == nil || err.Error() != "SERENA_PRIVATE_PUBLISHER_ACTIVITY_ALREADY_CLAIMED" {
				t.Fatalf("a second preparation was admitted: %v", err)
			}
			observation, err := a.ObservePrivateSerenaPublisherNamespace(ctx)
			if observation != nil || err == nil || err.Error() != "SERENA_PRIVATE_PUBLISHER_ACTIVITY_ALREADY_CLAIMED" {
				t.Fatalf("a second namespace was admitted: observation=%v error=%v", observation != nil, err)
			}
			if tc.probeFailure && owner.failure != failure || tc.closeFailure && owner.effects != retained {
				t.Fatal("refusal lost the original retained failure/effects")
			}
			assembly.assertUnused(t)
		})
	}
	t.Run("concurrent_claims", func(t *testing.T) {
		a := NewAPI()
		owner := &privateSerenaPublisherOwner{api: a}
		results := make(chan error, 2)
		go func() { results <- owner.claimActivity(a, privateSerenaPreparationActivity) }()
		go func() { results <- owner.claimActivity(a, privateSerenaNamespaceActivity) }()
		accepted, refused := 0, 0
		for range 2 {
			err := <-results
			if err == nil {
				accepted++
			} else if err.Error() == "SERENA_PRIVATE_PUBLISHER_ACTIVITY_ALREADY_CLAIMED" {
				refused++
			} else {
				t.Fatal(err)
			}
		}
		if accepted != 1 || refused != 1 {
			t.Fatalf("competing effect opportunities: accepted=%d refused=%d", accepted, refused)
		}
	})
}

func TestPrivateSerenaPublisherAcquisitionCancellationPreservesCleanupBound(t *testing.T) {
	for _, which := range []string{"work", "original", "cleanup"} {
		t.Run(which, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cleanupEnd := time.Now().Add(time.Minute)
				original, cancelOriginal := context.WithDeadline(context.Background(), cleanupEnd)
				defer cancelOriginal()
				work, cancelWork := context.WithDeadline(original, cleanupEnd.Add(-10*time.Second))
				defer cancelWork()
				cleanup, cancelCleanup := context.WithDeadline(context.WithoutCancel(work), cleanupEnd)
				defer cancelCleanup()
				// No native/image fields: this is only a cancellation/bound
				// check and cannot qualify a publisher or create a namespace.
				owner := &privateSerenaPublisherOwner{original: original, deadline: cleanupEnd}
				acquisition, release := owner.acquisitionContext(work, cleanup)
				defer release()
				end, finite := acquisition.Deadline()
				if !finite || !end.Equal(cleanupEnd) {
					t.Fatalf("namespace cleanup bound changed: finite=%v end=%v", finite, end)
				}
				switch which {
				case "work":
					cancelWork()
				case "original":
					cancelOriginal()
				case "cleanup":
					cancelCleanup()
				}
				synctest.Wait()
				if !errors.Is(acquisition.Err(), context.Canceled) {
					t.Fatalf("new acquisition missed %s cancellation: %v", which, acquisition.Err())
				}
				if which != "cleanup" && cleanup.Err() != nil {
					t.Fatalf("work/case cancellation revoked existing resource cleanup: %v", cleanup.Err())
				}
			})
		})
	}
}

func TestPrivateSerenaPublisherInvocationContextsWaitForFinalization(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "in_flight_then_complete"
		if partial {
			name = "partial_close_retains_contexts"
		}
		t.Run(name, func(t *testing.T) {
			original, cancelOriginal := context.WithTimeout(context.Background(), time.Minute)
			defer cancelOriginal()
			end, _ := original.Deadline()
			first, cancelFirst := context.WithCancel(original)
			defer cancelFirst()
			second, cancelSecond := context.WithCancel(original)
			defer cancelSecond()
			released := 0
			// This is only the local context lifecycle: no publisher, lease,
			// namespace or successful resource observation is manufactured.
			scope := &PreparedSerenaConsumerInvocation{deadline: end, entered: 1}
			scope.privateContextReleases = []context.CancelFunc{
				func() { released++; cancelFirst() },
				func() { released++; cancelSecond() },
			}
			if partial {
				// An attempted acquisition with no returned lease is incomplete.
				scope.packageAttempted = true
			}
			pending, err := scope.CloseObserved()
			if err == nil || pending == nil || !pending.Snapshot().Pending || pending.Snapshot().Complete {
				t.Fatalf("in-flight scope falsely completed: close=%+v error=%v", pending.Snapshot(), err)
			}
			if released != 0 || first.Err() != nil || second.Err() != nil {
				t.Fatal("in-flight close revoked effect contexts before receiving")
			}
			scope.leave()
			closed, closeErr := scope.CloseObserved()
			if partial {
				if closed.Snapshot().Complete || closed.Validate(scope) == nil {
					t.Fatalf("partial acquisition became complete: %+v", closed.Snapshot())
				}
				if released != 0 || len(scope.privateContextReleases) != 2 || first.Err() != nil || second.Err() != nil {
					t.Fatal("incomplete close lost its retained context releases")
				}
			} else {
				if closeErr != nil || closed.Validate(scope) != nil || !closed.Snapshot().Complete {
					t.Fatalf("resource-free finalization failed: close=%+v error=%v", closed.Snapshot(), closeErr)
				}
				if released != 2 || len(scope.privateContextReleases) != 0 || !errors.Is(first.Err(), context.Canceled) || !errors.Is(second.Err(), context.Canceled) {
					t.Fatal("complete original finalization did not release both contexts")
				}
			}
			wantReleases := 2
			if partial {
				wantReleases = 0
			}
			replayed, replayErr := scope.CloseObserved()
			if replayed != closed || replayErr != closeErr || released != wantReleases {
				t.Fatalf("close replay changed the original result: releases=%d error=%v", released, replayErr)
			}
		})
	}
}
