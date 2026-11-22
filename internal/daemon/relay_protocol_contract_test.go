package daemon

import (
	"bytes"
	"context"
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

type relayProtocolTransport func(*http.Request) (*http.Response, error)

func (f relayProtocolTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func relayProtocolResponse(req *http.Request, status int, media, sid, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", media)
	if sid != "" {
		header.Set("MCP-Session-Id", sid)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

// The control build uses the exact preceding relay implementation and this
// same test file. Tests therefore observe HTTP/output behavior, not new fields
// or parser signatures. Every fake exchange has a finite terminal outcome.
func TestRelayProtocolVersionHeaders(t *testing.T) {
	for _, media := range []string{"application/json", "text/event-stream"} {
		for _, sid := range []string{"session", ""} {
			t.Run(media+"/session_"+sid, func(t *testing.T) {
				const negotiated = "2025-06-18"
				var methods, versions, sessions []string
				posts := 0
				client := &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					methods = append(methods, req.Method)
					versions = append(versions, req.Header.Get("MCP-Protocol-Version"))
					sessions = append(sessions, req.Header.Get("MCP-Session-Id"))
					if req.Method == http.MethodPost {
						posts++
						if posts == 1 {
							body := `{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18"}}`
							if media == "text/event-stream" {
								body = "event: prime\nid: initial\ndata:\n\n" +
									"data: {\"jsonrpc\":\"2.0\",\"id\":\"other\",\"result\":{\"protocolVersion\":\"2025-03-26\"}}\n\n" +
									"data: " + body + "\n\n" +
									"data: {\"jsonrpc\":\"2.0\",\"id\":\"initialize\",\"result\":{\"protocolVersion\":\"2025-11-25\"}}\n\n"
							}
							return relayProtocolResponse(req, 200, media, sid, body), nil
						}
						return relayProtocolResponse(req, 202, "", "late-header-must-not-establish-session", ""), nil
					}
					return relayProtocolResponse(req, 405, "", "", ""), nil
				})}
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client}
				r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","id":"initialize","method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
				r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
				if sid != "" {
					ctx, cancel := context.WithCancel(context.Background())
					if r.getSessionID() == "" {
						cancel()
					}
					if err := r.sseListener(ctx); err != nil {
						t.Errorf("GET listener: %v", err)
					}
					cancel()
				}
				r.terminateSession()
				wantCount := 2
				if sid != "" {
					wantCount = 4
				}
				if len(methods) != wantCount {
					t.Fatalf("methods=%v; want %d requests", methods, wantCount)
				}
				for i, version := range versions {
					wantVersion, wantSession := negotiated, sid
					if i == 0 {
						wantVersion, wantSession = "", ""
					}
					if version != wantVersion || sessions[i] != wantSession {
						t.Errorf("request %d %s: version=%q session=%q; want %q %q", i, methods[i], version, sessions[i], wantVersion, wantSession)
					}
				}
			})
		}
	}
}

func TestRelayProtocolVersionMatchesJSONID(t *testing.T) {
	for _, tc := range []struct {
		name, requestID, responseID string
		match                       bool
	}{
		{"escaped_string", `"handshake"`, `"hand\u0073hake"`, true},
		{"unicode_string", `"\ud83d\ude00"`, `"😀"`, true},
		{"empty_string", `""`, `""`, true},
		{"large_integer", `9007199254740993`, `9007199254740993`, true},
		{"different_large_integer", `9007199254740993`, `9007199254740992`, false},
		{"decimal_integer", `1.25e2`, `125`, true},
		{"fraction_is_not_mcp_id", `1.25`, `1.25`, false},
		{"exponent_integer", `1e+03`, `1000`, true},
		{"signed_zero", `-0`, `0.000e99`, true},
		{"unexpanded_exponent", `10e99999999999999999999999999999`, `1e100000000000000000000000000000`, true},
		{"long_positive_exponent_borrow", "0.1e1000000000000000000", "1e999999999999999999", true},
		{"bounded_request_exponent_carry", "10e" + strings.Repeat("9", maxStdinLineBytes-256), "1e1" + strings.Repeat("0", maxStdinLineBytes-256), true},
		{"bounded_response_exponent_zeros", "10", "1e" + strings.Repeat("0", int(maxRelayResponseBytes)-256) + "1", true},
		{"long_negative_exponent_borrow", "1e-1000000000000000000", "10e-1000000000000000001", false},
		{"different_exponent", `1e99999999999999999999999999999`, `1e100000000000000000000000000000`, false},
		{"string_is_not_number", `"1"`, `1`, false},
		{"different_string", `"initialize"`, `"other"`, false},
		{"null_is_not_request_id", `null`, `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			observed := ""
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard}
			r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete {
					return relayProtocolResponse(req, 200, "", "", ""), nil
				}
				posts++
				if posts == 1 {
					body := `{"jsonrpc":"2.0","id":` + tc.responseID + `,"result":{"protocolVersion":"2025-11-25"}}`
					return relayProtocolResponse(req, 200, "application/json", "session", body), nil
				}
				observed = req.Header.Get("MCP-Protocol-Version")
				return relayProtocolResponse(req, 202, "", "", ""), nil
			})}
			r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","id":`+tc.requestID+`,"method":"initialize"}`))
			r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
			want := ""
			if tc.match {
				want = "2025-11-25"
			}
			if observed != want || (r.getSessionID() != "") != tc.match {
				t.Fatalf("version=%q session-present=%t; want %q %t", observed, r.getSessionID() != "", want, tc.match)
			}
		})
	}
}

func TestRelayProtocolVersionRejectsUnrelatedResponses(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty_result", `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{"null_result", `{"jsonrpc":"2.0","id":1,"result":null}`},
		{"rpc_error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"failed"}}`},
		{"result_and_error", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"},"error":null}`},
		{"wrong_id", `{"jsonrpc":"2.0","id":2,"result":{"protocolVersion":"2025-11-25"}}`},
		{"missing_id", `{"jsonrpc":"2.0","result":{"protocolVersion":"2025-11-25"}}`},
		{"notification", `{"jsonrpc":"2.0","id":1,"method":"notice","result":{"protocolVersion":"2025-11-25"}}`},
		{"wrong_jsonrpc", `{"jsonrpc":"1.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`},
		{"empty_version", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":""}}`},
		{"nonstring_version", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":20251125}}`},
		{"invalid_date", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-99-99"}}`},
		{"header_injection", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25\r\nInjected: yes"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			observed := ""
			var output bytes.Buffer
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: &output, Stderr: io.Discard}
			r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete {
					return relayProtocolResponse(req, 200, "", "", ""), nil
				}
				posts++
				if posts == 1 {
					return relayProtocolResponse(req, 200, "application/json", "session", tc.body), nil
				}
				observed = req.Header.Get("MCP-Protocol-Version")
				return relayProtocolResponse(req, 202, "", "late-header-must-not-establish-session", ""), nil
			})}
			r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
			r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
			if observed != "" || r.getSessionID() != "" || output.String() != tc.body+"\n" {
				t.Fatalf("unrelated/invalid result established session: version=%q session=%q output=%q", observed, r.getSessionID(), output.String())
			}
		})
	}
}

type relayProtocolGatedBody struct {
	io.Reader
	entered, release chan struct{}
	started          bool
}

func (b *relayProtocolGatedBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		close(b.entered)
		<-b.release
	}
	return b.Reader.Read(p)
}
func (b *relayProtocolGatedBody) Close() error { return nil }

type relayProtocolGatedWriter struct {
	bytes.Buffer
	entered, release chan struct{}
	fail             error
}

func (w *relayProtocolGatedWriter) Write(p []byte) (int, error) {
	if bytes.Equal(p, []byte("\n")) {
		if w.entered != nil {
			close(w.entered)
			<-w.release
		}
		if w.fail != nil {
			return 0, w.fail
		}
	}
	return w.Buffer.Write(p)
}

func TestRelayProtocolVersionWaitsForCompleteOutput(t *testing.T) {
	for _, media := range []string{"application/json", "text/event-stream"} {
		t.Run(media, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				wire := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`
				if media == "text/event-stream" {
					wire = "data: " + wire + "\n\n"
				}
				body := &relayProtocolGatedBody{Reader: strings.NewReader(wire), entered: make(chan struct{}), release: make(chan struct{})}
				output := &relayProtocolGatedWriter{entered: make(chan struct{}), release: make(chan struct{})}
				var gets atomic.Int32
				version := ""
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: output, Stderr: io.Discard}
				r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodPost {
						response := relayProtocolResponse(req, 200, media, "session", "")
						response.Body = body
						return response, nil
					}
					gets.Add(1)
					version = req.Header.Get("MCP-Protocol-Version")
					return relayProtocolResponse(req, 405, "", "", ""), nil
				})}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				posted, listened := make(chan struct{}), make(chan error, 1)
				go func() { r.handlePOST(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); close(posted) }()
				go func() { listened <- r.sseListener(ctx) }()
				<-body.entered
				time.Sleep(300 * time.Millisecond)
				synctest.Wait()
				beforeBody := gets.Load()
				close(body.release)
				<-output.entered
				time.Sleep(300 * time.Millisecond)
				synctest.Wait()
				beforeNewline := gets.Load()
				close(output.release)
				<-posted
				time.Sleep(100 * time.Millisecond)
				synctest.Wait()
				cancel()
				err := <-listened
				if beforeBody != 0 || beforeNewline != 0 || gets.Load() != 1 || version != "2025-11-25" || err != nil {
					t.Fatalf("GET before body=%d before complete stdout=%d total=%d version=%q error=%v", beforeBody, beforeNewline, gets.Load(), version, err)
				}
			})
		})
	}
}

func TestRelayProtocolVersionOutputFailureDoesNotPublishSession(t *testing.T) {
	for _, media := range []string{"application/json", "text/event-stream"} {
		t.Run(media, func(t *testing.T) {
			cause := errors.New("initialize newline rejected")
			output := &relayProtocolGatedWriter{fail: cause}
			deletes, cleanupVersion := 0, ""
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: output, Stderr: io.Discard}
			r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete {
					deletes++
					cleanupVersion = req.Header.Get("MCP-Protocol-Version")
					return relayProtocolResponse(req, 200, "", "", ""), nil
				}
				wire := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`
				if media == "text/event-stream" {
					wire = "data: " + wire + "\n\n"
				}
				return relayProtocolResponse(req, 200, media, "session", wire), nil
			})}
			r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
			if r.getSessionID() != "" || !errors.Is(r.stdoutFailure(), cause) || deletes != 1 || cleanupVersion != "2025-11-25" {
				t.Fatalf("failed initialize: session=%q error=%v DELETE=%d cleanup version=%q", r.getSessionID(), r.stdoutFailure(), deletes, cleanupVersion)
			}
		})
	}
	t.Run("bounded_cleanup_joins_run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cause := errors.New("initialize output failed before cleanup")
			deletes := 0
			cleanupStartedCanceled := false
			r := &HTTPToStdioRelay{
				URL:    "http://relay.fixture/mcp",
				Stdin:  strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n"),
				Stdout: &relayProtocolGatedWriter{fail: cause}, Stderr: io.Discard,
			}
			r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete {
					deletes++
					cleanupStartedCanceled = req.Context().Err() != nil
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				if req.Method == http.MethodGet {
					return relayProtocolResponse(req, 405, "", "", ""), nil
				}
				return relayProtocolResponse(req, 200, "application/json", "session", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`), nil
			})}
			start := time.Now()
			err := r.Run(context.Background())
			if !errors.Is(err, cause) || deletes != 1 || cleanupStartedCanceled || time.Since(start) != 2*time.Second || r.getSessionID() != "" {
				t.Fatalf("cleanup ownership/bound: error=%v deletes=%d inherited cancellation=%t elapsed=%s session=%q", err, deletes, cleanupStartedCanceled, time.Since(start), r.getSessionID())
			}
		})
	})
}

func TestRelayProtocolVersionResetsBetweenRuns(t *testing.T) {
	for _, sid := range []string{"session", ""} {
		t.Run("session_"+sid, func(t *testing.T) {
			r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard}
			for _, negotiated := range []string{"2025-06-18", "2026-07-28"} {
				posts := 0
				var versions []string
				r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPost {
						return relayProtocolResponse(req, 405, "", "", ""), nil
					}
					posts++
					versions = append(versions, req.Header.Get("MCP-Protocol-Version"))
					if posts == 1 {
						return relayProtocolResponse(req, 200, "application/json", sid, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"`+negotiated+`"}}`), nil
					}
					return relayProtocolResponse(req, 202, "", "", ""), nil
				})}
				r.Stdin = strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
				if err := r.Run(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(versions) != 2 || versions[0] != "" || versions[1] != negotiated || r.getSessionID() != "" {
					t.Fatalf("run version=%q observed=%v remaining session=%q", negotiated, versions, r.getSessionID())
				}
			}
		})
	}
}

func TestRelayServerRetryFields(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		delay       time.Duration
	}{
		{"default", "", time.Second},
		{"milliseconds", "retry: 37\n\n", 37 * time.Millisecond},
		{"zero", "retry:0\n\n", 0},
		{"leading_zeros", "retry: 000037\n\n", 37 * time.Millisecond},
		{"many_leading_zeros", "retry: " + strings.Repeat("0", 4096) + "37\n\n", 37 * time.Millisecond},
		{"without_blank_line", "retry:37\n", 37 * time.Millisecond},
		{"field_at_eof", "retry:37", 37 * time.Millisecond},
		{"last_valid_wins", "retry: 12\nretry:37\n", 37 * time.Millisecond},
		{"empty", "retry:\n", time.Second},
		{"bare_field", "retry\n", time.Second},
		{"two_spaces", "retry:  37\n", time.Second},
		{"trailing_space", "retry:37 \n", time.Second},
		{"tab", "retry:\t37\n", time.Second},
		{"plus", "retry:+37\n", time.Second},
		{"negative", "retry:-37\n", time.Second},
		{"fraction", "retry:3.7\n", time.Second},
		{"exponent", "retry:37e0\n", time.Second},
		{"unicode_digits", "retry:３７\n", time.Second},
		{"case_sensitive", "Retry:37\n", time.Second},
		{"invalid_keeps_valid", "retry:37\nretry:bad\n", 37 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var instants []time.Time
				resume := ""
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard}
				r.setSessionID("session")
				r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					instants = append(instants, time.Now())
					if len(instants) == 1 {
						wire := "id: committed\n\n" + tc.field
						if tc.field == "" || strings.HasSuffix(tc.field, "\n") {
							wire += "id: pending\n"
						}
						return relayProtocolResponse(req, 200, "text/event-stream", "", wire), nil
					}
					resume = req.Header.Get("Last-Event-ID")
					return relayProtocolResponse(req, 405, "", "", ""), nil
				})}
				if err := r.sseListener(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(instants) != 2 || instants[1].Sub(instants[0]) != tc.delay || resume != "committed" {
					t.Fatalf("instants=%v cursor=%q; want delay=%s and committed cursor", instants, resume, tc.delay)
				}
			})
		})
	}
}

func TestRelayServerRetryPersistsAcrossReconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var instants []time.Time
		r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard}
		r.setSessionID("session")
		r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
			instants = append(instants, time.Now())
			switch len(instants) {
			case 1:
				return relayProtocolResponse(req, 200, "text/event-stream", "", "retry: 23\n\n"), nil
			case 2:
				return relayProtocolResponse(req, 200, "text/event-stream", "", "retry: invalid\n\n"), nil
			case 3:
				return nil, errors.New("transient GET failure")
			case 4:
				return relayProtocolResponse(req, 503, "", "", ""), nil
			default:
				return relayProtocolResponse(req, 405, "", "", ""), nil
			}
		})}
		if err := r.sseListener(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(instants) != 5 {
			t.Fatalf("attempts=%d; want 5", len(instants))
		}
		for i := 1; i < len(instants); i++ {
			if got := instants[i].Sub(instants[i-1]); got != 23*time.Millisecond {
				t.Errorf("retry %d waited %s", i, got)
			}
		}
	})
}

func TestRelayServerRetryCancellation(t *testing.T) {
	for _, millis := range []string{"3600000", "9223372036854"} {
		t.Run(millis, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				time.AfterFunc(5*time.Millisecond, cancel)
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard}
				r.setSessionID("session")
				r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					attempts++
					if attempts > 1 {
						return relayProtocolResponse(req, 405, "", "", ""), nil
					}
					return relayProtocolResponse(req, 200, "text/event-stream", "", "retry:"+millis+"\n\n"), nil
				})}
				start := time.Now()
				if err := r.sseListener(ctx); err != nil || attempts != 1 || time.Since(start) != 5*time.Millisecond {
					t.Fatalf("cancellation: error=%v attempts=%d elapsed=%s", err, attempts, time.Since(start))
				}
			})
		})
	}
}

func TestRelayServerRetryOverflowStops(t *testing.T) {
	for _, millis := range []string{"9223372036855", "18446744073709551616", strings.Repeat("9", 4096)} {
		t.Run(fmt.Sprintf("digits_%d", len(millis)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard}
				r.setSessionID("session")
				r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					attempts++
					if attempts > 1 {
						return relayProtocolResponse(req, 405, "", "", ""), nil
					}
					return relayProtocolResponse(req, 200, "text/event-stream", "", "retry:"+millis+"\n\n"), nil
				})}
				start := time.Now()
				err := r.sseListener(context.Background())
				if err == nil || !strings.Contains(err.Error(), "retry exceeds supported duration") || attempts != 1 || time.Since(start) != 0 {
					t.Fatalf("overflow must settle before reconnect: error=%v attempts=%d elapsed=%s", err, attempts, time.Since(start))
				}
			})
		})
	}
}

func TestRelayServerRetryDoesNotRecoverParserFailure(t *testing.T) {
	for _, wire := range []string{"data: {broken}\n\n", "data: {}\n"} {
		t.Run(wire, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: io.Discard, Stderr: io.Discard}
				r.setSessionID("session")
				r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					attempts++
					if attempts > 1 {
						return relayProtocolResponse(req, 405, "", "", ""), nil
					}
					return relayProtocolResponse(req, 200, "text/event-stream", "", "retry: 0\nid: uncommitted\n"+wire), nil
				})}
				if err := r.sseListener(context.Background()); err == nil || attempts != 1 {
					t.Fatalf("parser failure retried: error=%v attempts=%d", err, attempts)
				}
			})
		})
	}
}
