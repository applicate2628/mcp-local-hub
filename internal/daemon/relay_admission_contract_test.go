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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The original POST cannot finish until the same stdio transport delivers the
// client's response (and, in the saturation case, cancellation). Context
// cancellation releases the fixture on the old deadlocking implementation.
type relayAdmissionGatedReader struct {
	ctx   context.Context
	gates []<-chan struct{}
	io.Reader
}

func (r *relayAdmissionGatedReader) Read(p []byte) (int, error) {
	for _, gate := range r.gates {
		select {
		case <-gate:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	return r.Reader.Read(p)
}

func TestRelayAdmissionKeepsNestedResponsesLive(t *testing.T) {
	for _, tc := range []struct {
		name, session string
		requests      int
	}{
		{"stateless", "", 1},
		{"saturated", "session", maxConcurrentPOSTs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				input, inputWriter := io.Pipe()
				output, outputWriter := io.Pipe()
				allStarted, nestedForwarded := make(chan struct{}), make(chan struct{})
				replied, cancelled := make(chan struct{}), make(chan struct{})
				var requests, replies, cancellations atomic.Int32
				client := &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPost {
						return relayProtocolResponse(req, 405, "", "", ""), nil
					}
					body, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					meta, err := decodeJSONRPCMeta(body)
					if err != nil {
						return nil, err
					}
					if meta.Method == "initialize" {
						return relayProtocolResponse(req, 200, "application/json", tc.session, `{"jsonrpc":"2.0","id":"init","result":{"protocolVersion":"2025-11-25"}}`), nil
					}
					if req.Header.Get(mcpSessionIDHeader) != tc.session || req.Header.Get(mcpProtocolVersionHeader) != "2025-11-25" {
						t.Errorf("negotiated headers: session=%q version=%q", req.Header.Get(mcpSessionIDHeader), req.Header.Get(mcpProtocolVersionHeader))
					}
					switch meta.Method {
					case "notifications/initialized":
						return relayProtocolResponse(req, 202, "", "", ""), nil
					case "":
						if string(meta.ID) != `"nested"` || replies.Add(1) != 1 {
							t.Errorf("unexpected or duplicate nested response: %s", body)
						} else {
							close(replied)
						}
						return relayProtocolResponse(req, 202, "", "", ""), nil
					case "notifications/cancelled":
						if cancellations.Add(1) == 1 {
							close(cancelled)
						}
						return relayProtocolResponse(req, 202, "", "", ""), nil
					case "tools/call":
						if requests.Add(1) == int32(tc.requests) {
							close(allStarted)
						}
						if string(meta.ID) == "999" {
							t.Error("over-capacity request reached the effect owner")
						}
						prefix := ""
						if string(meta.ID) == "1" {
							prefix = "data: {\"jsonrpc\":\"2.0\",\"id\":\"nested\",\"method\":\"sampling/createMessage\",\"params\":{}}\n\n"
						}
						gates := []<-chan struct{}{replied}
						if tc.requests == maxConcurrentPOSTs {
							gates = append(gates, cancelled)
						}
						result := fmt.Sprintf("data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", meta.ID)
						response := relayProtocolResponse(req, 200, "text/event-stream", "", "")
						response.Body = io.NopCloser(io.MultiReader(strings.NewReader(prefix), &relayAdmissionGatedReader{ctx: req.Context(), gates: gates, Reader: strings.NewReader(result)}))
						return response, nil
					default:
						return nil, fmt.Errorf("unexpected method %q", meta.Method)
					}
				})}
				r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: input, Stdout: outputWriter, Stderr: io.Discard, InterruptStdin: inputWriter.Close, HTTPClient: client}
				done := make(chan error, 1)
				go func() { err := r.Run(ctx); _ = outputWriter.CloseWithError(err); done <- err }()
				type capture struct {
					lines []string
					err   error
				}
				captured := make(chan capture, 1)
				go func() {
					var lines []string
					scanner := bufio.NewScanner(output)
					for scanner.Scan() {
						line := scanner.Text()
						lines = append(lines, line)
						if strings.Contains(line, `"method":"sampling/createMessage"`) {
							close(nestedForwarded)
						}
					}
					captured <- capture{lines, scanner.Err()}
				}()
				settled, captureJoined := false, false
				t.Cleanup(func() {
					cancel()
					_ = inputWriter.Close()
					_ = input.Close()
					_ = output.Close()
					_ = outputWriter.Close()
					if !settled {
						<-done
					}
					if !captureJoined {
						<-captured
					}
				})
				var initial strings.Builder
				initial.WriteString("{\"jsonrpc\":\"2.0\",\"id\":\"init\",\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
				for id := 1; id <= tc.requests; id++ {
					fmt.Fprintf(&initial, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"tools/call\"}\n", id)
				}
				if _, err := io.WriteString(inputWriter, initial.String()); err != nil {
					t.Fatal(err)
				}
				for _, ready := range []<-chan struct{}{allStarted, nestedForwarded} {
					select {
					case <-ready:
					case <-ctx.Done():
						t.Fatal("request/stdio barrier did not complete")
					}
				}
				control := ""
				if tc.requests == maxConcurrentPOSTs {
					control = "{\"jsonrpc\":\"2.0\",\"id\":999,\"method\":\"tools/call\"}\n"
				}
				control += "{\"jsonrpc\":\"2.0\",\"id\":\"nested\",\"result\":{}}\n"
				if tc.requests == maxConcurrentPOSTs {
					control += "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":1}}\n"
				}
				if _, err := io.WriteString(inputWriter, control); err != nil {
					t.Fatal(err)
				}
				_ = inputWriter.Close()
				err := <-done
				settled = true
				got := <-captured
				captureJoined = true
				if err != nil || got.err != nil {
					t.Fatalf("transport did not settle cleanly: Run=%v capture=%v", err, got.err)
				}
				if requests.Load() != int32(tc.requests) || replies.Load() != 1 {
					t.Fatalf("delivered requests=%d replies=%d", requests.Load(), replies.Load())
				}
				results, refusals := 0, 0
				for _, line := range got.lines {
					var reply struct {
						ID     json.RawMessage `json:"id"`
						Result json.RawMessage `json:"result"`
						Error  json.RawMessage `json:"error"`
					}
					if err := json.Unmarshal([]byte(line), &reply); err != nil {
						t.Fatal(err)
					}
					if string(reply.ID) == "999" && len(reply.Error) != 0 && strings.Contains(line, "was not forwarded") {
						refusals++
					} else if len(reply.Result) != 0 && string(reply.ID) != `"init"` {
						results++
					}
				}
				if results != tc.requests || (tc.requests == maxConcurrentPOSTs && (refusals != 1 || cancellations.Load() != 1)) {
					t.Fatalf("results=%d refusals=%d cancellations=%d", results, refusals, cancellations.Load())
				}
			})
		})
	}
}

func TestRelaySessionExpiryStopsGETAndClearsNextInitialization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		input, writer := io.Pipe()
		defer input.Close()
		defer writer.Close()
		var gets atomic.Int32
		r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, InterruptStdin: writer.Close}
		r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				return relayProtocolResponse(req, 200, "application/json", "expired", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`), nil
			}
			if req.Method == http.MethodGet {
				gets.Add(1)
				return relayProtocolResponse(req, 404, "", "", ""), nil
			}
			return relayProtocolResponse(req, 202, "", "", ""), nil
		})}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		if _, err := io.WriteString(writer, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n"); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, errRelaySessionLost) || errors.Is(err, context.DeadlineExceeded) || gets.Load() != 1 {
			t.Fatalf("expired GET: error=%v requests=%d", err, gets.Load())
		}
		r.Stdin = strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n")
		r.InterruptStdin = nil
		r.HTTPClient = &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || req.Header.Get(mcpSessionIDHeader) != "" || req.Header.Get(mcpProtocolVersionHeader) != "" {
				t.Errorf("new lifetime inherited negotiation: method=%s headers=%v", req.Method, req.Header)
			}
			return relayProtocolResponse(req, 200, "application/json", "", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`), nil
		})}
		if err := r.Run(context.Background()); err != nil {
			t.Fatalf("fresh stateless initialization: %v", err)
		}
	})
}

func TestRelaySessionExpiryTerminatesSDKConsumerWithoutReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		input, inputWriter := io.Pipe()
		output, outputWriter := io.Pipe()
		var initializes, effects atomic.Int32
		client := &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				return relayProtocolResponse(req, 405, "", "", ""), nil
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			meta, err := decodeJSONRPCMeta(body)
			if err != nil {
				return nil, err
			}
			switch meta.Method {
			case "initialize":
				initializes.Add(1)
				return relayProtocolResponse(req, 200, "application/json", "expired", fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"expiry-fixture","version":"1"}}}`, meta.ID)), nil
			case "notifications/initialized":
				return relayProtocolResponse(req, 202, "", "", ""), nil
			case "tools/call":
				effects.Add(1)
				return relayProtocolResponse(req, 404, "", "", ""), nil
			default:
				return nil, fmt.Errorf("unexpected method %q", meta.Method)
			}
		})}
		r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdin: input, Stdout: outputWriter, Stderr: io.Discard, InterruptStdin: inputWriter.Close, HTTPClient: client}
		done := make(chan error, 1)
		go func() { err := r.Run(ctx); _ = outputWriter.CloseWithError(err); done <- err }()
		var session *mcp.ClientSession
		settled := false
		t.Cleanup(func() {
			cancel()
			_ = inputWriter.Close()
			_ = input.Close()
			_ = output.Close()
			_ = outputWriter.Close()
			if session != nil {
				_ = session.Close()
			}
			if !settled {
				<-done
			}
		})
		consumer := mcp.NewClient(&mcp.Implementation{Name: "expiry-consumer", Version: "1"}, nil)
		var err error
		session, err = consumer.Connect(ctx, &mcp.IOTransport{Reader: output, Writer: inputWriter}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "effect", Arguments: map[string]any{}}); err == nil {
			t.Error("SDK accepted a result from an expired session")
		}
		err = <-done
		settled = true
		if !errors.Is(err, errRelaySessionLost) || ctx.Err() != nil || initializes.Load() != 1 || effects.Load() != 1 {
			t.Fatalf("session expiry: error=%v context=%v initializes=%d effects=%d", err, ctx.Err(), initializes.Load(), effects.Load())
		}
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "effect", Arguments: map[string]any{}}); err == nil || effects.Load() != 1 {
			t.Fatalf("expired SDK transport remained usable: error=%v effects=%d", err, effects.Load())
		}
	})
}

func TestRelayDoesNotRespondToFailedClientResponse(t *testing.T) {
	var output bytes.Buffer
	r := &HTTPToStdioRelay{URL: "http://relay.fixture/mcp", Stdout: &output, Stderr: io.Discard, HTTPClient: &http.Client{Transport: relayProtocolTransport(func(req *http.Request) (*http.Response, error) {
		return relayProtocolResponse(req, 400, "", "", ""), nil
	})}}
	r.handlePOST(context.Background(), []byte(`{"jsonrpc":"2.0","id":"server-request","result":{}}`))
	if output.Len() != 0 || r.postErr == nil || !strings.Contains(r.postErr.Error(), "HTTP 400") {
		t.Fatalf("response refusal: stdout=%q failure=%v", output.String(), r.postErr)
	}
}
