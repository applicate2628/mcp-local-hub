package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const relayResumeRequest = `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"effect"}}`
const relayResumeResult = `{"jsonrpc":"2.0","id":42,"result":{"ok":true}}`
const relayResumeNotice = `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`

type relayResumeTransport func(*http.Request) (*http.Response, error)

func (f relayResumeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type relayResumeBody struct {
	reader            io.Reader
	readErr, closeErr error
	closes            int
}

func (b *relayResumeBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF && b.readErr != nil {
		err = b.readErr
	}
	return n, err
}

func (b *relayResumeBody) Close() error { b.closes++; return b.closeErr }

func relayResumeResponse(req *http.Request, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}
}

// A real HTTP exchange proves the consumer sends one effectful POST and
// continues that response through GET. It does not benchmark a deployed server.
func TestRelayPOSTResumesOriginalStream(t *testing.T) {
	var initializes, effects, resumes atomic.Int32
	bothPosts := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPost:
			var message struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
				t.Error(err)
				return
			}
			if message.Method == "initialize" {
				initializes.Add(1)
				w.Header().Set(mcpSessionIDHeader, "original-session")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)
				return
			}
			if message.Method != "tools/call" {
				t.Errorf("unexpected POST method %q", message.Method)
			}
			if effects.Add(1) == 2 {
				close(bothPosts)
			}
			select {
			case <-bothPosts:
			case <-req.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "id: post-"+string(message.ID)+"\nretry: 0\ndata:\n\n")
		case http.MethodGet:
			if req.Header.Get(lastEventIDHeader) == "" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			resumes.Add(1)
			cursor := req.Header.Get(lastEventIDHeader)
			if (cursor != "post-42" && cursor != "post-43") || req.Header.Get(mcpSessionIDHeader) != "original-session" || req.Header.Get(mcpProtocolVersionHeader) != "2025-11-25" {
				t.Errorf("continuation lost the original request binding")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			result := relayResumeResult
			if cursor == "post-43" {
				result = strings.Replace(result, `"id":42`, `"id":43`, 1)
			}
			_, _ = io.WriteString(w, "id: reply-"+cursor+"\ndata: "+result+"\n\ndata: must-not-forward\n\n")
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected HTTP method %s", req.Method)
		}
	}))
	defer server.Close()
	var output bytes.Buffer
	r := &HTTPToStdioRelay{URL: server.URL, Stdin: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" + relayResumeRequest + "\n" + strings.Replace(relayResumeRequest, `"id":42`, `"id":43`, 1) + "\n"), Stdout: &output, Stderr: io.Discard, HTTPClient: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("resumed relay failed: %v", err)
	}
	if initializes.Load() != 1 || effects.Load() != 2 || resumes.Load() != 2 {
		t.Fatalf("wrong operation counts: initialize=%d effect=%d resume=%d", initializes.Load(), effects.Load(), resumes.Load())
	}
	if strings.Count(output.String(), relayResumeResult) != 1 || strings.Count(output.String(), strings.Replace(relayResumeResult, `"id":42`, `"id":43`, 1)) != 1 || strings.Contains(output.String(), "must-not-forward") {
		t.Fatalf("matching response delivery differs: %s", output.String())
	}
}

func TestRelayPOSTResumptionCursorAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name, version, events string
		wantGet               bool
	}{
		{"priming", "2025-11-25", "id: cursor\n\n", true},
		{"older_legacy", "2025-03-26", "id: cursor\n\n", true},
		{"identified_message", "2025-06-18", "id: cursor\ndata: " + relayResumeNotice + "\n\n", true},
		{"unidentified_after_cursor", "2025-11-25", "id: cursor\n\ndata: " + relayResumeNotice + "\n\n", false},
		{"repeated_cursor_does_not_cover_message", "2025-11-25", "id: cursor\n\ndata: " + relayResumeNotice + "\n\nid: cursor\n\n", false},
		{"later_cursor_covers_message", "2025-11-25", "data: " + relayResumeNotice + "\n\nid: cursor\n\n", true},
		{"reset_cursor", "2025-11-25", "id: cursor\n\nid:\n\n", false},
		{"pending_cursor", "2025-11-25", "id: cursor\n", false},
		{"partial_event_after_cursor", "2025-11-25", "id: cursor\n\nid: pending\ndata: {", true},
		{"same_id_server_request", "2025-11-25", "id: cursor\ndata: {\"jsonrpc\":\"2.0\",\"id\":42,\"method\":\"ping\"}\n\n", true},
		{"different_response_id", "2025-11-25", "id: cursor\ndata: " + strings.Replace(relayResumeResult, `"id":42`, `"id":43`, 1) + "\n\n", true},
		{"unnegotiated", "", "id: cursor\n\n", false},
		{"new_transport", "2026-07-28", "id: cursor\n\n", false},
		{"unknown_version", "2099-01-01", "id: cursor\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var output bytes.Buffer
				post, get := 0, 0
				initial := &relayResumeBody{reader: strings.NewReader("retry: 0\n\n" + tc.events)}
				resumed := &relayResumeBody{reader: strings.NewReader("id: reply\ndata: " + relayResumeResult + "\n\n")}
				r := &HTTPToStdioRelay{URL: "http://relay.invalid/mcp", Stdout: &output, Stderr: io.Discard, sessionID: "original", protocolVersion: tc.version}
				r.HTTPClient = &http.Client{Transport: relayResumeTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodPost {
						post++
						// Later state must not replace this request's original operands.
						r.sessionID, r.protocolVersion = "later-session", "2026-07-28"
						return relayResumeResponse(req, initial), nil
					}
					get++
					if req.Method != http.MethodGet || req.Header.Get(lastEventIDHeader) != "cursor" || req.Header.Get(mcpSessionIDHeader) != "original" || req.Header.Get(mcpProtocolVersionHeader) != tc.version {
						t.Errorf("request-owned continuation differs")
					}
					return relayResumeResponse(req, resumed), nil
				})}
				r.handlePOST(context.Background(), []byte(relayResumeRequest))
				wantGets := 0
				if tc.wantGet {
					wantGets = 1
				}
				if post != 1 || get != wantGets || initial.closes != 1 || resumed.closes != wantGets {
					t.Fatalf("POST=%d GET=%d closes=%d/%d, want one POST and %d GET", post, get, initial.closes, resumed.closes, wantGets)
				}
				if tc.wantGet {
					if r.postErr != nil || strings.Count(output.String(), relayResumeResult) != 1 {
						t.Fatalf("resumption failed: %v; output=%q", r.postErr, output.String())
					}
				} else if r.postErr == nil {
					t.Fatal("unresolved response was reported as success")
				}
			})
		})
	}
}

func TestRelayPOSTResumptionPreservesReadAndCloseErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		read, close bool
	}{{"read", true, false}, {"close", false, true}, {"both", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				readCause, closeCause := errors.New("original read failed"), errors.New("original close failed")
				initial := &relayResumeBody{reader: strings.NewReader("id: cursor\nretry: 0\n\n")}
				if tc.read {
					initial.readErr = readCause
				}
				if tc.close {
					initial.closeErr = closeCause
				}
				post, get := 0, 0
				r := &HTTPToStdioRelay{URL: "http://relay.invalid/mcp", Stdout: io.Discard, Stderr: io.Discard, sessionID: "original", protocolVersion: "2025-11-25"}
				r.HTTPClient = &http.Client{Transport: relayResumeTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodPost {
						post++
						return relayResumeResponse(req, initial), nil
					}
					get++
					return relayResumeResponse(req, io.NopCloser(strings.NewReader("data: "+relayResumeResult+"\n\n"))), nil
				})}
				r.handlePOST(context.Background(), []byte(relayResumeRequest))
				if initial.closes != 1 || post != 1 {
					t.Fatalf("original body/POST ownership differs: closes=%d POST=%d", initial.closes, post)
				}
				if tc.close {
					if get != 0 || !errors.Is(r.postErr, closeCause) || (tc.read && !errors.Is(r.postErr, readCause)) {
						t.Fatalf("read/close causes lost or unsafe continuation: GET=%d error=%v", get, r.postErr)
					}
				} else if get != 1 || r.postErr != nil {
					t.Fatalf("read interruption did not resume: GET=%d error=%v", get, r.postErr)
				}
			})
		})
	}
}

type relayResumeFailWriter struct{}

func (relayResumeFailWriter) Write([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRelayPOSTResumptionRejectsParserAndOutputFailure(t *testing.T) {
	for _, tc := range []struct {
		name, events string
		writer       io.Writer
	}{
		{"line_limit", "data: " + strings.Repeat("x", maxStdinLineBytes+1) + "\n\n", io.Discard},
		{"bad_json", "data: not-json\n\n", io.Discard},
		{"failed_output", "data: " + relayResumeNotice + "\n\n", relayResumeFailWriter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			post, get := 0, 0
			body := &relayResumeBody{reader: strings.NewReader("id: cursor\nretry: 0\n\n" + tc.events)}
			r := &HTTPToStdioRelay{URL: "http://relay.invalid/mcp", Stdout: tc.writer, Stderr: io.Discard, sessionID: "original", protocolVersion: "2025-11-25"}
			r.HTTPClient = &http.Client{Transport: relayResumeTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					post++
					return relayResumeResponse(req, body), nil
				}
				get++
				return nil, errors.New("unsafe parser/output retry")
			})}
			r.handlePOST(context.Background(), []byte(relayResumeRequest))
			if post != 1 || get != 0 || body.closes != 1 || r.postErr == nil {
				t.Fatalf("failure was retried or lost: POST=%d GET=%d closes=%d error=%v", post, get, body.closes, r.postErr)
			}
		})
	}
}

func TestRelayPOSTResumptionKeepsAbsoluteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		post, get := 0, 0
		var bodies []*relayResumeBody
		r := &HTTPToStdioRelay{URL: "http://relay.invalid/mcp", Stdout: io.Discard, Stderr: io.Discard, sessionID: "original", protocolVersion: "2025-11-25"}
		r.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond, Transport: relayResumeTransport(func(req *http.Request) (*http.Response, error) {
			id := "first"
			if req.Method == http.MethodPost {
				post++
			} else {
				get++
				id = "second"
			}
			body := &relayResumeBody{reader: strings.NewReader("id: " + id + "\nretry: 70\n\n")}
			bodies = append(bodies, body)
			return relayResumeResponse(req, body), nil
		})}
		started := time.Now()
		r.handlePOST(context.Background(), []byte(relayResumeRequest))
		if post != 1 || get != 1 || time.Since(started) != 100*time.Millisecond || !errors.Is(r.postErr, context.DeadlineExceeded) {
			t.Fatalf("operation deadline renewed/lost: POST=%d GET=%d elapsed=%s error=%v", post, get, time.Since(started), r.postErr)
		}
		for _, body := range bodies {
			if body.closes != 1 {
				t.Fatalf("response body closed %d times", body.closes)
			}
		}
	})
}
