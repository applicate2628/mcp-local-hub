package gui

import (
	"bytes"
	"fmt"
	"runtime/pprof"
	"strings"
	"testing"
)

// newEphemeralBroadcaster owns a broadcaster used by tests that exercise live
// SSE delivery but do not assert GUI event-log persistence. Persistence is
// disabled before the broadcaster is exposed and cleanup joins its workers.
func newEphemeralBroadcaster(t testing.TB) *Broadcaster {
	t.Helper()
	b := NewBroadcaster()
	b.DisableGUIEventLog = true
	t.Cleanup(b.Close)
	return b
}

// newEphemeralServer owns the broadcaster of a handler-style test server.
// It keeps production routes and in-memory event delivery intact while making
// the unrelated disk persistence side effect inert for the test lifetime.
func newEphemeralServer(t testing.TB, cfg Config) *Server {
	t.Helper()
	s := NewServer(cfg)
	s.Broadcaster().DisableGUIEventLog = true
	t.Cleanup(s.Broadcaster().Close)
	return s
}

// assertNoBroadcasterWorkers reports leaked broadcaster workers after every
// test cleanup has run. It deliberately observes only: it never closes or
// otherwise mutates a leaked broadcaster, so a missed lifecycle owner remains
// actionable rather than being hidden at package teardown.
func assertNoBroadcasterWorkers() error {
	profile := pprof.Lookup("goroutine")
	if profile == nil {
		return fmt.Errorf("TEST_BROADCASTER_LIFECYCLE_LEAK goroutine profile unavailable")
	}
	var dump bytes.Buffer
	if err := profile.WriteTo(&dump, 2); err != nil {
		return fmt.Errorf("TEST_BROADCASTER_LIFECYCLE_LEAK goroutine profile: %w", err)
	}
	stacks := dump.String()
	drainPersist := strings.Count(stacks, "(*Broadcaster).drainPersist")
	runDropReporter := strings.Count(stacks, "(*Broadcaster).runDropReporter")
	if drainPersist == 0 && runDropReporter == 0 {
		return nil
	}
	return fmt.Errorf("TEST_BROADCASTER_LIFECYCLE_LEAK drainPersist=%d runDropReporter=%d", drainPersist, runDropReporter)
}
