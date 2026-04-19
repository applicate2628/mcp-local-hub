package daemon

import (
	"encoding/json"
	"net/http"
)

// Resource-to-tool bridge for stdio-hosted MCP servers.
//
// Claude Code CLI surfaces MCP resources to the human user through a
// picker UI but does not inject them into the agent's tool context.
// Any wrapped MCP server that exposes resources therefore hides that
// data from the agent unless each server also ships tool wrappers.
//
// The bridge here adds a single synthetic __read_resource__ tool to
// tools/list responses whenever the subprocess advertised resources
// capability during initialize. Calls to that synthetic tool are
// rewritten to resources/read on the way to the subprocess, and the
// resources/read result is reshaped back into a tools/call result on
// the way out. This is a universal fix that benefits every stdio-
// hosted MCP server, not just the ones we own.

// syntheticReadResourceToolJSON is the static JSON for the injected
// tool entry. Stored as a raw message so we can splice it into the
// tools[] array without re-marshaling on every request.
var syntheticReadResourceToolJSON = json.RawMessage(`{"name":"__read_resource__","description":"Read an MCP resource by URI. Resources carry server-side read-only data (catalogs, discovery info, workflow guides). Many MCP clients surface resources to the human user but not to the agent — this tool bridges the gap. Call resources/list for discoverable URIs, or consult the server's documentation for template-URI patterns (e.g. resource://compilers/{language_id}). Response is the resource body as text.","inputSchema":{"type":"object","properties":{"uri":{"type":"string","description":"The resource URI to read (absolute, e.g. 'resource://tools' or 'resource://compilers/c++')."}},"required":["uri"]}}`)

// hasResourcesCapability returns true when the cached initialize
// response advertises a non-empty resources capability.
func (h *StdioHost) hasResourcesCapability() bool {
	h.initMu.Lock()
	cached := h.initCached
	h.initMu.Unlock()
	if len(cached) == 0 {
		return false
	}
	var msg struct {
		Result struct {
			Capabilities struct {
				Resources json.RawMessage `json:"resources"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(cached, &msg); err != nil {
		return false
	}
	return len(msg.Result.Capabilities.Resources) > 0
}

// injectReadResourceTool appends the synthetic __read_resource__ tool
// to the tools[] array inside a tools/list response body. On any parse
// failure it returns the body unchanged — the worst case is the tool
// being unreachable, never a corrupted response.
func injectReadResourceTool(respBody json.RawMessage) json.RawMessage {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody
	}
	resultRaw, ok := msg["result"]
	if !ok {
		return respBody
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return respBody
	}
	var tools []json.RawMessage
	if t, ok := result["tools"]; ok {
		if err := json.Unmarshal(t, &tools); err != nil {
			return respBody
		}
	}
	tools = append(tools, syntheticReadResourceToolJSON)
	newTools, err := json.Marshal(tools)
	if err != nil {
		return respBody
	}
	result["tools"] = newTools
	newResult, err := json.Marshal(result)
	if err != nil {
		return respBody
	}
	msg["result"] = newResult
	out, err := json.Marshal(msg)
	if err != nil {
		return respBody
	}
	return out
}

// readResourceRewrite is the outcome of classifying an outgoing message:
// whether it is a __read_resource__ tool call, and — when it is — the
// transformed payload plus the URI for diagnostic messages.
type readResourceRewrite struct {
	applied    bool
	payload    []byte // rewritten JSON with method=resources/read
	uri        string // uri extracted from tool arguments
	parseError error  // non-nil when params.arguments was malformed or missing uri
}

// maybeRewriteReadResourceCall inspects an incoming JSON-RPC message and
// — if it is tools/call with name __read_resource__ — returns a rewrite
// that swaps the method to resources/read and lifts arguments.uri into
// params.uri. The original id and other framing fields are preserved.
func maybeRewriteReadResourceCall(msg map[string]json.RawMessage) readResourceRewrite {
	var method string
	if m, ok := msg["method"]; ok {
		_ = json.Unmarshal(m, &method)
	}
	if method != "tools/call" {
		return readResourceRewrite{}
	}
	paramsRaw, ok := msg["params"]
	if !ok {
		return readResourceRewrite{}
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return readResourceRewrite{}
	}
	if params.Name != "__read_resource__" {
		return readResourceRewrite{}
	}

	var args struct {
		URI string `json:"uri"`
	}
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return readResourceRewrite{applied: true, parseError: err}
		}
	}
	if args.URI == "" {
		return readResourceRewrite{applied: true, parseError: errMissingURI}
	}

	newParams, err := json.Marshal(map[string]string{"uri": args.URI})
	if err != nil {
		return readResourceRewrite{applied: true, parseError: err}
	}
	rewritten := make(map[string]json.RawMessage, len(msg))
	for k, v := range msg {
		rewritten[k] = v
	}
	rewritten["method"] = json.RawMessage(`"resources/read"`)
	rewritten["params"] = newParams

	payload, err := json.Marshal(rewritten)
	if err != nil {
		return readResourceRewrite{applied: true, parseError: err}
	}
	return readResourceRewrite{applied: true, payload: payload, uri: args.URI}
}

// reshapeReadResourceResponse converts a resources/read response body
// into the shape a tools/call response should have. Input result:
//
//	{"contents":[{"uri":..., "mimeType":..., "text":"..."}, ...]}
//
// Output result:
//
//	{"content":[{"type":"text","text":"..."}, ...]}
//
// On parse failure the body is returned unchanged, so a malformed
// subprocess response still reaches the caller instead of being lost.
func reshapeReadResourceResponse(respBody json.RawMessage) json.RawMessage {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody
	}
	resultRaw, ok := msg["result"]
	if !ok {
		return respBody
	}
	var result struct {
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return respBody
	}
	content := make([]map[string]string, 0, len(result.Contents))
	for _, c := range result.Contents {
		content = append(content, map[string]string{
			"type": "text",
			"text": c.Text,
		})
	}
	newResult, err := json.Marshal(map[string]any{"content": content})
	if err != nil {
		return respBody
	}
	msg["result"] = newResult
	out, err := json.Marshal(msg)
	if err != nil {
		return respBody
	}
	return out
}

// errMissingURI is returned via readResourceRewrite.parseError when a
// __read_resource__ call lacks the required uri argument.
var errMissingURI = jsonRPCError("missing required argument: uri")

// jsonRPCError is a tiny error type that prints as its wrapped string
// — used to keep the rewrite struct free of heavy dependencies.
type jsonRPCError string

func (e jsonRPCError) Error() string { return string(e) }

// writeToolCallError sends a synthetic tools/call error response to the
// HTTP client without forwarding to the subprocess. Used when the
// bridge detects a __read_resource__ call we can reject synchronously
// (missing uri, server with no resources capability).
func writeToolCallError(w http.ResponseWriter, origIDRaw json.RawMessage, message string) {
	errResult, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": message}},
		"isError": true,
	})
	msg := map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"result":  errResult,
	}
	if len(origIDRaw) > 0 {
		msg["id"] = origIDRaw
	}
	out, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
