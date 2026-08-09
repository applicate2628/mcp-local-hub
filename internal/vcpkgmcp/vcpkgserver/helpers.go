package vcpkgserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
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

// jsonResult is the only vcpkg MCP success serialization boundary. It accepts
// only package-owned Projectable results and measures the exact indented JSON
// text before publishing it.
func jsonResult(v publicresult.Projectable) (*mcp.CallToolResult, error) {
	body, err := publicresult.MarshalIndent(v)
	if err != nil {
		return errResult("failed to marshal result: " + err.Error()), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}
