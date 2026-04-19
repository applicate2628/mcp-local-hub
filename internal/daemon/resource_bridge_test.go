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

// --- Pure-function unit tests (ProtocolBridge + registry) ---

func TestProtocolBridge_InjectTools_AppendsSyntheticEntry(t *testing.T) {
	b := NewProtocolBridge()
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`))

	in := json.RawMessage(`{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"compile_code"}]}}`)
	out := b.InjectTools(in)

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

func TestProtocolBridge_InjectTools_EmptyTools(t *testing.T) {
	b := NewProtocolBridge()
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`))

	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	out := b.InjectTools(in)

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

func TestProtocolBridge_InjectTools_NoCapability_NoInjection(t *testing.T) {
	// No initialize cached → no capabilities known → no injection.
	b := NewProtocolBridge()
	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"only_tool"}]}}`)
	out := b.InjectTools(in)
	if string(out) != string(in) {
		t.Errorf("expected passthrough when capability unknown, got: %s", out)
	}
}

func TestProtocolBridge_InjectTools_MalformedBodyReturnsUnchanged(t *testing.T) {
	b := NewProtocolBridge()
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`))
	in := json.RawMessage(`not json`)
	out := b.InjectTools(in)
	if string(out) != string(in) {
		t.Errorf("expected passthrough on parse failure, got %q", out)
	}
}

func TestProtocolBridge_TransformRequest_RewritesReadResource(t *testing.T) {
	b := NewProtocolBridge()
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`))

	raw := json.RawMessage(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"__read_resource__","arguments":{"uri":"resource://workflow"}}}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(raw, &msg)

	action := b.TransformRequest(msg)
	if action.Active == nil {
		t.Fatal("expected Active non-nil for __read_resource__ call")
	}
	if action.SynthError != nil {
		t.Fatalf("unexpected SynthError: %v", action.SynthError)
	}
	if action.Active.Name != "__read_resource__" {
		t.Errorf("Active.Name = %q", action.Active.Name)
	}

	// msg should be rewritten in place: method → resources/read, params → {uri}.
	var gotMethod string
	_ = json.Unmarshal(msg["method"], &gotMethod)
	if gotMethod != "resources/read" {
		t.Errorf("method = %q, want resources/read", gotMethod)
	}
	var params struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(msg["params"], &params)
	if params.URI != "resource://workflow" {
		t.Errorf("params.uri = %q", params.URI)
	}
	if string(msg["id"]) != "42" {
		t.Errorf("id not preserved: %q", msg["id"])
	}
}

func TestProtocolBridge_TransformRequest_IgnoresOtherTools(t *testing.T) {
	b := NewProtocolBridge()
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`))

	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"compile_code","arguments":{"source":"int main(){}"}}}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(raw, &msg)

	action := b.TransformRequest(msg)
	if action.Active != nil || action.SynthError != nil {
		t.Errorf("expected empty BridgeAction for non-synthetic tool, got %+v", action)
	}
}

func TestProtocolBridge_TransformRequest_MissingURIReturnsError(t *testing.T) {
	b := NewProtocolBridge()
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`))

	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"__read_resource__","arguments":{}}}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(raw, &msg)

	action := b.TransformRequest(msg)
	if action.Active == nil {
		t.Fatal("Active should be set so caller can write error response")
	}
	if action.SynthError == nil {
		t.Fatal("SynthError should be non-nil for missing uri")
	}
}

func TestProtocolBridge_TransformRequest_CapabilityGate(t *testing.T) {
	b := NewProtocolBridge()
	// Cache an initialize that does NOT declare resources.
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"tools":{}}}}`))

	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"__read_resource__","arguments":{"uri":"resource://x"}}}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(raw, &msg)

	action := b.TransformRequest(msg)
	if action.SynthError == nil {
		t.Fatal("expected capability-gate error when server lacks resources capability")
	}
}

func TestReadResourceTool_MapResult_ConvertsContentsToContent(t *testing.T) {
	synth := readResourceSyntheticTool()
	in := json.RawMessage(`{"jsonrpc":"2.0","id":3,"result":{"contents":[{"uri":"resource://workflow","mimeType":"text/markdown","text":"hello"},{"uri":"resource://workflow","mimeType":"text/markdown","text":"world"}]}}`)
	out := synth.MapResult(in)

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

func TestReadResourceTool_MapResult_MalformedReturnsUnchanged(t *testing.T) {
	synth := readResourceSyntheticTool()
	in := json.RawMessage(`not json`)
	out := synth.MapResult(in)
	if string(out) != string(in) {
		t.Errorf("expected passthrough on parse failure")
	}
}

func TestHasCapability_ReadsCachedInitialize(t *testing.T) {
	// Empty cache → false.
	if hasCapability(nil, "resources") {
		t.Error("expected false for empty cache")
	}
	// Missing capability → false.
	if hasCapability(json.RawMessage(`{"result":{"capabilities":{"tools":{}}}}`), "resources") {
		t.Error("expected false when capabilities.resources is absent")
	}
	// Present capability → true.
	if !hasCapability(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`), "resources") {
		t.Error("expected true for empty-object capability declaration")
	}
	// Empty capability name → false (defensive).
	if hasCapability(json.RawMessage(`{"result":{"capabilities":{"resources":{}}}}`), "") {
		t.Error("expected false for empty capability name")
	}
}

func TestSyntheticTools_NamesAreUnique(t *testing.T) {
	// Defensive: if two registry entries claim the same Name, findSyntheticTool
	// becomes ambiguous. This test catches accidental duplicates at the
	// earliest possible point.
	seen := map[string]bool{}
	for _, s := range syntheticTools() {
		if seen[s.Name] {
			t.Errorf("duplicate synthetic tool Name: %q", s.Name)
		}
		seen[s.Name] = true
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
