//go:build linux

package api

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
)

// TestPidForSocketInodeContext_CanceledCtxAborts (Codex PR-3 P2-C): the /proc
// owner walk must honor the caller deadline DURING the scan. A pre-canceled ctx
// aborts at the first /proc entry (before any per-fd readlink) with the ctx error,
// so a large/slow /proc tree cannot block the controller loop past the deadline.
func TestPidForSocketInodeContext_CanceledCtxAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, err := pidForSocketInodeContext(ctx, "999999999")
	if ok {
		t.Fatalf("ok = true, want false on a canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (the /proc walk must abort promptly on the deadline)", err)
	}
}

// TestLoopbackPortOwnerPIDContext_CanceledCtx (P2-C): the context-bounded owner
// probe returns the ctx error on a canceled ctx (fail-closed → the F1 gate treats
// it as a probe error → fail-open proceed, matching the Windows deadline path).
func TestLoopbackPortOwnerPIDContext_CanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, err := LoopbackPortOwnerPIDContext(ctx, 12345)
	if ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("got (ok=%v, err=%v), want (false, context.Canceled)", ok, err)
	}
}

// TestLoopbackPortOwnerPID_BackgroundDelegateByteIdentical (P2-C): the non-ctx
// entry point (a context.Background() delegate) still resolves the current process
// as the owner of a live loopback listener — the background ctx never trips.
func TestLoopbackPortOwnerPID_BackgroundDelegateByteIdentical(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	pid, ok, err := loopbackPortOwnerPID(port)
	if err != nil || !ok {
		t.Fatalf("loopbackPortOwnerPID(%d) = (%d, %v, %v), want the current pid", port, pid, ok, err)
	}
	if pid != os.Getpid() {
		t.Fatalf("loopbackPortOwnerPID(%d) pid=%d, want current pid %d", port, pid, os.Getpid())
	}
}

func TestLoopbackPortOwnerPIDLinuxResolvesListeningProcess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	pid, ok, err := loopbackPortOwnerPID(port)
	if err != nil {
		t.Fatalf("loopbackPortOwnerPID(%d): %v", port, err)
	}
	if !ok {
		t.Fatalf("loopbackPortOwnerPID(%d) ok=false, want true", port)
	}
	if pid != os.Getpid() {
		t.Fatalf("loopbackPortOwnerPID(%d) pid=%d, want current pid %d", port, pid, os.Getpid())
	}
}

func TestLoopbackTCPListenInodeFromProcNetRequiresLoopbackListen(t *testing.T) {
	path := t.TempDir() + "/tcp"
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:238B 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 12345 1 0000000000000000 100 0 0 10 0
   1: 0200007F:238B 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 99999 1 0000000000000000 100 0 0 10 0
   2: 0100007F:238C 00000000:0000 01 00000000:00000000 00:00000000 00000000 1000 0 77777 1 0000000000000000 100 0 0 10 0
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write proc fixture: %v", err)
	}
	inode, ok, err := loopbackTCPListenInodeFromProcNet(path, 9099)
	if err != nil {
		t.Fatalf("loopbackTCPListenInodeFromProcNet: %v", err)
	}
	if !ok || inode != "12345" {
		t.Fatalf("inode=%q ok=%v, want 12345 true", inode, ok)
	}
}

// countingErrCtx counts ctx.Err() polls and can trip after `trigger` calls, so a
// test can observe HOW MANY times the /proc walk polls the deadline (P2-iv).
type countingErrCtx struct {
	context.Context
	calls   *int
	trigger int // once *calls > trigger, Err() returns DeadlineExceeded (0 = never)
}

func (c *countingErrCtx) Err() error {
	*c.calls++
	if c.trigger > 0 && *c.calls > c.trigger {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// TestPidForSocketInodeContext_ChecksCtxPerFd (P2-iv): the deadline is honored
// INSIDE a single entry's fd scan, not only once per /proc entry. Prove it by
// counting ctx.Err() polls across a full walk: per-fd checks make the poll count
// exceed the number of /proc entries (each readable fd dir adds one poll per fd).
func TestPidForSocketInodeContext_ChecksCtxPerFd(t *testing.T) {
	// Ensure this process has socket fds so its own fd loop runs during the walk.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	calls := 0
	ctx := &countingErrCtx{Context: context.Background(), calls: &calls} // trigger 0 → never trips
	// A non-existent inode forces a FULL walk of /proc + every readable fd dir.
	_, _, _ = pidForSocketInodeContext(ctx, "999999999")

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	// per-entry checks == len(entries); per-fd checks add strictly more (this test
	// process alone has many fds). If the count only equaled the entry count, the
	// per-fd deadline check would be missing.
	if calls <= len(entries) {
		t.Fatalf("ctx.Err() polled %d times for %d /proc entries; per-fd checks must add MORE polls (P2-iv per-fd deadline missing)", calls, len(entries))
	}
}

// TestPidForSocketInodeContext_DeadlineTripsMidWalk (P2-iv): a deadline that fires
// AFTER the top-of-loop pre-check still aborts the walk promptly with the ctx
// error (the walk keeps polling ctx.Err() as it scans).
func TestPidForSocketInodeContext_DeadlineTripsMidWalk(t *testing.T) {
	calls := 0
	ctx := &countingErrCtx{Context: context.Background(), calls: &calls, trigger: 1} // pass call 1, trip on call 2
	_, ok, err := pidForSocketInodeContext(ctx, "999999999")
	if ok {
		t.Fatalf("ok=true, want false on a mid-walk deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded (deadline enforced during the walk, not only at the top)", err)
	}
	if calls <= 1 {
		t.Fatalf("ctx.Err() polled %d times; expected the walk to poll past the trigger", calls)
	}
}
