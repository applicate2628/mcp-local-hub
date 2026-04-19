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

// --- __list_prompts__ ---

func TestListPromptsTool_MapArgs_EmptyInputAcceptedAndNormalized(t *testing.T) {
	synth := listPromptsSyntheticTool()
	cases := []json.RawMessage{
		nil,
		json.RawMessage(`{}`),
		json.RawMessage(`{"ignored":"silently"}`), // extra keys dropped
	}
	for i, in := range cases {
		out, err := synth.MapArgs(in)
		if err != nil {
			t.Errorf("case %d: unexpected error: %v", i, err)
			continue
		}
		// Must be {} — prompts/list has no params per MCP spec.
		if string(out) != `{}` {
			t.Errorf("case %d: out = %q, want {}", i, out)
		}
	}
}

func TestListPromptsTool_MapArgs_RejectsMalformedJSON(t *testing.T) {
	synth := listPromptsSyntheticTool()
	_, err := synth.MapArgs(json.RawMessage(`not json`))
	if err == nil {
		t.Error("expected error for malformed input")
	}
}

func TestListPromptsTool_MapResult_WrapsPromptsArrayAsTextContent(t *testing.T) {
	synth := listPromptsSyntheticTool()
	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"prompts":[{"name":"review","description":"Review a PR","arguments":[{"name":"pr_id","required":true}]}]}}`)
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
		t.Fatalf("unmarshal: %v (body=%s)", err, out)
	}
	if len(parsed.Result.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(parsed.Result.Content))
	}
	if parsed.Result.Content[0].Type != "text" {
		t.Errorf("content type = %q, want text", parsed.Result.Content[0].Type)
	}
	if !strings.Contains(parsed.Result.Content[0].Text, `"name":"review"`) {
		t.Errorf("content text missing prompt name: %s", parsed.Result.Content[0].Text)
	}
}

func TestListPromptsTool_MapResult_EmptyPromptsReturnsEmptyArray(t *testing.T) {
	synth := listPromptsSyntheticTool()
	// Server with no prompts returns result without prompts field.
	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	out := synth.MapResult(in)

	var parsed struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	_ = json.Unmarshal(out, &parsed)
	if len(parsed.Result.Content) != 1 || parsed.Result.Content[0].Text != "[]" {
		t.Errorf("expected text=[], got %+v", parsed.Result.Content)
	}
}

// --- __get_prompt__ ---

func TestGetPromptTool_MapArgs_ForwardsNameAndArguments(t *testing.T) {
	synth := getPromptSyntheticTool()
	in := json.RawMessage(`{"name":"summarize","arguments":{"url":"https://x.com"}}`)
	out, err := synth.MapArgs(in)
	if err != nil {
		t.Fatalf("MapArgs: %v", err)
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(out, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.Name != "summarize" {
		t.Errorf("name = %q, want summarize", params.Name)
	}
	if !strings.Contains(string(params.Arguments), `"url":"https://x.com"`) {
		t.Errorf("arguments not forwarded: %s", params.Arguments)
	}
}

func TestGetPromptTool_MapArgs_AcceptsNameWithoutArguments(t *testing.T) {
	synth := getPromptSyntheticTool()
	out, err := synth.MapArgs(json.RawMessage(`{"name":"ping"}`))
	if err != nil {
		t.Fatalf("MapArgs: %v", err)
	}
	// Params must contain name and must NOT have arguments key when absent.
	if !strings.Contains(string(out), `"name":"ping"`) {
		t.Errorf("name missing: %s", out)
	}
	if strings.Contains(string(out), `"arguments"`) {
		t.Errorf("arguments key should not be present when omitted: %s", out)
	}
}

func TestGetPromptTool_MapArgs_MissingNameErrors(t *testing.T) {
	synth := getPromptSyntheticTool()
	_, err := synth.MapArgs(json.RawMessage(`{"arguments":{"x":1}}`))
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestGetPromptTool_MapResult_WrapsMessagesAsTextContent(t *testing.T) {
	synth := getPromptSyntheticTool()
	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"description":"hi","messages":[{"role":"user","content":{"type":"text","text":"hello"}},{"role":"assistant","content":{"type":"text","text":"world"}}]}}`)
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
		t.Fatalf("unmarshal: %v (body=%s)", err, out)
	}
	if len(parsed.Result.Content) != 1 {
		t.Fatalf("expected 1 content block (JSON-encoded messages array), got %d", len(parsed.Result.Content))
	}
	// The serialized messages array should contain both roles.
	text := parsed.Result.Content[0].Text
	if !strings.Contains(text, `"role":"user"`) || !strings.Contains(text, `"role":"assistant"`) {
		t.Errorf("serialized messages missing roles: %s", text)
	}
	if !strings.Contains(text, `"hello"`) || !strings.Contains(text, `"world"`) {
		t.Errorf("serialized messages missing bodies: %s", text)
	}
}

// --- Registry + bridge integration ---

func TestSyntheticTools_PromptsRegistered(t *testing.T) {
	tools := syntheticTools()
	seen := map[string]bool{}
	for _, s := range tools {
		seen[s.Name] = true
	}
	for _, want := range []string{"__read_resource__", "__list_prompts__", "__get_prompt__"} {
		if !seen[want] {
			t.Errorf("registry missing %q", want)
		}
	}
}

func TestProtocolBridge_InjectTools_PromptsAndResources(t *testing.T) {
	b := NewProtocolBridge()
	// Server declares BOTH capabilities — all three synthetic tools should appear.
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"resources":{},"prompts":{}}}}`))

	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"native"}]}}`)
	out := b.InjectTools(in)

	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range parsed.Result.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"native", "__read_resource__", "__list_prompts__", "__get_prompt__"} {
		if !names[want] {
			t.Errorf("missing tool %q in %+v", want, names)
		}
	}
}

func TestProtocolBridge_InjectTools_OnlyPrompts(t *testing.T) {
	b := NewProtocolBridge()
	// Server declares ONLY prompts — resources tool must NOT appear.
	b.CacheInitialize(json.RawMessage(`{"result":{"capabilities":{"prompts":{}}}}`))

	in := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	out := b.InjectTools(in)

	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	_ = json.Unmarshal(out, &parsed)
	names := map[string]bool{}
	for _, tl := range parsed.Result.Tools {
		names[tl.Name] = true
	}
	if names["__read_resource__"] {
		t.Error("__read_resource__ should NOT be injected for prompts-only server")
	}
	if !names["__list_prompts__"] || !names["__get_prompt__"] {
		t.Errorf("prompts tools missing: %+v", names)
	}
}

// --- End-to-end through handlePOST ---

func TestPromptBridge_EndToEnd_ListAndGet(t *testing.T) {
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
            "capabilities":{"prompts":{},"tools":{}},
        }}
    elif method == "tools/list":
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{"tools":[
            {"name":"native_tool"}
        ]}}
    elif method == "prompts/list":
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{"prompts":[
            {"name":"review","description":"PR review","arguments":[{"name":"pr","required":True}]}
        ]}}
    elif method == "prompts/get":
        name = (msg.get("params") or {}).get("name", "")
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{"messages":[
            {"role":"user","content":{"type":"text","text":"prompt " + name}}
        ]}}
    else:
        resp = {"jsonrpc":"2.0","id":msg["id"],"error":{"code":-32601,"message":"nope"}}
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

	// initialize → cache capabilities.
	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)

	// tools/list → native_tool + __list_prompts__ + __get_prompt__. No __read_resource__
	// because the subprocess did not declare resources capability.
	listed := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ := listed["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"native_tool", "__list_prompts__", "__get_prompt__"} {
		if !names[want] {
			t.Errorf("missing %q in tools/list: %+v", want, names)
		}
	}
	if names["__read_resource__"] {
		t.Error("__read_resource__ should NOT be injected for prompts-only server")
	}

	// tools/call __list_prompts__ → server sees prompts/list, client sees
	// single text content containing the serialized prompts array.
	called := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"__list_prompts__","arguments":{}}}`)
	result, _ = called["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d: %+v", len(content), result)
	}
	first, _ := content[0].(map[string]any)
	// Python json.dumps defaults to separators=(', ', ': '), so the serialized
	// prompts array contains "review" somewhere with or without spaces around colons.
	if !strings.Contains(first["text"].(string), `"review"`) {
		t.Errorf("prompt name missing from response: %+v", first)
	}

	// tools/call __get_prompt__ with name → server sees prompts/get, client
	// sees serialized messages array.
	got := post(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"__get_prompt__","arguments":{"name":"review"}}}`)
	result, _ = got["result"].(map[string]any)
	content, _ = result["content"].([]any)
	first, _ = content[0].(map[string]any)
	if !strings.Contains(first["text"].(string), `prompt review`) {
		t.Errorf("prompt body missing: %+v", first)
	}

	// tools/call __get_prompt__ WITHOUT name → synthetic error, no subprocess call.
	errResp := post(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"__get_prompt__","arguments":{}}}`)
	result, _ = errResp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Errorf("expected isError for missing name: %+v", result)
	}
}
