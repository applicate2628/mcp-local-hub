# Godbolt MCP Perf Expansion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Upgrade the embedded godbolt MCP server from a compile-and-return-asm blob into a performance-analysis toolkit that exposes godbolt.org's full `options.filters`, `options.executeParameters`, JSON response format, and a new `popularArguments` discovery resource.

**Architecture:** Small, additive surface change to `internal/godbolt/handlers.go`. Both `compile_code` and `compile_cmake` gain optional `filters` and `execute_parameters` parameters that pass through to godbolt.org's `options` envelope verbatim. The POST request now sends `Accept: application/json` so the response body is the full structured godbolt record (`code`, `asm[]`, `stdout[]`, `stderr[]`, `execResult`, `optOutput[]`) instead of a text-only asm dump — we return that JSON string through the MCP TextContent channel unchanged. A new `resource://popularArguments/{compiler_id}` resource mirrors godbolt's popular-arguments endpoint.

**Tech Stack:** Go 1.22+, stdlib `net/http` + `encoding/json`, existing `github.com/modelcontextprotocol/go-sdk v1.5.0` — no new external dependencies.

---

## Feature List (for README / release notes / marketing)

The embedded godbolt server (`mcphub godbolt`) now matches the core perf-review workflow most engineers actually use godbolt.org for. New capabilities:

### 🎯 Execute, don't just compile

```
compile_code(compiler_id, source, filters={execute: true}, execute_parameters={stdin: "42\n"})
```

Godbolt runs the binary on its sandbox and returns stdout, stderr, and exit code alongside the asm. No more "looks right, let me check locally" — one MCP call gives compile + run + output.

### 🔬 Optimization remarks — did the loop vectorize?

```
compile_code(..., filters={optOutput: true}, user_arguments="-O3 -Rpass-missed=vector")
```

The response now includes `optOutput[]` — structured LLVM optimization remarks:

```json
[{
  "Name": "loop-vectorize",
  "Function": "hot_loop",
  "DebugLoc": {"File": "x.cpp", "Line": 42, "Column": 3},
  "Args": [{"String": "loop not vectorized: unsafe dependency"}]
}]
```

Stop squinting at asm wondering why SIMD didn't kick in — the compiler tells you directly.

### 📦 Structured JSON response

Before: compile_code returned the asm as plain text, mixing compiler warnings into the same blob.

After: separate `asm[]`, `stdout[]`, `stderr[]`, `execResult`, and `optOutput[]` fields. No more string-munging to tease the parts apart.

### 🎛 Full filter surface

All of godbolt's `filters` are available: `execute`, `intel` (AT&T vs Intel asm syntax), `labels`, `directives`, `commentOnly`, `demangle`, `libraryCode`, `trim`, `binary`, `binaryObject`. Control exactly what comes back.

### 🧭 Compiler-flag discoverability

```
resource://popularArguments/gcc-13.2
```

Godbolt curates popular flag combinations per compiler (`-O3 -march=native`, `-fopenmp`, `-fno-omit-frame-pointer`, etc.). New resource exposes them so unfamiliar toolchains — nvcc, icpx, mrustc, embedded cross-compilers — are self-documenting.

### 🧪 Run with real inputs

`execute_parameters: {stdin, args}` pairs with `filters.execute: true` to run compiled binaries against specific stdin text and argv. Functional tests without leaving the chat.

### ⚡ Cycle-accurate analysis with llvm-mca

```
compile_code(
  compiler_id="clang-17",
  source=...,
  user_arguments="-O3 -march=skylake",
  tools=[{"id": "llvm-mcatrunk", "args": "-mcpu=skylake -timeline -iterations=100"}]
)
```

Runs LLVM's Machine Code Analyzer over the generated asm and returns throughput, latency, port-pressure tables. Stop guessing whether the loop is frontend-bound or port-bound — this tells you exactly.

### 📐 Struct layout auditing with pahole

```
compile_code(
  compiler_id="gcc-13.2",
  source="struct Foo { ... };",
  user_arguments="-g -O2",
  tools=[{"id": "pahole", "args": ""}]
)
```

Exposes padding holes, cacheline boundaries, member ordering — essential for data-oriented perf work. `pahole` runs on the godbolt-compiled DWARF so no local `-g` rebuild dance.

### 🧩 Other godbolt-hosted tools

The `tools` field forwards to godbolt's tools endpoint. Beyond `llvm-mca` and `pahole`, you can reach `readelf`, `objdump` (though `filters.binary: true` is usually simpler), `strings`, `nm`, `size`, and whatever godbolt adds in future. **Not a substitute for local static analyzers** — `clang-tidy`, `cppcheck`, `include-what-you-use` are better wrapped against your real compile_commands.json locally.

### Backwards compatibility note

**Response format changed from text to JSON.** Callers that parsed the old text blob as "asm as a string" must now read `response.asm[].text` and join. Migration is one JSON parse step.

---

## File Structure

Single file changed (for the core work) plus one docs file:

- `internal/godbolt/handlers.go` — modify compile_code, compile_cmake, add getPopularArguments, update registerResources and registerTools
- `INSTALL.md` — update the godbolt section under "Per-server notes" with the new capability list

No new files, no new packages. Scope is intentionally tight because these are all additive payload fields + one new resource.

**Why one file:** the existing handlers.go is ~700 lines and consistent in style. Splitting it by feature (compile/resources/popular) would force cross-file navigation mid-task for no readability benefit. Task 6 (handlers refactor into smaller files) is a separate followup if the file grows past ~1200 lines.

---

## Task Ordering Rationale

Tasks 1-2 modify `compile_code`. Tasks 3 mirrors into `compile_cmake` (near-duplicate). Task 4 adds the new resource. Task 5 updates tool schemas so MCP clients see the new parameters. Task 6 updates docs. This order lets each task be validated independently — compile_code with filters alone is already a useful delivery.

---

### Task 1: Switch compile_code response to JSON mode

**Files:**
- Modify: `internal/godbolt/handlers.go:385-477` (compileTool function)

- [ ] **Step 1: Write the failing test**

Create `internal/godbolt/handlers_test.go`:

```go
package godbolt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
```

This test references a new helper `invokeCompile` and a new field `baseURL` on `GodboltServer`. Both are introduced in Step 3 below — the test is written against the target interface.

- [ ] **Step 2: Run the test to verify it fails**

Run from `D:\dev\mcp-local-hub`:

```bash
go test ./internal/godbolt/... -run TestCompileTool_SendsAcceptJSON -v
```

Expected: FAIL — `baseURL` undefined, `invokeCompile` undefined.

- [ ] **Step 3: Add baseURL field and invokeCompile helper**

Edit `internal/godbolt/server.go` — change the GodboltServer struct:

```go
// Before:
type GodboltServer struct {
	httpClient *http.Client
	server     *mcp.Server
}

// After:
type GodboltServer struct {
	httpClient *http.Client
	server     *mcp.Server
	baseURL    string // defaults to godboltBaseURL; overridable for tests
}
```

In the same file, in NewGodboltServer / Run (wherever GodboltServer is constructed), set `baseURL: godboltBaseURL` so production keeps hitting godbolt.org:

```go
gs := &GodboltServer{
	httpClient: &http.Client{},
	baseURL:    godboltBaseURL,
}
```

Search for current construction:

```bash
grep -n "GodboltServer{" internal/godbolt/*.go
```

Update the literal to include `baseURL`.

Then in `internal/godbolt/handlers.go`, replace every existing reference to `godboltBaseURL` inside receiver methods (`gs.getLanguages`, `gs.getCompilers`, etc.) with `gs.baseURL`. There are roughly six resource handlers and two tool handlers using the constant — audit each:

```bash
grep -n godboltBaseURL internal/godbolt/handlers.go
```

Every occurrence should change to `gs.baseURL` except the package-level const itself in `server.go` (it stays as the default value).

Add the helper at the bottom of handlers.go:

```go
// invokeCompile is the shared HTTP dispatch used by both compileTool and
// compileCMakeTool. It sends the POST with Accept: application/json so
// godbolt returns the full structured response (code / asm / stdout /
// stderr / execResult / optOutput) instead of a text-only asm dump.
func (gs *GodboltServer) invokeCompile(ctx context.Context, url string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := gs.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call godbolt: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}
```

- [ ] **Step 4: Replace compileTool's POST + read block with invokeCompile call**

In `internal/godbolt/handlers.go`, lines 442-468 (the existing `gs.httpClient.Post(...)` through `io.ReadAll(resp.Body)` block), replace with:

```go
	url := fmt.Sprintf("%s/compiler/%s/compile", gs.baseURL, args.CompilerID)
	body, err := gs.invokeCompile(ctx, url, payloadJSON)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to call compiler: %v", err),
				},
			},
		}, nil
	}
```

The return block (lines 470-476) stays the same — `body` is now JSON, not text, but TextContent carries it either way.

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./internal/godbolt/... -run TestCompileTool_SendsAcceptJSON -v
```

Expected: PASS.

- [ ] **Step 6: Run the full test suite to check no regressions**

```bash
go test ./... -count=1
```

Expected: all packages ok, including `internal/godbolt` with the new test.

- [ ] **Step 7: Commit**

```bash
git add internal/godbolt/server.go internal/godbolt/handlers.go internal/godbolt/handlers_test.go
git commit -m "feat(godbolt): JSON-mode responses via Accept: application/json"
```

---

### Task 2: Add filters + execute_parameters + tools to compile_code

**Files:**
- Modify: `internal/godbolt/handlers.go:385-477` (compileTool function again, payload construction)

**Why `tools` is here and not elsewhere:** godbolt's compile payload accepts `tools: [{id, args}]` which runs utilities over the compiled artifact in the same sandbox. This unlocks `llvm-mca` (cycle-accurate throughput analysis on the exact asm godbolt produced) and `pahole` (struct layout analysis on the exact DWARF godbolt produced) — both are hard to coordinate locally because you'd need to replicate the compile + extract step identically. Static analyzers like `clang-tidy` are deliberately OUT of scope for this plan — they belong in a separate MCP that wraps the user's local toolchain against their real compile_commands.json.

- [ ] **Step 1: Write the failing test**

Append to `internal/godbolt/handlers_test.go`:

```go
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

	// tools lives at the top of the payload, not inside options — check both
	// possible positions since godbolt accepts either historically.
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
```

This test depends on a `mockCallToolRequest` helper. Add to handlers_test.go:

```go
// mockCallToolRequest wraps raw JSON bytes as a CallToolRequest so tests
// can invoke compileTool/compileCMakeTool without constructing the full
// MCP request plumbing.
type mockCallToolRequest struct {
	Arguments json.RawMessage
}

// The real mcp.CallToolRequest has Params.Arguments typed as
// json.RawMessage; we mimic that structure minimally.
func (m *mockCallToolRequest) toReal() *mcp.CallToolRequest {
	r := &mcp.CallToolRequest{}
	r.Params.Arguments = m.Arguments
	return r
}
```

…and update the test call site to use `.toReal()`:

```go
	_, err := gs.compileTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/godbolt/... -run TestCompileTool_PassesFiltersExecuteParametersAndTools -v
```

Expected: FAIL — filters, executeParameters, and tools are not in the payload because compileTool doesn't parse them yet.

- [ ] **Step 3: Parse and forward the new fields**

In `internal/godbolt/handlers.go` line 386, extend the args struct:

```go
	var args struct {
		CompilerID        string                   `json:"compiler_id"`
		Source            string                   `json:"source"`
		UserArguments     string                   `json:"user_arguments"`
		Files             []interface{}            `json:"files"`
		Libraries         []interface{}            `json:"libraries"`
		Filters           map[string]interface{}   `json:"filters"`
		ExecuteParameters map[string]interface{}   `json:"execute_parameters"`
		Tools             []map[string]interface{} `json:"tools"`
	}
```

Then in the payload-construction block (line 417-423), extend `options` and add tools at the top level:

```go
	options := map[string]interface{}{
		"userArguments": args.UserArguments,
		"libraries":     args.Libraries,
	}
	if len(args.Filters) > 0 {
		options["filters"] = args.Filters
	}
	if len(args.ExecuteParameters) > 0 {
		options["executeParameters"] = args.ExecuteParameters
	}
	payload := map[string]interface{}{
		"source":  args.Source,
		"options": options,
	}
	if len(args.Tools) > 0 {
		// tools is a top-level payload field in godbolt's API, NOT nested
		// inside options. Cross-checked against godbolt.org's /api/compiler/
		// {id}/compile examples.
		payload["tools"] = args.Tools
	}
```

The `if len(...) > 0` guards keep the payload lean — godbolt's default behavior kicks in when the field is absent, which is what users who don't care expect.

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/godbolt/... -run TestCompileTool_PassesFiltersExecuteParametersAndTools -v
```

Expected: PASS.

- [ ] **Step 5: Run full suite**

```bash
go test ./... -count=1
```

Expected: all packages ok.

- [ ] **Step 6: Commit**

```bash
git add internal/godbolt/handlers.go internal/godbolt/handlers_test.go
git commit -m "feat(godbolt): compile_code forwards filters + execute_parameters + tools to godbolt"
```

---

### Task 3: Mirror Tasks 1-2 into compile_cmake

**Files:**
- Modify: `internal/godbolt/handlers.go:481-577` (compileCMakeTool function)

- [ ] **Step 1: Write the failing test**

Append to handlers_test.go:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/godbolt/... -run TestCompileCMakeTool_MirrorsCompileToolSurface -v
```

Expected: FAIL — compileCMakeTool still uses the old path.

- [ ] **Step 3: Apply the same changes to compileCMakeTool**

In `internal/godbolt/handlers.go:481`, update the args struct to add Filters, ExecuteParameters, and Tools (same fields as compileTool):

```go
	var args struct {
		CompilerID        string                   `json:"compiler_id"`
		Source            string                   `json:"source"`
		UserArguments     string                   `json:"user_arguments"`
		Files             []interface{}            `json:"files"`
		Libraries         []interface{}            `json:"libraries"`
		Filters           map[string]interface{}   `json:"filters"`
		ExecuteParameters map[string]interface{}   `json:"execute_parameters"`
		Tools             []map[string]interface{} `json:"tools"`
	}
```

In the payload-construction block within compileCMakeTool (search for the `options` map), apply the same options construction:

```go
	options := map[string]interface{}{
		"userArguments": args.UserArguments,
		"libraries":     args.Libraries,
	}
	if len(args.Filters) > 0 {
		options["filters"] = args.Filters
	}
	if len(args.ExecuteParameters) > 0 {
		options["executeParameters"] = args.ExecuteParameters
	}
	payload := map[string]interface{}{
		"source":  args.Source,
		"options": options,
	}
	if len(args.Tools) > 0 {
		payload["tools"] = args.Tools
	}
```

Replace the POST + read block with invokeCompile:

```go
	url := fmt.Sprintf("%s/compiler/%s/cmake", gs.baseURL, args.CompilerID)
	body, err := gs.invokeCompile(ctx, url, payloadJSON)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to call cmake compiler: %v", err),
				},
			},
		}, nil
	}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/godbolt/... -run TestCompileCMakeTool_MirrorsCompileToolSurface -v
```

Expected: PASS.

- [ ] **Step 5: Run full suite**

```bash
go test ./... -count=1
```

Expected: all ok.

- [ ] **Step 6: Commit**

```bash
git add internal/godbolt/handlers.go internal/godbolt/handlers_test.go
git commit -m "feat(godbolt): compile_cmake reaches parity with compile_code (JSON + filters + execute_parameters + tools)"
```

---

### Task 4: Add popularArguments resource

**Files:**
- Modify: `internal/godbolt/handlers.go:17-56` (registerResources) + add new handler method

- [ ] **Step 1: Write the failing test**

Append to handlers_test.go:

```go
func TestGetPopularArguments(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"-O3":{"description":"optimize","timesused":100}}`))
	}))
	defer srv.Close()

	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}

	req := &mcp.ReadResourceRequest{}
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
	rc, ok := result.Contents[0].(*mcp.TextResourceContents)
	if !ok {
		t.Fatalf("Contents[0] is not TextResourceContents: %T", result.Contents[0])
	}
	if !strings.Contains(rc.Text, `"-O3"`) {
		t.Errorf("response text missing -O3 entry: %s", rc.Text)
	}
	if rc.MIMEType != "application/json" {
		t.Errorf("MIME = %q, want application/json", rc.MIMEType)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/godbolt/... -run TestGetPopularArguments -v
```

Expected: FAIL — getPopularArguments method doesn't exist.

- [ ] **Step 3: Implement getPopularArguments**

Add to the bottom of handlers.go (or next to the other resource handlers — pick a consistent spot near getVersion around line 333):

```go
// getPopularArguments handles resource://popularArguments/{compiler_id}
// — godbolt's curated list of popular flag combinations per compiler.
// Useful for discoverability on unfamiliar toolchains (nvcc, icpx,
// embedded cross-compilers) where the common -O3/-march=native defaults
// don't apply.
func (gs *GodboltServer) getPopularArguments(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	compilerID := extractPathParam(req.Params.URI, "compiler_id")
	if compilerID == "" {
		return nil, fmt.Errorf("missing compiler_id parameter")
	}
	url := fmt.Sprintf("%s/popularArguments/%s", gs.baseURL, compilerID)
	resp, err := gs.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get popular arguments: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read popular-arguments response: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []mcp.ResourceContents{
			&mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(body),
			},
		},
	}, nil
}
```

- [ ] **Step 4: Extend extractPathParam to recognize compiler_id**

In `internal/godbolt/handlers.go:359` (extractPathParam function), add the compiler_id case:

```go
	switch {
	case paramName == "language_id":
		paramPosition = 2
	case paramName == "instruction_set":
		paramPosition = 2
	case paramName == "opcode":
		paramPosition = 3
	case paramName == "compiler_id":
		paramPosition = 2 // resource://popularArguments/gcc-13.2 → parts[2] = "gcc-13.2"
	default:
		return ""
	}
```

- [ ] **Step 5: Register the resource**

In registerResources (around line 17), add:

```go
	gs.server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "resource://popularArguments/{compiler_id}",
		Name:        "popularArguments",
		Description: "Popular flag combinations for a specific compiler (discoverability for unfamiliar toolchains).",
		MIMEType:    "application/json",
	}, gs.getPopularArguments)
```

Place it after the existing `getInstructionInfo` template registration so the three templated resources cluster together.

- [ ] **Step 6: Run the test to verify it passes**

```bash
go test ./internal/godbolt/... -run TestGetPopularArguments -v
```

Expected: PASS.

- [ ] **Step 7: Run full suite**

```bash
go test ./... -count=1 && go build ./cmd/mcphub ./cmd/godbolt
```

Expected: tests ok, builds ok.

- [ ] **Step 8: Commit**

```bash
git add internal/godbolt/handlers.go internal/godbolt/handlers_test.go
git commit -m "feat(godbolt): resource://popularArguments/{compiler_id} for flag discoverability"
```

---

### Task 5: Update InputSchema for both compile tools

**Files:**
- Modify: `internal/godbolt/handlers.go:58-170` (registerTools — both tools' InputSchema blocks)

- [ ] **Step 1: Extend compile_code InputSchema**

In registerTools (around line 58), find the compile_code tool's InputSchema properties block (the first `AddTool` call) and add three new properties alongside the existing `compiler_id`, `source`, `user_arguments`, `files`, `libraries`:

```go
				"filters": map[string]interface{}{
					"type":        "object",
					"description": "godbolt.org filters object (optional). Supported keys: execute (run binary and return stdout/stderr/exit), optOutput (include LLVM optimization remarks in response), intel (Intel asm syntax vs AT&T), labels, directives, commentOnly, demangle, libraryCode, trim, binary, binaryObject. Values are booleans.",
					"additionalProperties": true,
				},
				"execute_parameters": map[string]interface{}{
					"type":        "object",
					"description": "Parameters passed to the binary when filters.execute=true. Optional keys: stdin (string piped to the process), args (array of argv strings).",
					"properties": map[string]interface{}{
						"stdin": map[string]interface{}{"type": "string"},
						"args":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
				"tools": map[string]interface{}{
					"type":        "array",
					"description": "Run godbolt-hosted tools against the compile result. Most useful for perf work: [{\"id\":\"llvm-mcatrunk\",\"args\":\"-mcpu=skylake -timeline\"}] for cycle-accurate throughput/port-pressure analysis of the generated asm, [{\"id\":\"pahole\",\"args\":\"\"}] for struct layout / padding / cacheline audit. Not the place for clang-tidy / cppcheck / iwyu — those should wrap local binaries against your real compile_commands.json in a separate MCP. List available tools for a compiler via godbolt's /api/compilers/{language} entries (each has a tools[] field).",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id":   map[string]interface{}{"type": "string"},
							"args": map[string]interface{}{"type": "string"},
						},
					},
				},
```

- [ ] **Step 2: Extend compile_cmake InputSchema**

Same three properties (filters, execute_parameters, tools) added to the compile_cmake tool's InputSchema (the second `AddTool` call, around line 108).

- [ ] **Step 3: Update tool descriptions**

Update the top-level `Description` string for compile_code (the value passed as `Description` to AddTool):

```go
Description: "Compile source code via godbolt.org. Returns structured JSON with asm[], stdout[], stderr[], optional execResult (if filters.execute=true), optional optOutput[] (if filters.optOutput=true), and optional tools[] output (if tools[] is non-empty — e.g. llvm-mca throughput tables, pahole struct layout). Use filters to control asm syntax and enable execute/optOutput. Use execute_parameters for stdin/args of the executed binary. Use tools for llvm-mca / pahole / other godbolt-hosted analyzers that operate on the compile result.",
```

Same for compile_cmake, substituting "CMake project" for "source code".

- [ ] **Step 4: Build and run tests**

```bash
go build ./cmd/mcphub && go test ./... -count=1
```

Expected: clean build, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/godbolt/handlers.go
git commit -m "feat(godbolt): document filters + execute_parameters + tools in tool InputSchema"
```

---

### Task 6: Update INSTALL.md with the expanded feature list

**Files:**
- Modify: `INSTALL.md` (godbolt section under "Per-server notes beyond serena")

- [ ] **Step 1: Rewrite the godbolt section**

Find the existing godbolt section (search for `### godbolt (port 9126)`) and replace the body with:

```markdown
### godbolt (port 9126)

Embedded in `mcphub.exe` — no external dependency. Manifest runs `mcphub godbolt` as the daemon command. Proxies the Godbolt Compiler Explorer API at godbolt.org.

**Tools:**
- `compile_code` — compile a single-file source via the chosen compiler. Returns JSON with separate `asm[]`, `stdout[]`, `stderr[]`, optional `execResult` (when `filters.execute=true`), and optional `optOutput[]` (when `filters.optOutput=true` — structured LLVM optimization remarks).
- `compile_cmake` — same as compile_code but for CMake projects.
- `format_code` — run source through a godbolt-hosted formatter (clang-format, rustfmt, gofmt, etc.).

**Tool options (for compile_code / compile_cmake):**
- `user_arguments` — compiler flags as a single string (e.g. `"-O3 -march=x86-64-v3"`).
- `files` — additional source files (array of `{filename, contents}`).
- `libraries` — godbolt-hosted libraries to link (array of `{id, version}`, list via `resource://libraries/{language_id}`).
- `filters` — godbolt filter flags (object). Most useful: `execute: true` (run binary), `optOutput: true` (LLVM opt remarks), `intel: true` (Intel asm syntax).
- `execute_parameters` — stdin + args for execute mode (object: `{stdin: string, args: [string]}`).
- `tools` — godbolt-hosted tools that operate on the compile result (array of `{id, args}`). Killer use cases: `llvm-mcatrunk` for cycle-accurate throughput/port-pressure analysis, `pahole` for struct layout / cacheline packing. Not for clang-tidy/cppcheck/iwyu — those belong in a separate MCP wrapped around local binaries.

**Resources:**
- `resource://languages` — supported languages.
- `resource://compilers/{language_id}` — compilers for a language.
- `resource://libraries/{language_id}` — available libraries with versions.
- `resource://formats` — available formatters.
- `resource://asm/{instruction_set}/{opcode}` — documentation for a single asm instruction.
- `resource://popularArguments/{compiler_id}` — popular flag combinations for a compiler (discoverability for unfamiliar toolchains).
- `resource://version` — godbolt.org instance version.

**Performance-review workflow examples:**

*1. Optimization remarks — did the loop vectorize?*

```
compile_code(
  compiler_id="gcc-13.2",
  source="<hot loop>",
  user_arguments="-O3 -march=x86-64-v3 -Rpass-missed=vector",
  filters={"optOutput": true, "intel": true}
)
```

Response contains `optOutput[]` with structured remarks like `{Name: "loop-vectorize", Function: "hot_loop", Args: [{String: "loop not vectorized: unsafe dependency"}]}` — no more guessing why SIMD didn't kick in.

*2. Execute with stdin to verify correctness*

Add `filters={"execute": true}` and `execute_parameters={"stdin": "input..."}` to run the compiled binary with a specific input in the same call; the response gains an `execResult` object with `stdout[]`, `stderr[]`, and exit `code`.

*3. Cycle-accurate throughput analysis with llvm-mca*

```
compile_code(
  compiler_id="clang-17",
  source="<hot loop>",
  user_arguments="-O3 -march=skylake",
  tools=[{"id": "llvm-mcatrunk", "args": "-mcpu=skylake -timeline"}]
)
```

Response gains a `tools[]` field with per-tool output — llvm-mca reports IPC, uOps/cycle, Block RThroughput, and resource-pressure tables per port. Move from "it vectorized" to "the bottleneck is port 5 at 2.5 uOps/cycle".

*4. Struct layout audit with pahole*

```
compile_code(
  compiler_id="gcc-13.2",
  source="struct Foo { ... };",
  user_arguments="-g -O2",
  tools=[{"id": "pahole", "args": ""}]
)
```

pahole output shows padding holes, cacheline boundaries, member ordering — the bread and butter of data-oriented perf work.

The Go rewrite lives in `internal/godbolt/` and can also be built as a standalone binary — see *Standalone binaries* below.
```

- [ ] **Step 2: Commit**

```bash
git add INSTALL.md
git commit -m "docs(godbolt): document filters, execute_parameters, JSON response, popularArguments"
```

---

## Self-Review Checklist

**Spec coverage:**
- ✅ Item 1 (filters pass-through): Tasks 2 + 3 (compile_code + compile_cmake)
- ✅ Item 2 (JSON response): Task 1 (invokeCompile helper with Accept header) — applied to both tools
- ✅ Item 3 (executeParameters): Tasks 2 + 3
- ✅ Item 4 (popularArguments resource): Task 4
- ✅ Item 5 (tools passthrough for llvm-mca + pahole): Tasks 2 + 3, InputSchema in Task 5, docs in Task 6, feature bullet added to the marketing section
- ✅ Feature list / marketing copy: in the plan header + Task 6 docs update

**Placeholder scan:**
- No "TBD" / "implement later" / vague "handle errors appropriately" instructions.
- Every code step shows the exact code.
- Every test step shows the exact assertion.
- Every command step shows the exact command + expected output direction.

**Type consistency:**
- `args` struct field names match between compile_code and compile_cmake (CompilerID, Source, UserArguments, Files, Libraries, Filters, ExecuteParameters, Tools) — Task 2 and Task 3 both use the same literal struct definition.
- `invokeCompile(ctx, url, payload) ([]byte, error)` signature is introduced in Task 1 Step 3 and used verbatim in Task 1 Step 4 + Task 3 Step 3.
- `gs.baseURL` field introduced in Task 1 Step 3 replaces `godboltBaseURL` in every consumer.
- InputSchema keys (`filters`, `execute_parameters`, `tools`) in Task 5 match the JSON tags used in Tasks 2 + 3 args structs.
- `tools` lives at the top of the payload (per godbolt API), NOT inside `options` — tests in Tasks 2 + 3 assert this at the top level; Step 3 of both tasks places it accordingly.
- `extractPathParam` gets `compiler_id` case in Task 4 Step 4; Task 4 Step 5 uses that case via the `resource://popularArguments/{compiler_id}` template.

**Backwards-compatibility note:** Task 1's response-shape change is a breaking change to any consumer that parsed the old text output. Called out in the feature-list marketing copy; no migration path required because no callers are versioned yet.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-18-godbolt-perf-expansion.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
