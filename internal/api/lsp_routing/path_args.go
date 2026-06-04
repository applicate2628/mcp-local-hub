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

// ExtractPathArg scans an LSP tool-call argument object for the first
// path-bearing field used by gopls MCP and mcp-language-server tools.
//
// Priority:
//   - file
//   - files (first non-empty string in the array)
//   - filePath
//   - dir
func ExtractPathArg(arguments json.RawMessage) (string, bool) {
	paths, ok := ExtractPathArgs(arguments)
	if !ok {
		return "", false
	}
	return paths[0], true
}

// ExtractPathArgs scans an LSP tool-call argument object for path-bearing
// fields. For files arrays, it returns every non-empty path so callers can
// verify that a batch belongs to one workspace before forwarding it.
//
// Priority matches ExtractPathArg:
//   - file
//   - files (all non-empty strings in the array)
//   - filePath
//   - dir
func ExtractPathArgs(arguments json.RawMessage) ([]string, bool) {
	if len(arguments) == 0 {
		return nil, false
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &args); err != nil {
		return nil, false
	}
	for _, key := range []string{"file", "files", "filePath", "dir"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		if key == "files" {
			if v, ok := nonEmptyStrings(raw); ok {
				return v, true
			}
			continue
		}
		if v, ok := stringArg(raw); ok {
			return []string{v}, true
		}
	}
	return nil, false
}

func stringArg(raw json.RawMessage) (string, bool) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || v == "" {
		return "", false
	}
	return v, true
}

func firstNonEmptyString(raw json.RawMessage) (string, bool) {
	vals, ok := nonEmptyStrings(raw)
	if !ok {
		return "", false
	}
	return vals[0], true
}

func nonEmptyStrings(raw json.RawMessage) ([]string, bool) {
	var vals []string
	if err := json.Unmarshal(raw, &vals); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
