package api

import (
	"context"
	"testing"
	"time"
)

func TestDaemonRestartWatcher_MarksStaleOnPIDChange(t *testing.T) {
	var marked []int
	rows := []DaemonStatus{{Port: 9123, PID: 100}}
	w := NewDaemonRestartWatcher(
		func(context.Context) ([]DaemonStatus, error) { return append([]DaemonStatus(nil), rows...), nil },
		func(port int) int { marked = append(marked, port); return 1 },
		time.Second,
	)
	ctx := context.Background()

	// First observation only records the PID — a fresh watcher must NOT mark
	// every daemon stale on startup.
	w.checkOnce(ctx)
	if len(marked) != 0 {
		t.Fatalf("first observation marked stale: %v", marked)
	}
	// Same PID: no restart, no mark.
	w.checkOnce(ctx)
	if len(marked) != 0 {
		t.Fatalf("unchanged PID marked stale: %v", marked)
	}
	// PID changes (the daemon was restarted): mark.
	rows[0].PID = 200
	w.checkOnce(ctx)
	if len(marked) != 1 || marked[0] != 9123 {
		t.Fatalf("restart (PID change) not marked exactly once: %v", marked)
	}
	// Stopped (PID 0): skip; must NOT mark and must NOT clobber the 200 baseline.
	rows[0].PID = 0
	w.checkOnce(ctx)
	if len(marked) != 1 {
		t.Fatalf("PID 0 caused a spurious mark: %v", marked)
	}
	// Restart after the stop (new PID): marked again (last live PID was 200).
	rows[0].PID = 300
	w.checkOnce(ctx)
	if len(marked) != 2 || marked[1] != 9123 {
		t.Fatalf("restart after stop not marked: %v", marked)
	}
}

func TestDaemonRestartWatcher_StatusErrorIsNonFatal(t *testing.T) {
	calls := 0
	w := NewDaemonRestartWatcher(
		func(context.Context) ([]DaemonStatus, error) { calls++; return nil, context.DeadlineExceeded },
		func(port int) int { t.Fatalf("markStale must not fire on a status error"); return 0 },
		time.Second,
	)
	w.checkOnce(context.Background())
	w.checkOnce(context.Background())
	if calls != 2 {
		t.Fatalf("statusFn calls=%d want 2 (error is swallowed, next tick retries)", calls)
	}
}
