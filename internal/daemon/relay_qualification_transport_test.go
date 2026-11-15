package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

var errQualificationDialProbe = errors.New("connection probe refuses live admission")

// The probe can observe an original socket but always refuses admission.
// Its embedded endpoint/native-close methods also refuse all authority.
type qualificationDialProbe struct {
	qualificationEndpointBindingProbe
	observe func(context.Context, *MCPHTTPOriginalConnection)
}

func (p *qualificationDialProbe) ValidateOriginalConnection(ctx context.Context, c *MCPHTTPOriginalConnection) error {
	if p.observe != nil {
		p.observe(ctx, c)
	}
	return errQualificationDialProbe
}

func newQualificationTransportOwner(t *testing.T, observe func(context.Context, *MCPHTTPOriginalConnection)) (*mcpHTTPQualificationOwner, string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := &HTTPToStdioRelay{URL: "http://" + listener.Addr().String() + "/mcp"}
	o := &mcpHTTPQualificationOwner{relay: relay, binding: MCPHTTPQualificationBinding{EndpointSource: &qualificationDialProbe{observe: observe}}}
	o.transport = &http.Transport{Proxy: nil, DisableKeepAlives: true, ForceAttemptHTTP2: false, DialContext: o.dialContext}
	o.client = &http.Client{Transport: o.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	relay.qualification = o
	t.Cleanup(func() {
		o.transport.CloseIdleConnections()
		o.mu.Lock()
		conns := append([]*mcpObservedConnection(nil), o.conns...)
		dials := o.activeDials
		o.mu.Unlock()
		if dials != 0 {
			t.Errorf("owned dial callbacks remain: %d", dials)
		}
		for _, conn := range conns {
			if err := conn.Close(); err != nil {
				t.Errorf("close owned socket: %v", err)
			}
		}
		if err := listener.Close(); err != nil {
			t.Errorf("close owned loopback listener: %v", err)
		}
	})
	return o, listener.Addr().String()
}

func TestQualificationDialPreservesRequestBudgets(t *testing.T) {
	type phaseKey struct{}
	type seenBudget struct {
		end    time.Time
		finite bool
		phase  any
		err    error
	}
	t.Run("production-post-and-delete", func(t *testing.T) {
		seen := make(chan seenBudget, 2)
		o, _ := newQualificationTransportOwner(t, func(ctx context.Context, _ *MCPHTTPOriginalConnection) {
			end, finite := ctx.Deadline()
			seen <- seenBudget{end, finite, ctx.Value(phaseKey{}), ctx.Err()}
		})
		workEnd := time.Now().Add(10 * time.Second)
		work, cancelWork := context.WithDeadline(context.WithValue(context.Background(), phaseKey{}, "work"), workEnd)
		defer cancelWork()
		_, err := o.exchange(work, "initialize", "1", nil, false)
		if !errors.Is(err, errQualificationDialProbe) {
			t.Fatalf("POST did not reach the refusing original-connection probe: %v", err)
		}
		first := <-seen
		if !first.finite || !first.end.Equal(workEnd) || first.phase != "work" || first.err != nil {
			t.Errorf("POST connection budget = %+v, want exact work deadline %s and live work values", first, workEnd)
		}
		cancelWork()
		o.finished = true
		o.snapshot.PrimaryError = err.Error()
		// This private fixture token exercises DELETE construction; it grants
		// no protocol, native, or completed qualification evidence.
		o.relay.setSessionID("private-cleanup-fixture")
		cleanupEnd := time.Now().Add(20 * time.Second)
		cleanup, cancelCleanup := context.WithDeadline(context.WithValue(context.Background(), phaseKey{}, "cleanup"), cleanupEnd)
		defer cancelCleanup()
		closeProof, closeErr := o.relay.CloseSessionObserved(cleanup)
		if !errors.Is(closeErr, errQualificationDialProbe) {
			t.Fatalf("DELETE did not reach the refusing original-connection probe: %v", closeErr)
		}
		second := <-seen
		if !second.finite || !second.end.Equal(cleanupEnd) || second.phase != "cleanup" || second.err != nil {
			t.Errorf("DELETE connection budget = %+v, want distinct live cleanup deadline %s after work cancellation", second, cleanupEnd)
		}
		if closeProof == nil || closeProof.complete || o.snapshot.Complete || (&MCPHTTPQualificationObservation{owner: o}).ReceiptSHA256() != "" {
			t.Fatal("refusing transport probe issued completed qualification evidence")
		}
	})
	t.Run("earlier-transport-deadline", func(t *testing.T) {
		var got time.Time
		var finite bool
		o, address := newQualificationTransportOwner(t, func(ctx context.Context, _ *MCPHTTPOriginalConnection) {
			got, finite = ctx.Deadline()
		})
		original, cancelOriginal := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelOriginal()
		end := time.Now().Add(10 * time.Second)
		transport, cancelTransport := context.WithDeadline(context.Background(), end)
		defer cancelTransport()
		transport = context.WithValue(transport, mcpQualificationRequestContextKey{}, mcpQualificationRequestContext{o, original})
		_, err := o.dialContext(transport, "tcp4", address)
		if !errors.Is(err, errQualificationDialProbe) || !finite || !got.Equal(end) {
			t.Fatalf("earlier transport deadline extended: finite=%v got=%s want=%s error=%v", finite, got, end, err)
		}
	})
}

func TestQualificationDialRejectsInvalidRequestBudgets(t *testing.T) {
	for _, name := range []string{"missing", "foreign-owner", "nil-original", "wrong-value", "no-deadline", "original-canceled", "expired", "transport-canceled"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			o, address := newQualificationTransportOwner(t, func(context.Context, *MCPHTTPOriginalConnection) { calls++ })
			original, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			value := mcpQualificationRequestContext{o, original}
			transport := context.Context(context.Background())
			want := "MCP_QUALIFICATION_REQUEST_CONTEXT_UNBOUND"
			var wantCause error
			switch name {
			case "foreign-owner":
				value.owner = &mcpHTTPQualificationOwner{}
			case "nil-original":
				value.original = nil
			case "no-deadline":
				value.original = context.Background()
				want = "MCP_QUALIFICATION_REQUEST_DEADLINE_REQUIRED"
			case "original-canceled":
				cancel()
				wantCause = context.Canceled
			case "expired":
				expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
				value.original = expired
				wantCause = context.DeadlineExceeded
			case "transport-canceled":
				canceled, stop := context.WithCancel(transport)
				stop()
				transport = canceled
				wantCause = context.Canceled
			}
			if name != "missing" {
				transport = context.WithValue(transport, mcpQualificationRequestContextKey{}, value)
			}
			if name == "wrong-value" {
				transport = context.WithValue(transport, mcpQualificationRequestContextKey{}, "not a request binding")
			}
			conn, err := o.dialContext(transport, "tcp4", address)
			if conn != nil {
				_ = conn.Close()
				t.Error("invalid budget returned a socket")
			}
			if wantCause != nil {
				if !errors.Is(err, wantCause) {
					t.Errorf("invalid budget error = %v, want cause %v", err, wantCause)
				}
			} else if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("invalid budget error = %v, want %s", err, want)
			}
			if calls != 0 || len(o.conns) != 0 {
				t.Errorf("invalid budget reached original socket validation: calls=%d sockets=%d", calls, len(o.conns))
			}
		})
	}
}

func TestQualificationDialCancellationKeepsValidationOwned(t *testing.T) {
	for _, mode := range []string{"production-request", "original-context", "transport-context"} {
		t.Run(mode, func(t *testing.T) {
			guard, cancelGuard := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancelGuard)
			original, cancelOriginal := context.WithCancel(guard)
			t.Cleanup(cancelOriginal)
			transport, cancelTransport := context.WithCancel(guard)
			t.Cleanup(cancelTransport)
			entered := make(chan context.Context, 1)
			releaseCh := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
			o, address := newQualificationTransportOwner(t, func(ctx context.Context, _ *MCPHTTPOriginalConnection) {
				entered <- ctx
				<-releaseCh
			})
			dialDone := make(chan struct{})
			dial := o.transport.DialContext
			o.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				defer close(dialDone)
				return dial(ctx, network, address)
			}
			t.Cleanup(func() {
				release()
				settle := time.NewTimer(5 * time.Second)
				defer settle.Stop()
				select {
				case <-dialDone:
				case <-settle.C:
					t.Error("owned dial callback did not settle")
				}
			})
			result := make(chan error, 1)
			if mode == "production-request" {
				go func() {
					_, err := o.exchange(original, "initialize", "1", nil, false)
					o.mu.Lock()
					o.finished = true
					if err != nil {
						o.snapshot.PrimaryError = err.Error()
					}
					o.mu.Unlock()
					result <- err
				}()
			} else {
				o.finished = true
				bound := context.WithValue(transport, mcpQualificationRequestContextKey{}, mcpQualificationRequestContext{o, original})
				go func() {
					conn, err := o.transport.DialContext(bound, "tcp4", address)
					if conn != nil {
						_ = conn.Close()
					}
					result <- err
				}()
			}
			var validation context.Context
			select {
			case validation = <-entered:
			case <-guard.Done():
				t.Fatal("original connection validation did not start")
			}
			if mode == "transport-context" {
				cancelTransport()
				if original.Err() != nil {
					t.Fatal("transport cancellation contaminated original request fixture")
				}
			} else {
				cancelOriginal()
				if mode == "original-context" && transport.Err() != nil {
					t.Fatal("original cancellation contaminated transport fixture")
				}
			}
			if mode == "production-request" {
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("canceled POST returned %v", err)
					}
				case <-guard.Done():
					t.Fatal("canceled POST did not return while validation was held")
				}
			}
			select {
			case <-validation.Done():
				if !errors.Is(validation.Err(), context.Canceled) {
					t.Errorf("validation cancellation = %v", validation.Err())
				}
			case <-guard.Done():
				t.Error("request or transport cancellation did not reach original validation")
			}
			o.mu.Lock()
			dials := o.activeDials
			o.mu.Unlock()
			if dials != 1 {
				t.Errorf("held original validation has activeDials=%d, want 1", dials)
			}
			cleanup, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCleanup()
			proof, closeErr := o.relay.CloseSessionObserved(cleanup)
			if proof != nil || closeErr == nil || closeErr.Error() != "MCP_QUALIFICATION_CLOSE_INFLIGHT" || o.snapshot.ResourcesComplete {
				t.Errorf("in-flight validation admitted close: proof=%v error=%v resources_complete=%v", proof != nil, closeErr, o.snapshot.ResourcesComplete)
			}
			release()
			select {
			case <-dialDone:
			case <-cleanup.Done():
				t.Fatal("released original validation did not settle")
			}
			if mode != "production-request" {
				select {
				case err := <-result:
					if !errors.Is(err, errQualificationDialProbe) {
						t.Errorf("released connection probe returned %v", err)
					}
				case <-cleanup.Done():
					t.Fatal("released dial result was not received")
				}
			}
			o.mu.Lock()
			dials = o.activeDials
			o.mu.Unlock()
			if dials != 0 {
				t.Fatalf("returned callback retains activeDials=%d", dials)
			}
			if _, err := o.relay.CloseSessionObserved(cleanup); err != nil {
				t.Errorf("close settled owned sockets: %v", err)
			}
			if (&MCPHTTPQualificationObservation{owner: o}).ReceiptSHA256() != "" {
				t.Fatal("refusing cancellation probe issued qualification receipt")
			}
		})
	}
}
