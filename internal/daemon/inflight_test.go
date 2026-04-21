package daemon

import (
	"context"
	"errors"
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
