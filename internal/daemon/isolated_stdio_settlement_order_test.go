package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsolatedStdioSettlementObserverFollowsReceiptAndRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	p := &IsolatedStdioSessions{
		limits:   IsolatedStdioLimits{TreeCost: 8, Teardown: time.Second, EOFGrace: time.Millisecond},
		reserved: 8, sessions: make(map[string]*isolatedStdioSession),
		changed: make(chan struct{}), stopLifetime: func() bool { return true },
	}
	s := &isolatedStdioSession{
		owner: p, host: &StdioHost{}, sid: "inert", sequence: 9,
		startCtx: start, cancelStart: cancelStart, stopSession: func() bool { return true },
		revoked: make(chan struct{}), launchDone: make(chan struct{}), receiptDone: make(chan struct{}),
	}
	p.sessions[s.sid] = s
	receipt := p.Retirement(s.sid)
	if receipt == nil {
		t.Fatal("missing original receipt")
	}
	type observation struct {
		event                  IsolatedStdioSettlementDiagnostic
		receiptAndReleaseReady bool
	}
	reached := make(chan observation, 1)
	releaseSink := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSink) }) }
	defer release()
	done := make(chan struct{})
	var calls atomic.Int32
	sinkFailure := errors.New("inert diagnostic sink refused write")
	sink := func() error {
		<-releaseSink
		return sinkFailure
	}
	sinkResult := make(chan error, 1)
	p.observe = func(d IsolatedStdioSettlementDiagnostic) {
		if calls.Add(1) != 1 {
			return
		}
		ready := false
		select {
		case <-s.receiptDone:
			if p.mu.TryLock() {
				ready = p.reserved == 0 && len(p.sessions) == 0
				p.mu.Unlock()
			}
		default:
		}
		reached <- observation{d, ready}
		sinkResult <- sink()
	}
	s.retire()
	firstDeadline := s.stopDeadline
	s.retire()
	if !firstDeadline.Equal(s.stopDeadline) || calls.Load() != 0 {
		t.Fatal("duplicate retirement changed the original deadline or emitted early")
	}
	close(s.launchDone)
	go func() { defer close(done); s.run() }()
	select {
	case got := <-reached:
		if !got.receiptAndReleaseReady || got.event != (IsolatedStdioSettlementDiagnostic{Sequence: 9, Complete: true}) {
			t.Fatalf("observer preceded receipt/release or leaked an unobserved generation: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("no terminal observer call")
	}
	settled, err := receipt.Await(ctx)
	if err != nil || !settled.Complete() || !settled.NoNativeAttempt {
		t.Fatalf("blocked sink affected the original terminal receipt: complete=%t error=%v", settled.Complete(), err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("blocked sink affected close: %v", err)
	}
	s.retire()
	release()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("session worker did not return after sink release")
	}
	if !errors.Is(<-sinkResult, sinkFailure) || calls.Load() != 1 {
		t.Fatalf("diagnostic rejection was not reached, or duplicate callback: calls=%d", calls.Load())
	}
	settled, err = receipt.Await(ctx)
	if err != nil || !settled.Complete() {
		t.Fatalf("sink failure rewrote the original receipt: complete=%t error=%v", settled.Complete(), err)
	}
}
