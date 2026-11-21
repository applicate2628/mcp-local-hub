package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type relaySessionRetryTransport func(*http.Request) (*http.Response, error)

func (f relaySessionRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// No socket or outside goroutine is used: the bubble owns the listener,
// session, fake HTTP exchange, and clock. This verifies retry accounting, not
// real-server lifetime, network latency, or the installed Product route.
func TestRelaySSEWaitingForSessionDoesNotSpendRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts atomic.Int32
		transient := errors.New("first GET transport failure")
		r := &HTTPToStdioRelay{
			URL: "http://relay.invalid/mcp", Stdout: io.Discard, Stderr: io.Discard,
			HTTPClient: &http.Client{Transport: relaySessionRetryTransport(func(req *http.Request) (*http.Response, error) {
				current := attempts.Add(1)
				if req.Method != http.MethodGet || req.Header.Get(mcpSessionIDHeader) != "delayed-session" {
					return nil, errors.New("wrong SSE request method or session")
				}
				if current == 1 {
					return nil, transient
				}
				return &http.Response{StatusCode: http.StatusMethodNotAllowed, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- r.sseListener(ctx) }()
		synctest.Wait()
		// More than three 100 ms session polls; only fake time advances.
		time.Sleep(time.Second)
		synctest.Wait()
		if got := attempts.Load(); got != 0 {
			t.Fatalf("GET before session: %d attempts", got)
		}
		r.setSessionID("delayed-session")
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if got := attempts.Load(); got != 1 {
			t.Fatalf("first admitted GET attempts=%d, want 1", got)
		}
		select {
		case err := <-done:
			t.Fatalf("listener exhausted retries after one real GET: %v", err)
		default:
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := attempts.Load(); got != 2 {
			t.Fatalf("transport failure was not retried: attempts=%d", got)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("405 after recovery should end optional SSE cleanly: %v", err)
			}
		default:
			t.Fatal("listener did not settle after 405")
		}
	})
}
