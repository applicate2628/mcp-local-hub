package api

import (
	"context"
	"testing"
	"time"
)

func TestEventLoop_OrderingFIFO(t *testing.T) {
	loop := NewEventLoop(16)
	got := make([]string, 0, 3)
	loop.RegisterHandler(func(e LoopEvent) {
		got = append(got, e.TaskName)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "A"})
	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "B"})
	loop.Post(LoopEvent{Kind: EvChildExit, TaskName: "C"})

	time.Sleep(50 * time.Millisecond)
	if len(got) != 3 || got[0] != "A" || got[1] != "B" || got[2] != "C" {
		t.Fatalf("FIFO order broken: %v", got)
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
