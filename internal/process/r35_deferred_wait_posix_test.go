//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package process

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestR35ContainedLeaderReapRequiresPostSignalStart(t *testing.T) {
	child := &posixContainedChild{pid: 42}
	initializePlatformContainedWait(child)
	observed := atomic.Bool{}
	waitResult := observeContainedLeaderExit(child, func(pid int) error {
		if pid != 42 {
			t.Fatalf("observed pid=%d, want 42", pid)
		}
		observed.Store(true)
		return nil
	})
	if waitResult.err != nil || !observed.Load() {
		t.Fatalf("observation result=%+v observed=%v", waitResult, observed.Load())
	}

	reapCalls := atomic.Int32{}
	select {
	case <-child.reapDone:
		t.Fatal("leader was reaped before the post-signal start")
	default:
	}
	startContainedLeaderReap(child, func() containedWaitResult {
		reapCalls.Add(1)
		return containedWaitResult{exitCode: 7, exited: true}
	})
	select {
	case <-child.reapDone:
	case <-time.After(time.Second):
		t.Fatal("post-signal reap did not complete")
	}
	if reapCalls.Load() != 1 || child.reapResult.exitCode != 7 {
		t.Fatalf("reap calls/result=%d/%+v, want exact-one exit 7", reapCalls.Load(), child.reapResult)
	}
}
