package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// ProtocolBridge is the shared per-server bridge state: cached
// initialize (capabilities probe) + request/response transforms driven
// by the SyntheticTool registry. Used by both StdioHost (stdio-bridge
// transport) and HTTPHost (native-http transport) so they share one
// implementation of the agent-visible tool-injection contract.
type ProtocolBridge struct {
	mu         sync.Mutex
	initCached json.RawMessage
}

// NewProtocolBridge returns an empty bridge. The bridge becomes active
// once CacheInitialize is called with the subprocess's initialize
// response — before that no capabilities are known, so no transforms fire.
func NewProtocolBridge() *ProtocolBridge {
	return &ProtocolBridge{}
}

// CacheInitialize stores the subprocess's initialize response. Only the
// first successful caller wins; subsequent calls are ignored so concurrent
// first-callers (two HTTP clients hitting initialize at the same instant)
// see a stable capability set. Caller should hand in the raw body only
// after confirming it parses as JSON.
func (b *ProtocolBridge) CacheInitialize(body json.RawMessage) {
	var resp struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	if len(resp.Error) > 0 && !bytes.Equal(bytes.TrimSpace(resp.Error), []byte("null")) {
		// Do not cache initialize failures. Caching an error would let a
		// single bad/hostile caller poison all future initialize requests.
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initCached == nil {
		b.initCached = append(json.RawMessage(nil), body...)
	}
}

// InitCached returns the cached initialize response, or nil if not yet
// cached. Exposed for the initialize-cache short-circuit in the host.
func (b *ProtocolBridge) InitCached() json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initCached == nil {
		return nil
	}
	// Return a copy so callers can safely mutate (e.g. id rewrite) without
	// racing with a second reader.
	out := make(json.RawMessage, len(b.initCached))
	copy(out, b.initCached)
	return out
}

// BridgeAction describes the outcome of TransformRequest. Exactly one of
// Active or SynthError will be set when the bridge recognizes the
// incoming message; both nil means the message is not a synthetic-tool
// call and should be forwarded unchanged.
type BridgeAction struct {
	// Active points at the registry entry whose Name matched the
	// tools/call target. When non-nil, msg has been rewritten in place
	// to address UpstreamMethod instead of tools/call.
	Active *SyntheticTool

	// SynthError is non-nil when the bridge rejected the call without
	// forwarding (missing required argument, unavailable capability).
	// The host should synthesize a tools/call isError response and
	// return to the client without touching the subprocess.
	SynthError error
}

// TransformRequest inspects the JSON-RPC message; if it is a tools/call
// targeting a registered synthetic tool, rewrites msg in place to call
// the underlying UpstreamMethod with MapArgs-translated params, and
// returns a BridgeAction whose Active points at the registry entry.
// Non-tools/call messages and tools/call with unknown names pass
// through unchanged and return an empty BridgeAction.
func (b *ProtocolBridge) TransformRequest(msg map[string]json.RawMessage) BridgeAction {
	var method string
	if m, ok := msg["method"]; ok {
		_ = json.Unmarshal(m, &method)
	}
	if method != "tools/call" {
		return BridgeAction{}
	}
	paramsRaw, ok := msg["params"]
	if !ok {
		return BridgeAction{}
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return BridgeAction{}
	}
	synth := findSyntheticTool(params.Name)
	if synth == nil {
		return BridgeAction{}
	}

	// Capability gate: server must have advertised the backing capability
	// during initialize. Without it, the call would just bounce with
	// -32601 — better to short-circuit with a clearer error.
	if !hasCapability(b.InitCached(), synth.Capability) {
		return BridgeAction{
			Active: synth,
			SynthError: fmt.Errorf("%s unavailable: server does not expose %s capability",
				synth.Name, synth.Capability),
		}
	}

	upstreamParams, err := synth.MapArgs(params.Arguments)
	if err != nil {
		return BridgeAction{Active: synth, SynthError: err}
	}

	// Rewrite msg in place. Method is a JSON string literal so needs quoting.
	methodJSON, _ := json.Marshal(synth.UpstreamMethod)
	msg["method"] = methodJSON
	msg["params"] = upstreamParams

	return BridgeAction{Active: synth}
}

// TransformResponse applies outbound transforms to a subprocess response:
//   - if active is non-nil, the request was a synthetic tool call and
//     the response is reshaped via active.MapResult
//   - otherwise, if origMethod was tools/list, any applicable synthetic
//     tools (gated by capability) are injected into result.tools[]
//
// Passthrough for every other case.
func (b *ProtocolBridge) TransformResponse(origMethod string, active *SyntheticTool, respBody json.RawMessage) json.RawMessage {
	if active != nil {
		return active.MapResult(respBody)
	}
	if origMethod == "tools/list" {
		return b.InjectTools(respBody)
	}
	return respBody
}

// InjectTools appends every registry entry whose Capability is declared
// by the cached initialize response to the tools[] array inside a
// tools/list response body. On any parse failure the body is returned
// unchanged — injection never corrupts a valid response.
func (b *ProtocolBridge) InjectTools(respBody json.RawMessage) json.RawMessage {
	var applicable []SyntheticTool
	for _, s := range syntheticTools() {
		if hasCapability(b.InitCached(), s.Capability) {
			applicable = append(applicable, s)
		}
	}
	if len(applicable) == 0 {
		return respBody
	}

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
	for _, s := range applicable {
		tools = append(tools, s.Definition)
	}
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

// writeToolCallError sends a synthetic tools/call error response to the
// HTTP client without forwarding to the subprocess. Shared between
// StdioHost and HTTPHost for the unified BridgeAction.SynthError path.
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
