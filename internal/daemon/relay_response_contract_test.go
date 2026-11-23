package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// These tests use only in-memory readers/writers and the existing fake
// RoundTripper. They do not start an HTTP server or any child process.
const relayResponseRequest = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\"}\n"

type relayResponseBody struct {
	io.Reader
	read   int64
	closed int
}

func (b *relayResponseBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *relayResponseBody) Close() error {
	b.closed++
	return nil
}

type relayResponseFill struct{}

func (relayResponseFill) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// Return bytes together with a non-EOF error, as io.Reader explicitly permits.
// The scanner may already know this error while it returns buffered tokens.
type relayResponseReadFailure struct {
	data   []byte
	cause  error
	cancel context.CancelFunc
}

func (r *relayResponseReadFailure) Read(p []byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.cause
	}
	return n, nil
}

func TestRelayResponseHTTPBodyLimit(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		for _, size := range []int64{maxHTTPHostBodyBytes, maxHTTPHostBodyBytes + 1, maxHTTPHostBodyBytes + 8192} {
			t.Run(fmt.Sprintf("status_%d_bytes_%d", status, size), func(t *testing.T) {
				const prefix = "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"padding\":\""
				const suffix = "\"}}"
				body := &relayResponseBody{Reader: io.MultiReader(
					strings.NewReader(prefix),
					io.LimitReader(relayResponseFill{}, size-int64(len(prefix)+len(suffix))),
					strings.NewReader(suffix),
				)}
				posts := 0
				client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPost {
						return nil, fmt.Errorf("unexpected method %s", req.Method)
					}
					posts++
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})}
				var out bytes.Buffer
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: &out, Stderr: io.Discard, HTTPClient: client}
				err := r.Run(context.Background())
				if size > maxHTTPHostBodyBytes {
					if !errors.Is(err, errBodyTooLarge) {
						t.Fatalf("oversize body cause lost: %v", err)
					}
				} else if err != nil {
					t.Fatalf("body at cap was rejected: %v", err)
				}
				if body.read > maxHTTPHostBodyBytes+1 || body.read != min(size, maxHTTPHostBodyBytes+1) {
					t.Fatalf("body reads = %d; want min(%d, cap+1)", body.read, size)
				}
				if body.closed != 1 || posts != 1 {
					t.Fatalf("body closes=%d posts=%d; want 1/1", body.closed, posts)
				}
				if status == http.StatusOK && size == maxHTTPHostBodyBytes && int64(out.Len()) != size+1 {
					t.Fatalf("complete at-cap frame length=%d; want %d", out.Len(), size+1)
				}
			})
		}
	}
}

func relayResponseSSEEvent(size int) string {
	const prefix = "{\"jsonrpc\":\"2.0\","
	const suffix = "\"id\":1,\"result\":{}}"
	var payload strings.Builder
	payload.WriteString(prefix)
	remaining := size - len(prefix) - len(suffix)
	for remaining > 0 {
		n := min(remaining, 32*1024)
		payload.WriteString(strings.Repeat(" ", n-1))
		payload.WriteByte('\n')
		remaining -= n
	}
	payload.WriteString(suffix)
	return "data: " + strings.ReplaceAll(payload.String(), "\n", "\ndata: ") + "\n\n"
}

func TestRelayResponseSSEAggregateLimit(t *testing.T) {
	for _, size := range []int{int(maxHTTPHostBodyBytes), int(maxHTTPHostBodyBytes) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var out bytes.Buffer
			r := &HTTPToStdioRelay{Stdout: &out}
			err := r.pipeSSE(context.Background(), strings.NewReader(relayResponseSSEEvent(size)))
			if size > int(maxHTTPHostBodyBytes) {
				if !errors.Is(err, errStdinLineTooLong) || out.Len() != 0 {
					t.Fatalf("oversize event: error=%v output bytes=%d", err, out.Len())
				}
			} else if err != nil || out.String() != "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n" {
				t.Fatalf("at-cap event must produce one compact frame: error=%v output bytes=%d", err, out.Len())
			}
		})
	}
}

func TestRelayResponseSSELineFailureIsReturned(t *testing.T) {
	r := &HTTPToStdioRelay{Stdout: io.Discard}
	err := r.pipeSSE(context.Background(), strings.NewReader("data: "+strings.Repeat("x", maxStdinLineBytes+1)+"\n\n"))
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("scanner failure was lost: %v", err)
	}
}

func TestRelayResponsePOSTReadFailureIsReturned(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		prefix      string
	}{
		{"json", 200, "application/json", "{\"jsonrpc\":\"2.0\""},
		{"error_body", 503, "text/plain", "partial error"},
		{"post_sse", 200, "text/event-stream", "data: partial\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := errors.New("response body failed")
			body := &relayResponseBody{Reader: &relayResponseReadFailure{data: []byte(tc.prefix), cause: cause}}
			posts := 0
			client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
				posts++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: body}, nil
			})}
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client}
			if err := r.Run(context.Background()); !errors.Is(err, cause) {
				t.Fatalf("response read cause lost: %v", err)
			}
			if body.closed != 1 || posts != 1 {
				t.Fatalf("body closes=%d requests=%d; want 1/1", body.closed, posts)
			}
		})
	}
}

func TestRelayResponseReadAndOutputFailuresRemainVisible(t *testing.T) {
	readCause, outputCause := errors.New("response read failed"), errors.New("synthetic output failed")
	body := &relayResponseBody{Reader: &relayResponseReadFailure{cause: readCause}}
	client := &http.Client{Transport: relayOutputRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}
	out := &relayFailingOutput{mode: "partial", failAt: 1, cause: outputCause, failed: make(chan struct{})}
	r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: out, Stderr: io.Discard, HTTPClient: client}
	err := r.Run(context.Background())
	if !errors.Is(err, readCause) || !errors.Is(err, outputCause) {
		t.Fatalf("must retain response and output causes: %v", err)
	}
	if body.closed != 1 || out.calls != 1 || out.closes != 0 {
		t.Fatalf("body closes=%d output calls=%d output closes=%d", body.closed, out.calls, out.closes)
	}
}

func TestRelayResponsePOSTTransportFailureSurvivesCancellation(t *testing.T) {
	cause := errors.New("POST transport failed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := 0
	client := &http.Client{Transport: relayOutputRoundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		cancel()
		return nil, errors.Join(context.Canceled, cause)
	})}
	var out bytes.Buffer
	r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: &out, Stderr: io.Discard, HTTPClient: client}
	if err := r.Run(ctx); !errors.Is(err, cause) || requests != 1 || out.Len() != 0 {
		t.Fatalf("POST cancellation lost cause or resent: error=%v requests=%d output=%q", err, requests, out.String())
	}
}

func TestRelayResponsePOSTRedirectNeverResends(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests, redirects := 0, 0
			client := &http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil },
				Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
					requests++
					if req.URL.Path == "/mcp" {
						return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"/second-effect"}}, Body: http.NoBody}, nil
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
				}),
			}
			var out bytes.Buffer
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: &out, Stderr: io.Discard, HTTPClient: client}
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if requests != 1 || redirects != 0 || !strings.Contains(out.String(), fmt.Sprintf("HTTP %d", status)) {
				t.Fatalf("redirect response must remain local: requests=%d supplied policy calls=%d output=%q", requests, redirects, out.String())
			}
			if err := client.CheckRedirect(nil, nil); err != nil || redirects != 1 {
				t.Fatal("caller-owned client policy was changed")
			}
		})
	}
}

func TestRelayResponseGETTransportFailureSurvivesCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered := make(chan struct{})
		input := &relayOutputStartedReader{Reader: reader, entered: entered}
		cause := errors.New("GET transport failed")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var gets atomic.Int32
		client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
			}
			<-entered
			gets.Add(1)
			cancel()
			return nil, errors.Join(context.Canceled, cause)
		})}
		r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client, InterruptStdin: func() error { return writer.CloseWithError(context.Canceled) }}
		r.setSessionID("fixture-session")
		if err := r.Run(ctx); !errors.Is(err, cause) || gets.Load() != 1 {
			t.Fatalf("GET cancellation lost cause or retried: error=%v gets=%d", err, gets.Load())
		}
	})
}

func TestRelayResponseGETFailureStopsAndJoins(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprintf("truncated_%t", truncated), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				entered := make(chan struct{})
				input := &relayOutputStartedReader{Reader: reader, entered: entered}
				readCause, interruptCause := errors.New("GET body failed"), errors.New("input cleanup failed")
				wantCause := readCause
				if truncated {
					wantCause = io.ErrUnexpectedEOF
				}
				var gets, closes, interruptions atomic.Int32
				client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodGet {
						return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
					}
					<-entered
					gets.Add(1)
					var body io.Reader = &relayResponseReadFailure{data: []byte("id: not-delivered\ndata: incomplete\n"), cause: readCause}
					if truncated {
						body = strings.NewReader("id: not-delivered\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n")
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &relayResponseClosingBody{
						Reader: body,
						closed: &closes,
					}}, nil
				})}
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client, InterruptStdin: func() error {
					interruptions.Add(1)
					return errors.Join(writer.CloseWithError(context.Canceled), interruptCause)
				}}
				r.setSessionID("fixture-session")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := r.Run(ctx)
				if !errors.Is(err, wantCause) || !errors.Is(err, interruptCause) || ctx.Err() != nil {
					t.Fatalf("GET failure did not drive settled cancellation: error=%v deadline=%v", err, ctx.Err())
				}
				if gets.Load() != 1 || closes.Load() != 1 || interruptions.Load() != 1 {
					t.Fatalf("gets=%d closes=%d interruptions=%d; want 1/1/1", gets.Load(), closes.Load(), interruptions.Load())
				}
			})
		})
	}
}

type relayResponseClosingBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b *relayResponseClosingBody) Close() error { b.closed.Add(1); return nil }

func TestRelayResponseGETCleanEOFStillResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		var gets atomic.Int32
		var resumeID string
		client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
			}
			if gets.Add(1) == 1 {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("id: delivered\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n\n"))}, nil
			}
			resumeID = req.Header.Get(lastEventIDHeader)
			_ = writer.Close()
			return &http.Response{StatusCode: 405, Header: make(http.Header), Body: http.NoBody}, nil
		})}
		var out bytes.Buffer
		r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: reader, Stdout: &out, Stderr: io.Discard, HTTPClient: client, InterruptStdin: writer.Close}
		r.setSessionID("fixture-session")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.Run(ctx); err != nil || ctx.Err() != nil {
			t.Fatalf("clean resume failed: error=%v deadline=%v", err, ctx.Err())
		}
		if gets.Load() != 2 || resumeID != "delivered" || out.String() != "{\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n" {
			t.Fatalf("gets=%d resumed ID=%q output=%q", gets.Load(), resumeID, out.String())
		}
	})
}

func TestRelayResponseParserPreservesObservedCauses(t *testing.T) {
	readCause, outputCause := errors.New("scanner read failed"), errors.New("receiver failed")
	reader := &relayResponseReadFailure{data: []byte("data: {}\n\n"), cause: readCause}
	_, err := readRelaySSE(context.Background(), reader, "", 8, func([]byte) (bool, error) { return false, outputCause }, nil)
	if !errors.Is(err, readCause) || !errors.Is(err, outputCause) {
		t.Fatalf("scanner and receiver errors must remain visible: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader = &relayResponseReadFailure{data: []byte("data: {}\n\n"), cause: readCause, cancel: cancel}
	_, err = readRelaySSE(ctx, reader, "", 8, func([]byte) (bool, error) { t.Error("canceled receiver called"); return false, nil }, nil)
	if !errors.Is(err, readCause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("scanner and cancellation errors must remain visible: %v", err)
	}
}

func TestRelayResponseParserFiniteReceiverContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"at_cap", "id: first\ndata: abc\ndata: defg\n\ndata: not-read-as-event\n\n", "abc\ndefg"},
		{"over_cap", "data: abc\ndata: defgh\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var delivered string
			id, err := readRelaySSE(context.Background(), strings.NewReader(tc.data), "", 8, func(payload []byte) (bool, error) {
				calls++
				delivered = string(payload)
				return true, nil
			}, nil)
			if tc.want == "" {
				if !errors.Is(err, errStdinLineTooLong) || calls != 0 {
					t.Fatalf("finite overflow: error=%v calls=%d", err, calls)
				}
			} else if err != nil || calls != 1 || delivered != tc.want || id != "first" {
				t.Fatalf("finite early stop: error=%v calls=%d payload=%q id=%q", err, calls, delivered, id)
			}
		})
	}
}

func TestRelayResponseFramingFeedsOneStdioMessage(t *testing.T) {
	const message = "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 9007199254740993,\n  \"result\": {\"text\": \"first\\nsecond\", \"literal\": \"backslash\\\\n\"}\n}"
	for _, media := range []string{"application/json", "text/event-stream"} {
		t.Run(media, func(t *testing.T) {
			payload := message
			if media == "text/event-stream" {
				payload = "data: " + strings.ReplaceAll(message, "\n", "\ndata: ") + "\n\n"
			}
			body := &relayResponseBody{Reader: strings.NewReader(payload)}
			client := &http.Client{Transport: relayOutputRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{media}}, Body: body}, nil
			})}
			var out bytes.Buffer
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(strings.Replace(relayResponseRequest, `"id":1`, `"id":9007199254740993`, 1)), Stdout: &out, Stderr: io.Discard, HTTPClient: client}
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if bytes.Count(out.Bytes(), []byte("\n")) != 1 || body.closed != 1 {
				t.Fatalf("stdio frame count/close: output=%q closes=%d", out.String(), body.closed)
			}
			// Consume the actual output with the production line reader, then
			// decode exactly that line. A whole-buffer JSON decoder would miss
			// the old bug because JSON whitespace may include embedded newlines.
			consumer := bufio.NewReader(&out)
			line, err := readJSONRPCLine(consumer)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  struct {
					Text    string `json:"text"`
					Literal string `json:"literal"`
				} `json:"result"`
			}
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatalf("one stdio line was not a complete message: %v; %q", err, line)
			}
			if got.JSONRPC != "2.0" || string(got.ID) != "9007199254740993" || got.Result.Text != "first\nsecond" || got.Result.Literal != "backslash\\n" {
				t.Fatalf("compaction changed message data: %#v", got)
			}
			if extra, err := readJSONRPCLine(consumer); err != io.EOF || len(extra) != 0 {
				t.Fatalf("stdio consumer found an extra frame: %q %v", extra, err)
			}
		})
	}
}

func TestRelayResponseFramingRejectsMalformedJSON(t *testing.T) {
	for _, tc := range []struct {
		name       string
		message    string
		wantSyntax bool
	}{
		{"empty", "", true},
		{"whitespace_only", " ", true},
		{"double_empty_data", "\n", true},
		{"truncated", "{\"jsonrpc\":\"2.0\",\"id\":1", true},
		{"two_values", "{\"id\":1}\n{\"id\":2}", true},
		{"raw_newline", "{\"text\":\"first\nsecond\"}", true},
		{"invalid_utf8", "{\"text\":\"\xff\"}", false},
	} {
		mediaTypes := []string{"application/json", "text/event-stream"}
		if tc.message == "" {
			// Empty SSE data is an MCP priming/control event, covered by
			// TestRelayResponseSSESelectsMCPMessages. HTTP JSON is still invalid.
			mediaTypes = mediaTypes[:1]
		}
		for _, media := range mediaTypes {
			t.Run(tc.name+"/"+media, func(t *testing.T) {
				payload := tc.message
				if media == "text/event-stream" {
					payload = "data: " + strings.ReplaceAll(payload, "\n", "\ndata: ") + "\n\n"
				}
				body := &relayResponseBody{Reader: strings.NewReader(payload)}
				client := &http.Client{Transport: relayOutputRoundTrip(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{media}}, Body: body}, nil
				})}
				var out bytes.Buffer
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: &out, Stderr: io.Discard, HTTPClient: client}
				err := r.Run(context.Background())
				var syntax *json.SyntaxError
				if err == nil || (tc.wantSyntax && !errors.As(err, &syntax)) || out.Len() != 0 || body.closed != 1 {
					t.Fatalf("invalid frame was written or its cause lost: error=%v output=%q closes=%d", err, out.String(), body.closed)
				}
				if after := r.writeLine([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}")); after == nil || out.Len() != 0 {
					t.Fatalf("framing failure did not stop subsequent output: %v %q", after, out.String())
				}
			})
		}
	}
}

func TestRelayResponseFramingPreservesCompoundCauses(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed_%t", malformed), func(t *testing.T) {
			readCause, outputCause := errors.New("SSE read failed"), errors.New("stdio write failed")
			data := "data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n"
			if malformed {
				data = "data: {invalid}\n\n"
			}
			body := &relayResponseBody{Reader: &relayResponseReadFailure{data: []byte(data), cause: readCause}}
			client := &http.Client{Transport: relayOutputRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
			})}
			out := &relayFailingOutput{mode: "partial", failAt: 1, cause: outputCause, failed: make(chan struct{})}
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: out, Stderr: io.Discard, HTTPClient: client}
			err := r.Run(context.Background())
			if !errors.Is(err, readCause) || body.closed != 1 || out.closes != 0 {
				t.Fatalf("read cause/body/output ownership: error=%v body closes=%d output closes=%d", err, body.closed, out.closes)
			}
			var syntax *json.SyntaxError
			if malformed {
				if !errors.As(err, &syntax) || out.calls != 0 {
					t.Fatalf("syntax failure wrote bytes or lost its cause: %v calls=%d", err, out.calls)
				}
			} else if !errors.Is(err, outputCause) || out.calls != 1 || bytes.Contains(out.accepted.Bytes(), []byte("\n")) {
				t.Fatalf("compacted output failure lost: %v calls=%d accepted=%q", err, out.calls, out.accepted.String())
			}
		})
	}
}

func TestRelayResponseFramingEnforcesCommonByteCap(t *testing.T) {
	var out bytes.Buffer
	r := &HTTPToStdioRelay{Stdout: &out}
	// Compact output would be one byte; admission still bounds the input
	// supplied to JSON compaction, including its whitespace.
	msg := []byte(strings.Repeat(" ", int(maxHTTPHostBodyBytes)) + "0")
	if err := r.writeLine(msg); !errors.Is(err, errBodyTooLarge) || out.Len() != 0 {
		t.Fatalf("compaction input was not bounded: %v output bytes=%d", err, out.Len())
	}
}

func TestRelayResponseSSEUnterminatedEventDoesNotDispatch(t *testing.T) {
	for _, payload := range []string{
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n",
		"data: {\"jsonrpc\":\"2.0\",\"id\":1",
		"data:\n",
	} {
		t.Run(fmt.Sprintf("bytes_%d", len(payload)), func(t *testing.T) {
			var out bytes.Buffer
			r := &HTTPToStdioRelay{Stdout: &out}
			if err := r.pipeSSE(context.Background(), strings.NewReader(payload)); !errors.Is(err, io.ErrUnexpectedEOF) || out.Len() != 0 {
				t.Fatalf("uncommitted event was dispatched: %v output=%q", err, out.String())
			}
		})
	}
	id, err := readRelaySSE(context.Background(), strings.NewReader("id: committed\n\nid: pending\n"), "old", 8, func([]byte) (bool, error) {
		t.Error("id-only blocks must not deliver a data payload")
		return false, nil
	}, nil)
	if err != nil || id != "committed" {
		t.Fatalf("uncommitted id escaped EOF: id=%q error=%v", id, err)
	}
}

func TestRelayResponseSSESelectsMCPMessages(t *testing.T) {
	const message = "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}"
	for _, tc := range []struct {
		name   string
		prefix string
		suffix string
		wantID string
		stop   bool
	}{
		{"sdk_prime_empty", "event: prime\nid: prime-1\ndata: \n\n", "", "prime-1", false},
		{"empty_default_data", "data:\n\n", "", "before", false},
		{"bare_data", "data\n\n", "", "before", false},
		{"empty_message_data", "event: message\ndata:\n\n", "", "before", false},
		{"named_non_json", "event: control\nid: control-1\ndata: not-json\n\n", "", "control-1", false},
		{"named_json", "event: control\ndata: " + message + "\n\n", "", "before", false},
		{"reset_after_type_only", "event: prime\n\n", "", "before", false},
		{"reset_after_id_only", "event: prime\nid: control-1\n\n", "", "control-1", false},
		{"reset_after_empty_block", "event: prime\ndata:\n\n", "", "before", false},
		{"explicit_message", "event: message\n", "", "before", false},
		{"last_event_type_wins", "event: prime\nevent: message\n", "", "before", false},
		{"bare_event_resets_type", "event: prime\nevent\n", "", "before", false},
		{"one_space_only", "event:  message\ndata: not-json\n\n", "", "before", false},
		{"case_sensitive_type", "event: Message\ndata: not-json\n\n", "", "before", false},
		{"finite_receiver_stop", "event: prime\nid: prime-1\ndata:\n\nid: actual-1\n", "data: not-json\n\n", "actual-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := &HTTPToStdioRelay{Stdout: &out}
			calls := 0
			id, err := readRelaySSE(context.Background(), strings.NewReader(tc.prefix+"data: "+message+"\n\n"+tc.suffix), "before", 128, func(payload []byte) (bool, error) {
				calls++
				return tc.stop, r.writeLine(payload)
			}, nil)
			if err != nil || calls != 1 || id != tc.wantID || out.String() != message+"\n" {
				t.Fatalf("SSE message selection: error=%v calls=%d id=%q output=%q", err, calls, id, out.String())
			}
		})
	}
}

func TestRelayResponseSSEControlCursorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		wantID  string
		wantEOF bool
	}{
		{"complete_prime", "event: prime\nid: prime-1\ndata: \n\n", "prime-1", false},
		{"complete_control", "event: control\nid: control-1\ndata: not-json\n\n", "control-1", false},
		{"complete_id_only", "id: committed\n\n", "committed", false},
		{"incomplete_prime", "event: prime\nid: pending\ndata: \n", "before", true},
		{"incomplete_control", "event: control\nid: pending\ndata: not-json\n", "before", true},
		{"incomplete_id_only", "id: pending\n", "before", false},
		{"pending_after_commit", "id: committed\n\nid: pending\n", "committed", false},
		{"id_whitespace", "id:  visible  \n\n", " visible  ", false},
		{"nul_id_ignored", "id: invalid\x00id\n\n", "before", false},
		{"nul_after_valid_id", "id: retained\nid: invalid\x00id\n\n", "retained", false},
		{"bare_id_resets", "id\n\n", "", false},
		{"last_id_wins", "id: first\nid: last\n\n", "last", false},
		{"field_case_sensitive", "ID: ignored\n\n", "before", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			id, err := readRelaySSE(context.Background(), strings.NewReader(tc.input), "before", 128, func([]byte) (bool, error) {
				calls++
				return false, nil
			}, nil)
			if errors.Is(err, io.ErrUnexpectedEOF) != tc.wantEOF || (!tc.wantEOF && err != nil) || calls != 0 || id != tc.wantID {
				t.Fatalf("SSE control cursor: error=%v calls=%d id=%q; want EOF=%t id=%q", err, calls, id, tc.wantEOF, tc.wantID)
			}
		})
	}
}

func TestRelayResponseSSEControlSelectionPreservesFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		data       string
		wantSyntax bool
		failOutput bool
	}{
		{"malformed_message", "data: {invalid}\n\n", true, false},
		{"whitespace_message", "data:  \n\n", true, false},
		{"double_empty_data", "data:\ndata:\n\n", true, false},
		{"invalid_utf8_message", "data: {\"text\":\"\xff\"}\n\n", false, false},
		{"output_failure", "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readCause, outputCause := errors.New("SSE source read failed"), errors.New("stdio receiver failed")
			input := "event: prime\nid: committed\ndata:\n\nid: pending\nevent: message\n" + tc.data
			reader := &relayResponseReadFailure{data: []byte(input), cause: readCause}
			var out bytes.Buffer
			r := &HTTPToStdioRelay{Stdout: &out}
			failedOut := &relayFailingOutput{mode: "partial", failAt: 1, cause: outputCause, failed: make(chan struct{})}
			if tc.failOutput {
				r.Stdout = failedOut
			}
			calls := 0
			id, err := readRelaySSE(context.Background(), reader, "before", 128, func(payload []byte) (bool, error) {
				calls++
				return false, r.writeLine(payload)
			}, nil)
			var syntax *json.SyntaxError
			if err == nil || !errors.Is(err, readCause) || calls != 1 || id != "committed" || out.Len() != 0 {
				t.Fatalf("SSE selected-message failure/cursor: error=%v calls=%d id=%q output=%q", err, calls, id, out.String())
			}
			if tc.wantSyntax && !errors.As(err, &syntax) {
				t.Fatalf("SSE selected-message syntax cause lost: %v", err)
			}
			if tc.failOutput && (!errors.Is(err, outputCause) || failedOut.calls != 1 || bytes.Contains(failedOut.accepted.Bytes(), []byte("\n"))) {
				t.Fatalf("SSE selected-message output cause lost: error=%v calls=%d accepted=%q", err, failedOut.calls, failedOut.accepted.String())
			}
			if !tc.failOutput && r.stdoutFailure() == nil {
				t.Fatal("SSE malformed message did not poison subsequent output")
			}
		})
	}
}

func TestRelayResponseSSEControlEventsRemainBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		overflow bool
	}{
		{"at_cap", "data: abc\ndata: defg\n\n", false},
		{"over_cap", "data: abc\ndata: defgh\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			id, err := readRelaySSE(context.Background(), strings.NewReader("event: control\nid: control-1\n"+tc.payload), "before", 8, func([]byte) (bool, error) {
				calls++
				return false, nil
			}, nil)
			wantID := "control-1"
			if tc.overflow {
				wantID = "before"
			}
			if errors.Is(err, errStdinLineTooLong) != tc.overflow || (!tc.overflow && err != nil) || calls != 0 || id != wantID {
				t.Fatalf("SSE control envelope: error=%v calls=%d id=%q; want overflow=%t id=%q", err, calls, id, tc.overflow, wantID)
			}
		})
	}
}

func TestRelayResponsePOSTSSEPrimingReachesStdio(t *testing.T) {
	const request = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"relay-fixture\",\"version\":\"1\"}}}\n"
	const response = "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"serverInfo\":{\"name\":\"relay-fixture\",\"version\":\"1\"}}}"
	// Exact field layout from go-sdk v1.6.1 writeEvent: prime and message
	// are complete SSE events. No SDK server or network is started here.
	body := &relayResponseBody{Reader: strings.NewReader("event: prime\nid: stream_0\ndata: \n\nevent: message\nid: stream_1\ndata: " + response + "\n\n")}
	posts := 0
	client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
		posts++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	})}
	var out bytes.Buffer
	r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(request), Stdout: &out, Stderr: io.Discard, HTTPClient: client}
	if err := r.Run(context.Background()); err != nil || posts != 1 || body.closed != 1 || out.String() != response+"\n" {
		t.Fatalf("priming POST did not reach stdio: error=%v posts=%d closes=%d output=%q", err, posts, body.closed, out.String())
	}
}

type relayResponseContextReader struct {
	ctx context.Context
}

func (r relayResponseContextReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestRelayResponseDefaultGETFollowsRunContextAcrossRuns(t *testing.T) {
	// No Parallel: replacing DefaultTransport exercises the nil-client path.
	// Restore only after every Run and its original input/body have settled.
	synctest.Test(t, func(t *testing.T) {
		original := http.DefaultTransport
		defer func() { http.DefaultTransport = original }()
		r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stderr: io.Discard}
		for run := 0; run < 2; run++ {
			func() {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				inputEntered, started := make(chan struct{}), make(chan struct{})
				var gets, closes, interrupts atomic.Int32
				http.DefaultTransport = relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodDelete {
						return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
					}
					if req.Method != http.MethodGet {
						return nil, fmt.Errorf("unexpected method %s", req.Method)
					}
					<-inputEntered
					if gets.Add(1) == 1 {
						close(started)
					}
					body := &relayResponseClosingBody{
						Reader: io.MultiReader(
							strings.NewReader("id: live\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n\n"),
							relayResponseContextReader{ctx: req.Context()},
						),
						closed: &closes,
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
				})
				var out bytes.Buffer
				r.Stdin = &relayOutputStartedReader{Reader: reader, entered: inputEntered}
				r.Stdout = &out
				r.InterruptStdin = func() error {
					interrupts.Add(1)
					return writer.CloseWithError(context.Canceled)
				}
				r.setSessionID("fixture-session")
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(defaultHTTPTimeoutSec+10)*time.Second)
				done := make(chan error, 1)
				returned := false
				go func() { done <- r.Run(ctx) }()
				defer func() {
					cancel()
					if !returned {
						<-done
					}
				}()
				select {
				case <-started:
				case err := <-done:
					returned = true
					t.Fatalf("Run %d ended before GET opened: %v", run, err)
				case <-ctx.Done():
					t.Fatalf("Run %d did not open GET before its guard deadline", run)
				}
				synctest.Wait()
				time.Sleep(time.Duration(defaultHTTPTimeoutSec+1) * time.Second)
				synctest.Wait()
				select {
				case err := <-done:
					returned = true
					t.Fatalf("Run %d ended a live default GET before owner cancellation: %v", run, err)
				default:
				}
				if ctx.Err() != nil || gets.Load() != 1 || closes.Load() != 0 || interrupts.Load() != 0 || r.HTTPClient != nil {
					t.Fatalf("Run %d default stream restarted or caller config changed: GET=%d close=%d interrupt=%d client=%p", run, gets.Load(), closes.Load(), interrupts.Load(), r.HTTPClient)
				}
				if out.String() != "{\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n" {
					t.Fatalf("Run %d lost or duplicated the live event: %q", run, out.String())
				}
				cancel()
				err := <-done
				returned = true
				if err != nil || closes.Load() != 1 || interrupts.Load() != 1 {
					t.Fatalf("Run %d did not settle on explicit cancellation: error=%v close=%d interrupt=%d", run, err, closes.Load(), interrupts.Load())
				}
			}()
		}
	})
}

func TestRelayResponseDefaultPOSTTimeoutRemainsBounded(t *testing.T) {
	// No Parallel: the default transport is a scoped, in-memory test seam.
	synctest.Test(t, func(t *testing.T) {
		original := http.DefaultTransport
		defer func() { http.DefaultTransport = original }()
		var posts, closes atomic.Int32
		http.DefaultTransport = relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
			switch req.Method {
			case http.MethodPost:
				posts.Add(1)
				body := &relayResponseClosingBody{Reader: relayResponseContextReader{ctx: req.Context()}, closed: &closes}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
			case http.MethodGet:
				return &http.Response{StatusCode: 405, Header: make(http.Header), Body: http.NoBody}, nil
			default:
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
			}
		})
		r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: io.Discard, Stderr: io.Discard}
		r.setSessionID("fixture-session")
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(defaultHTTPTimeoutSec+5)*time.Second)
		defer cancel()
		started := time.Now()
		err := r.Run(ctx)
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || time.Since(started) != time.Duration(defaultHTTPTimeoutSec)*time.Second {
			t.Fatalf("default POST did not retain its own timeout: error=%v Run context=%v elapsed=%s", err, ctx.Err(), time.Since(started))
		}
		if posts.Load() != 1 || closes.Load() != 1 || r.HTTPClient != nil {
			t.Fatalf("POST repeated, body not closed or config changed: posts=%d closes=%d client=%p", posts.Load(), closes.Load(), r.HTTPClient)
		}
	})
}

func TestRelayResponseExplicitClientTimeoutIsPreserved(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				inputEntered := make(chan struct{})
				var requests, closes, interrupts atomic.Int32
				client := &http.Client{
					Timeout: 2 * time.Second,
					Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
						if req.Method == method {
							if method == http.MethodGet {
								<-inputEntered
							}
							requests.Add(1)
							body := &relayResponseClosingBody{Reader: relayResponseContextReader{ctx: req.Context()}, closed: &closes}
							contentType := "application/json"
							if method == http.MethodGet {
								contentType = "text/event-stream"
							}
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: body}, nil
						}
						if req.Method == http.MethodGet {
							return &http.Response{StatusCode: 405, Header: make(http.Header), Body: http.NoBody}, nil
						}
						return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
					}),
				}
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: strings.NewReader(relayResponseRequest), Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client}
				if method == http.MethodGet {
					r.Stdin = &relayOutputStartedReader{Reader: reader, entered: inputEntered}
					r.InterruptStdin = func() error {
						interrupts.Add(1)
						return writer.CloseWithError(context.Canceled)
					}
				}
				r.setSessionID("fixture-session")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				started := time.Now()
				err := r.Run(ctx)
				if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || time.Since(started) != 2*time.Second {
					t.Fatalf("caller timeout was changed or hidden: error=%v Run context=%v elapsed=%s", err, ctx.Err(), time.Since(started))
				}
				wantInterrupts := int32(0)
				if method == http.MethodGet {
					wantInterrupts = 1
				}
				if requests.Load() != 1 || closes.Load() != 1 || interrupts.Load() != wantInterrupts || r.HTTPClient != client || client.Timeout != 2*time.Second {
					t.Fatalf("caller policy/lifetime changed: requests=%d closes=%d interrupts=%d client=%p timeout=%s", requests.Load(), closes.Load(), interrupts.Load(), r.HTTPClient, client.Timeout)
				}
			})
		})
	}
}
