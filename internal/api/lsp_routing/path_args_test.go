package lsp_routing

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestToolCallParamsAndExtractPathArg(t *testing.T) {
	tests := []struct {
		name     string
		params   string
		wantName string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "gopls file",
			params:   `{"name":"go_file_context","arguments":{"file":"C:/work/app/main.go"}}`,
			wantName: "go_file_context",
			wantPath: "C:/work/app/main.go",
			wantOK:   true,
		},
		{
			name:     "gopls files first non-empty",
			params:   `{"name":"go_diagnostics","arguments":{"files":["","C:/work/app/a.go","C:/work/app/b.go"]}}`,
			wantName: "go_diagnostics",
			wantPath: "C:/work/app/a.go",
			wantOK:   true,
		},
		{
			name:     "gopls dir",
			params:   `{"name":"go_vulncheck","arguments":{"dir":"C:/work/app"}}`,
			wantName: "go_vulncheck",
			wantPath: "C:/work/app",
			wantOK:   true,
		},
		{
			name:     "gopls pathless workspace",
			params:   `{"name":"go_workspace","arguments":{}}`,
			wantName: "go_workspace",
		},
		{
			name:     "mcp-language-server filePath",
			params:   `{"name":"diagnostics","arguments":{"filePath":"C:/work/app/src/lib.rs"}}`,
			wantName: "diagnostics",
			wantPath: "C:/work/app/src/lib.rs",
			wantOK:   true,
		},
		{
			name:     "mcp-language-server pathless definition",
			params:   `{"name":"definition","arguments":{"symbolName":"pkg.Symbol"}}`,
			wantName: "definition",
		},
		{
			name:     "priority file before files",
			params:   `{"name":"go_diagnostics","arguments":{"file":"C:/work/app/active.go","files":["C:/work/app/other.go"]}}`,
			wantName: "go_diagnostics",
			wantPath: "C:/work/app/active.go",
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, args := ToolCallParams(json.RawMessage(tt.params))
			if name != tt.wantName {
				t.Fatalf("ToolCallParams name = %q, want %q", name, tt.wantName)
			}
			got, ok := ExtractPathArg(args)
			if got != tt.wantPath || ok != tt.wantOK {
				t.Fatalf("ExtractPathArg = (%q, %v), want (%q, %v)", got, ok, tt.wantPath, tt.wantOK)
			}
		})
	}
}

func TestToolCallParamsRejectsNonObject(t *testing.T) {
	name, args := ToolCallParams(json.RawMessage(`[]`))
	if name != "" || args != nil {
		t.Fatalf("ToolCallParams(non-object) = (%q, %s), want empty", name, string(args))
	}
}

func TestExtractPathArgsReturnsAllFiles(t *testing.T) {
	_, args := ToolCallParams(json.RawMessage(`{"name":"go_diagnostics","arguments":{"files":["","C:/work/app/a.go","C:/work/app/b.go"]}}`))
	got, ok := ExtractPathArgs(args)
	want := []string{"C:/work/app/a.go", "C:/work/app/b.go"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractPathArgs = (%v, %v), want (%v, true)", got, ok, want)
	}

	first, ok := ExtractPathArg(args)
	if first != want[0] || !ok {
		t.Fatalf("ExtractPathArg compatibility = (%q, %v), want (%q, true)", first, ok, want[0])
	}
}
