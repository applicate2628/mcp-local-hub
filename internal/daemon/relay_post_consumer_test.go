package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const relayPOSTConsumerResult = "relay-post-result"

type relayPOSTConsumerStore struct {
	*relayConsumerEventStore
	mu                           sync.Mutex
	stored                       chan struct{}
	storedOnce                   sync.Once
	resultSession, resultStream  string
	resultAppends, resultReplays int
}

func (s *relayPOSTConsumerStore) Append(ctx context.Context, session, stream string, data []byte) error {
	if err := s.EventStore.Append(ctx, session, stream, data); err != nil {
		return err
	}
	if bytes.Contains(data, []byte(relayPOSTConsumerResult)) {
		s.mu.Lock()
		s.resultSession, s.resultStream = session, stream
		s.resultAppends++
		s.mu.Unlock()
		s.storedOnce.Do(func() { close(s.stored) })
	}
	return nil
}

func (s *relayPOSTConsumerStore) After(ctx context.Context, session, stream string, index int) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for data, err := range s.EventStore.After(ctx, session, stream, index) {
			if err == nil && bytes.Contains(data, []byte(relayPOSTConsumerResult)) {
				s.mu.Lock()
				if session == s.resultSession && stream == s.resultStream {
					s.resultReplays++
				}
				s.mu.Unlock()
			}
			if !yield(data, err) {
				return
			}
		}
	}
}

// The real SDK client receives a tool result through stdio. The SDK server
// closes the original POST stream and stores the result before the resumed
// GET is allowed to replay it. No test code fabricates event IDs or responses.
func TestRelayPOSTConsumerSDKResumesStoredResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var effects, initializePosts, toolPosts, resumes, active atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "relay-post-server", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "effect"}, func(_ context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		effects.Add(1)
		if req.Extra == nil || req.Extra.CloseSSEStream == nil {
			return nil, nil, fmt.Errorf("SDK did not provide request-stream ownership")
		}
		req.Extra.CloseSSEStream(mcp.CloseSSEStreamArgs{RetryAfter: 10 * time.Millisecond})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: relayPOSTConsumerResult}}}, nil, nil
	})
	store := &relayPOSTConsumerStore{relayConsumerEventStore: &relayConsumerEventStore{EventStore: mcp.NewMemoryEventStore(nil), closed: make(chan string, 1)}, stored: make(chan struct{})}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{EventStore: store})
	postReturned := make(chan struct{})
	var returnedOnce sync.Once
	var mu sync.Mutex
	var postSession, postVersion, resumedSession, resumedVersion, resumedCursor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		if req.Method == http.MethodPost {
			body, err := io.ReadAll(req.Body)
			closeErr := req.Body.Close()
			if err != nil || closeErr != nil {
				t.Errorf("read incoming SDK POST: %v / %v", err, closeErr)
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			var message struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(body, &message); err != nil {
				t.Error(err)
				return
			}
			switch message.Method {
			case "initialize":
				initializePosts.Add(1)
			case "tools/call":
				toolPosts.Add(1)
				mu.Lock()
				postSession, postVersion = req.Header.Get(mcpSessionIDHeader), req.Header.Get(mcpProtocolVersionHeader)
				mu.Unlock()
				defer returnedOnce.Do(func() { close(postReturned) })
			}
		}
		if req.Method == http.MethodGet && req.Header.Get(lastEventIDHeader) != "" {
			resumes.Add(1)
			mu.Lock()
			resumedSession, resumedVersion, resumedCursor = req.Header.Get(mcpSessionIDHeader), req.Header.Get(mcpProtocolVersionHeader), req.Header.Get(lastEventIDHeader)
			mu.Unlock()
			for _, ready := range []<-chan struct{}{postReturned, store.stored} {
				select {
				case <-ready:
				case <-req.Context().Done():
					return
				}
			}
		}
		handler.ServeHTTP(w, req)
	}))
	transport := &http.Transport{Proxy: nil}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	var stdio bytes.Buffer
	relay := &HTTPToStdioRelay{URL: srv.URL, Stdin: inputReader, Stdout: io.MultiWriter(&stdio, outputWriter), Stderr: io.Discard, InterruptStdin: inputWriter.Close, HTTPClient: &http.Client{Transport: transport}}
	done := make(chan error, 1)
	go func() { err := relay.Run(ctx); _ = outputWriter.CloseWithError(err); done <- err }()
	client := mcp.NewClient(&mcp.Implementation{Name: "relay-post-client", Version: "1"}, nil)
	var session *mcp.ClientSession
	settled := false
	t.Cleanup(func() {
		cancel()
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
			case err := <-done:
				t.Logf("failed-path relay settled: %v", err)
			case <-time.After(3 * time.Second):
				t.Error("owned relay did not settle")
			}
		}
		transport.CloseIdleConnections()
		srv.CloseClientConnections()
		srv.Close()
	})
	var err error
	session, err = client.Connect(ctx, &mcp.IOTransport{Reader: outputReader, Writer: inputWriter}, nil)
	if err != nil {
		t.Fatalf("SDK connection through Relay: %v", err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "effect", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("SDK POST-origin replay: %v", err)
	}
	if result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("SDK tool result differs: %+v", result)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok || content.Text != relayPOSTConsumerResult {
		t.Fatalf("SDK consumed an unexpected result: %#v", result.Content[0])
	}
	if err := session.Close(); err != nil {
		t.Fatalf("SDK client close: %v", err)
	}
	select {
	case err := <-done:
		settled = true
		if err != nil {
			t.Fatalf("relay settlement: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("relay did not settle after SDK client close")
	}
	select {
	case <-store.closed:
	case <-ctx.Done():
		t.Fatal("SDK session cleanup was not observed")
	}
	srv.CloseClientConnections()
	srv.Close()
	mu.Lock()
	originalSession, originalVersion, continuationSession, continuationVersion, cursor := postSession, postVersion, resumedSession, resumedVersion, resumedCursor
	mu.Unlock()
	store.mu.Lock()
	appends, replays, storedSession, storedStream := store.resultAppends, store.resultReplays, store.resultSession, store.resultStream
	store.mu.Unlock()
	if effects.Load() != 1 || initializePosts.Load() != 1 || toolPosts.Load() != 1 || resumes.Load() != 1 || appends != 1 || replays != 1 {
		t.Errorf("operation/delivery counts: effects=%d initialize=%d POST=%d GET-resume=%d stored=%d replayed=%d", effects.Load(), initializePosts.Load(), toolPosts.Load(), resumes.Load(), appends, replays)
	}
	if originalSession == "" || originalSession != continuationSession || originalSession != storedSession || originalVersion != "2025-11-25" || originalVersion != continuationVersion || storedStream == "" || cursor == "" || strings.HasPrefix(cursor, "_") {
		t.Errorf("POST stream binding differs: original=%q/%q resumed=%q/%q cursor=%q stored=%q/%q", originalSession, originalVersion, continuationSession, continuationVersion, cursor, storedSession, storedStream)
	}
	if strings.Count(stdio.String(), relayPOSTConsumerResult) != 1 || active.Load() != 0 || store.closeCalls.Load() != 1 {
		t.Errorf("delivery/settlement: result frames=%d active HTTP=%d session closes=%d", strings.Count(stdio.String(), relayPOSTConsumerResult), active.Load(), store.closeCalls.Load())
	}
	for range server.Sessions() {
		t.Error("SDK retained an open session")
	}
	t.Logf("real SDK POST consumer: effects=%d POST=%d GET-resume=%d stored=%d replayed=%d session closes=%d", effects.Load(), toolPosts.Load(), resumes.Load(), appends, replays, store.closeCalls.Load())
}
