package vcpkgserver

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errResult is the shared error-return helper used by every tool in this
// package, mirroring internal/perftools's convention — keeps the error
// surface consistent so MCP clients see the same shape regardless of
// which tool failed. Used only for argument-unmarshal failures; a tool's
// OWN inability to answer is expressed through the tri-state
// evidence.Status contract in the JSON body instead (never as an MCP
// protocol error), per the design doc's "never hide uncertainty"
// invariant — an unknown(reason) is a normal, successful tool result.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// jsonResult marshals v (one of the *Result types from discovery/
// lastfailure/cmakewrap) as indented JSON and wraps it as a successful
// tool result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult("failed to marshal result: " + err.Error()), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}
