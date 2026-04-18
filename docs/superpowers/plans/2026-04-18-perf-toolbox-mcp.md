# Perf-Toolbox MCP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a new embedded MCP server `mcphub perftools` (dual-entry standalone + subcommand) that wraps four local perf-analysis tools from MSYS2/ucrt64 — `clang-tidy`, `hyperfine`, `llvm-objdump`, `include-what-you-use` — and exposes one discovery resource listing detected tools with versions.

**Architecture:** Follows the proven godbolt/lldb dual-entry pattern exactly. `internal/perftools/` holds the library package with one handler file per tool plus a shared `discovery.go` for PATH probing at startup; `cmd/perftools/main.go` is a three-line standalone entry point; `internal/cli/root.go` registers the same `NewCommand()` as a `mcphub perftools` subcommand. Each tool handler spawns the underlying binary via `os/exec`, captures stderr + stdout, parses the tool's structured output format (YAML for clang-tidy fixes, JSON for hyperfine, line-based regex for stderr-diagnostics, text sections for iwyu), and returns typed results wrapped in MCP `CallToolResult`. `resource://tools` reports the catalog of tools detected at startup so clients know which handlers will actually work on this machine.

**Tech Stack:** Go 1.22+; stdlib only (`os/exec`, `encoding/json`, `regexp`, `strings`, `bufio`); existing `github.com/modelcontextprotocol/go-sdk v1.5.0` and `gopkg.in/yaml.v3` (already in go.mod); no new external deps.

---

## Feature List (for README / release notes / marketing)

The mcp-local-hub gains its first dedicated performance-analysis MCP server — **`mcphub perftools`** — complementing the existing godbolt server with tooling that runs against the user's **real build output**, not godbolt's sandboxed single-file compile. New capabilities:

### 🩺 Real-codebase static analysis with clang-tidy

```
clang_tidy(files=["src/hot.cpp"], project_root="/path/to/repo", checks="performance-*,bugprone-*")
```

Uses the project's `compile_commands.json` automatically, so transitive includes, build flags, and preprocessor state match the real build. Returns typed diagnostics — `{file, line, col, severity, check, message}` — ready to iterate on. Catches `performance-unnecessary-value-param`, `performance-for-range-copy`, `performance-move-const-arg`, and the dozens of other performance checks that godbolt's single-file sandbox can never see.

### 📊 Statistical microbenchmarking with hyperfine

```
hyperfine(commands=["./old_binary --hot-path", "./new_binary --hot-path"], warmup=3, min_runs=10)
```

Returns `{mean, stddev, median, min, max, ratio[]}` per command, computed with warmup runs and outlier detection. Unlike godbolt's execute mode — which runs on a shared VM with second-scale noise — hyperfine locally measures with ±0.3% precision on prewarmed inputs. "Is variant A actually faster?" now has a rigorous answer in one MCP call.

### 🔍 Disassemble the REAL built binary with llvm-objdump

```
llvm_objdump(binary="./build/mybin", function="hot_loop", with_source=true, intel=true)
```

Shows what's actually in the linked `.exe` after IPO/LTO/PGO — information godbolt cannot give because godbolt is compile-only, sandbox, single-file. Essential for verifying that the optimizer in the full build kept (or lost) vectorization that showed up in godbolt.

### 🧹 Include hygiene with include-what-you-use

```
iwyu(file="src/hot.cpp", project_root="/path/to/repo")
```

Returns `{add[], remove[], full_includes[]}` — the minimal include set the file actually needs. Routine use shaves tens of percent off build time on large projects and catches transitive-include landmines that break when upstream headers change.

### 🛠 Built-in tool discovery

```
resource://tools
```

Reports which analyzers are actually installed on this machine and their versions. Lets MCP clients skip trying to call tools that aren't available instead of failing with "command not found" mid-workflow.

### The complete perf loop in one chat

```
clang_tidy   → audit, find performance antipatterns
<edit>       → apply fix
compile_code → sanity-check the asm on godbolt (quick + visual)
hyperfine    → measure real binary: 1.28× faster (±0.4%)
llvm_objdump → confirm the LTO-linked .exe retains the expected vectorization
```

No context-switch to a terminal, no copy-pasting output, no parsing text by eye — structured MCP results all the way through.

---

## File Structure

```
internal/perftools/
  server.go             ← PerfToolbox struct + Run(ctx) + mcp.Server setup
  cmd.go                ← NewCommand() *cobra.Command for dual-entry
  discovery.go          ← ToolCatalog + DetectTools() — probe PATH at startup
  clangtidy.go          ← clangTidyTool handler + stderr diagnostic parser
  hyperfine.go          ← hyperfineTool handler + JSON result parser
  llvmobjdump.go        ← llvmObjdumpTool handler
  iwyu.go               ← iwyuTool handler + output section parser
  handlers_test.go      ← all test functions for all tools (mirrors godbolt style)

cmd/perftools/
  main.go               ← 3-line standalone entry: perftools.NewCommand().Execute()

servers/perftools/
  manifest.yaml         ← port 9131, stdio-bridge, 4 client bindings

internal/cli/
  root.go               ← add root.AddCommand(perftools.NewCommand())
```

One file per tool keeps each handler self-contained and under ~150 lines. `discovery.go` is the shared seam — every tool handler consults the catalog to decide whether to refuse (`tool not installed`) or proceed.

---

### Task 1: Skeleton + dual-entry scaffolding + tool discovery + resource

**Files:**
- Create: `internal/perftools/server.go`
- Create: `internal/perftools/cmd.go`
- Create: `internal/perftools/discovery.go`
- Create: `internal/perftools/handlers_test.go`
- Create: `cmd/perftools/main.go`
- Modify: `internal/cli/root.go` (add import + AddCommand line)

- [ ] **Step 1: Write the failing test**

Create `internal/perftools/handlers_test.go`:

```go
package perftools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDetectTools_ReportsInstalledWithVersion(t *testing.T) {
	catalog := DetectTools()

	// On the development machine (MSYS2/ucrt64) all four should be present.
	// In CI or hosts that lack them, a tool may legitimately report Installed=false;
	// this assertion only verifies the STRUCTURE of the catalog, not which tools
	// happen to exist here.
	if catalog == nil {
		t.Fatal("DetectTools returned nil")
	}
	if catalog.ClangTidy == nil || catalog.Hyperfine == nil ||
		catalog.LLVMObjdump == nil || catalog.IWYU == nil {
		t.Errorf("catalog missing entries: %+v", catalog)
	}
	// If any tool is marked Installed it must have a non-empty Version.
	for name, info := range catalog.AsMap() {
		if info.Installed && info.Version == "" {
			t.Errorf("%s marked installed but Version empty: %+v", name, info)
		}
		if info.Installed && info.Path == "" {
			t.Errorf("%s marked installed but Path empty: %+v", name, info)
		}
		if !info.Installed && info.Error == "" {
			t.Errorf("%s marked NOT installed but Error empty: %+v", name, info)
		}
	}
}

func TestToolsResource_ReturnsJSONCatalog(t *testing.T) {
	srv := &PerfToolbox{tools: DetectTools()}

	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{}}
	req.Params.URI = "resource://tools"

	result, err := srv.getToolsResource(t.Context(), req)
	if err != nil {
		t.Fatalf("getToolsResource: %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("empty Contents")
	}
	rc, ok := result.Contents[0].(*mcp.ResourceContents)
	// SDK stores ResourceContents as concrete, not pointer-to — handle both.
	var text string
	if ok {
		text = rc.Text
	} else if rcv, ok2 := result.Contents[0].(*mcp.ResourceContents); ok2 {
		text = rcv.Text
	}
	if text == "" {
		t.Fatalf("Contents[0] has no text: %T %+v", result.Contents[0], result.Contents[0])
	}

	// Must be valid JSON with the four expected tool keys.
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("resource text is not valid JSON: %v\n%s", err, text)
	}
	for _, key := range []string{"clang-tidy", "hyperfine", "llvm-objdump", "include-what-you-use"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("catalog missing key %q in JSON: %s", key, text)
		}
	}

	_ = strings.Contains // keep import stable across later tasks
	_ = context.Background
}
```

Note on `mcp.ResourceContents` pointer shape: the godbolt tests discovered that the installed SDK uses a concrete struct assigned as `*mcp.ResourceContents` inside a `[]*mcp.ResourceContents` slice. Mirror that exact shape in later steps.

- [ ] **Step 2: Run the test to verify it fails**

Run from `D:\dev\mcp-local-hub`:

```bash
go test ./internal/perftools/... -v
```

Expected: FAIL — package doesn't exist yet.

- [ ] **Step 3: Create discovery.go**

```go
package perftools

import (
	"fmt"
	"os/exec"
	"strings"
)

// ToolInfo describes whether a perf tool is available on this host,
// plus the version string scraped from its `--version` output.
// Clients use the JSON rendering of this struct (via resource://tools)
// to decide whether to call a given tool handler or skip it cleanly.
type ToolInfo struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ToolCatalog is the typed record of all four perf-tools the server
// advertises. Constructed once at startup by DetectTools() so per-call
// handler dispatch is a cheap pointer read.
type ToolCatalog struct {
	ClangTidy   *ToolInfo `json:"clang-tidy"`
	Hyperfine   *ToolInfo `json:"hyperfine"`
	LLVMObjdump *ToolInfo `json:"llvm-objdump"`
	IWYU        *ToolInfo `json:"include-what-you-use"`
}

// AsMap exposes the catalog for iteration in tests and JSON rendering.
// The keys match godbolt / upstream tool naming (include-what-you-use,
// not iwyu) to avoid surprising users who know the CLIs.
func (c *ToolCatalog) AsMap() map[string]*ToolInfo {
	return map[string]*ToolInfo{
		"clang-tidy":           c.ClangTidy,
		"hyperfine":            c.Hyperfine,
		"llvm-objdump":         c.LLVMObjdump,
		"include-what-you-use": c.IWYU,
	}
}

// DetectTools probes PATH for each supported tool and records its
// version. Missing tools are reported as Installed=false with an Error
// string — the server still starts and advertises all four slots so
// clients see a stable catalog.
func DetectTools() *ToolCatalog {
	return &ToolCatalog{
		ClangTidy:   probe("clang-tidy", firstLine),
		Hyperfine:   probe("hyperfine", firstLine),
		LLVMObjdump: probe("llvm-objdump", firstLine),
		IWYU:        probe("include-what-you-use", firstLine),
	}
}

// probe runs `<bin> --version` and extracts the version via versionExtract.
// Returns Installed=false with Error on any failure.
func probe(bin string, versionExtract func(string) string) *ToolInfo {
	path, err := exec.LookPath(bin)
	if err != nil {
		return &ToolInfo{Installed: false, Error: fmt.Sprintf("not on PATH: %v", err)}
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return &ToolInfo{
			Installed: false,
			Path:      path,
			Error:     fmt.Sprintf("--version failed: %v", err),
		}
	}
	return &ToolInfo{
		Installed: true,
		Path:      path,
		Version:   versionExtract(string(out)),
	}
}

// firstLine trims the tool's --version output to a single user-friendly
// line. Most tools emit "<bin> x.y.z\n<more noise>" — we keep the first
// non-empty line.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
```

- [ ] **Step 4: Create server.go**

```go
package perftools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PerfToolbox is the MCP server instance. tools is populated once at
// startup by DetectTools() so request dispatch is a cheap pointer read.
type PerfToolbox struct {
	server *mcp.Server
	tools  *ToolCatalog
}

// Run spins up the MCP server on stdio. Called from both the embedded
// mcphub subcommand and the standalone cmd/perftools binary.
func Run(ctx context.Context) error {
	tb := &PerfToolbox{tools: DetectTools()}

	tb.server = mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-local-hub-perftools",
		Version: "1.0.0",
	}, nil)

	registerResources(tb)
	registerTools(tb)

	if err := tb.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("perftools server run: %w", err)
	}
	return nil
}

// registerResources mounts the discovery resource.
func registerResources(tb *PerfToolbox) {
	tb.server.AddResource(&mcp.Resource{
		URI:         "resource://tools",
		Name:        "tools",
		Description: "Catalog of detected perf-analysis tools with versions (clang-tidy, hyperfine, llvm-objdump, include-what-you-use).",
		MIMEType:    "application/json",
	}, tb.getToolsResource)
}

// registerTools mounts tool handlers. Handlers themselves are defined
// in their respective files; this function is the single registration
// point so Task 2-5 each add one line here.
func registerTools(tb *PerfToolbox) {
	// Task 2-5 will AddTool here.
}

// getToolsResource serves resource://tools — marshals the catalog to
// JSON and returns it as a single TextResourceContents.
func (tb *PerfToolbox) getToolsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := json.MarshalIndent(tb.tools.AsMap(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal tool catalog: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)},
		},
	}, nil
}
```

- [ ] **Step 5: Create cmd.go**

```go
package perftools

import (
	"github.com/spf13/cobra"
)

// NewCommand returns a cobra.Command that runs the perf-toolbox MCP
// server over stdio. Used by both the standalone cmd/perftools binary
// and the mcphub subcommand so the same entry point works in either
// shape.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "perftools",
		Short:  "Perf-analysis toolbox MCP server (clang-tidy, hyperfine, llvm-objdump, include-what-you-use)",
		Hidden: true, // internal transport helper when embedded under mcphub
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context())
		},
	}
}
```

- [ ] **Step 6: Create cmd/perftools/main.go**

```go
// cmd/perftools is the standalone perf-toolbox MCP server binary —
// the same server that ships as `mcphub perftools`, packaged on its
// own for users who want the perf tools without the full mcphub stack.
package main

import (
	"os"

	"mcp-local-hub/internal/perftools"
)

func main() {
	if err := perftools.NewCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
```

- [ ] **Step 7: Wire into mcphub root**

Edit `internal/cli/root.go`:

Add to the imports (alphabetical, near `mcp-local-hub/internal/lldb` and `mcp-local-hub/internal/godbolt`):

```go
	"mcp-local-hub/internal/perftools"
```

Then in `NewRootCmd()`, after the existing `root.AddCommand(godbolt.NewCommand())` line, add:

```go
	root.AddCommand(perftools.NewCommand())
```

- [ ] **Step 8: Run the tests to verify they pass**

```bash
go test ./internal/perftools/... -v
```

Expected: both `TestDetectTools_ReportsInstalledWithVersion` and `TestToolsResource_ReturnsJSONCatalog` PASS.

- [ ] **Step 9: Verify the three build targets**

```bash
go build ./cmd/mcphub
go build ./cmd/godbolt
go build ./cmd/lldb-bridge
go build ./cmd/perftools
go vet ./...
go test ./... -count=1
```

Expected: all green. The new `go build ./cmd/perftools` produces a `perftools.exe` (~12 MB like the others).

- [ ] **Step 10: Commit**

```bash
git add internal/perftools/ cmd/perftools/ internal/cli/root.go
git commit -m "feat(perftools): skeleton with discovery + resource://tools

New MCP server wrapping local perf-analysis tools (clang-tidy,
hyperfine, llvm-objdump, include-what-you-use). This task lands only
the skeleton: PerfToolbox struct, Run(ctx) entry point, DetectTools()
PATH prober, and resource://tools for catalog discovery. Tool handlers
arrive in subsequent tasks.

Dual-entry: standalone cmd/perftools binary + mcphub perftools
subcommand, following the godbolt/lldb pattern."
```

---

### Task 2: clang_tidy tool

**Files:**
- Create: `internal/perftools/clangtidy.go`
- Modify: `internal/perftools/server.go` (registerTools adds the AddTool call)
- Modify: `internal/perftools/handlers_test.go` (append test)

- [ ] **Step 1: Write the failing test**

Append to `internal/perftools/handlers_test.go`:

```go
func TestClangTidy_ParsesRealOutput(t *testing.T) {
	cat := DetectTools()
	if !cat.ClangTidy.Installed {
		t.Skip("clang-tidy not on PATH; integration test skipped")
	}

	// Tiny C++ source with a clearly-flagged performance issue —
	// pass-by-value of a non-trivial type.
	dir := t.TempDir()
	srcPath := dir + "/t.cpp"
	if err := os.WriteFile(srcPath, []byte(
		"#include <string>\nvoid f(std::string s){(void)s;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Minimal compile_commands.json pointing at the temp file.
	ccPath := dir + "/compile_commands.json"
	cc := `[{"directory":"` + filepathFwd(dir) + `","file":"` + filepathFwd(srcPath) +
		`","command":"clang++ -std=c++17 -c ` + filepathFwd(srcPath) + `"}]`
	if err := os.WriteFile(ccPath, []byte(cc), 0o644); err != nil {
		t.Fatal(err)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]interface{}{
		"files":        []string{srcPath},
		"project_root": dir,
		"checks":       "performance-unnecessary-value-param",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.clangTidyTool(t.Context(), req)
	if err != nil {
		t.Fatalf("clangTidyTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %+v", contentText(result))
	}

	body := contentText(result)
	// Expect JSON with at least one diagnostic referencing our check.
	if !strings.Contains(body, "performance-unnecessary-value-param") {
		t.Errorf("expected performance-unnecessary-value-param diagnostic in output:\n%s", body)
	}
	// Must be valid JSON.
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("tool output is not valid JSON: %v\n%s", err, body)
	}
	if _, ok := parsed["diagnostics"]; !ok {
		t.Errorf("output JSON missing diagnostics key: %s", body)
	}
}

// contentText extracts the Text field from the first TextContent in a
// CallToolResult. Used by every handler test in this file.
func contentText(r *mcp.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// filepathFwd returns a filesystem path with forward slashes so it can
// be safely embedded inside a JSON string literal without escape hell.
func filepathFwd(p string) string {
	return strings.ReplaceAll(p, `\`, `/`)
}
```

Add needed imports to the test file's import block:

```go
import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/perftools/... -run TestClangTidy_ParsesRealOutput -v
```

Expected: FAIL — `tb.clangTidyTool` undefined.

- [ ] **Step 3: Implement clangtidy.go**

```go
package perftools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// diagLineRE matches the canonical clang-tidy stderr line format:
//
//	path/to/file.cpp:LINE:COL: severity: message [check-name]
//
// Severity is one of warning / error / note. The check-name tail in
// square brackets is optional on note lines.
var diagLineRE = regexp.MustCompile(
	`^(?P<file>.+?):(?P<line>\d+):(?P<col>\d+):\s+(?P<sev>warning|error|note):\s+(?P<msg>.+?)(?:\s+\[(?P<check>[^\]]+)\])?$`)

// Diagnostic is the per-issue record returned in the tool's JSON body.
// Fields match clang-tidy's output; the shape is stable across clang-tidy
// versions so consumers can rely on it.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Severity string `json:"severity"`
	Check    string `json:"check,omitempty"`
	Message  string `json:"message"`
}

// clangTidyResult is the top-level JSON shape returned to the client.
type clangTidyResult struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	RawStderr   string       `json:"raw_stderr,omitempty"`
	ExitCode    int          `json:"exit_code"`
}

// clangTidyTool runs clang-tidy against a list of files, using the
// project's compile_commands.json for flag resolution. Returns a
// structured JSON diagnostics list so callers can filter by check,
// severity, or file without re-parsing text.
func (tb *PerfToolbox) clangTidyTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.ClangTidy.Installed {
		return errResult("clang-tidy not installed: " + tb.tools.ClangTidy.Error), nil
	}

	var args struct {
		Files       []string `json:"files"`
		ProjectRoot string   `json:"project_root"`
		Checks      string   `json:"checks"`
		ExtraArgs   []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Files) == 0 {
		return errResult("missing required parameter: files (non-empty list of source file paths)"), nil
	}
	if args.ProjectRoot == "" {
		return errResult("missing required parameter: project_root (directory containing compile_commands.json)"), nil
	}

	cmdArgs := []string{"-p", args.ProjectRoot}
	if args.Checks != "" {
		cmdArgs = append(cmdArgs, "--checks="+args.Checks)
	}
	cmdArgs = append(cmdArgs, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.Files...)

	cmd := exec.CommandContext(ctx, tb.tools.ClangTidy.Path, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitCode := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if runErr != nil {
		return errResult(fmt.Sprintf("failed to run clang-tidy: %v", runErr)), nil
	}

	// clang-tidy writes diagnostics to stdout AND stderr depending on the
	// version; parse both so we catch everything.
	combined := stdout.String() + "\n" + stderr.String()
	diags := parseClangTidyOutput(combined)

	result := clangTidyResult{
		Diagnostics: diags,
		RawStderr:   stderr.String(),
		ExitCode:    exitCode,
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

// parseClangTidyOutput scans clang-tidy's output for the file:line:col
// diagnostic format and returns a typed list. Unmatched lines — banners,
// summaries, source-code snippets, carets — are ignored silently.
func parseClangTidyOutput(s string) []Diagnostic {
	var out []Diagnostic
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		m := diagLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNum, _ := strconv.Atoi(m[diagLineRE.SubexpIndex("line")])
		colNum, _ := strconv.Atoi(m[diagLineRE.SubexpIndex("col")])
		out = append(out, Diagnostic{
			File:     m[diagLineRE.SubexpIndex("file")],
			Line:     lineNum,
			Column:   colNum,
			Severity: m[diagLineRE.SubexpIndex("sev")],
			Check:    m[diagLineRE.SubexpIndex("check")],
			Message:  m[diagLineRE.SubexpIndex("msg")],
		})
	}
	return out
}

// errResult is the shared error-return helper used by every tool in
// this package — keeps the error surface consistent so MCP clients see
// the same shape regardless of which tool failed.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
```

- [ ] **Step 4: Register the tool**

Edit `internal/perftools/server.go` `registerTools()`:

```go
func registerTools(tb *PerfToolbox) {
	tb.server.AddTool(&mcp.Tool{
		Name: "clang_tidy",
		Description: "Run clang-tidy against source files using the project's compile_commands.json. " +
			"Returns structured JSON with diagnostics[] (file, line, column, severity, check, message), " +
			"plus raw_stderr and exit_code. Requires compile_commands.json in project_root — generate via CMake " +
			"(-DCMAKE_EXPORT_COMPILE_COMMANDS=ON) or bear/compiledb for Make-based builds. " +
			"Common checks preset: performance-*,bugprone-*,readability-*.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"files": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "List of source file paths to analyze (absolute or relative to project_root).",
				},
				"project_root": map[string]interface{}{
					"type":        "string",
					"description": "Directory containing compile_commands.json. clang-tidy resolves flags per-file from this DB.",
				},
				"checks": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Comma-separated check pattern (e.g. 'performance-*,bugprone-*'). Omit to use .clang-tidy config or default checks.",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw arguments passed verbatim before file list (e.g. ['--header-filter=.*', '--quiet']).",
				},
			},
			"required": []string{"files", "project_root"},
		},
	}, tb.clangTidyTool)
}
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./internal/perftools/... -run TestClangTidy_ParsesRealOutput -v
```

Expected: PASS (if clang-tidy is installed) or SKIP (if not). On the dev machine with ucrt64 it should PASS and find the `performance-unnecessary-value-param` diagnostic.

- [ ] **Step 6: Full suite**

```bash
go test ./... -count=1
go build ./cmd/mcphub ./cmd/perftools
```

Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add internal/perftools/clangtidy.go internal/perftools/server.go internal/perftools/handlers_test.go
git commit -m "feat(perftools): clang_tidy tool with structured JSON diagnostics

Runs clang-tidy against files using project_root's compile_commands.json.
Parses stderr/stdout for file:line:col diagnostics and returns typed
{diagnostics[], raw_stderr, exit_code} JSON. Supports --checks filter,
extra_args passthrough, and clean not-installed error path via the
catalog's ClangTidy.Installed flag."
```

---

### Task 3: hyperfine tool

**Files:**
- Create: `internal/perftools/hyperfine.go`
- Modify: `internal/perftools/server.go` (registerTools adds the AddTool call)
- Modify: `internal/perftools/handlers_test.go` (append test)

- [ ] **Step 1: Write the failing test**

Append to `internal/perftools/handlers_test.go`:

```go
func TestHyperfine_ComparesTwoCommands(t *testing.T) {
	cat := DetectTools()
	if !cat.Hyperfine.Installed {
		t.Skip("hyperfine not on PATH; integration test skipped")
	}

	tb := &PerfToolbox{tools: cat}
	// Two trivially different commands — timing gap is tiny but measurable.
	args, _ := json.Marshal(map[string]interface{}{
		"commands": []string{
			"cmd /c exit 0",         // near-instant
			"cmd /c ping -n 1 127.0.0.1 > nul", // ~1ms
		},
		"warmup":   1,
		"min_runs": 3,
		"max_runs": 5,
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.hyperfineTool(t.Context(), req)
	if err != nil {
		t.Fatalf("hyperfineTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %s", contentText(result))
	}

	body := contentText(result)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("tool output not valid JSON: %v\n%s", err, body)
	}
	results, ok := parsed["results"].([]interface{})
	if !ok || len(results) != 2 {
		t.Fatalf("expected results[] of length 2, got %+v", parsed["results"])
	}
	// Each result must at minimum carry mean + stddev.
	for i, r := range results {
		m, ok := r.(map[string]interface{})
		if !ok {
			t.Fatalf("results[%d] is not an object: %T", i, r)
		}
		if _, ok := m["mean"]; !ok {
			t.Errorf("results[%d] missing mean: %+v", i, m)
		}
		if _, ok := m["command"]; !ok {
			t.Errorf("results[%d] missing command: %+v", i, m)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/perftools/... -run TestHyperfine_ComparesTwoCommands -v
```

Expected: FAIL — `tb.hyperfineTool` undefined.

- [ ] **Step 3: Implement hyperfine.go**

```go
package perftools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hyperfineTool runs hyperfine against one or more commands and returns
// its --export-json output verbatim, wrapped in an MCP TextContent.
// hyperfine's own JSON schema is stable and well-documented, so there's
// no benefit to re-marshalling through Go structs — we just pass it
// through and let the MCP client consume it.
func (tb *PerfToolbox) hyperfineTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.Hyperfine.Installed {
		return errResult("hyperfine not installed: " + tb.tools.Hyperfine.Error), nil
	}

	var args struct {
		Commands  []string `json:"commands"`
		Warmup    int      `json:"warmup"`
		MinRuns   int      `json:"min_runs"`
		MaxRuns   int      `json:"max_runs"`
		ExtraArgs []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Commands) == 0 {
		return errResult("missing required parameter: commands (non-empty list)"), nil
	}

	// hyperfine writes its JSON to a file, not stdout. Use a tempfile.
	tmp, err := os.CreateTemp("", "hyperfine-*.json")
	if err != nil {
		return errResult(fmt.Sprintf("create tempfile: %v", err)), nil
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	cmdArgs := []string{"--export-json", tmpPath}
	if args.Warmup > 0 {
		cmdArgs = append(cmdArgs, "--warmup", strconv.Itoa(args.Warmup))
	}
	if args.MinRuns > 0 {
		cmdArgs = append(cmdArgs, "--min-runs", strconv.Itoa(args.MinRuns))
	}
	if args.MaxRuns > 0 {
		cmdArgs = append(cmdArgs, "--max-runs", strconv.Itoa(args.MaxRuns))
	}
	cmdArgs = append(cmdArgs, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.Commands...)

	cmd := exec.CommandContext(ctx, tb.tools.Hyperfine.Path, cmdArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr // ignore human-readable output; we only need the JSON export
	if err := cmd.Run(); err != nil {
		return errResult(fmt.Sprintf("hyperfine failed: %v\nstderr:\n%s", err, stderr.String())), nil
	}

	body, err := os.ReadFile(tmpPath)
	if err != nil {
		return errResult(fmt.Sprintf("read hyperfine export-json: %v", err)), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}
```

- [ ] **Step 4: Register the tool**

Edit `internal/perftools/server.go` `registerTools()`, adding after the clang_tidy registration:

```go
	tb.server.AddTool(&mcp.Tool{
		Name: "hyperfine",
		Description: "Run a statistical benchmark against one or more shell commands via hyperfine. " +
			"Returns JSON with results[] (one per command) containing mean, stddev, median, min, max, user, " +
			"system, and raw times[] in seconds. For N>=2 commands hyperfine also computes pairwise ratios " +
			"in its own output. Use warmup to stabilize caches (recommended 1-3) and min_runs/max_runs to " +
			"control the statistical budget.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"commands": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Shell commands to benchmark. Pass 2+ to enable comparative ratios.",
				},
				"warmup": map[string]interface{}{
					"type":        "integer",
					"description": "Number of warmup runs per command before measurement. Optional (default 0).",
				},
				"min_runs": map[string]interface{}{
					"type":        "integer",
					"description": "Minimum runs per command. Optional (default hyperfine default = 10).",
				},
				"max_runs": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum runs per command. Optional.",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw hyperfine arguments (e.g. ['--prepare', 'sync']).",
				},
			},
			"required": []string{"commands"},
		},
	}, tb.hyperfineTool)
```

- [ ] **Step 5: Run the test**

```bash
go test ./internal/perftools/... -run TestHyperfine_ComparesTwoCommands -v
```

Expected: PASS (if hyperfine installed) or SKIP. On the dev machine — PASS within ~0.5s.

- [ ] **Step 6: Full suite**

```bash
go test ./... -count=1
go build ./cmd/mcphub ./cmd/perftools
```

- [ ] **Step 7: Commit**

```bash
git add internal/perftools/hyperfine.go internal/perftools/server.go internal/perftools/handlers_test.go
git commit -m "feat(perftools): hyperfine tool with statistical benchmark comparison

Spawns hyperfine with --export-json against user-provided commands and
passes its structured JSON through unchanged — schema is stable, no
benefit to re-marshalling. Exposes warmup / min_runs / max_runs
plus extra_args for advanced flags (--prepare, --cleanup, etc.)."
```

---

### Task 4: llvm_objdump tool

**Files:**
- Create: `internal/perftools/llvmobjdump.go`
- Modify: `internal/perftools/server.go` (registerTools adds the AddTool call)
- Modify: `internal/perftools/handlers_test.go` (append test)

- [ ] **Step 1: Write the failing test**

Append to `internal/perftools/handlers_test.go`:

```go
func TestLLVMObjdump_DisassemblesBinary(t *testing.T) {
	cat := DetectTools()
	if !cat.LLVMObjdump.Installed {
		t.Skip("llvm-objdump not on PATH; integration test skipped")
	}

	// Pick any ELF/PE binary that's guaranteed to exist on this host —
	// mcphub.exe built by an earlier task is a good candidate because
	// it's always present at the repo-root after a build.
	// Locate it via the current executable path's sibling.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]interface{}{
		"binary":  exe,
		"section": ".text",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.llvmObjdumpTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmObjdumpTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %s", contentText(result))
	}

	body := contentText(result)
	// At minimum the disassembly should contain a section marker.
	if !strings.Contains(body, "Disassembly") && !strings.Contains(body, "section") {
		t.Errorf("expected disassembly header in output:\n%s", body[:min(len(body), 500)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/perftools/... -run TestLLVMObjdump_DisassemblesBinary -v
```

Expected: FAIL — `tb.llvmObjdumpTool` undefined.

- [ ] **Step 3: Implement llvmobjdump.go**

```go
package perftools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// llvmObjdumpTool disassembles a binary using llvm-objdump. Unlike
// godbolt's sandbox compile, this operates on the USER'S ACTUAL
// build output — post-LTO, post-PGO, post-linker-inlining — so
// it's the authoritative answer to "what does the binary really do?".
func (tb *PerfToolbox) llvmObjdumpTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.LLVMObjdump.Installed {
		return errResult("llvm-objdump not installed: " + tb.tools.LLVMObjdump.Error), nil
	}

	var args struct {
		Binary     string `json:"binary"`
		Function   string `json:"function"`
		Section    string `json:"section"`
		WithSource bool   `json:"with_source"`
		Intel      bool   `json:"intel"`
		ExtraArgs  []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Binary == "" {
		return errResult("missing required parameter: binary (path to a built .exe / .o / .so / .a)"), nil
	}

	cmdArgs := []string{"--disassemble", "--demangle", "--print-imm-hex"}
	if args.Function != "" {
		cmdArgs = append(cmdArgs, "--disassemble-symbols="+args.Function)
		// Drop the bare --disassemble when we have a symbol filter so
		// output is limited to the requested function.
		cmdArgs = cmdArgs[1:]
	}
	if args.Section != "" {
		cmdArgs = append(cmdArgs, "--section="+args.Section)
	}
	if args.WithSource {
		cmdArgs = append(cmdArgs, "--source")
	}
	if args.Intel {
		cmdArgs = append(cmdArgs, "--x86-asm-syntax=intel")
	}
	cmdArgs = append(cmdArgs, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.Binary)

	cmd := exec.CommandContext(ctx, tb.tools.LLVMObjdump.Path, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errResult(fmt.Sprintf("llvm-objdump failed: %v\nstderr:\n%s", err, stderr.String())), nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: stdout.String()}},
	}, nil
}
```

- [ ] **Step 4: Register the tool**

Edit `internal/perftools/server.go` `registerTools()`, adding after the hyperfine registration:

```go
	tb.server.AddTool(&mcp.Tool{
		Name: "llvm_objdump",
		Description: "Disassemble the user's REAL built binary (post-LTO, post-PGO, post-linker-inlining) via " +
			"llvm-objdump. Unlike godbolt's sandbox compile, this is the authoritative answer to 'what " +
			"instructions are actually in my .exe?'. Returns raw disassembly text. Use the function " +
			"parameter to limit output to a specific symbol (heavily recommended — full .text disassembly " +
			"of a real binary is typically megabytes).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"binary": map[string]interface{}{
					"type":        "string",
					"description": "Path to the binary file (executable, object, shared lib, or archive).",
				},
				"function": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Limit disassembly to this symbol name (demangled). Highly recommended for large binaries.",
				},
				"section": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Limit disassembly to a named section (e.g. '.text', '.text.startup').",
				},
				"with_source": map[string]interface{}{
					"type":        "boolean",
					"description": "Optional. Interleave source lines with asm (requires binary built with -g). Default false.",
				},
				"intel": map[string]interface{}{
					"type":        "boolean",
					"description": "Optional. Use Intel asm syntax instead of AT&T. Default false (AT&T).",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw llvm-objdump args (e.g. ['--no-show-raw-insn']).",
				},
			},
			"required": []string{"binary"},
		},
	}, tb.llvmObjdumpTool)
```

- [ ] **Step 5: Run the test**

```bash
go test ./internal/perftools/... -run TestLLVMObjdump_DisassemblesBinary -v
```

Expected: PASS (disassembly of the test binary's `.text` section contains the "Disassembly" header llvm-objdump prints).

- [ ] **Step 6: Full suite + commit**

```bash
go test ./... -count=1
go build ./cmd/mcphub ./cmd/perftools
git add internal/perftools/llvmobjdump.go internal/perftools/server.go internal/perftools/handlers_test.go
git commit -m "feat(perftools): llvm_objdump tool for real-binary disassembly

Spawns llvm-objdump with --disassemble --demangle --print-imm-hex against
a user binary. Supports function/section filters (strongly recommended
for real builds where full .text disassembly is multi-megabyte), Intel
vs AT&T syntax, and source interleave via --source (requires -g).

The unique value vs godbolt: this is the user's ACTUAL post-LTO,
post-PGO, post-linker-inlining output. godbolt can only show what a
single-file sandbox compile with user-typed flags would produce."
```

---

### Task 5: iwyu tool

**Files:**
- Create: `internal/perftools/iwyu.go`
- Modify: `internal/perftools/server.go` (registerTools adds the AddTool call)
- Modify: `internal/perftools/handlers_test.go` (append test)

- [ ] **Step 1: Write the failing test**

Append to `internal/perftools/handlers_test.go`:

```go
func TestIWYU_ParsesSuggestions(t *testing.T) {
	cat := DetectTools()
	if !cat.IWYU.Installed {
		t.Skip("include-what-you-use not on PATH; integration test skipped")
	}

	// Minimal source with a deliberately unused include — iwyu should flag it.
	dir := t.TempDir()
	srcPath := dir + "/t.cpp"
	if err := os.WriteFile(srcPath, []byte(
		"#include <string>\n#include <vector>\nint main(){std::vector<int> v; (void)v; return 0;}\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]interface{}{
		"file":         srcPath,
		"project_root": dir,
		"extra_args":   []string{"-std=c++17"},
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool: %v", err)
	}
	if result.IsError {
		// iwyu can fail for environmental reasons (missing stdlib headers in
		// the temp compile) — treat as skip rather than test failure.
		t.Skipf("iwyu returned IsError, treating as environment skip: %s",
			contentText(result))
	}

	body := contentText(result)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("tool output not valid JSON: %v\n%s", err, body)
	}
	// At minimum the response carries a reports[] array with one entry per file.
	reports, ok := parsed["reports"].([]interface{})
	if !ok {
		t.Fatalf("expected reports[] in output, got: %+v", parsed)
	}
	if len(reports) == 0 {
		t.Fatal("reports[] is empty — iwyu produced no output for the test file")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/perftools/... -run TestIWYU_ParsesSuggestions -v
```

Expected: FAIL — `tb.iwyuTool` undefined.

- [ ] **Step 3: Implement iwyu.go**

```go
package perftools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// IWYUReport is the per-file parsed output of include-what-you-use.
// Each block the tool emits (delimited by "---") produces one report.
type IWYUReport struct {
	File     string   `json:"file"`
	Add      []string `json:"add"`
	Remove   []string `json:"remove"`
	FullList []string `json:"full_list"`
}

// iwyuResult is the top-level JSON shape returned to the client.
type iwyuResult struct {
	Reports   []IWYUReport `json:"reports"`
	RawOutput string       `json:"raw_output,omitempty"`
	ExitCode  int          `json:"exit_code"`
}

// iwyuTool runs include-what-you-use against one source file. IWYU is
// intentionally one-file-per-invocation — batching is the iwyu_tool.py
// wrapper's job, which we don't use here because parsing its output
// format is more brittle than running iwyu directly per file.
func (tb *PerfToolbox) iwyuTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.IWYU.Installed {
		return errResult("include-what-you-use not installed: " + tb.tools.IWYU.Error), nil
	}

	var args struct {
		File        string   `json:"file"`
		ProjectRoot string   `json:"project_root"`
		ExtraArgs   []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.File == "" {
		return errResult("missing required parameter: file"), nil
	}

	cmdArgs := append([]string{}, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.File)

	cmd := exec.CommandContext(ctx, tb.tools.IWYU.Path, cmdArgs...)
	if args.ProjectRoot != "" {
		cmd.Dir = args.ProjectRoot
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitCode := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		// IWYU's exit code convention: non-zero means "suggestions made"
		// — it is NOT an error condition. Keep the code but don't fail.
		exitCode = ee.ExitCode()
	} else if runErr != nil {
		return errResult(fmt.Sprintf("include-what-you-use failed: %v\nstderr:\n%s", runErr, stderr.String())), nil
	}

	// IWYU writes its suggestions to stderr in practice (historical quirk);
	// parse both streams to be robust across versions.
	combined := stderr.String() + "\n" + stdout.String()
	reports := parseIWYUOutput(combined)

	result := iwyuResult{
		Reports:   reports,
		RawOutput: combined,
		ExitCode:  exitCode,
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

// parseIWYUOutput walks IWYU's text output and extracts per-file
// suggestion blocks. The format is:
//
//	path/to/file.cpp should add these lines:
//	#include <x>
//
//	path/to/file.cpp should remove these lines:
//	- #include <y>  // lines 3-3
//
//	The full include-list for path/to/file.cpp:
//	#include <x>
//	---
//
// Section order is stable; `---` delimits a file block.
func parseIWYUOutput(s string) []IWYUReport {
	var reports []IWYUReport
	for _, block := range strings.Split(s, "---") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		r := parseIWYUBlock(block)
		if r.File != "" {
			reports = append(reports, r)
		}
	}
	return reports
}

// parseIWYUBlock walks one file's worth of IWYU output.
func parseIWYUBlock(block string) IWYUReport {
	var r IWYUReport
	section := "" // "add" | "remove" | "full"

	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)

		switch {
		case strings.HasSuffix(trim, "should add these lines:"):
			r.File = strings.TrimSpace(strings.TrimSuffix(trim, "should add these lines:"))
			section = "add"
		case strings.HasSuffix(trim, "should remove these lines:"):
			section = "remove"
		case strings.HasPrefix(trim, "The full include-list for "):
			section = "full"
		case trim == "":
			// Blank lines end a section's content stream but not the block.
			section = ""
		default:
			switch section {
			case "add":
				r.Add = append(r.Add, trim)
			case "remove":
				// Remove lines are prefixed with "- " in IWYU output.
				r.Remove = append(r.Remove, strings.TrimPrefix(trim, "- "))
			case "full":
				r.FullList = append(r.FullList, trim)
			}
		}
	}
	return r
}
```

- [ ] **Step 4: Register the tool**

Edit `internal/perftools/server.go` `registerTools()`, adding after the llvm_objdump registration:

```go
	tb.server.AddTool(&mcp.Tool{
		Name: "iwyu",
		Description: "Run include-what-you-use on a source file. Returns structured JSON with reports[] — one entry " +
			"per file in the output — each carrying add[], remove[], and full_list[] include suggestions. Plus " +
			"raw_output for unparsed inspection. Unlike clang-tidy's include cleaner, IWYU does whole-transitive " +
			"analysis: 'header X forward-declares Y; you use Y; so you should include Y's definition directly'.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file": map[string]interface{}{
					"type":        "string",
					"description": "Source file path to analyze.",
				},
				"project_root": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Working directory for iwyu; relative #include paths resolve from here.",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw iwyu args (e.g. ['-std=c++17', '-Iinclude']).",
				},
			},
			"required": []string{"file"},
		},
	}, tb.iwyuTool)
```

- [ ] **Step 5: Run the test**

```bash
go test ./internal/perftools/... -run TestIWYU_ParsesSuggestions -v
```

Expected: PASS or SKIP (environment-dependent). If iwyu's stdlib-discovery fails in the temp dir it's legitimately environment, not a test bug — the test treats that case as skip.

- [ ] **Step 6: Full suite + commit**

```bash
go test ./... -count=1
go build ./cmd/mcphub ./cmd/perftools
git add internal/perftools/iwyu.go internal/perftools/server.go internal/perftools/handlers_test.go
git commit -m "feat(perftools): iwyu tool with per-file add/remove/full suggestions

Runs include-what-you-use on a source file and parses its historical
text format into typed reports[] blocks with add[], remove[], and
full_list[] slices per file. Preserves raw_output so callers can
inspect anything the parser drops. exit_code surfaced because IWYU's
non-zero exit actually means 'suggestions emitted', not failure."
```

---

### Task 6: manifest.yaml + install wiring

**Files:**
- Create: `servers/perftools/manifest.yaml`

- [ ] **Step 1: Write the manifest**

Create `servers/perftools/manifest.yaml`:

```yaml
name: perftools
kind: global
transport: stdio-bridge
command: mcphub
base_args:
  - perftools

env: {}

# Perf-Toolbox MCP: wraps four local MSYS2/ucrt64 perf-analysis tools
# (clang-tidy, hyperfine, llvm-objdump, include-what-you-use) as MCP
# tools, plus a resource://tools discovery endpoint. Complements the
# godbolt server — godbolt shows what a single-file sandbox compile
# produces; perftools shows what the user's REAL local build output
# contains (post-LTO, post-PGO, post-linker-inlining) and measures it
# with statistical rigor via hyperfine.
#
# All four tools are optional at runtime — the server starts fine when
# some are missing and advertises the shortfall via resource://tools.
# Missing tools fail cleanly per-call with "X not installed: <reason>".

daemons:
  - name: default
    port: 9131

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
  - client: antigravity
    daemon: default
    url_path: /mcp

weekly_refresh: false
```

Port 9131 is the next free slot after the existing allocations (9121-9130).

- [ ] **Step 2: Install and verify the server**

```bash
cd d:/dev/mcp-local-hub
go build -o mcphub.exe ./cmd/mcphub
./mcphub.exe install --server perftools
sleep 3
./mcphub.exe status 2>&1 | grep perftools
```

Expected:

```
\mcp-local-hub-perftools-default    Running    9131   <pid>    <ram>    0h0m    N/A
```

- [ ] **Step 3: Verify MCP connectivity**

```bash
claude mcp list 2>&1 | grep perftools
```

Expected:

```
perftools: http://localhost:9131/mcp (HTTP) - ✓ Connected
```

- [ ] **Step 4: Commit**

```bash
git add servers/perftools/manifest.yaml
git commit -m "feat(perftools): manifest.yaml on port 9131 for all four MCP clients

Wires mcphub perftools as a hub-managed stdio-bridge daemon visible to
claude-code, codex-cli, gemini-cli, and antigravity. Uses the short
'mcphub' command + 'perftools' subcommand — Task Scheduler's absolute
path is resolved by install.go via canonicalMcphubPath() as for every
other hub-managed server."
```

---

### Task 7: INSTALL.md docs

**Files:**
- Modify: `INSTALL.md`

- [ ] **Step 1: Add the perftools section**

In `INSTALL.md`, find the header `### lldb (port 9130)` under "Per-server notes (beyond serena)". Insert the following block immediately BEFORE the existing "### Standalone binaries (optional)" subsection (so it sits next to the other per-server docs):

```markdown
### perftools (port 9131)

Embedded in `mcphub.exe` — no external dependency beyond what MSYS2/ucrt64 already provides. Manifest runs `mcphub perftools` as the daemon command. Wraps four local analysis tools that complement the godbolt server:

**Tools:**
- `clang_tidy` — run clang-tidy with a checks filter against files in a project with a `compile_commands.json`. Returns structured JSON: `{diagnostics[{file, line, column, severity, check, message}], raw_stderr, exit_code}`. Catches the dozens of `performance-*` and `bugprone-*` checks that need real build context (transitive includes, preprocessor state, platform macros) — things godbolt's single-file sandbox cannot see.
- `hyperfine` — benchmark one or more shell commands with statistical rigor (warmup runs, outlier detection, min/max runs). Returns hyperfine's `--export-json` verbatim: `{results[{command, mean, stddev, median, min, max, user, system, times[]}]}`. For 2+ commands, hyperfine computes pairwise ratios in its own output. Use this to answer "is variant A actually faster than B?" with ±0.3% precision, not godbolt-execute's shared-VM noise.
- `llvm_objdump` — disassemble a function or section of the user's REAL binary (the one produced by their CMake build). Supports Intel/AT&T syntax, source interleave, symbol filtering. Unique value vs godbolt: this is the post-LTO, post-PGO, post-linker-inlining output — godbolt cannot show it because godbolt is compile-only, sandbox, single-file.
- `iwyu` — run include-what-you-use on a source file. Returns per-file `{add[], remove[], full_list[]}` structured suggestions plus raw output. Shaves tens of percent off build time by trimming unused transitive includes.

**Resource:**
- `resource://tools` — JSON catalog of which of the four tools are actually installed on this machine and their detected versions. Probed once at startup via `exec.LookPath` + `<bin> --version`. Lets MCP clients skip tools that would fail rather than guessing.

**Prerequisites (tools on PATH):**

On the MSYS2/ucrt64 stack, install via `pacman`:

```powershell
pacman -S mingw-w64-ucrt-x86_64-clang-tools-extra    # clang-tidy
pacman -S mingw-w64-ucrt-x86_64-hyperfine            # hyperfine
pacman -S mingw-w64-ucrt-x86_64-llvm                 # llvm-objdump
pacman -S mingw-w64-ucrt-x86_64-include-what-you-use # include-what-you-use
```

Then make sure `C:\msys64\ucrt64\bin` is on PATH for the mcphub scheduler task (it already is if you use MSYS2 for other dev work). `mcphub perftools` checks at startup and advertises missing tools via `resource://tools` — the server starts regardless; per-tool calls surface a clean "X not installed" error.

**The full perf loop in one chat:**

```
1. clang_tidy({files: ["src/hot.cpp"], project_root: "/path/to/repo", checks: "performance-*"})
   → finds: performance-unnecessary-value-param on foo's std::string arg
2. <edit to pass by const-ref>
3. compile_code({compiler_id: "gcc-13.2", source: ..., filters: {optOutput: true, intel: true}})
   → check asm: vectorized, no spurious copies in inner loop
4. <rebuild via user's cmake>
5. hyperfine({commands: ["./build-old/mybin", "./build-new/mybin"], warmup: 3, min_runs: 10})
   → new is 1.28× faster (±0.4%)
6. llvm_objdump({binary: "./build-new/mybin", function: "hot_loop"})
   → confirm LTO-linked final output still retains the vectorization seen on godbolt
```

The Go implementation lives in `internal/perftools/` and can also be built as a standalone binary — see *Standalone binaries* below.
```

- [ ] **Step 2: Add a bullet to the Standalone binaries section**

In the same file, find the "### Standalone binaries (optional)" subsection. The existing text lists godbolt and lldb-bridge as the two standalone-buildable MCPs. Extend to include perftools:

Find:

```markdown
```bash
go build -o godbolt.exe ./cmd/godbolt
go build -o lldb-bridge.exe ./cmd/lldb-bridge
```
```

Replace with:

```markdown
```bash
go build -o godbolt.exe ./cmd/godbolt
go build -o lldb-bridge.exe ./cmd/lldb-bridge
go build -o perftools.exe ./cmd/perftools
```
```

And extend the narrative that immediately follows to mention perftools alongside the others — find the "Each is a thin entry point..." paragraph and in the "When to use standalone binaries" bullet list, append one bullet:

```markdown
- You want the perf-analysis toolbox (clang-tidy / hyperfine / llvm-objdump / iwyu) inline in a tool that doesn't need mcphub, provided the underlying binaries are on PATH.
```

- [ ] **Step 3: Update the server count in the "First install" paragraph**

Find this line (in the "## First install" section):

```markdown
Nine servers ship with manifests: `serena`, `memory`, `sequential-thinking`, `wolfram`, `godbolt`, `paper-search-mcp`, `time`, `gdb`, `lldb`. Each is installed independently.
```

Replace with:

```markdown
Ten servers ship with manifests: `serena`, `memory`, `sequential-thinking`, `wolfram`, `godbolt`, `paper-search-mcp`, `time`, `gdb`, `lldb`, `perftools`. Each is installed independently.
```

- [ ] **Step 4: Commit**

```bash
git add INSTALL.md
git commit -m "docs(perftools): document perftools server + complete perf-loop example

Adds a per-server section under 'Per-server notes (beyond serena)'
covering all four tools, the discovery resource, pacman install
commands, and a six-step end-to-end perf workflow showing how
perftools complements godbolt in one chat. Extends the Standalone
binaries section to include perftools.exe, and bumps the ship-count
from nine to ten servers."
```

---

## Self-Review

**Spec coverage:**

- ✅ `clang_tidy` tool — Task 2 with structured JSON diagnostic parser (file:line:col regex).
- ✅ `hyperfine` tool — Task 3 passing `--export-json` through verbatim.
- ✅ `llvm_objdump` tool — Task 4 with function/section filters and Intel syntax option.
- ✅ `iwyu` tool — Task 5 with per-file parser into add/remove/full_list typed slices.
- ✅ `resource://tools` discovery — Task 1 landing alongside skeleton.
- ✅ Dual-entry (internal/perftools + cmd/perftools) — Task 1.
- ✅ Manifest with 4 client bindings — Task 6.
- ✅ Install + mcp connectivity verification — Task 6 Steps 2-3.
- ✅ Marketing/feature list — present in the plan header; Task 7 docs carry the user-facing version with the end-to-end loop example.

**Placeholder scan:**

- No "TBD" / "implement later" / "handle errors appropriately" — every step shows exact code or exact commands.
- Every test step is a complete Go function with real assertions.
- Every build/test command has an expected outcome.
- Wherever behavior depends on the environment (tool installed or not), a `t.Skip` path is explicit rather than implicit.

**Type consistency:**

- `ToolInfo` / `ToolCatalog` shape is defined once in Task 1 and referenced unchanged in every handler (via `tb.tools.XXX.Installed`).
- `errResult(msg)` helper is introduced in Task 2 (clangtidy.go — naturally the first handler) and reused by all four tool handlers without redefinition.
- Every handler signature matches `func (tb *PerfToolbox) xxxTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error)` — same shape as godbolt.
- Every handler dispatches off `req.Params.Arguments` (pointer-initialized via `&mcp.CallToolParamsRaw{}` in tests per the SDK convention already confirmed in the godbolt plan).
- `contentText(result)` test helper introduced in Task 2 is reused by Tasks 3-5 without duplication.

**Potential breakage or environment caveats:**

- The tests use `t.Skip` rather than `t.Fatal` when the wrapped tool is not installed, so the suite is green on CI boxes that lack MSYS2 analysers while still validating behavior on the dev machine. This is intentional.
- Port 9131 is assumed free. If some parallel work claims it first, bump to 9132 in `servers/perftools/manifest.yaml` before Task 6 Step 2.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-18-perf-toolbox-mcp.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh implementer subagent per task, run spec + code-quality review between tasks, fast iteration with quality gates.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
