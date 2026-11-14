package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type relayLoadServer struct {
	server        *httptest.Server
	client        *http.Client
	sessions      atomic.Int64
	calls         atomic.Int64
	notifications atomic.Int64
	deleted       atomic.Int64
}
type relayLoadParams struct {
	Payload string `json:"payload"`
	Stream  bool   `json:"stream"`
}
type relayLoadMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  relayLoadParams `json:"params"`
}
type relayLoadResult struct {
	Payload string `json:"payload"`
	Session string `json:"session"`
}
type relayLoadResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  relayLoadResult `json:"result"`
}

func newRelayLoadServer(tb testing.TB) *relayLoadServer {
	tb.Helper()
	f := &relayLoadServer{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodDelete:
			if r.Header.Get(mcpSessionIDHeader) == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxStdinLineBytes+1))
			var msg relayLoadMessage
			if err != nil || len(raw) > maxStdinLineBytes || json.Unmarshal(raw, &msg) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sid := r.Header.Get(mcpSessionIDHeader)
			if msg.Method == "initialize" {
				if sid != "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				sid = fmt.Sprintf("rx-session-%d", f.sessions.Add(1))
				w.Header().Set(mcpSessionIDHeader, sid)
			} else {
				if !strings.HasPrefix(sid, "rx-session-") {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(msg.ID) == 0 {
					f.notifications.Add(1)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if msg.Method == "fixture/failure" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				f.calls.Add(1)
			}
			response, err := json.Marshal(relayLoadResponse{JSONRPC: "2.0", ID: msg.ID, Result: relayLoadResult{Payload: msg.Params.Payload, Session: sid}})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if msg.Params.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", response)
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(response)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	tb.Cleanup(f.server.Close)
	transport := &http.Transport{MaxIdleConns: 64, MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64}
	f.client = &http.Client{Transport: transport, Timeout: 10 * time.Second}
	tb.Cleanup(transport.CloseIdleConnections)
	return f
}

func relayLoadInput(calls, payloadBytes int, stream bool) []byte {
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	_ = encoder.Encode(relayLoadMessage{JSONRPC: "2.0", ID: json.RawMessage("0"), Method: "initialize"})
	_ = encoder.Encode(relayLoadMessage{JSONRPC: "2.0", Method: "notifications/initialized"})
	for i := 1; i <= calls; i++ {
		id := json.RawMessage(fmt.Sprintf("%d", i))
		if i%2 == 0 {
			id = json.RawMessage(fmt.Sprintf("\"call-%d\"", i))
		}
		_ = encoder.Encode(relayLoadMessage{JSONRPC: "2.0", ID: id, Method: "tools/call", Params: relayLoadParams{Payload: strings.Repeat("x", payloadBytes), Stream: stream}})
	}
	return input.Bytes()
}

func verifyRelayLoadOutput(output []byte, calls, payloadBytes int) error {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxStdinLineBytes)
	seen := make(map[string]bool)
	sid := ""
	for scanner.Scan() {
		var got relayLoadResponse
		if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
			return err
		}
		id := string(got.ID)
		if got.JSONRPC != "2.0" || len(id) == 0 || seen[id] {
			return fmt.Errorf("invalid/duplicate response ID %q", id)
		}
		seen[id] = true
		if id == "0" {
			sid = got.Result.Session
			if !strings.HasPrefix(sid, "rx-session-") {
				return fmt.Errorf("invalid initialized session %q", sid)
			}
			continue
		}
		if sid == "" || got.Result.Session != sid || got.Result.Payload != strings.Repeat("x", payloadBytes) {
			return fmt.Errorf("response %s lost payload or crossed sessions", id)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(seen) != calls+1 {
		return fmt.Errorf("responses=%d want=%d", len(seen), calls+1)
	}
	for i := 1; i <= calls; i++ {
		id := fmt.Sprintf("%d", i)
		if i%2 == 0 {
			id = fmt.Sprintf("\"call-%d\"", i)
		}
		if !seen[id] {
			return fmt.Errorf("missing response %s", id)
		}
	}
	return nil
}

func TestRelayLoadContract(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, clients := range []int{1, 8, 32} {
			t.Run(fmt.Sprintf("sse_%t/clients_%d", stream, clients), func(t *testing.T) {
				f := newRelayLoadServer(t)
				input := relayLoadInput(8, 1024, stream)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				failures := make(chan error, clients)
				var wg sync.WaitGroup
				for i := 0; i < clients; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						var output bytes.Buffer
						r := HTTPToStdioRelay{URL: f.server.URL, Stdin: bytes.NewReader(input), Stdout: &output, Stderr: io.Discard, HTTPClient: f.client}
						if err := r.Run(ctx); err != nil {
							failures <- err
							return
						}
						failures <- verifyRelayLoadOutput(output.Bytes(), 8, 1024)
					}()
				}
				wg.Wait()
				close(failures)
				for err := range failures {
					if err != nil {
						t.Error(err)
					}
				}
				if f.sessions.Load() != int64(clients) || f.calls.Load() != int64(clients*8) || f.notifications.Load() != int64(clients) || f.deleted.Load() != int64(clients) {
					t.Fatalf("server counts: sessions=%d calls=%d notifications=%d deleted=%d", f.sessions.Load(), f.calls.Load(), f.notifications.Load(), f.deleted.Load())
				}
			})
		}
	}
}

func TestRelayLoadHTTPErrorPreservesID(t *testing.T) {
	f := newRelayLoadServer(t)
	input := append(relayLoadInput(0, 0, false), []byte("{\"jsonrpc\":\"2.0\",\"id\":\"failed-call\",\"method\":\"fixture/failure\"}\n")...)
	var output bytes.Buffer
	r := HTTPToStdioRelay{URL: f.server.URL, Stdin: bytes.NewReader(input), Stdout: &output, Stderr: io.Discard, HTTPClient: f.client}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(&output)
	found := false
	for scanner.Scan() {
		var response struct {
			ID    json.RawMessage `json:"id"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if string(response.ID) == `"failed-call"` {
			if response.Error == nil || !strings.Contains(response.Error.Message, "503") {
				t.Fatalf("missing typed failure: %s", scanner.Text())
			}
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("lost failed request response")
	}
}

// Validate the oracle itself: equal-length corruption and duplicate IDs must
// fail; byte-count-only checks would incorrectly accept either result.
func TestRelayLoadOracleRejectsCorruption(t *testing.T) {
	valid := []byte("{\"jsonrpc\":\"2.0\",\"id\":0,\"result\":{\"session\":\"rx-session-1\",\"payload\":\"\"}}\n{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"session\":\"rx-session-1\",\"payload\":\"xxx\"}}\n")
	if err := verifyRelayLoadOutput(valid, 1, 3); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{bytes.Replace(valid, []byte("xxx"), []byte("xYx"), 1), append(append([]byte{}, valid...), valid...), bytes.Replace(valid, []byte(`"id":1`), []byte(`"id":9`), 1)} {
		if verifyRelayLoadOutput(bad, 1, 3) == nil {
			t.Fatal("oracle accepted corrupted response")
		}
	}
}

// Barriers, rather than a timing sleep, prove stdin has ended while the final
// response is still pending. Run must not destroy that response on normal EOF.
func TestRelayLoadEOFDrainsFinalResponse(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	eof := make(chan struct{})
	var releaseOnce sync.Once
	f := newFakeServer(t)
	f.onPOST = func(body []byte, _ string) (int, string, []byte) {
		var meta struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(body, &meta); err != nil {
			return 400, "", nil
		}
		if meta.ID == 2 {
			close(entered)
			<-release
		}
		return 200, "application/json", []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, meta.ID))
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	input := &relayLoadEOFReader{Reader: strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\"}\n"), ended: eof}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	r := HTTPToStdioRelay{URL: srv.URL, Stdin: input, Stdout: &output, Stderr: io.Discard, HTTPClient: srv.Client()}
	go func() { done <- r.Run(ctx) }()
	defer releaseOnce.Do(func() { close(release) })
	for _, barrier := range []<-chan struct{}{entered, eof} {
		select {
		case <-barrier:
		case <-ctx.Done():
			releaseOnce.Do(func() { close(release) })
			<-done
			t.Fatal("fixture did not reach response/EOF barriers")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("returned before final response release: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("drain did not finish")
	}
	if !strings.Contains(output.String(), `"id":2`) || f.deleteCount.Load() != 1 {
		t.Fatalf("final response/delete missing: %q", output.String())
	}
}

type relayLoadEOFReader struct {
	*strings.Reader
	ended chan struct{}
	once  sync.Once
}

func (r *relayLoadEOFReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.once.Do(func() { close(r.ended) })
	}
	return n, err
}
