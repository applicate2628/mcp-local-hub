package daemon

import (
	"encoding/json"
)

// SyntheticTool describes one agent-visible tool that the bridge injects
// into a wrapped MCP server's tools/list response when the server
// advertises the matching capability. Calls to that synthetic tool are
// rewritten to the underlying native MCP method on the way in, and the
// native response is reshaped into tools/call result shape on the way
// out.
//
// Adding coverage for a new MCP capability (prompts, sampling, roots,
// elicitation, …) means adding one SyntheticTool entry to the registry
// — no changes to ProtocolBridge, StdioHost, or HTTPHost.
type SyntheticTool struct {
	// Name is the tool's externally visible name (e.g. "__read_resource__").
	// Must start with "__" and be unique across the registry — agent clients
	// treat this as the tool's identity.
	Name string

	// Capability is the key under result.capabilities in the server's
	// initialize response that gates this tool's injection. When the
	// server does not declare this capability, the tool is NOT injected
	// (no phantom tools that would just return -32601).
	Capability string

	// Definition is the full JSON entry inserted into tools/list — name,
	// description, inputSchema. Stored pre-marshaled so splice into the
	// tools[] array is zero-alloc per request.
	Definition json.RawMessage

	// UpstreamMethod is the native MCP method this tool translates to
	// on the wire (e.g. "resources/read"). The bridge rewrites
	// tools/call → UpstreamMethod before forwarding to the subprocess.
	UpstreamMethod string

	// MapArgs translates the tools/call arguments object into the params
	// object expected by UpstreamMethod. On validation failure it
	// returns an error — the bridge then synthesizes a tools/call error
	// response without ever touching the subprocess.
	MapArgs func(toolArgs json.RawMessage) (upstreamParams json.RawMessage, err error)

	// MapResult translates an UpstreamMethod response body into a
	// tools/call response body. The input and output are full JSON-RPC
	// messages ({jsonrpc, id, result}). Typical shape mapping:
	//   resources/read result.contents[] → tools/call result.content[]
	//   prompts/get     result.messages[] → tools/call result.content[]
	MapResult func(respBody json.RawMessage) json.RawMessage
}

// syntheticTools returns the canonical registry. Declared as a function
// (not a package var) so the SyntheticTool literals can reference
// per-entry helpers defined alongside them and so tests can substitute
// a minimal registry without touching package state.
func syntheticTools() []SyntheticTool {
	return []SyntheticTool{
		readResourceSyntheticTool(),
		listPromptsSyntheticTool(),
		getPromptSyntheticTool(),
	}
}

// findSyntheticTool returns the registry entry whose Name matches, or
// nil when no entry claims that name. O(N) scan is fine — N is small
// (~1-5) and lookups only happen on tools/call.
func findSyntheticTool(name string) *SyntheticTool {
	for i := range syntheticTools() {
		if syntheticTools()[i].Name == name {
			tool := syntheticTools()[i]
			return &tool
		}
	}
	return nil
}

// hasCapability reports whether the cached initialize response declares
// the named capability with a non-empty value. Empty JSON object ({}) is
// treated as "declared" per MCP spec — servers often declare a
// capability without options (e.g. "resources": {}).
func hasCapability(initCached json.RawMessage, capName string) bool {
	if len(initCached) == 0 || capName == "" {
		return false
	}
	var msg struct {
		Result struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initCached, &msg); err != nil {
		return false
	}
	raw, ok := msg.Result.Capabilities[capName]
	if !ok {
		return false
	}
	return len(raw) > 0
}
