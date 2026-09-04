package vtune

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The driver deliberately withholds settlement, even after cancellation.
// Tests release it explicitly; no native VTune process is started.
type durableBoundsDriver struct {
	started chan context.Context
	release chan struct{}
	once    sync.Once
}

func (d *durableBoundsDriver) collect(ctx context.Context, _ vtuneRunRequest) (*runOutput, error) {
	d.started <- ctx
	<-d.release
	return nil, errors.New("test collector settled")
}
func (*durableBoundsDriver) stop(context.Context, vtuneRunRequest) error      { return nil }
func (*durableBoundsDriver) forceStop(context.Context, vtuneRunRequest) error { return nil }
func (*durableBoundsDriver) finalize(context.Context, vtuneRunRequest) error  { return nil }
func (*durableBoundsDriver) report(context.Context, vtuneRunRequest) (*runOutput, error) {
	return nil, nil
}
func (d *durableBoundsDriver) releaseAll() { d.once.Do(func() { close(d.release) }) }

func newDurableBoundsOwner(t *testing.T) (*vtuneRunOwnerV1, *durableBoundsDriver) {
	t.Helper()
	d := &durableBoundsDriver{started: make(chan context.Context, maxDurableVTuneRuns+1), release: make(chan struct{})}
	o, err := newVTuneRunOwnerV1(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.releaseAll()
		settled := make(chan struct{})
		go func() { o.wg.Wait(); close(settled) }()
		select {
		case <-settled:
		case <-time.After(5 * time.Second):
			t.Error("test workers did not settle after release")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := o.Close(ctx); err != nil {
			t.Errorf("close test owner: %v", err)
		}
	})
	return o, d
}

func awaitDurableCollect(t *testing.T, d *durableBoundsDriver) context.Context {
	t.Helper()
	select {
	case ctx := <-d.started:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("collect was not called")
		return nil
	}
}

func TestVTuneRunOwner_BoundsDurableWork(t *testing.T) {
	o, d := newDurableBoundsOwner(t)
	for _, timeout := range []int{-1, 0, maxDurableTimeoutSec + 1} {
		if _, _, err := o.Start(vtuneRunRequest{TimeoutSec: timeout}); !errors.Is(err, errVTuneTimeoutOutOfRange) {
			t.Fatalf("timeout %d: %v, want timeout range error", timeout, err)
		}
	}
	for i := 0; i < maxDurableVTuneRuns; i++ {
		req := vtuneRunRequest{RunID: fmt.Sprintf("bounds-%d", i), Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: maxDurableTimeoutSec, IdempotencyKey: fmt.Sprintf("key-%d", i)}
		if _, _, err := o.Start(req); err != nil {
			t.Fatal(err)
		}
		awaitDurableCollect(t, d)
	}
	if _, _, err := o.Start(vtuneRunRequest{RunID: "extra", TimeoutSec: 60}); !errors.Is(err, errVTuneAdmissionLimited) {
		t.Fatalf("extra start = %v, want admission limit", err)
	}
	// An idempotent retry does not acquire another slot, even at capacity.
	replayed, disposition, err := o.Start(vtuneRunRequest{RunID: "ignored-on-replay", Target: "target.exe", AnalysisType: "hotspots", TimeoutSec: maxDurableTimeoutSec, IdempotencyKey: "key-0"})
	if err != nil || disposition != "replayed" || replayed.RunID != "bounds-0" {
		t.Fatalf("replay at capacity: run=%s disposition=%s error=%v", replayed.RunID, disposition, err)
	}
}

func TestVTuneRunOwner_DurableCollectHasDeadline(t *testing.T) {
	o, d := newDurableBoundsOwner(t)
	before := time.Now()
	if _, _, err := o.Start(vtuneRunRequest{RunID: "deadline", Target: "target.exe", TimeoutSec: 60}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	ctx := awaitDurableCollect(t, d)
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(time.Minute)) || deadline.After(after.Add(time.Minute)) {
		t.Fatalf("collect deadline = %v, present=%v; not derived from the accepted request", deadline, ok)
	}
}

func TestVTuneRunOwner_AdmissionWaitsForWorkerSettlement(t *testing.T) {
	o, d := newDurableBoundsOwner(t)
	o.settlementTimeout = time.Millisecond
	ids := make([]string, maxDurableVTuneRuns)
	for i := range ids {
		ids[i] = fmt.Sprintf("unsettled-%d", i)
		if _, _, err := o.Start(vtuneRunRequest{RunID: ids[i], Target: "target.exe", TimeoutSec: 60}); err != nil {
			t.Fatal(err)
		}
		awaitDurableCollect(t, d)
	}
	// Stop publishes a terminal failure after its bounded wait. The driver
	// is still alive, so terminal state alone must not free admission.
	stopped, _, err := o.Stop(ids[0], "stop", true)
	if err != nil || !stopped.Terminal() || stopped.FailureID != failureStopSettlementTimeout {
		t.Fatalf("expected unsettled terminal stop, got state=%s failure=%s error=%v", stopped.State, stopped.FailureID, err)
	}
	if _, _, err := o.Start(vtuneRunRequest{RunID: "too-early", TimeoutSec: 60}); !errors.Is(err, errVTuneAdmissionLimited) {
		t.Fatalf("start before worker settlement = %v, want admission limit", err)
	}
	d.releaseAll()
	for _, id := range ids {
		if !o.waitSettled(id, 5*time.Second) {
			t.Fatalf("worker %s did not settle", id)
		}
	}
	if _, _, err := o.Start(vtuneRunRequest{RunID: "after-settlement", TimeoutSec: 60}); err != nil {
		t.Fatalf("settled workers did not free admission: %v", err)
	}
}

func TestVTuneRunOwner_WorkerExitCancelsCollectionTimer(t *testing.T) {
	o, d := newDurableBoundsOwner(t)
	if _, _, err := o.Start(vtuneRunRequest{RunID: "early-exit", TimeoutSec: maxDurableTimeoutSec}); err != nil {
		t.Fatal(err)
	}
	ctx := awaitDurableCollect(t, d)
	d.releaseAll()
	if !o.waitSettled("early-exit", 5*time.Second) {
		t.Fatal("worker did not settle")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("collection context survived worker exit: %v", ctx.Err())
	}
}
