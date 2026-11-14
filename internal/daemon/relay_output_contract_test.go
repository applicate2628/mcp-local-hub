package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The failing stream can accept some bytes before error. After that point no
// producer may append another frame or even its newline to the damaged stream.
type relayFailingOutput struct {
	mode                  string
	failAt, calls, closes int
	cause                 error
	accepted              bytes.Buffer
	failed                chan struct{}
	once                  sync.Once
}

func (w *relayFailingOutput) Write(p []byte) (int, error) {
	w.calls++
	if w.calls != w.failAt {
		return w.accepted.Write(p)
	}
	defer w.once.Do(func() { close(w.failed) })
	n := 0
	if w.mode == "partial" || w.mode == "short-nil" {
		n = len(p) / 2
	}
	if w.mode == "complete-error" {
		n = len(p)
	}
	_, _ = w.accepted.Write(p[:n])
	if w.mode == "short-nil" {
		return n, nil
	}
	return n, w.cause
}
func (w *relayFailingOutput) Close() error { w.closes++; return nil }

type relayEOFOnOutputFailure struct {
	failure <-chan struct{}
	ctx     context.Context
}

func (r relayEOFOnOutputFailure) Read([]byte) (int, error) {
	select {
	case <-r.failure:
		return 0, io.EOF
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

func TestRelayOutputFailureMatrix(t *testing.T) {
	for _, source := range []string{"json", "post-sse", "get-sse", "synthetic-error"} {
		for _, mode := range []string{"before-bytes", "partial", "short-nil", "newline", "complete-error"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				cause := errors.New("owned test output failed")
				w := &relayFailingOutput{mode: mode, failAt: 1, cause: cause, failed: make(chan struct{})}
				if mode == "newline" {
					w.failAt = 2
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
					switch req.Method {
					case http.MethodDelete:
						out.WriteHeader(http.StatusNoContent)
					case http.MethodGet:
						if source != "get-sse" {
							out.WriteHeader(http.StatusMethodNotAllowed)
							return
						}
						out.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(out, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notification/one\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notification/two\"}\n\n")
					case http.MethodPost:
						if source == "synthetic-error" {
							out.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						if source == "post-sse" {
							out.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(out, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notification/two\"}\n\n")
							return
						}
						out.Header().Set("Content-Type", "application/json")
						fmt.Fprint(out, `{"jsonrpc":"2.0","id":1,"result":{}}`)
					}
				}))
				defer srv.Close()
				r := &HTTPToStdioRelay{URL: srv.URL, Stdin: strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n"), Stdout: w, Stderr: io.Discard, HTTPClient: srv.Client()}
				if source == "get-sse" {
					r.setSessionID("fixture-session")
					r.Stdin = relayEOFOnOutputFailure{w.failed, ctx}
				}
				err := r.Run(ctx)
				want := cause
				if mode == "short-nil" {
					want = io.ErrShortWrite
				}
				if !errors.Is(err, want) {
					t.Errorf("source %s lost output failure: got %v, want %v", source, err, want)
				}
				if w.calls != w.failAt {
					t.Errorf("writer invoked after damaged frame: calls=%d want=%d", w.calls, w.failAt)
				}
				if bytes.HasSuffix(w.accepted.Bytes(), []byte("\n")) {
					t.Errorf("damaged frame was falsely terminated: %q", w.accepted.String())
				}
				if w.closes != 0 {
					t.Errorf("borrowed output was closed %d times", w.closes)
				}
			})
		}
	}
}

func TestRelayOutputFailureConcurrentProducers(t *testing.T) {
	const requests = 8
	var received atomic.Int32
	ready := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			out.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if received.Add(1) == requests {
			close(ready)
		}
		select {
		case <-ready:
		case <-req.Context().Done():
			return
		}
		out.Header().Set("Content-Type", "application/json")
		fmt.Fprint(out, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()
	cause := errors.New("concurrent output failure")
	w := &relayFailingOutput{mode: "partial", failAt: 1, cause: cause, failed: make(chan struct{})}
	input := strings.Repeat("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\"}\n", requests)
	r := &HTTPToStdioRelay{URL: srv.URL, Stdin: strings.NewReader(input), Stdout: w, Stderr: io.Discard, HTTPClient: srv.Client()}
	r.setSessionID("fixture-session")
	if err := r.Run(ctx); !errors.Is(err, cause) {
		t.Fatalf("failed stream cause lost: %v", err)
	}
	if w.calls != 1 {
		t.Fatalf("damaged output received %d writes, want 1", w.calls)
	}
	if received.Load() != requests {
		t.Fatalf("fixture did not exercise accepted concurrent requests: %d", received.Load())
	}
}

type relayErrorAfterLine struct {
	line     []byte
	err      error
	observed chan struct{}
}

func (r *relayErrorAfterLine) Read(p []byte) (int, error) {
	if len(r.line) > 0 {
		n := copy(p, r.line)
		r.line = r.line[n:]
		return n, nil
	}
	select {
	case <-r.observed:
	default:
		close(r.observed)
	}
	return 0, r.err
}
func TestRelayOutputFailurePreservesInputFailure(t *testing.T) {
	inputCause, outputCause := errors.New("input failure"), errors.New("output failure")
	reader := &relayErrorAfterLine{line: []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\"}\n"), err: inputCause, observed: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			out.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		select {
		case <-reader.observed:
		case <-req.Context().Done():
			return
		}
		fmt.Fprint(out, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()
	w := &relayFailingOutput{mode: "partial", failAt: 1, cause: outputCause, failed: make(chan struct{})}
	r := &HTTPToStdioRelay{URL: srv.URL, Stdin: reader, Stdout: w, Stderr: io.Discard, HTTPClient: srv.Client()}
	r.setSessionID("fixture-session")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := r.Run(ctx)
	if !errors.Is(err, inputCause) || !errors.Is(err, outputCause) {
		t.Fatalf("must retain both independently observed failures: %v", err)
	}
}

func TestRelayOutputFailureDoesNotPoisonNewRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			out.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprint(out, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n"
	cause := errors.New("first lifetime output failure")
	w := &relayFailingOutput{mode: "partial", failAt: 1, cause: cause, failed: make(chan struct{})}
	r := &HTTPToStdioRelay{URL: srv.URL, Stdin: strings.NewReader(input), Stdout: w, Stderr: io.Discard, HTTPClient: srv.Client()}
	if err := r.Run(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("first lifetime lost error: %v", err)
	}
	// Explicitly new test lifetime, not an automatic retry of an unknown request.
	var output bytes.Buffer
	r.Stdin = strings.NewReader(input)
	r.Stdout = &output
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("previous output error leaked to new lifetime: %v", err)
	}
	if output.String() != "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n" {
		t.Fatalf("wrong successful frame: %q", output.String())
	}
}

func TestRelayOutputFailureCancelsPendingHTTP(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			out.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		requests.Add(1)
		body, _ := io.ReadAll(req.Body)
		if strings.Contains(string(body), `"id":1`) {
			close(entered)
			<-req.Context().Done()
			close(canceled)
			return
		}
		select {
		case <-entered:
		case <-req.Context().Done():
			return
		}
		fmt.Fprint(out, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	}))
	defer srv.Close()
	cause := errors.New("answer stream failed")
	w := &relayFailingOutput{mode: "partial", failAt: 1, cause: cause, failed: make(chan struct{})}
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\"}\n"
	r := &HTTPToStdioRelay{URL: srv.URL, Stdin: strings.NewReader(input), Stdout: w, Stderr: io.Discard, HTTPClient: srv.Client()}
	r.setSessionID("fixture-session")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Run(ctx); !errors.Is(err, cause) {
		t.Fatalf("cancellation hid output failure: %v", err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("accepted HTTP worker did not observe cancellation")
	}
	if ctx.Err() != nil {
		t.Fatal("worker stopped only because the outer deadline expired")
	}
	if requests.Load() != 2 {
		t.Fatalf("request was replayed: posts=%d", requests.Load())
	}
}

type relayOutputRoundTrip func(*http.Request) (*http.Response, error)

func (f relayOutputRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Borrowed input is a distinct contract. We may cancel HTTP, but must neither
// close the caller's pipe nor return while an unowned read is still in flight.
func TestRelayOutputFailureWaitsForBorrowedInputOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered := make(chan struct{})
		input := &relayOutputStartedReader{Reader: reader, entered: entered}
		cause := errors.New("SSE output failed")
		out := &relayFailingOutput{mode: "partial", failAt: 1, cause: cause, failed: make(chan struct{})}
		client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				<-entered
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n\n"))}, nil
			}
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
		})}
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: out, Stderr: io.Discard, HTTPClient: client}
		r.setSessionID("fixture-session")
		done := make(chan error, 1)
		go func() { done <- r.Run(context.Background()) }()
		<-out.failed
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("returned before borrowed input settled: %v", err)
		default:
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, cause) {
			t.Fatalf("owner release erased output failure: %v", err)
		}
		if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("relay closed borrowed input: %v", err)
		}
		if out.closes != 0 {
			t.Fatal("relay closed borrowed output")
		}
	})
}

func TestRelayOutputSuccessDrainsAfterEOF(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	reader := &relayErrorAfterLine{line: []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\"}\n"), err: io.EOF, observed: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			out.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		close(entered)
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		fmt.Fprint(out, `{"jsonrpc":"2.0","id":1,"result":{"complete":true}}`)
	}))
	defer srv.Close()
	defer releaseOnce.Do(func() { close(release) })
	var output bytes.Buffer
	r := &HTTPToStdioRelay{URL: srv.URL, Stdin: reader, Stdout: &output, Stderr: io.Discard, HTTPClient: srv.Client()}
	r.setSessionID("fixture-session")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	for _, barrier := range []<-chan struct{}{entered, reader.observed} {
		select {
		case <-barrier:
		case <-ctx.Done():
			t.Fatal("fixture did not reach read/response barriers")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("normal EOF dropped final response: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("response did not settle")
	}
	if output.String() != "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"complete\":true}}\n" {
		t.Fatalf("wrong final frame: %q", output.String())
	}
}

type relayOutputStartedReader struct {
	io.Reader
	entered chan struct{}
	once    sync.Once
}

func (r *relayOutputStartedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.Reader.Read(p)
}
