package lsp_routing

import (
	"bytes"
	"encoding/json"
)

// ToolCallParams parses a tools/call envelope's params into the tool name and
// argument object needed for path-aware routing. Non-object params yield empty
// values so callers can fall back to their normal malformed-call handling.
func ToolCallParams(params json.RawMessage) (name string, arguments json.RawMessage) {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", nil
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return "", nil
	}
	return p.Name, p.Arguments
}

// ExtractPathArg scans an LSP tool-call argument object for the path-bearing
// fields used by gopls MCP and mcp-language-server tools.
//
// Priority:
//   - file
//   - files (first non-empty string in the array)
//   - filePath
//   - dir
func ExtractPathArg(arguments json.RawMessage) (string, bool) {
	if len(arguments) == 0 {
		return "", false
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", false
	}
	for _, key := range []string{"file", "files", "filePath", "dir"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		if key == "files" {
			if v, ok := firstNonEmptyString(raw); ok {
				return v, true
			}
			continue
		}
		if v, ok := stringArg(raw); ok {
			return v, true
		}
	}
	return "", false
}

func stringArg(raw json.RawMessage) (string, bool) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || v == "" {
		return "", false
	}
	return v, true
}

func firstNonEmptyString(raw json.RawMessage) (string, bool) {
	var vals []string
	if err := json.Unmarshal(raw, &vals); err != nil {
		return "", false
	}
	for _, v := range vals {
		if v != "" {
			return v, true
		}
	}
	return "", false
}
