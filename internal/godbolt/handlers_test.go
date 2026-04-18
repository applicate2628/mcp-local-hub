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

func TestGetPopularArguments(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"-O3":{"description":"optimize","timesused":100}}`))
	}))
	defer srv.Close()

	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}

	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{}}
	req.Params.URI = "resource://popularArguments/gcc-13.2"

	result, err := gs.getPopularArguments(t.Context(), req)
	if err != nil {
		t.Fatalf("getPopularArguments: %v", err)
	}
	if gotURL != "/api/popularArguments/gcc-13.2" {
		t.Errorf("godbolt URL = %q, want /api/popularArguments/gcc-13.2", gotURL)
	}
	if len(result.Contents) == 0 {
		t.Fatal("empty Contents")
	}
	rc := result.Contents[0]
	if rc == nil {
		t.Fatal("Contents[0] is nil")
	}
	if !strings.Contains(rc.Text, `"-O3"`) {
		t.Errorf("response text missing -O3 entry: %s", rc.Text)
	}
	if rc.MIMEType != "application/json" {
		t.Errorf("MIME = %q, want application/json", rc.MIMEType)
	}
	if rc.URI != "resource://popularArguments/gcc-13.2" {
		t.Errorf("URI = %q, want resource://popularArguments/gcc-13.2", rc.URI)
	}
}

func TestGetCompilers_ExtractsLanguageID(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"gcc-13.2","name":"GCC 13.2"}]`))
	}))
	defer srv.Close()
	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}

	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{}}
	req.Params.URI = "resource://compilers/cpp"
	_, err := gs.getCompilers(t.Context(), req)
	if err != nil {
		t.Fatalf("getCompilers: %v", err)
	}
	if gotURL != "/api/compilers/cpp" {
		t.Errorf("godbolt URL = %q, want /api/compilers/cpp", gotURL)
	}
}

func TestGetLibraries_ExtractsLanguageID(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}

	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{}}
	req.Params.URI = "resource://libraries/rust"
	_, err := gs.getLibraries(t.Context(), req)
	if err != nil {
		t.Fatalf("getLibraries: %v", err)
	}
	if gotURL != "/api/libraries/rust" {
		t.Errorf("godbolt URL = %q, want /api/libraries/rust", gotURL)
	}
}

func TestGetInstructionInfo_ExtractsBothParams(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("MOV instruction docs"))
	}))
	defer srv.Close()
	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}

	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{}}
	req.Params.URI = "resource://asm/x86/mov"
	_, err := gs.getInstructionInfo(t.Context(), req)
	if err != nil {
		t.Fatalf("getInstructionInfo: %v", err)
	}
	if gotURL != "/api/asm/x86/mov" {
		t.Errorf("godbolt URL = %q, want /api/asm/x86/mov (both instruction_set and opcode must extract correctly)", gotURL)
	}
}

func TestCompileCMakeTool_MirrorsCompileToolSurface(t *testing.T) {
	var gotAccept string
	var gotPayload map[string]interface{}
	srv := fakeGodbolt(t, &gotAccept, &gotPayload)
	defer srv.Close()

	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}

	rawArgs, _ := json.Marshal(map[string]interface{}{
		"compiler_id":    "gcc-13.2",
		"source":         "cmake_minimum_required(VERSION 3.20)\nproject(x)\n",
		"user_arguments": "-O3",
		"filters":        map[string]interface{}{"execute": true},
		"execute_parameters": map[string]interface{}{
			"stdin": "hello\n",
		},
		"tools": []map[string]interface{}{
			{"id": "pahole", "args": ""},
		},
	})

	_, err := gs.compileCMakeTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("compileCMakeTool: %v", err)
	}

	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q, want application/json", gotAccept)
	}
	opts := gotPayload["options"].(map[string]interface{})
	filters := opts["filters"].(map[string]interface{})
	if filters["execute"] != true {
		t.Errorf("filters.execute not forwarded: %+v", filters)
	}
	execParams := opts["executeParameters"].(map[string]interface{})
	if execParams["stdin"] != "hello\n" {
		t.Errorf("executeParameters.stdin not forwarded: %+v", execParams)
	}
	tools, ok := gotPayload["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not forwarded or wrong count: %+v", gotPayload["tools"])
	}
}
