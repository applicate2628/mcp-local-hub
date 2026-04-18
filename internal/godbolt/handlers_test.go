package godbolt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeGodbolt is a minimal godbolt-like stub that echoes the Accept
// header and payload back so tests can assert we sent the right request
// and still receive a valid JSON response to exercise the parser.
func fakeGodbolt(t *testing.T, gotAccept *string, gotPayload *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAccept = r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, gotPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"asm":[{"text":"ret"}],"stdout":[],"stderr":[]}`))
	}))
}

func TestCompileTool_SendsAcceptJSON(t *testing.T) {
	var gotAccept string
	var gotPayload map[string]interface{}
	srv := fakeGodbolt(t, &gotAccept, &gotPayload)
	defer srv.Close()

	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}
	out, err := gs.invokeCompile(t.Context(), srv.URL+"/api/compiler/gcc/compile", []byte(`{"source":"int main(){}"}`))
	if err != nil {
		t.Fatalf("invokeCompile: %v", err)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q, want application/json", gotAccept)
	}
	if !strings.Contains(string(out), `"asm":[{"text":"ret"}]`) {
		t.Errorf("response body missing structured asm field: %s", out)
	}
}

// mockCallToolRequest wraps raw JSON bytes as a CallToolRequest so tests
// can invoke compileTool/compileCMakeTool without constructing the full
// MCP request plumbing.
type mockCallToolRequest struct {
	Arguments json.RawMessage
}

// The real mcp.CallToolRequest has Params typed as *CallToolParamsRaw,
// whose Arguments field is json.RawMessage; we mimic that structure
// minimally.
func (m *mockCallToolRequest) toReal() *mcp.CallToolRequest {
	r := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}}
	r.Params.Arguments = m.Arguments
	return r
}

func TestCompileTool_PassesFiltersExecuteParametersAndTools(t *testing.T) {
	var gotAccept string
	var gotPayload map[string]interface{}
	srv := fakeGodbolt(t, &gotAccept, &gotPayload)
	defer srv.Close()

	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}

	rawArgs, _ := json.Marshal(map[string]interface{}{
		"compiler_id":    "gcc-13.2",
		"source":         "int main(){return 0;}",
		"user_arguments": "-O3",
		"filters": map[string]interface{}{
			"execute":   true,
			"intel":     true,
			"optOutput": true,
		},
		"execute_parameters": map[string]interface{}{
			"stdin": "42\n",
			"args":  []string{"--flag"},
		},
		"tools": []map[string]interface{}{
			{"id": "llvm-mcatrunk", "args": "-mcpu=skylake -timeline"},
			{"id": "pahole", "args": ""},
		},
	})

	_, err := gs.compileTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("compileTool returned error: %v", err)
	}

	opts, ok := gotPayload["options"].(map[string]interface{})
	if !ok {
		t.Fatalf("options missing from payload: %+v", gotPayload)
	}
	filters, ok := opts["filters"].(map[string]interface{})
	if !ok {
		t.Fatalf("filters not forwarded: options=%+v", opts)
	}
	if filters["execute"] != true || filters["intel"] != true || filters["optOutput"] != true {
		t.Errorf("filters missing expected values: %+v", filters)
	}
	execParams, ok := opts["executeParameters"].(map[string]interface{})
	if !ok {
		t.Fatalf("executeParameters not forwarded: options=%+v", opts)
	}
	if execParams["stdin"] != "42\n" {
		t.Errorf("stdin not forwarded: %+v", execParams)
	}

	// tools lives at the TOP of the payload, not inside options.
	tools, ok := gotPayload["tools"].([]interface{})
	if !ok {
		t.Fatalf("tools not forwarded at top level: payload=%+v", gotPayload)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %+v", len(tools), tools)
	}
	firstTool, ok := tools[0].(map[string]interface{})
	if !ok {
		t.Fatalf("tools[0] is not an object: %T", tools[0])
	}
	if firstTool["id"] != "llvm-mcatrunk" {
		t.Errorf("tools[0].id = %v, want llvm-mcatrunk", firstTool["id"])
	}
}
