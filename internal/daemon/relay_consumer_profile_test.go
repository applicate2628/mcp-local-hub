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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type relayConsumerEventStore struct {
	mcp.EventStore
	closed     chan string
	closeCalls atomic.Int32
}

func (s *relayConsumerEventStore) SessionClosed(ctx context.Context, id string) error {
	err := s.EventStore.SessionClosed(ctx, id)
	s.closeCalls.Add(1)
	select {
	case s.closed <- id:
	default:
	}
	return err
}

// This wrapper injects only transport control/faults into the SDK's real GET
// stream. SDK event IDs, persisted messages, replay, and JSON-RPC consumption
// stay intact. The SDK's public CloseSSEStream applies to a request stream; this
// test deliberately does not claim to resume a POST-origin stream.
type relayConsumerCutStream struct {
	http.ResponseWriter
	cancel             context.CancelFunc
	ready              chan struct{}
	readyOnce          sync.Once
	retry              time.Duration
	cutAfter, messages int
	lastID             string
}

func (w *relayConsumerCutStream) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *relayConsumerCutStream) Flush() {
	w.readyOnce.Do(func() {
		_, _ = fmt.Fprintf(w.ResponseWriter, "retry: %d\n\n", w.retry.Milliseconds())
		_ = http.NewResponseController(w.ResponseWriter).Flush()
		close(w.ready)
	})
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *relayConsumerCutStream) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		return n, err
	}
	for _, line := range strings.Split(string(p), "\n") {
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			w.lastID = id
		}
	}
	if bytes.Contains(p, []byte(`"method":"notifications/message"`)) {
		w.messages++
		if w.messages == w.cutAfter {
			_ = http.NewResponseController(w.ResponseWriter).Flush()
			w.cancel()
		}
	}
	return n, nil
}

// One real SDK client owns stdio pipes; Relay.Run owns its HTTP workers; the
// test owns the loopback SDK server and MemoryEventStore. This finite profile
// checks headers, GET cursor resumption, delivery, and settlement. It is not a
// peak/retained-memory benchmark, installed Product, or external-client claim.
func TestRelayConsumerGETSSEResume(t *testing.T) {
	const messages, beforeCut = 32, 4
	const retryDelay = 75 * time.Millisecond
	const payload = "consumer payload with escaped newline\nand escaped backslash\\"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initialized := make(chan *mcp.ServerSession, 1)
	server := mcp.NewServer(&mcp.Implementation{Name: "relay-get-consumer", Version: "1"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}},
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			select {
			case initialized <- req.Session:
			default:
			}
		},
	})
	store := &relayConsumerEventStore{EventStore: mcp.NewMemoryEventStore(nil), closed: make(chan string, 1)}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{EventStore: store})
	type exchange struct {
		method, version, session, cursor string
		at                               time.Time
	}
	var mu sync.Mutex
	var exchanges []exchange
	var gets, active atomic.Int32
	firstReady, firstReturned := make(chan struct{}), make(chan struct{})
	secondArrived, allowResume := make(chan struct{}), make(chan struct{})
	var allowOnce sync.Once
	releaseResume := func() { allowOnce.Do(func() { close(allowResume) }) }
	var firstEnded time.Time
	var firstCursor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		mu.Lock()
		if len(exchanges) < 32 {
			exchanges = append(exchanges, exchange{req.Method, req.Header.Get("MCP-Protocol-Version"), req.Header.Get("MCP-Session-Id"), req.Header.Get("Last-Event-ID"), time.Now()})
		}
		mu.Unlock()
		if req.Method == http.MethodGet {
			switch gets.Add(1) {
			case 1:
				streamCtx, endStream := context.WithCancel(req.Context())
				defer endStream()
				stream := &relayConsumerCutStream{ResponseWriter: w, cancel: endStream, ready: firstReady, retry: retryDelay, cutAfter: beforeCut}
				handler.ServeHTTP(stream, req.WithContext(streamCtx))
				firstCursor, firstEnded = stream.lastID, time.Now()
				close(firstReturned)
				return
			case 2:
				close(secondArrived)
				select {
				case <-allowResume:
				case <-req.Context().Done():
					return
				}
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
		}
		handler.ServeHTTP(w, req)
	}))
	transport := &http.Transport{Proxy: nil}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	var stdio bytes.Buffer
	r := &HTTPToStdioRelay{
		URL: srv.URL, Stdin: inputReader, Stdout: io.MultiWriter(&stdio, outputWriter), Stderr: io.Discard,
		InterruptStdin: inputWriter.Close, HTTPClient: &http.Client{Transport: transport},
	}
	relayDone := make(chan error, 1)
	go func() {
		err := r.Run(ctx)
		_ = outputWriter.CloseWithError(err)
		relayDone <- err
	}()
	type delivery struct {
		sequence int
		payload  string
		err      error
	}
	delivered := make(chan delivery, messages*2)
	var deliveryOverflow atomic.Int32
	client := mcp.NewClient(&mcp.Implementation{Name: "relay-stdio-consumer", Version: "1"}, &mcp.ClientOptions{
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			var data struct {
				Sequence int    `json:"sequence"`
				Payload  string `json:"payload"`
			}
			raw, err := json.Marshal(req.Params.Data)
			if err == nil {
				err = json.Unmarshal(raw, &data)
			}
			select {
			case delivered <- delivery{data.Sequence, data.Payload, err}:
			default:
				deliveryOverflow.Add(1)
			}
		},
	})
	var session *mcp.ClientSession
	settled := false
	t.Cleanup(func() {
		cancel()
		releaseResume()
		_ = inputWriter.Close()
		_ = inputReader.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		if session != nil {
			_ = session.Close()
		}
		for ss := range server.Sessions() {
			_ = ss.Close()
		}
		if !settled {
			select {
			case err := <-relayDone:
				t.Logf("failed-path relay settled: %v", err)
			case <-time.After(3 * time.Second):
				t.Error("owned relay did not settle after closing its pipes and contexts")
			}
		}
		transport.CloseIdleConnections()
		srv.CloseClientConnections()
		srv.Close()
	})
	var err error
	session, err = client.Connect(ctx, &mcp.IOTransport{Reader: outputReader, Writer: inputWriter}, nil)
	if err != nil {
		t.Fatalf("SDK initialize over stdio/relay/HTTP SSE: %v", err)
	}
	var serverSession *mcp.ServerSession
	select {
	case serverSession = <-initialized:
	case <-ctx.Done():
		t.Fatal("SDK initialized notification was not observed")
	}
	if err := session.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: "info"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstReady:
	case <-ctx.Done():
		t.Fatal("initial GET stream did not open")
	}
	send := func(sequence int) {
		t.Helper()
		if err := serverSession.Log(ctx, &mcp.LoggingMessageParams{Level: "info", Logger: "relay-consumer", Data: map[string]any{"sequence": sequence, "payload": payload}}); err != nil {
			t.Fatalf("SDK notification %d: %v", sequence, err)
		}
	}
	for i := 1; i <= beforeCut; i++ {
		send(i)
	}
	select {
	case <-firstReturned:
	case <-ctx.Done():
		t.Fatal("injected GET disconnect did not settle")
	}
	// Subsequent notifications enter the actual EventStore while GET is absent.
	// The second GET cannot acquire/replay until all accepted messages are stored.
	for i := beforeCut + 1; i <= messages; i++ {
		send(i)
	}
	select {
	case <-secondArrived:
	case <-ctx.Done():
		t.Fatal("GET did not reconnect")
	}
	releaseResume()
	seen := make(map[int]int)
	for i := 0; i < messages; i++ {
		select {
		case item := <-delivered:
			if item.err != nil || item.payload != payload {
				t.Fatalf("SDK payload: %+v", item)
			}
			seen[item.sequence]++
		case <-ctx.Done():
			t.Fatalf("SDK received %d of %d notifications", len(seen), messages)
		}
	}
	if err := session.Ping(ctx, nil); err != nil {
		t.Fatalf("session after GET replay: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("SDK client close: %v", err)
	}
	select {
	case err := <-relayDone:
		settled = true
		if err != nil {
			t.Fatalf("relay settlement: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("relay did not settle after client close")
	}
	select {
	case <-store.closed:
	case <-ctx.Done():
		t.Fatal("SDK EventStore did not observe session cleanup")
	}
	if err := serverSession.Wait(); err != nil {
		t.Fatalf("SDK server session settlement: %v", err)
	}
	// Wait for HTTP handler defers too, before inspecting active count/headers.
	srv.CloseClientConnections()
	srv.Close()
	for len(delivered) > 0 {
		item := <-delivered
		seen[item.sequence]++
	}
	for i := 1; i <= messages; i++ {
		if seen[i] != 1 {
			t.Errorf("notification %d observed %d times", i, seen[i])
		}
	}
	if len(seen) != messages || deliveryOverflow.Load() != 0 || active.Load() != 0 || gets.Load() != 2 || store.closeCalls.Load() != 1 {
		t.Errorf("settlement/delivery: unique=%d overflow=%d active HTTP=%d GETs=%d store closes=%d", len(seen), deliveryOverflow.Load(), active.Load(), gets.Load(), store.closeCalls.Load())
	}
	for range server.Sessions() {
		t.Error("SDK server retained an open session")
	}
	mu.Lock()
	observed := append([]exchange(nil), exchanges...)
	mu.Unlock()
	var reconnectAt time.Time
	deleteCount, initializeCount, getCount := 0, 0, 0
	for _, e := range observed {
		if e.method == http.MethodPost && e.session == "" {
			initializeCount++
			if e.version != "" {
				t.Errorf("initial POST version=%q", e.version)
			}
			continue
		}
		if e.version != "2025-11-25" || e.session == "" {
			t.Errorf("subsequent request missing negotiated metadata: %+v", e)
		}
		switch e.method {
		case http.MethodGet:
			getCount++
			if getCount == 1 && e.cursor != "" {
				t.Errorf("fresh GET cursor=%q", e.cursor)
			}
			if getCount == 2 {
				reconnectAt = e.at
				if e.cursor != firstCursor || !strings.HasPrefix(e.cursor, "_") {
					t.Errorf("GET resume cursor=%q; delivered main-stream cursor=%q", e.cursor, firstCursor)
				}
			}
		case http.MethodDelete:
			deleteCount++
		}
	}
	if initializeCount != 1 || deleteCount != 1 || reconnectAt.Sub(firstEnded) < retryDelay {
		t.Errorf("initialize=%d DELETE=%d reconnect delay=%s; server minimum=%s", initializeCount, deleteCount, reconnectAt.Sub(firstEnded), retryDelay)
	}
	// Confirm the relay's actual stdio frame order independently of callback
	// scheduling within the SDK consumer.
	scanner := bufio.NewScanner(bytes.NewReader(stdio.Bytes()))
	var order []int
	for scanner.Scan() {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Data struct {
					Sequence int `json:"sequence"`
				} `json:"data"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			t.Fatalf("stdio JSON frame: %v", err)
		}
		if msg.Method == "notifications/message" {
			order = append(order, msg.Params.Data.Sequence)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(order) != messages {
		t.Fatalf("stdio notification frames=%d; want %d", len(order), messages)
	}
	for i, sequence := range order {
		if sequence != i+1 {
			t.Errorf("stdio frame %d has sequence %d", i, sequence)
		}
	}
	t.Logf("real SDK GET-SSE consumer: sent=%d delivered=%d GETs=%d cursor=%q server_retry=%s observed_reconnect=%s active_HTTP_after_close=%d", messages, len(order), gets.Load(), firstCursor, retryDelay, reconnectAt.Sub(firstEnded), active.Load())
}
