package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
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

// This probe checks argument binding only. It always refuses live admission
// and cannot issue native, connection, or resource-close evidence.
type qualificationEndpointBindingProbe struct {
	endpointSHA, startSHA string
	refusal               error
	calls                 int
}

func (p *qualificationEndpointBindingProbe) ValidateOriginalEndpoint(_ context.Context, endpointSHA, startSHA string) error {
	p.calls++
	if endpointSHA != p.endpointSHA || startSHA != p.startSHA {
		return errors.New("qualification endpoint digest differs")
	}
	if p.refusal == nil {
		return errors.New("binding probe grants no live admission")
	}
	return p.refusal
}
func (*qualificationEndpointBindingProbe) ValidateOriginalConnection(context.Context, *MCPHTTPOriginalConnection) error {
	return errors.New("binding probe has no original connection")
}
func (*qualificationEndpointBindingProbe) CloseObserved(context.Context) (*process.OriginalJobMemberCloseObservation, error) {
	return nil, errors.New("binding probe has no native close")
}
func (*qualificationEndpointBindingProbe) ValidateOriginalClose(context.Context, *process.OriginalJobMemberCloseObservation, string) error {
	return errors.New("binding probe has no original close evidence")
}

func TestQualificationLiveBindingPassesExactDigests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	const endpoint = "http://127.0.0.1:32123/mcp"
	refusal := errors.New("original endpoint admission refused")
	source := &qualificationEndpointBindingProbe{
		endpointSHA: qualificationSHA([]byte(endpoint)), startSHA: strings.Repeat("c", 64), refusal: refusal,
	}
	binding := MCPHTTPQualificationBinding{
		OperationID: "operation", QualificationID: "qualification", GenerationID: "generation",
		RequestIdentitySHA256: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64),
		HostStartSHA256: source.startSHA, EndpointSHA256: source.endpointSHA, EndpointSource: source,
	}
	if err := binding.validate(ctx, endpoint); !errors.Is(err, refusal) {
		t.Fatalf("live admission did not receive exact endpoint/start digests: %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("original live admission calls = %d, want 1", source.calls)
	}
}

func TestQualificationReceivedObservationDoesNotReopenEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{"missing-close", "MCP_QUALIFICATION_CLOSE_FOREIGN"},
		{"incomplete-close", "MCP_QUALIFICATION_INCOMPLETE"},
		{"binding-drift", "MCP_QUALIFICATION_BINDING_DRIFT"},
		{"endpoint-drift", "MCP_QUALIFICATION_ENDPOINT_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			const endpoint = "http://127.0.0.1:32123/mcp"
			source := &qualificationEndpointBindingProbe{
				endpointSHA: qualificationSHA([]byte(endpoint)), startSHA: strings.Repeat("c", 64),
				refusal: errors.New("original endpoint is already closed"),
			}
			binding := MCPHTTPQualificationBinding{
				OperationID: "operation", QualificationID: "qualification", GenerationID: "generation",
				RequestIdentitySHA256: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64),
				HostStartSHA256: source.startSHA, EndpointSHA256: source.endpointSHA, EndpointSource: source,
			}
			owner := &mcpHTTPQualificationOwner{
				relay: &HTTPToStdioRelay{URL: endpoint}, binding: binding,
			}
			switch tc.name {
			case "incomplete-close":
				owner.close = &MCPHTTPQualificationCloseObservation{owner: owner}
			case "binding-drift":
				binding.GenerationID = "foreign-generation"
			case "endpoint-drift":
				binding.EndpointSHA256 = strings.Repeat("d", 64)
				owner.binding = binding
			}
			observation := &MCPHTTPQualificationObservation{owner: owner}
			err := observation.Validate(ctx, binding)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("received observation error = %v, want %s", err, tc.want)
			}
			if source.calls != 0 {
				t.Fatalf("received observation re-entered live admission %d times", source.calls)
			}
			if observation.ReceiptSHA256() != "" {
				t.Fatal("unsettled HTTP fixture issued a qualification receipt")
			}
		})
	}
}
