package api

// Tests for the hot-swap (a) self-heal error classifier (Phase 1). The
// transport-vs-HTTP split is the load-bearing safety property: only a
// transport failure (request never landed) is safe to retry; an HTTP-level
// rejection (daemon received it) must not be, because a non-idempotent tool's
// side effect may already have run.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

func TestIsRetriableTransportFailure(t *testing.T) {
	refusedWrapped := &url.Error{
		Op:  "Post",
		URL: "http://127.0.0.1:9999/mcp",
		Err: &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"daemon HTTP 400 (received → side effect may have run)", &daemonHTTPError{code: 400}, false},
		{"daemon HTTP 503", &daemonHTTPError{code: 503}, false},
		{"daemon HTTP wrapped", fmt.Errorf("postToolsCall: %w", &daemonHTTPError{code: 500}), false},
		{"context deadline (ambiguous)", context.DeadlineExceeded, false},
		{"context canceled", context.Canceled, false},
		{"net timeout (ambiguous)", fakeNetTimeout{}, false},
		{"connection refused (bare)", syscall.ECONNREFUSED, true},
		{"connection refused (wrapped url.Error → net.OpError)", refusedWrapped, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"generic error (conservative: no retry)", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetriableTransportFailure(tc.err); got != tc.want {
				t.Errorf("isRetriableTransportFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
