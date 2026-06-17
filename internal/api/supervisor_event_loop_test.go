package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventLoop_OrderingFIFO(t *testing.T) {
	loop := NewEventLoop(16)
	// got is appended by the loop's handler goroutine and read by this
	// test goroutine. Guard it with mu so the write/read pair has a
	// happens-before barrier; without it `go test -race` flags a data
	// race. snapshot() copies the slice under the lock for assertion.
	var mu sync.Mutex
	got := make([]string, 0, 3)
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
	loop.RegisterHandler(func(e LoopEvent) {
		mu.Lock()
		got = append(got, e.TaskName)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "A"})
	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "B"})
	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "C"})

	time.Sleep(50 * time.Millisecond)
	if g := snapshot(); len(g) != 3 || g[0] != "A" || g[1] != "B" || g[2] != "C" {
		t.Fatalf("FIFO order broken: %v", g)
	}
}

func TestEventLoop_NonBlockingPostFromSideGoroutine(t *testing.T) {
	loop := NewEventLoop(16)
	loop.RegisterHandler(func(e LoopEvent) {
		time.Sleep(10 * time.Millisecond) // simulate slow handler
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Side-goroutine posts must not block even when handler is slow.
	start := time.Now()
	for i := 0; i < 16; i++ {
		loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "X"})
	}
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Fatalf("Post blocked: %v", elapsed)
	}
}

// TestEventLoop_PostSelfNonBlockingUntilSelfChFull verifies PostSelf
// returns true when selfCh has room, false when full. Used by the
// supervisor controller's handler-self-post path; the controller
// MUST be able to detect saturation without blocking. Closes bot
// finding on PR #236 1c0ea09 (C3 priority channel design).
func TestEventLoop_PostSelfNonBlockingUntilSelfChFull(t *testing.T) {
	loop := NewEventLoop(16)
	// Don't start the loop's Run goroutine so selfCh accumulates.
	for i := 0; i < 16; i++ {
		if !loop.PostSelf(LoopEvent{Kind: EvHealthOK, TaskName: "X"}) {
			t.Fatalf("PostSelf returned false at i=%d/16; should fit (cap matches main ch)", i)
		}
	}
	// 17th PostSelf must return false (selfCh full).
	if loop.PostSelf(LoopEvent{Kind: EvHealthOK, TaskName: "X"}) {
		t.Fatalf("PostSelf returned true at i=17/16; selfCh should be full")
	}
}

// TestEventLoop_PriorityDrainsSelfBeforeMain verifies the C3 priority
// drain: a PostSelf event lands ahead of any pre-queued external
// events on the main channel. Closes bot finding on PR #236 1c0ea09
// (C3 priority channel ordering).
func TestEventLoop_PriorityDrainsSelfBeforeMain(t *testing.T) {
	loop := NewEventLoop(16)
	// got is appended by the loop's handler goroutine and polled by this
	// test goroutine. Guard it with mu so the write/read pair has a
	// happens-before barrier; without it `go test -race` flags a data
	// race. snapshot() copies the slice under the lock for assertion.
	var mu sync.Mutex
	got := make([]string, 0, 4)
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
	loop.RegisterHandler(func(e LoopEvent) {
		mu.Lock()
		got = append(got, e.TaskName)
		mu.Unlock()
	})

	// Queue main-channel events FIRST while loop is not running.
	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "main-1"})
	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "main-2"})
	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "main-3"})
	// Queue ONE self-event AFTER the main events.
	if !loop.PostSelf(LoopEvent{Kind: EvHealthOK, TaskName: "self-1"}) {
		t.Fatalf("PostSelf returned false; expected room")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(snapshot()) < 4 {
		time.Sleep(10 * time.Millisecond)
	}
	g := snapshot()
	if len(g) < 4 {
		t.Fatalf("only %d/4 events drained: %v", len(g), g)
	}
	// The first event drained MUST be "self-1" despite being posted
	// after the main events. This is the priority-drain guarantee.
	if g[0] != "self-1" {
		t.Fatalf("priority drain failed: got order=%v; expected self-1 first", g)
	}
}

// TestEventLoop_RegisterHandlerConcurrentWithRun is the Conc-F5 (PR #268
// deep-sec P3) regression guard: the supervisor registers ctrl.handleLoopEvent
// AFTER `go loop.Run` has already started (ctrl is constructed only after the
// loop goroutine launches). The old plain-slice `append` in RegisterHandler
// data-raced the dispatch goroutine's read of the slice. With the atomic
// copy-on-write the registration is safe. Run under `-race`: pre-fix this fails
// the race detector; post-fix it is clean. It also asserts the late-registered
// handler actually receives events posted after its registration.
func TestEventLoop_RegisterHandlerConcurrentWithRun(t *testing.T) {
	loop := NewEventLoop(64)

	var firstSeen, lateSeen atomic.Int64
	// First handler registered before Run (the supervise.go:538 pattern).
	loop.RegisterHandler(func(e LoopEvent) { firstSeen.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Concurrently: keep posting events (dispatch reads handlers) WHILE a
	// second handler is registered after Run started (the supervise.go:910
	// pattern). This is the exact register→Run overlap Conc-F5 describes.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "T"})
		}
	}()
	loop.RegisterHandler(func(e LoopEvent) { lateSeen.Add(1) })
	// Post more events AFTER the late registration so the late handler is
	// guaranteed some traffic it must observe.
	for i := 0; i < 200; i++ {
		loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "T"})
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && lateSeen.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if firstSeen.Load() == 0 {
		t.Fatalf("first handler never fired")
	}
	if lateSeen.Load() == 0 {
		t.Fatalf("late-registered handler never received an event after registration")
	}
}

// TestEventLoop_SetPanicHandlerConcurrentWithRun guards the onPanic field
// against the SAME unsynchronized-field-read class the COW fixed for handlers.
// dispatch reads l.onPanic on the loop goroutine while SetPanicHandler writes
// it; pre-fix (plain field) the race detector flags dispatch's read vs the
// write. With onPanic behind an atomic.Pointer this is clean. Run under
// `-race`: pre-fix fails, post-fix passes. It also asserts the concurrently
// installed observer fires when a handler panics.
func TestEventLoop_SetPanicHandlerConcurrentWithRun(t *testing.T) {
	loop := NewEventLoop(64)

	var observed atomic.Int64
	loop.RegisterHandler(func(e LoopEvent) {
		if e.TaskName == "boom" {
			panic("boom")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Concurrently post non-panicking events (dispatch loads onPanic on every
	// event) WHILE the panic handler is (re)installed from this goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "ok"})
		}
	}()
	for i := 0; i < 500; i++ {
		loop.SetPanicHandler(func(any, LoopEvent) { observed.Add(1) })
	}
	wg.Wait()

	// Now verify the installed observer actually fires on a real panic. The
	// loop goroutine re-raises after the observer, which would crash the test
	// process — so exercise the observed-then-reraised path on a throwaway
	// loop via dispatch directly (single goroutine, no -race concern).
	probe := NewEventLoop(16)
	probe.SetPanicHandler(func(any, LoopEvent) { observed.Add(1) })
	probe.RegisterHandler(func(LoopEvent) { panic("boom") })
	before := observed.Load()
	func() {
		defer func() { _ = recover() }()
		probe.dispatch(LoopEvent{TaskName: "boom"})
	}()
	if observed.Load() != before+1 {
		t.Fatalf("panic observer installed via SetPanicHandler did not fire: before=%d after=%d", before, observed.Load())
	}
}

// TestEventLoop_SetPanicHandlerNilClearsObserver verifies SetPanicHandler(nil)
// preserves the pre-atomic behavior: nil removes the panic observer instead of
// installing a non-nil pointer to a nil function value. A later handler panic
// must therefore propagate as the original panic, with no secondary nil-call
// panic masking it.
func TestEventLoop_SetPanicHandlerNilClearsObserver(t *testing.T) {
	loop := NewEventLoop(16)

	var observed atomic.Int64
	loop.SetPanicHandler(func(any, LoopEvent) { observed.Add(1) })
	loop.SetPanicHandler(nil)
	loop.RegisterHandler(func(LoopEvent) { panic("original-handler-panic") })

	defer func() {
		r := recover()
		if r != "original-handler-panic" {
			t.Fatalf("dispatch panic = %v; want original-handler-panic", r)
		}
		if got := observed.Load(); got != 0 {
			t.Fatalf("cleared panic observer fired %d times; want 0", got)
		}
	}()
	loop.dispatch(LoopEvent{TaskName: "boom"})
}
