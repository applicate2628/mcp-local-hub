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

// --- Pure-function unit tests ---

func TestInjectReadResourceTool_AppendsSyntheticEntry(t *testing.T) {
	in := json.RawMessage(`{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"compile_code"}]}}`)
	out := injectReadResourceTool(in)

	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal injected body: %v (body=%s)", err, out)
	}
	if len(parsed.Result.Tools) != 2 {
		t.Fatalf("expected 2 tools after injection, got %d: %s", len(parsed.Result.Tools), out)
	}
	if parsed.Result.Tools[0].Name != "compile_code" {
		t.Errorf("existing tool reordered/lost: %+v", parsed.Result.Tools)
	}
	if parsed.Result.Tools[1].Name != "__read_resource__" {
		t.Errorf("synthetic tool name = %q, want __read_resource__", parsed.Result.Tools[1].Name)
	}
}

func TestInjectReadResourceTool_HandlesEmptyTools(t *testing.T) {
	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	out := injectReadResourceTool(in)

	var parsed struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Result.Tools) != 1 {
		t.Fatalf("expected exactly 1 tool (the synthetic one), got %d", len(parsed.Result.Tools))
	}
}

func TestInjectReadResourceTool_MalformedBodyReturnsUnchanged(t *testing.T) {
	in := json.RawMessage(`not json`)
	out := injectReadResourceTool(in)
	if string(out) != string(in) {
		t.Errorf("expected passthrough on parse failure, got %q", out)
	}
}

func TestMaybeRewriteReadResourceCall_TransformsMethodAndParams(t *testing.T) {
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"__read_resource__","arguments":{"uri":"resource://workflow"}}}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(raw, &msg)

	rw := maybeRewriteReadResourceCall(msg)
	if !rw.applied {
		t.Fatal("expected applied=true")
	}
	if rw.parseError != nil {
		t.Fatalf("unexpected parseError: %v", rw.parseError)
	}
	if rw.uri != "resource://workflow" {
		t.Errorf("uri = %q, want resource://workflow", rw.uri)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(rw.payload, &out); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	var gotMethod string
	_ = json.Unmarshal(out["method"], &gotMethod)
	if gotMethod != "resources/read" {
		t.Errorf("method = %q, want resources/read", gotMethod)
	}
	var params struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(out["params"], &params)
	if params.URI != "resource://workflow" {
		t.Errorf("params.uri = %q, want resource://workflow", params.URI)
	}
	// id must survive the rewrite.
	if string(out["id"]) != "42" {
		t.Errorf("id not preserved: got %q", out["id"])
	}
}

func TestMaybeRewriteReadResourceCall_IgnoresOtherTools(t *testing.T) {
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"compile_code","arguments":{"source":"int main(){}"}}}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(raw, &msg)

	rw := maybeRewriteReadResourceCall(msg)
	if rw.applied {
		t.Errorf("expected applied=false for non-__read_resource__ call, got %+v", rw)
	}
}

func TestMaybeRewriteReadResourceCall_MissingURIReturnsParseError(t *testing.T) {
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"__read_resource__","arguments":{}}}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(raw, &msg)

	rw := maybeRewriteReadResourceCall(msg)
	if !rw.applied {
		t.Fatal("expected applied=true so handler can return a tool-call error")
	}
	if rw.parseError == nil {
		t.Fatal("expected non-nil parseError for missing uri")
	}
}

func TestReshapeReadResourceResponse_ConvertsContentsToContent(t *testing.T) {
	in := json.RawMessage(`{"jsonrpc":"2.0","id":3,"result":{"contents":[{"uri":"resource://workflow","mimeType":"text/markdown","text":"hello"},{"uri":"resource://workflow","mimeType":"text/markdown","text":"world"}]}}`)
	out := reshapeReadResourceResponse(in)

	var parsed struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal reshaped body: %v", err)
	}
	if len(parsed.Result.Content) != 2 {
		t.Fatalf("expected 2 content entries, got %d: %s", len(parsed.Result.Content), out)
	}
	if parsed.Result.Content[0].Type != "text" || parsed.Result.Content[0].Text != "hello" {
		t.Errorf("entry 0 = %+v, want {text, hello}", parsed.Result.Content[0])
	}
	if parsed.Result.Content[1].Text != "world" {
		t.Errorf("entry 1 = %+v", parsed.Result.Content[1])
	}
}

func TestReshapeReadResourceResponse_MalformedReturnsUnchanged(t *testing.T) {
	in := json.RawMessage(`not json`)
	out := reshapeReadResourceResponse(in)
	if string(out) != string(in) {
		t.Errorf("expected passthrough on parse failure")
	}
}

func TestHasResourcesCapability_ReadsCachedInitialize(t *testing.T) {
	h := &StdioHost{}

	// No cached initialize yet → false.
	if h.hasResourcesCapability() {
		t.Error("expected false before initialize is cached")
	}

	// Cache an initialize response without resources → false.
	h.initCached = json.RawMessage(`{"result":{"capabilities":{"tools":{}}}}`)
	if h.hasResourcesCapability() {
		t.Error("expected false when capabilities.resources is absent")
	}

	// Cache an initialize response WITH resources → true.
	h.initCached = json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`)
	if !h.hasResourcesCapability() {
		t.Error("expected true when capabilities.resources is present (even if empty object)")
	}
}

// --- End-to-end integration test ---

// TestResourceBridge_EndToEnd_WrapsResourceAsTool stands up a scripted
// Python subprocess that advertises resources and responds to
// resources/read, then drives it through handlePOST to verify the
// bridge injects __read_resource__ and round-trips a tools/call through
// to a resources/read response that comes back as tools/call content.
func TestResourceBridge_EndToEnd_WrapsResourceAsTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := NewStdioHost(HostConfig{
		Command: "python",
		Args: []string{"-u", "-c", `
import sys, json
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method")
    if method == "initialize":
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{
            "protocolVersion":"2025-03-26",
            "capabilities":{"resources":{},"tools":{}},
        }}
    elif method == "tools/list":
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{"tools":[
            {"name":"native_tool","description":"existing native tool"}
        ]}}
    elif method == "resources/read":
        uri = (msg.get("params") or {}).get("uri", "")
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{"contents":[
            {"uri":uri,"mimeType":"text/plain","text":"resource body for " + uri}
        ]}}
    else:
        resp = {"jsonrpc":"2.0","id":msg["id"],"error":{"code":-32601,"message":"method not found"}}
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`},
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	post := func(body string) map[string]any {
		resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("response not JSON: %v (body=%s)", err, raw)
		}
		return out
	}

	// 1. initialize — required before the bridge will inject, because the
	//    capabilities check reads the cached initialize body.
	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)

	// 2. tools/list → expect __read_resource__ appended to the existing tool.
	listed := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	result, _ := listed["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools/list should have 2 tools (native + __read_resource__), got %d: %+v", len(tools), tools)
	}
	seenRR := false
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		if m["name"] == "__read_resource__" {
			seenRR = true
		}
	}
	if !seenRR {
		t.Fatalf("__read_resource__ not injected: %+v", tools)
	}

	// 3. tools/call __read_resource__ with a uri → subprocess sees
	//    resources/read, we see a tools/call-shaped response.
	called := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"__read_resource__","arguments":{"uri":"resource://workflow"}}}`)
	result, _ = called["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content entry, got %d: %+v", len(content), result)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || !strings.Contains(first["text"].(string), "resource body for resource://workflow") {
		t.Errorf("unexpected content entry: %+v", first)
	}

	// 4. tools/call __read_resource__ without uri → synthetic error, subprocess
	//    never invoked. isError:true on the tools/call shape.
	errorResult := post(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"__read_resource__","arguments":{}}}`)
	result, _ = errorResult["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Errorf("expected isError=true for missing uri: %+v", result)
	}
}

// TestResourceBridge_NoResourcesCapability_NoInjection verifies the
// safety net: subprocess that did not advertise resources must not see
// __read_resource__ appended (it would be a hallucinated tool).
func TestResourceBridge_NoResourcesCapability_NoInjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := NewStdioHost(HostConfig{
		Command: "python",
		Args: []string{"-u", "-c", `
import sys, json
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method")
    if method == "initialize":
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{
            "protocolVersion":"2025-03-26",
            "capabilities":{"tools":{}},
        }}
    elif method == "tools/list":
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{"tools":[
            {"name":"only_tool"}
        ]}}
    else:
        resp = {"jsonrpc":"2.0","id":msg["id"],"error":{"code":-32601,"message":"no"}}
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`},
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	post := func(body string) map[string]any {
		resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}

	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	listed := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	result, _ := listed["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected only native tool (no injection), got %d: %+v", len(tools), tools)
	}
	m, _ := tools[0].(map[string]any)
	if m["name"] != "only_tool" {
		t.Errorf("unexpected tool: %+v", m)
	}
}
