package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestAutoCleanupTicker_OptOutEnvSkipsPOST verifies that
// fireAutoCleanupTick is not called when the operator sets
// MCPHUB_DISABLE_AUTO_CLEANUP=1.
//
// Strategy:
//   - Spin up a tiny httptest.Server that increments an atomic
//     counter on every inbound request. The ticker's POST target
//     becomes this loopback server's port.
//   - With env var set to "1", drive runAutoCleanupTicker briefly:
//     even if the 5-min ticker fired (it won't in 100ms), the
//     opt-out gate must short-circuit before any HTTP request lands.
//   - Assert: 0 requests after 100ms.
//
// We don't shrink the 5-min interval here — the test is verifying
// the gate, not the timer. With t.Setenv("MCPHUB_DISABLE_AUTO_CLEANUP",
// "1") the gate fires before any POST attempt regardless of the
// ticker cadence, so the test stays fast and deterministic.
func TestAutoCleanupTicker_OptOutEnvSkipsPOST(t *testing.T) {
	t.Setenv("MCPHUB_DISABLE_AUTO_CLEANUP", "1")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"killed":0,"skipped":0}`))
	}))
	t.Cleanup(srv.Close)

	port := mustParsePort(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		runAutoCleanupTicker(ctx, port)
		close(done)
	}()

	// Give the goroutine time to enter its select loop; the 5-min
	// ticker cannot fire within 100ms, but if a future regression
	// added a startup tick, the opt-out env gate would still skip
	// the POST and `hits` would remain 0.
	time.Sleep(100 * time.Millisecond)

	if got := hits.Load(); got != 0 {
		t.Fatalf("expected 0 HTTP requests with MCPHUB_DISABLE_AUTO_CLEANUP=1, got %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runAutoCleanupTicker did not exit after ctx cancel")
	}
}

// TestAutoCleanupOptedOut_RecognizedValues verifies the
// case-insensitive "1"/"true" recognition of MCPHUB_DISABLE_AUTO_CLEANUP.
// Unrecognized values (including empty/unset) leave the ticker enabled.
func TestAutoCleanupOptedOut_RecognizedValues(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"  1  ", true},  // TrimSpace
		{"  true  ", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"yes", false},
		{"on", false},
		{"2", false},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv("MCPHUB_DISABLE_AUTO_CLEANUP", c.val)
			if got := autoCleanupOptedOut(); got != c.want {
				t.Fatalf("autoCleanupOptedOut(%q) = %v, want %v", c.val, got, c.want)
			}
		})
	}
}

// mustParsePort pulls the integer port out of an httptest.Server's URL.
// httptest binds 127.0.0.1:<random> so we can pass <random> as the
// ticker's port arg and the loopback POST lands on our test handler.
func mustParsePort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("atoi port %q: %v", u.Port(), err)
	}
	return port
}
