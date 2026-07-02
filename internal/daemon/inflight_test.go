package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInflight_FirstCallMaterializes(t *testing.T) {
	var calls atomic.Int32
	g := NewInflightGate(10 * time.Millisecond)
	fn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		return "ep", nil
	}
	got, err := g.Do(context.Background(), "k1", fn)
	if err != nil {
		t.Fatal(err)
	}
	if got.(string) != "ep" || calls.Load() != 1 {
		t.Errorf("Do returned %v, calls=%d", got, calls.Load())
	}
}

func TestInflight_ConcurrentCallsShareOne(t *testing.T) {
	var calls atomic.Int32
	g := NewInflightGate(10 * time.Millisecond)
	fn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return "ep", nil
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, err := g.Do(context.Background(), "k1", fn)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 fn call under singleflight, got %d", calls.Load())
	}
}

func TestInflight_FailureReturnsError(t *testing.T) {
	g := NewInflightGate(10 * time.Millisecond)
	boom := errors.New("boom")
	fn := func(ctx context.Context) (any, error) { return nil, boom }
	_, err := g.Do(context.Background(), "k1", fn)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestInflight_RetryThrottleHonored(t *testing.T) {
	g := NewInflightGate(50 * time.Millisecond)
	boom := errors.New("boom")
	callsFn := func(ctx context.Context) (any, error) { return nil, boom }
	// First call fails.
	if _, err := g.Do(context.Background(), "k1", callsFn); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// Immediate retry — must return the cached error WITHOUT calling fn.
	var calls atomic.Int32
	noFn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		return nil, errors.New("should not run")
	}
	_, err := g.Do(context.Background(), "k1", noFn)
	if err == nil {
		t.Fatal("expected cached error, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("cached error should wrap boom: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("throttle breached: fn called %d times", calls.Load())
	}
	// After the throttle window elapses, fn runs again.
	time.Sleep(80 * time.Millisecond)
	calls.Store(0)
	_, err = g.Do(context.Background(), "k1", noFn)
	if err == nil {
		t.Fatal("expected new error after throttle")
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 fn call after throttle, got %d", calls.Load())
	}
}

func TestInflight_SuccessResetsThrottle(t *testing.T) {
	g := NewInflightGate(50 * time.Millisecond)
	// Fail once.
	if _, err := g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) {
		return nil, errors.New("x")
	}); err == nil {
		t.Fatal("expected initial failure")
	}
	// Sleep past throttle, then succeed.
	time.Sleep(80 * time.Millisecond)
	if _, err := g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	// Immediately after success, next call must run (no throttle).
	var ran atomic.Int32
	if _, err := g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) {
		ran.Add(1)
		return "ok2", nil
	}); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Errorf("throttle leaked across success: ran = %d", ran.Load())
	}
}

func TestInflight_ForgetClearsThrottle(t *testing.T) {
	g := NewInflightGate(500 * time.Millisecond)
	boom := errors.New("boom")
	if _, err := g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) {
		return nil, boom
	}); err == nil {
		t.Fatal("expected initial failure")
	}
	g.Forget("k1")
	var ran atomic.Int32
	if _, err := g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) {
		ran.Add(1)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Errorf("Forget did not clear throttle; ran = %d", ran.Load())
	}
}

// TestInflight_WinnerCancellationDoesNotAbortLosers guards the context-
// isolation invariant under the bounded-join model: a winner that cancels
// its OWN request context gets ctx.Err() back from the bounded join (the join
// is ctx-aware), but the shared materialization must NOT observe that cancel
// (fn's context is detached) and the retry throttle must NOT cache a
// canceled-error, so healthy concurrent callers are unaffected.
func TestInflight_WinnerCancellationDoesNotAbortLosers(t *testing.T) {
	g := NewInflightGate(10 * time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	fnReturned := make(chan struct{})
	var observedCancel atomic.Bool

	fn := func(ctx context.Context) (any, error) {
		close(started)
		<-release
		// Observe whether the detached context was canceled. It must NOT
		// be: the gate must detach fn from the winner's request context.
		if ctx.Err() != nil {
			observedCancel.Store(true)
		}
		close(fnReturned)
		return "ok", nil
	}

	winnerCtx, cancel := context.WithCancel(context.Background())
	winnerErr := make(chan error, 1)
	go func() {
		_, err := g.DoBounded(winnerCtx, "k", 10*time.Second, fn)
		winnerErr <- err
	}()
	<-started
	cancel() // winner disconnects mid-materialize

	// The bounded join is ctx-aware: the winner's own cancel returns ctx.Err().
	if err := <-winnerErr; !errors.Is(err, context.Canceled) {
		t.Errorf("winner bounded join should return ctx cancel, got: %v", err)
	}

	// The shared materialize continues detached; release it and confirm it
	// never saw the winner's cancel.
	close(release)
	<-fnReturned
	if observedCancel.Load() {
		t.Error("fn observed canceled context — materialization aborted by winner's cancel (regression)")
	}
	// Critical: the retry throttle must NOT hold a cached canceled-error.
	// A fresh call on the same key must not be blocked by a stale error.
	if _, err := g.DoBounded(context.Background(), "k", 10*time.Second, func(ctx context.Context) (any, error) {
		return "ok2", nil
	}); err != nil {
		t.Errorf("subsequent call blocked by stale canceled-error (throttle poisoned): %v", err)
	}
}

// TestInflight_MaterializeContextUsesFixedHardCeilingNotWinnerDeadline
// replaces the old winner-deadline-propagation guard. Under the bounded-join
// model, fn's detached context must carry the FIXED materializeHardCeiling —
// NOT the winner's short per-request deadline — so a short-budget bounded
// caller cannot abort the shared background materialize early.
func TestInflight_MaterializeContextUsesFixedHardCeilingNotWinnerDeadline(t *testing.T) {
	g := NewInflightGate(10 * time.Millisecond)
	// A short 50ms caller deadline must NOT bound fn.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var fnDeadlineOK atomic.Bool
	_, err := g.DoBounded(ctx, "k", 0, func(fctx context.Context) (any, error) {
		dl, ok := fctx.Deadline()
		if !ok {
			return nil, errors.New("fn context has no deadline — the fixed hard ceiling was dropped")
		}
		// Remaining should be ~materializeHardCeiling (120s), i.e. WAY beyond
		// the 50ms caller deadline. Assert it exceeds any plausible caller bound.
		if remaining := time.Until(dl); remaining < 60*time.Second {
			return nil, fmt.Errorf("deadline too near: %v (expected ~%v hard ceiling)", remaining, materializeHardCeiling)
		}
		fnDeadlineOK.Store(true)
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("DoBounded: %v", err)
	}
	if !fnDeadlineOK.Load() {
		t.Error("fn did not observe the fixed hard ceiling on the detached context")
	}
}

// TestInflightGate_DoBounded_TimerExpiryReturnsInFlightWhileWinnerContinues
// verifies the core bounded-join contract: a joiner whose budget elapses gets
// ErrMaterializeInFlight while the shared materialization keeps running and
// eventually completes (its result lands for the next caller).
func TestInflightGate_DoBounded_TimerExpiryReturnsInFlightWhileWinnerContinues(t *testing.T) {
	g := NewInflightGate(10 * time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	var winnerDone atomic.Bool

	winnerRes := make(chan any, 1)
	go func() {
		v, _ := g.DoBounded(context.Background(), "k", 10*time.Second, func(ctx context.Context) (any, error) {
			close(started)
			<-release
			winnerDone.Store(true)
			return "ep", nil
		})
		winnerRes <- v
	}()
	<-started

	// A joiner with a tiny budget must time out to ErrMaterializeInFlight while
	// the winner's fn is still blocked on release.
	_, err := g.DoBounded(context.Background(), "k", 20*time.Millisecond, func(ctx context.Context) (any, error) {
		return "should-not-run", nil
	})
	if !errors.Is(err, ErrMaterializeInFlight) {
		t.Fatalf("bounded joiner err = %v, want ErrMaterializeInFlight", err)
	}
	if winnerDone.Load() {
		t.Fatal("winner materialization aborted by the joiner's budget expiry (regression)")
	}

	// Release the winner; the shared materialize completes normally.
	close(release)
	if v := <-winnerRes; v != "ep" {
		t.Fatalf("winner result = %v, want ep", v)
	}
}

// TestInflightGate_DoBounded_CtxCancelReturnsCtxErr verifies a bounded joiner
// whose OWN context is canceled returns ctx.Err() (not ErrMaterializeInFlight)
// while the shared materialization is unaffected.
func TestInflightGate_DoBounded_CtxCancelReturnsCtxErr(t *testing.T) {
	g := NewInflightGate(10 * time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})

	winnerDone := make(chan struct{})
	go func() {
		_, _ = g.DoBounded(context.Background(), "k", 10*time.Second, func(ctx context.Context) (any, error) {
			close(started)
			<-release
			return "ep", nil
		})
		close(winnerDone)
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.DoBounded(ctx, "k", 10*time.Second, func(ctx context.Context) (any, error) {
		return "should-not-run", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled joiner err = %v, want context.Canceled", err)
	}

	close(release)
	<-winnerDone
}

// TestInflightGate_HasActiveFlight_TrueWhileFnRunsThenFalse verifies the
// activeFlights accounting the cold-start-slot gate keys on (F3): HasActiveFlight
// is false before any flight, true while the singleflight fn is executing, and
// false again after it returns.
func TestInflightGate_HasActiveFlight_TrueWhileFnRunsThenFalse(t *testing.T) {
	g := NewInflightGate(10 * time.Millisecond)
	if g.HasActiveFlight("k") {
		t.Fatal("HasActiveFlight true before any flight started")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = g.DoBounded(context.Background(), "k", 10*time.Second, func(ctx context.Context) (any, error) {
			close(started)
			<-release
			return "ep", nil
		})
		close(done)
	}()
	<-started
	if !g.HasActiveFlight("k") {
		t.Fatal("HasActiveFlight false while the materialize fn is running")
	}
	if g.HasActiveFlight("other") {
		t.Fatal("HasActiveFlight true for an unrelated key")
	}

	close(release)
	<-done
	// Poll: the deferred decrement runs on the singleflight goroutine after the
	// caller returns, so allow a brief settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !g.HasActiveFlight("k") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("HasActiveFlight still true after the flight completed")
}

func TestInflight_IndependentKeysDoNotInterfere(t *testing.T) {
	g := NewInflightGate(100 * time.Millisecond)
	boom := errors.New("boom")
	if _, err := g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) {
		return nil, boom
	}); err == nil {
		t.Fatal("expected failure on k1")
	}
	// k2 must NOT be throttled.
	var ran atomic.Int32
	if _, err := g.Do(context.Background(), "k2", func(ctx context.Context) (any, error) {
		ran.Add(1)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Errorf("k2 throttled by k1's failure; ran = %d", ran.Load())
	}
}

func TestInflight_ConcurrentFailureBurstHonorsRetryThrottle(t *testing.T) {
	g := NewInflightGate(time.Hour)
	boom := errors.New("boom")

	var calls atomic.Int32
	fn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		// Keep first singleflight winner busy long enough to build a burst.
		time.Sleep(5 * time.Millisecond)
		return nil, boom
	}

	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			_, _ = g.Do(context.Background(), "k", fn)
		})
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("retry throttle bypassed in concurrent burst: fn called %d times", got)
	}
}
