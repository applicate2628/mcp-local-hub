package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type qualificationFakeHTTP func(*http.Request) (*http.Response, error)

func (f qualificationFakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type qualificationBody struct {
	io.Reader
	closes   int
	closeErr error
}

func (b *qualificationBody) Close() error { b.closes++; return b.closeErr }
func TestQualificationExchangeUsesRealReceiver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, tc := range []struct {
		name, wire, media, method, id string
		status                        int
		closeFault                    bool
		wantErr                       bool
	}{
		{"json", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`, "application/json", "initialize", "1", 200, false, false},
		{"wrong-id", `{"jsonrpc":"2.0","id":2,"result":{}}`, "application/json", "initialize", "1", 200, false, true},
		{"rpc-error", `{"jsonrpc":"2.0","id":1,"error":{"code":-1}}`, "application/json", "initialize", "1", 200, false, true},
		{"sse-notification-before-reply", "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n", "text/event-stream", "initialize", "1", 200, false, false},
		{"sse-missing", "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n", "text/event-stream", "initialize", "1", 200, false, true},
		{"initialized", "", "application/json", "notifications/initialized", "", 202, false, false},
		{"initialized-rejected", "", "application/json", "notifications/initialized", "", 200, false, true},
		{"close-fault", `{"jsonrpc":"2.0","id":1,"result":{}}`, "application/json", "initialize", "1", 200, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &qualificationBody{Reader: strings.NewReader(tc.wire)}
			if tc.closeFault {
				body.closeErr = errors.New("close fault")
			}
			relay := &HTTPToStdioRelay{URL: "http://127.0.0.1:32123/mcp"}
			o := &mcpHTTPQualificationOwner{relay: relay, client: &http.Client{Transport: qualificationFakeHTTP(func(r *http.Request) (*http.Response, error) {
				if r.Context() != ctx {
					t.Fatal("work context replaced")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.media}}, Body: body}, nil
			})}}
			_, err := o.exchange(ctx, tc.method, tc.id, nil, tc.id == "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if body.closes != 1 || len(o.snapshot.Bodies) != 1 || !o.snapshot.Bodies[0].CloseAttempted {
				t.Fatal("original body not received exactly once")
			}
			if relay.getSessionID() != "" {
				t.Fatal("unvalidated initialize adopted session")
			}
		})
	}
}
func TestQualificationSessionCloseIsCached(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, fault := range []bool{false, true} {
		body := &qualificationBody{Reader: strings.NewReader("")}
		if fault {
			body.closeErr = errors.New("delete body fault")
		}
		r := &HTTPToStdioRelay{URL: "http://127.0.0.1:32123/mcp"}
		r.setSessionID("private-fixture-token")
		calls := 0
		o := &mcpHTTPQualificationOwner{relay: r, finished: true, transport: &http.Transport{}, snapshot: MCPHTTPQualificationSnapshot{ProtocolComplete: true}}
		o.client = &http.Client{Transport: qualificationFakeHTTP(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method != "DELETE" || req.Header.Get(mcpSessionIDHeader) != "private-fixture-token" {
				t.Fatal("foreign session delete")
			}
			return &http.Response{StatusCode: 204, Body: body}, nil
		})}
		r.qualification = o
		first, err := r.CloseSessionObserved(ctx)
		second, again := r.CloseSessionObserved(ctx)
		if first != second || calls != 1 || body.closes != 1 {
			t.Fatal("cleanup replay")
		}
		if (err != nil) != fault || (again != nil) != fault || o.snapshot.Complete == fault {
			t.Fatal("failed close minted complete")
		}
	}
}
func TestQualificationZeroObservationRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if (&MCPHTTPQualificationObservation{}).Validate(ctx, MCPHTTPQualificationBinding{}) == nil {
		t.Fatal("zero observation admitted")
	}
	if (&MCPHTTPQualificationObservation{}).ReceiptSHA256() != "" {
		t.Fatal("zero receipt")
	}
}
