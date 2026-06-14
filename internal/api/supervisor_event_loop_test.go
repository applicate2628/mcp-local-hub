package api

import (
	"context"
	"sync"
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
