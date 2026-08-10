package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type sessionContractHTTPResult struct {
	status int
	header http.Header
	body   []byte
}

func sessionContractRequest(
	t *testing.T,
	client *http.Client,
	method string,
	url string,
	body string,
	sessionID string,
	protocolVersion string,
) sessionContractHTTPResult {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url+"/mcp", reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s /mcp: %v", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	return sessionContractHTTPResult{status: resp.StatusCode, header: resp.Header.Clone(), body: raw}
}

func TestStdioHostHTTPProtocolAndSessionContractR1(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := NewStdioHost(HostConfig{
		Command: "python",
		Args: []string{"-u", "-c", `
import json, sys
seen = 0
for line in sys.stdin:
    msg = json.loads(line)
    seen += 1
    if msg.get("method") == "initialize":
        result = {"protocolVersion":"2025-11-25", "capabilities":{}, "seen":seen}
    else:
        result = {"seen":seen}
    sys.stdout.write(json.dumps({"jsonrpc":"2.0", "id":msg.get("id"), "result":result}) + "\n")
    sys.stdout.flush()
`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"r1-test","version":"1"}}}`
	invalidInitialize := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, initBody, "", "qa-invalid-version")
	if invalidInitialize.status != http.StatusBadRequest {
		t.Fatalf("initialize with invalid protocol header status=%d want=400 body=%s", invalidInitialize.status, invalidInitialize.body)
	}
	first := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, initBody, "", "")
	second := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, initBody, "", "")
	if first.status != http.StatusOK || second.status != http.StatusOK {
		t.Fatalf("initialize statuses = %d, %d; want 200, 200", first.status, second.status)
	}
	sid1 := first.header.Get("Mcp-Session-Id")
	sid2 := second.header.Get("Mcp-Session-Id")
	if sid1 == "" || sid2 == "" || sid1 == sid2 {
		t.Fatalf("initialize session ids must be nonempty and distinct: first=%q second=%q", sid1, sid2)
	}

	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	for _, tc := range []struct {
		name    string
		sid     string
		version string
		want    int
	}{
		{name: "missing session", version: "2025-11-25", want: http.StatusBadRequest},
		{name: "unknown session", sid: "qa-invalid-session", version: "2025-11-25", want: http.StatusNotFound},
		{name: "unsupported version", sid: sid1, version: "qa-invalid-version", want: http.StatusBadRequest},
		{name: "session version mismatch", sid: sid1, version: "2025-03-26", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, callBody, tc.sid, tc.version)
			if got.status != tc.want {
				t.Fatalf("status=%d want=%d body=%s", got.status, tc.want, got.body)
			}
		})
	}

	deleted := sessionContractRequest(t, ts.Client(), http.MethodDelete, ts.URL, "", sid1, "2025-11-25")
	if deleted.status != http.StatusNoContent {
		t.Fatalf("DELETE status=%d want=204 body=%s", deleted.status, deleted.body)
	}
	afterDelete := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, callBody, sid1, "2025-11-25")
	if afterDelete.status != http.StatusNotFound {
		t.Fatalf("deleted session status=%d want=404 body=%s", afterDelete.status, afterDelete.body)
	}

	valid := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, callBody, sid2, "2025-11-25")
	if valid.status != http.StatusOK {
		t.Fatalf("second session status=%d want=200 body=%s", valid.status, valid.body)
	}
	var envelope struct {
		Result struct {
			Seen int `json:"seen"`
		} `json:"result"`
	}
	if err := json.Unmarshal(valid.body, &envelope); err != nil {
		t.Fatalf("decode valid response: %v body=%s", err, valid.body)
	}
	if envelope.Result.Seen != 2 {
		t.Fatalf("subprocess saw %d messages; want 2 (one initialize plus one valid call)", envelope.Result.Seen)
	}
}

func TestStdioHostSessionRegistryEvictsOldestAtBoundR1(t *testing.T) {
	h, err := NewStdioHost(HostConfig{Command: echoSubprocCommand(), Args: echoSubprocArgs()})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, maxStdioHTTPSessions+1)
	for i := 0; i < maxStdioHTTPSessions+1; i++ {
		sid, err := h.createSession("2025-11-25")
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
		ids = append(ids, sid)
	}
	h.sessionMu.Lock()
	gotCount := len(h.sessions)
	h.sessionMu.Unlock()
	if gotCount != maxStdioHTTPSessions {
		t.Fatalf("session count=%d want=%d", gotCount, maxStdioHTTPSessions)
	}
	if _, ok := h.lookupSession(ids[0]); ok {
		t.Fatal("oldest session survived bounded-registry eviction")
	}
	if _, ok := h.lookupSession(ids[len(ids)-1]); !ok {
		t.Fatal("newest session was not retained")
	}
}
