package api

import (
	"encoding/json"
	"fmt"
)

// ToolSchema is the embedded representation of one MCP tool entry as it
// appears in a tools/list response. The JSON shape matches the upstream
// MCP spec verbatim so it can be emitted as-is inside result.tools[].
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolCatalog is a versioned collection of tool schemas for one backend
// kind. CatalogVersion is the maintainer's human hook: bump it whenever
// the upstream tool set changes so the golden test's drift message names
// the right knob to turn.
type ToolCatalog struct {
	CatalogVersion     string
	ServerInfoName     string
	ServerInfoVersion  string
	ProtocolVersion    string
	InstructionsSource string // one-line note about where this was captured from
	Tools              []ToolSchema
}

// ToolCatalogForBackend returns the embedded catalog for the given backend
// kind ("mcp-language-server" | "gopls-mcp"), or (zero, false) if unknown.
// The returned catalog is safe to read but must not be mutated.
func ToolCatalogForBackend(kind string) (ToolCatalog, bool) {
	switch kind {
	case "mcp-language-server":
		return mcpLanguageServerCatalog, true
	case "gopls-mcp":
		return goplsMCPCatalog, true
	}
	return ToolCatalog{}, false
}

// SyntheticInitializeResponse builds a JSON-RPC initialize response using the
// embedded catalog's serverInfo + an empty capabilities envelope that
// advertises tools support. Response id = the request id so the client
// correlates. Caller writes the bytes to their HTTP body.
func SyntheticInitializeResponse(reqID json.RawMessage, kind string) ([]byte, error) {
	cat, ok := ToolCatalogForBackend(kind)
	if !ok {
		return nil, fmt.Errorf("unknown backend kind %q", kind)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result": map[string]any{
			"protocolVersion": cat.ProtocolVersion,
			"serverInfo": map[string]any{
				"name":    cat.ServerInfoName,
				"version": cat.ServerInfoVersion,
			},
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false},
				"prompts":   map[string]any{"listChanged": false},
			},
		},
	}
	return json.Marshal(payload)
}

// SyntheticToolsListResponse builds a JSON-RPC tools/list response from the
// embedded static catalog. Used by the lazy proxy for every client's
// initial tools/list — the heavy backend is not contacted.
func SyntheticToolsListResponse(reqID json.RawMessage, kind string) ([]byte, error) {
	cat, ok := ToolCatalogForBackend(kind)
	if !ok {
		return nil, fmt.Errorf("unknown backend kind %q", kind)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result":  map[string]any{"tools": cat.Tools},
	}
	return json.Marshal(payload)
}

// --- Embedded catalogs. Update via the maintainer workflow documented at
// the top of the golden test (TestToolCatalog_GoldenAgainstUpstream): run
// upstream, capture tools/list, diff, bump CatalogVersion, paste tools.

// mcpLanguageServerCatalog mirrors the tool set exposed by
// github.com/isaacphi/mcp-language-server. Tool schemas below were captured
// from a live `mcp-language-server.exe -workspace <d> -lsp gopls` handshake
// on 2026-04-20 (Windows, binary version v0.0.2). Regenerating: run the
// golden test (TestToolCatalog_GoldenAgainstUpstream/mcp-language-server),
// paste the captured tools[] into the Tools slice below, bump
// CatalogVersion.
var mcpLanguageServerCatalog = ToolCatalog{
	CatalogVersion:     "mcp-ls-2026-04-20",
	ServerInfoName:     "MCP Language Server",
	ServerInfoVersion:  "v0.0.2",
	ProtocolVersion:    "2024-11-05",
	InstructionsSource: "captured from live `mcp-language-server -workspace <d> -lsp gopls` handshake 2026-04-20 (binary v0.0.2)",
	Tools: []ToolSchema{
		{
			Name:        "definition",
			Description: "Read the source code definition of a symbol (function, type, constant, etc.) from the codebase. Returns the complete implementation code where the symbol is defined.",
			InputSchema: json.RawMessage(`{"properties":{"symbolName":{"description":"The name of the symbol whose definition you want to find (e.g. 'mypackage.MyFunction', 'MyType.MyMethod')","type":"string"}},"required":["symbolName"],"type":"object"}`),
		},
		{
			Name:        "diagnostics",
			Description: "Get diagnostic information for a specific file from the language server.",
			InputSchema: json.RawMessage(`{"properties":{"contextLines":{"default":false,"description":"Lines to include around each diagnostic.","type":"boolean"},"filePath":{"description":"The path to the file to get diagnostics for","type":"string"},"showLineNumbers":{"default":true,"description":"If true, adds line numbers to the output","type":"boolean"}},"required":["filePath"],"type":"object"}`),
		},
		{
			Name:        "edit_file",
			Description: "Apply multiple text edits to a file.",
			InputSchema: json.RawMessage(`{"properties":{"edits":{"description":"List of edits to apply","items":{"properties":{"endLine":{"description":"End line to replace, inclusive, one-indexed","type":"number"},"newText":{"description":"Replacement text. Replace with the new text. Leave blank to remove lines.","type":"string"},"startLine":{"description":"Start line to replace, inclusive, one-indexed","type":"number"}},"required":["startLine","endLine"],"type":"object"},"type":"array"},"filePath":{"description":"Path to the file to edit","type":"string"}},"required":["edits","filePath"],"type":"object"}`),
		},
		{
			Name:        "hover",
			Description: "Get hover information (type, documentation) for a symbol at the specified position.",
			InputSchema: json.RawMessage(`{"properties":{"column":{"description":"The column number where the hover is requested (1-indexed)","type":"number"},"filePath":{"description":"The path to the file to get hover information for","type":"string"},"line":{"description":"The line number where the hover is requested (1-indexed)","type":"number"}},"required":["filePath","line","column"],"type":"object"}`),
		},
		{
			Name:        "references",
			Description: "Find all usages and references of a symbol throughout the codebase. Returns a list of all files and locations where the symbol appears.",
			InputSchema: json.RawMessage(`{"properties":{"symbolName":{"description":"The name of the symbol to search for (e.g. 'mypackage.MyFunction', 'MyType')","type":"string"}},"required":["symbolName"],"type":"object"}`),
		},
		{
			Name:        "rename_symbol",
			Description: "Rename a symbol (variable, function, class, etc.) at the specified position and update all references throughout the codebase.",
			InputSchema: json.RawMessage(`{"properties":{"column":{"description":"The column number where the symbol is located (1-indexed)","type":"number"},"filePath":{"description":"The path to the file containing the symbol to rename","type":"string"},"line":{"description":"The line number where the symbol is located (1-indexed)","type":"number"},"newName":{"description":"The new name for the symbol","type":"string"}},"required":["filePath","line","column","newName"],"type":"object"}`),
		},
	},
}

// goplsMCPCatalog mirrors the tool set exposed by `gopls mcp`. Tool schemas
// below were captured from a live `gopls.exe mcp` handshake on 2026-04-20
// (Windows, binary version v1.0.0). Regenerating: run the golden test
// (TestToolCatalog_GoldenAgainstUpstream/gopls-mcp), paste the captured
// tools[] into the Tools slice below, bump CatalogVersion.
var goplsMCPCatalog = ToolCatalog{
	CatalogVersion:     "gopls-mcp-2026-04-20",
	ServerInfoName:     "gopls",
	ServerInfoVersion:  "v1.0.0",
	ProtocolVersion:    "2024-11-05",
	InstructionsSource: "captured from live `gopls mcp` handshake 2026-04-20 (binary v1.0.0)",
	Tools: []ToolSchema{
		{
			Name:        "go_diagnostics",
			Description: "Provides Go workspace diagnostics.\n\nChecks for parse and build errors across the entire Go workspace. If provided,\n\"files\" holds absolute paths for active files, on which additional linting is\nperformed.\n",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"files":{"type":"array","description":"absolute paths to active files, if any","items":{"type":"string"}}},"additionalProperties":false}`),
		},
		{
			Name:        "go_file_context",
			Description: "Summarizes a file's cross-file dependencies",
			InputSchema: json.RawMessage(`{"type":"object","required":["file"],"properties":{"file":{"type":"string","description":"the absolute path to the file"}},"additionalProperties":false}`),
		},
		{
			Name:        "go_package_api",
			Description: "Provides a summary of a Go package API",
			InputSchema: json.RawMessage(`{"type":"object","required":["packagePaths"],"properties":{"packagePaths":{"type":"array","description":"the go package paths to describe","items":{"type":"string"}}},"additionalProperties":false}`),
		},
		{
			Name:        "go_rename_symbol",
			Description: "Renames a symbol in the Go workspace\n\nFor example, given arguments {\"file\": \"/path/to/foo.go\", \"symbol\": \"Foo\", \"new_name\": \"Bar\"},\ngo_rename_symbol returns the edits necessary to rename the symbol \"Foo\" (located in the file foo.go) to\n\"Bar\" across the Go workspace.",
			InputSchema: json.RawMessage(`{"type":"object","required":["file","symbol","new_name"],"properties":{"file":{"type":"string","description":"the absolute path to the file containing the symbol"},"new_name":{"type":"string","description":"the new name for the symbol"},"symbol":{"type":"string","description":"the symbol or qualified symbol"}},"additionalProperties":false}`),
		},
		{
			Name:        "go_search",
			Description: "Search for symbols in the Go workspace.\n\nSearch for symbols using case-insensitive fuzzy search, which may match all or\npart of the fully qualified symbol name. For example, the query 'foo' matches\nGo symbols 'Foo', 'fooBar', 'futils.Oboe', 'github.com/foo/bar.Baz'.\n\nResults are limited to 100 symbols.\n",
			InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string","description":"the fuzzy search query to use for matching symbols"}},"additionalProperties":false}`),
		},
		{
			Name:        "go_symbol_references",
			Description: "Provides the locations of references to a (possibly qualified)\npackage-level Go symbol referenced from the current file.\n\nFor example, given arguments {\"file\": \"/path/to/foo.go\", \"name\": \"Foo\"},\ngo_symbol_references returns references to the symbol \"Foo\" declared\nin the current package.\n\nSimilarly, given arguments {\"file\": \"/path/to/foo.go\", \"name\": \"lib.Bar\"},\ngo_symbol_references returns references to the symbol \"Bar\" in the imported lib\npackage.\n\nFinally, symbol references supporting querying fields and methods: symbol\n\"T.M\" selects the \"M\" field or method of the \"T\" type (or value), and \"lib.T.M\"\ndoes the same for a symbol in the imported package \"lib\".\n",
			InputSchema: json.RawMessage(`{"type":"object","required":["file","symbol"],"properties":{"file":{"type":"string","description":"the absolute path to the file containing the symbol"},"symbol":{"type":"string","description":"the symbol or qualified symbol"}},"additionalProperties":false}`),
		},
		{
			Name:        "go_vulncheck",
			Description: "Runs a vulnerability check on the Go workspace.\n\n\tThe check is performed on a given package pattern within a specified directory.\n\tIf no directory is provided, it defaults to the workspace root.\n\tIf no pattern is provided, it defaults to \"./...\".",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"dir":{"type":"string","description":"directory to run the vulnerability check within"},"pattern":{"type":"string","description":"package pattern to check"}},"additionalProperties":false}`),
		},
		{
			Name:        "go_workspace",
			Description: "Summarize the Go programming language workspace",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	},
}
