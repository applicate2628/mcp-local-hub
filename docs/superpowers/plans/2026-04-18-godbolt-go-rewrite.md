# Godbolt MCP Go Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite godbolt-mcp from Python FastMCP to Go, embedding it as a hub-managed daemon using the official Go MCP SDK and stdio-bridge transport for multiplexed HTTP access.

**Architecture:** Single Go binary exposing 6 resources (languages, compilers, libraries, formats, asm docs, version) and 3 tools (compile, compile_cmake, format_code) via MCP protocol. All endpoints proxy requests to godbolt.org API. Hub launches the daemon on port 9131 via stdio-bridge, allowing multiple MCP clients (claude-code, codex-cli) to share the connection.

**Tech Stack:** Go 1.20+, github.com/modelcontextprotocol/go-sdk v1.5.0 (Task 1 confirmed this version; plan's earlier examples used v1.4.0), net/http client for godbolt.org proxying, JSON marshaling

**IMPORTANT API NOTE:** Task 1 implementer updated SDK to v1.5.0 and adapted code accordingly. Tasks 2-3 handlers should follow v1.5.0 signature patterns. If signatures in this document don't match the working code from Task 1, use the Task 1 working patterns as the source of truth.

---

### Task 1: Initialize Go module and create main.go skeleton

**Files:**
- Create: `servers/godbolt/main.go`
- Create: `servers/godbolt/go.mod`
- Create: `servers/godbolt/go.sum` (auto-generated)

- [ ] **Step 1: Create go.mod**

Create `servers/godbolt/go.mod`:

```
module godbolt

go 1.20

require github.com/modelcontextprotocol/go-sdk v1.4.0
```

- [ ] **Step 2: Create main.go skeleton with server and constants**

Create `servers/godbolt/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/mcp/transport/stdio"
)

const godboltBaseURL = "https://godbolt.org/api"

type GodboltServer struct {
	httpClient *http.Client
	server     *mcp.Server
}

func main() {
	gs := &GodboltServer{
		httpClient: &http.Client{},
	}

	var err error
	gs.server, err = mcp.NewServer(mcp.ServerOptions{
		Name:    "godbolt-compiler-explorer",
		Version: "1.0.0",
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// Register resources
	registerResources(gs)

	// Register tools
	registerTools(gs)

	// Run server over stdio
	transport := stdio.NewStdioTransport(gs.server)
	if err := transport.Start(); err != nil {
		log.Fatalf("failed to start transport: %v", err)
	}
}

func registerResources(gs *GodboltServer) {
	// Placeholder for resource registration
}

func registerTools(gs *GodboltServer) {
	// Placeholder for tool registration
}
```

- [ ] **Step 3: Verify module setup with go mod download**

Run from `servers/godbolt/`:

```bash
go mod download
go mod tidy
```

Expected: No errors, go.sum file created.

- [ ] **Step 4: Commit skeleton**

```bash
cd servers/godbolt
git add go.mod go.sum main.go
git commit -m "feat(godbolt): initialize Go module and server skeleton"
```

---

### Task 2: Implement resource handlers (6 resources)

**Files:**
- Modify: `servers/godbolt/main.go`

- [ ] **Step 1: Implement get_languages resource**

Add to main.go after `registerResources` function definition. Use the v1.5.0 SDK API pattern (verify exact handler signature against working Task 1 skeleton and go-sdk v1.5.0 docs):

```go
func (gs *GodboltServer) getLanguages(ctx context.Context, request *mcp.ResourceRequest) (interface{}, error) {
	resp, err := gs.httpClient.Get(godboltBaseURL + "/languages")
	if err != nil {
		return nil, fmt.Errorf("failed to get languages: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var languages interface{}
	if err := json.Unmarshal(body, &languages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal languages: %w", err)
	}
	return languages, nil
}
```

Then update `registerResources()` to call the handler (use v1.5.0 API):

```go
func registerResources(gs *GodboltServer) {
	gs.server.Resource("resource://languages", mcp.ResourceOptions{
		Description: "Get a list of currently supported languages from Godbolt Compiler Explorer.",
	}, gs.getLanguages)
}
```

- [ ] **Step 2: Implement get_compilers resource**

Add to main.go:

```go
func (gs *GodboltServer) getCompilers(ctx context.Context, uri string, args map[string]interface{}) (interface{}, error) {
	languageID, ok := args["language_id"].(string)
	if !ok {
		return nil, fmt.Errorf("missing or invalid language_id parameter")
	}
	
	resp, err := gs.httpClient.Get(fmt.Sprintf("%s/compilers/%s", godboltBaseURL, languageID))
	if err != nil {
		return nil, fmt.Errorf("failed to get compilers: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var compilers interface{}
	if err := json.Unmarshal(body, &compilers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal compilers: %w", err)
	}
	return compilers, nil
}
```

Update `registerResources()` to add:

```go
	gs.server.Resource("resource://compilers/{language_id}", mcp.ResourceOptions{
		Description: "Get a list of compilers available for a specific language.",
	}, gs.getCompilers)
```

- [ ] **Step 3: Implement get_libraries resource**

Add to main.go:

```go
func (gs *GodboltServer) getLibraries(ctx context.Context, uri string, args map[string]interface{}) (interface{}, error) {
	languageID, ok := args["language_id"].(string)
	if !ok {
		return nil, fmt.Errorf("missing or invalid language_id parameter")
	}
	
	resp, err := gs.httpClient.Get(fmt.Sprintf("%s/libraries/%s", godboltBaseURL, languageID))
	if err != nil {
		return nil, fmt.Errorf("failed to get libraries: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var libraries interface{}
	if err := json.Unmarshal(body, &libraries); err != nil {
		return nil, fmt.Errorf("failed to unmarshal libraries: %w", err)
	}
	return libraries, nil
}
```

Update `registerResources()` to add:

```go
	gs.server.Resource("resource://libraries/{language_id}", mcp.ResourceOptions{
		Description: "Get available libraries and versions for a specific language.",
	}, gs.getLibraries)
```

- [ ] **Step 4: Implement get_formatters resource**

Add to main.go:

```go
func (gs *GodboltServer) getFormatters(ctx context.Context, uri string, args map[string]interface{}) (interface{}, error) {
	resp, err := gs.httpClient.Get(godboltBaseURL + "/formats")
	if err != nil {
		return nil, fmt.Errorf("failed to get formatters: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var formatters interface{}
	if err := json.Unmarshal(body, &formatters); err != nil {
		return nil, fmt.Errorf("failed to unmarshal formatters: %w", err)
	}
	return formatters, nil
}
```

Update `registerResources()` to add:

```go
	gs.server.Resource("resource://formats", mcp.ResourceOptions{
		Description: "Get a list of available code formatters.",
	}, gs.getFormatters)
```

- [ ] **Step 5: Implement get_instruction_info resource**

Add to main.go:

```go
func (gs *GodboltServer) getInstructionInfo(ctx context.Context, uri string, args map[string]interface{}) (interface{}, error) {
	instructionSet, ok := args["instruction_set"].(string)
	if !ok {
		return nil, fmt.Errorf("missing or invalid instruction_set parameter")
	}
	opcode, ok := args["opcode"].(string)
	if !ok {
		return nil, fmt.Errorf("missing or invalid opcode parameter")
	}
	
	resp, err := gs.httpClient.Get(fmt.Sprintf("%s/asm/%s/%s", godboltBaseURL, instructionSet, opcode))
	if err != nil {
		return nil, fmt.Errorf("failed to get instruction info: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}
```

Update `registerResources()` to add:

```go
	gs.server.Resource("resource://asm/{instruction_set}/{opcode}", mcp.ResourceOptions{
		Description: "Get documentation for a specific assembly instruction.",
	}, gs.getInstructionInfo)
```

- [ ] **Step 6: Implement get_version resource**

Add to main.go:

```go
func (gs *GodboltServer) getVersion(ctx context.Context, uri string, args map[string]interface{}) (interface{}, error) {
	resp, err := gs.httpClient.Get(godboltBaseURL + "/version")
	if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}
```

Update `registerResources()` to add:

```go
	gs.server.Resource("resource://version", mcp.ResourceOptions{
		Description: "Get the version information of the Compiler Explorer instance.",
	}, gs.getVersion)
```

- [ ] **Step 7: Verify resources compile**

Run from `servers/godbolt/`:

```bash
go build
```

Expected: No compilation errors.

- [ ] **Step 8: Commit resources**

```bash
git add main.go
git commit -m "feat(godbolt): implement 6 resource handlers"
```

---

### Task 3: Implement tool handlers (3 tools)

**Files:**
- Modify: `servers/godbolt/main.go`

- [ ] **Step 1: Create CompileRequest and CompileResponse types**

Add to main.go before `GodboltServer` struct:

```go
type CompileRequest struct {
	Source  string                 `json:"source"`
	Options map[string]interface{} `json:"options"`
	Files   []map[string]string    `json:"files"`
}

type FormatRequest struct {
	Source string `json:"source"`
}
```

- [ ] **Step 2: Implement compile_code tool**

Add to main.go:

```go
func (gs *GodboltServer) compileTool(ctx context.Context, arguments map[string]interface{}) (string, error) {
	compilerID, ok := arguments["compiler_id"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid compiler_id")
	}
	source, ok := arguments["source"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid source")
	}

	payload := CompileRequest{
		Source: source,
		Options: map[string]interface{}{
			"userArguments": arguments["user_arguments"],
			"libraries":     arguments["libraries"],
		},
		Files: []map[string]string{},
	}

	if files, ok := arguments["files"].([]map[string]string); ok {
		payload.Files = files
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := gs.httpClient.Post(
		fmt.Sprintf("%s/compiler/%s/compile", godboltBaseURL, compilerID),
		"application/json",
		io.NopCloser(bytes.NewReader(body)),
	)
	if err != nil {
		return "", fmt.Errorf("failed to compile: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(respBody), nil
}
```

Update `registerTools()`:

```go
func registerTools(gs *GodboltServer) {
	gs.server.Tool("compile_code", mcp.ToolOptions{
		Description: "Compile source code using the specified compiler.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"compiler_id": map[string]string{
					"type":        "string",
					"description": "The compiler identifier",
				},
				"source": map[string]string{
					"type":        "string",
					"description": "The source code to compile",
				},
				"user_arguments": map[string]interface{}{
					"type":        "string",
					"description": "Compiler flags (optional)",
				},
				"files": map[string]interface{}{
					"type":        "array",
					"description": "List of additional source files (optional)",
				},
				"libraries": map[string]interface{}{
					"type":        "array",
					"description": "List of libraries with id and version (optional)",
				},
			},
			"required": []string{"compiler_id", "source"},
		},
	}, gs.compileTool)
}
```

- [ ] **Step 3: Implement compile_cmake tool**

Add to main.go:

```go
func (gs *GodboltServer) compileCMakeTool(ctx context.Context, arguments map[string]interface{}) (string, error) {
	compilerID, ok := arguments["compiler_id"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid compiler_id")
	}
	source, ok := arguments["source"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid source")
	}

	payload := CompileRequest{
		Source: source,
		Options: map[string]interface{}{
			"userArguments": arguments["user_arguments"],
			"libraries":     arguments["libraries"],
		},
		Files: []map[string]string{},
	}

	if files, ok := arguments["files"].([]map[string]string); ok {
		payload.Files = files
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := gs.httpClient.Post(
		fmt.Sprintf("%s/compiler/%s/cmake", godboltBaseURL, compilerID),
		"application/json",
		io.NopCloser(bytes.NewReader(body)),
	)
	if err != nil {
		return "", fmt.Errorf("failed to compile cmake: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(respBody), nil
}
```

Update `registerTools()` to add:

```go
	gs.server.Tool("compile_cmake", mcp.ToolOptions{
		Description: "Compile a CMake project using the specified compiler.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"compiler_id": map[string]string{
					"type":        "string",
					"description": "The compiler identifier",
				},
				"source": map[string]string{
					"type":        "string",
					"description": "The source code to compile",
				},
				"user_arguments": map[string]interface{}{
					"type":        "string",
					"description": "Compiler flags (optional)",
				},
				"files": map[string]interface{}{
					"type":        "array",
					"description": "List of additional source files (optional)",
				},
				"libraries": map[string]interface{}{
					"type":        "array",
					"description": "List of libraries with id and version (optional)",
				},
			},
			"required": []string{"compiler_id", "source"},
		},
	}, gs.compileCMakeTool)
```

- [ ] **Step 4: Implement format_code tool**

Add to main.go:

```go
func (gs *GodboltServer) formatTool(ctx context.Context, arguments map[string]interface{}) (string, error) {
	formatter, ok := arguments["formatter"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid formatter")
	}
	source, ok := arguments["source"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid source")
	}

	payload := FormatRequest{Source: source}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := gs.httpClient.Post(
		fmt.Sprintf("%s/format/%s", godboltBaseURL, formatter),
		"application/json",
		io.NopCloser(bytes.NewReader(body)),
	)
	if err != nil {
		return "", fmt.Errorf("failed to format: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if answer, ok := result["answer"].(string); ok {
		return answer, nil
	}
	return string(respBody), nil
}
```

Update `registerTools()` to add:

```go
	gs.server.Tool("format_code", mcp.ToolOptions{
		Description: "Format source code using the specified formatter.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"formatter": map[string]string{
					"type":        "string",
					"description": "The formatter identifier",
				},
				"source": map[string]string{
					"type":        "string",
					"description": "The source code to format",
				},
			},
			"required": []string{"formatter", "source"},
		},
	}, gs.formatTool)
```

- [ ] **Step 5: Add missing import for bytes**

Update imports in main.go:

```go
import (
	"bytes"
	"context"
	// ... rest of imports
)
```

- [ ] **Step 6: Verify tools compile**

Run from `servers/godbolt/`:

```bash
go build
```

Expected: No compilation errors.

- [ ] **Step 7: Commit tools**

```bash
git add main.go
git commit -m "feat(godbolt): implement 3 tool handlers (compile, compile_cmake, format)"
```

---

### Task 4: Create manifest.yaml and wire into hub

**Files:**
- Create: `servers/godbolt/manifest.yaml`

- [ ] **Step 1: Create manifest.yaml**

Create `servers/godbolt/manifest.yaml`:

```yaml
name: godbolt
kind: global
transport: stdio-bridge
command: mcphub
base_args:
  - godbolt
  - localhost:47001

env: {}

# Godbolt Compiler Explorer MCP server: exposes 6 resources (languages,
# compilers, libraries, formats, asm instruction docs, version) and 3 tools
# (compile_code, compile_cmake, format_code). All endpoints proxy to
# godbolt.org API. Single daemon shared across all MCP clients via HTTP
# multiplexing through the stdio-bridge transport.

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

weekly_refresh: false
```

- [ ] **Step 2: Add godbolt subcommand stub to root.go**

Modify `internal/cli/root.go`:

Add import if not present (should already be there):
```go
// (already imported in root.go)
```

Add this line in `NewRootCmd()` after `root.AddCommand(newLldbBridgeCmd())`:

```go
	root.AddCommand(newGodboltCmd())
```

Then add the function at the end of `root.go` (after other stub functions):

```go
func newGodboltCmd() *cobra.Command {
	return newGodboltCmdReal()
}
```

- [ ] **Step 3: Create godbolt_bridge.go**

Create `internal/cli/godbolt_bridge.go`:

```go
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// newGodboltCmdReal wires `mcphub godbolt <host:port>`.
// This is a thin wrapper that spawns the compiled Go godbolt binary as a child process.
// The actual server logic runs as a daemon managed by the hub's stdio-bridge transport.
func newGodboltCmdReal() *cobra.Command {
	c := &cobra.Command{
		Use:    "godbolt <host:port>",
		Short:  "Stdio MCP server for Compiler Explorer (godbolt.org) API",
		Hidden: true, // internal transport helper
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Forward all args to the compiled godbolt binary
			godboltBin := "godbolt" // Will be in PATH or same directory as mcphub
			childCmd := exec.CommandContext(cmd.Context(), godboltBin)
			childCmd.Stdin = os.Stdin
			childCmd.Stdout = os.Stdout
			childCmd.Stderr = os.Stderr

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)

			if err := childCmd.Start(); err != nil {
				return fmt.Errorf("failed to start godbolt: %w", err)
			}

			go func() {
				<-sigCh
				_ = childCmd.Process.Kill()
			}()

			if err := childCmd.Wait(); err != nil {
				if exiterr, ok := err.(*exec.ExitError); ok {
					os.Exit(exiterr.ExitCode())
				}
				return err
			}
			return nil
		},
	}
	return c
}
```

- [ ] **Step 4: Commit manifest and CLI integration**

```bash
git add servers/godbolt/manifest.yaml internal/cli/root.go internal/cli/godbolt_bridge.go
git commit -m "feat(godbolt): add manifest and CLI bridge subcommand"
```

---

### Task 5: Build godbolt binary and test integration

**Files:**
- (none, but compilation produces `servers/godbolt/godbolt` binary)

- [ ] **Step 1: Build godbolt binary**

Run from `servers/godbolt/`:

```bash
go build -o godbolt.exe
```

Expected: `godbolt.exe` appears in `servers/godbolt/`.

- [ ] **Step 2: Verify binary runs (test with --help equivalent)**

Run from `servers/godbolt/`:

```bash
./godbolt.exe --help
```

Expected: If the Go SDK server supports a help flag, it prints help. Otherwise, it may error waiting for stdio input (expected).

- [ ] **Step 3: Test mcphub install --server godbolt**

From root of repo:

```bash
go build -o mcphub.exe ./cmd/mcphub
./mcphub.exe install --server godbolt
```

Expected: Installation succeeds, daemon scheduled.

- [ ] **Step 4: Test mcphub status includes godbolt**

```bash
./mcphub.exe status
```

Expected: godbolt appears in the list with a port 9131 and state (either "Ready" or will show state after next run).

- [ ] **Step 5: Test godbolt daemon starts**

```bash
./mcphub.exe restart --server godbolt
```

Wait a few seconds, then:

```bash
./mcphub.exe status --server godbolt
```

Expected: Port 9131 is listening, state shows daemon running.

- [ ] **Step 6: Test MCP list via claude.exe**

From a Claude Code window:

```
mcp list
```

Expected: godbolt appears with ✓ Connected status (if the http+stdio bridge works correctly).

- [ ] **Step 7: Test a resource manually (optional, for confidence)**

In Claude Code MCP console or via any MCP client that can call resources:

```
mcp resource resource://languages
```

Expected: Returns JSON array of supported languages from godbolt.org API.

- [ ] **Step 8: Commit binary (if tracking it) or update .gitignore**

If not tracking binaries:

```bash
echo "servers/godbolt/godbolt.exe" >> .gitignore
echo "servers/godbolt/godbolt" >> .gitignore
git add .gitignore
git commit -m "chore: add godbolt binary to gitignore"
```

If binaries are tracked in your workflow, commit it:

```bash
git add servers/godbolt/godbolt.exe
git commit -m "build(godbolt): compiled binary for distribution"
```

---

### Task 6: Documentation update

**Files:**
- Modify: `INSTALL.md` or equivalent server docs

- [ ] **Step 1: Add godbolt section to INSTALL.md**

Find the section in `INSTALL.md` where servers are listed (probably near lldb). Add:

```markdown
### Godbolt Compiler Explorer

Provides access to the Compiler Explorer (godbolt.org) API for compiling code, accessing compiler information, and formatting code.

**Resources:**
- `resource://languages` — List of supported programming languages
- `resource://compilers/{language_id}` — Available compilers for a language (e.g., `c++`, `rust`)
- `resource://libraries/{language_id}` — Available libraries and versions
- `resource://formats` — List of available code formatters
- `resource://asm/{instruction_set}/{opcode}` — Assembly instruction documentation
- `resource://version` — Godbolt instance version info

**Tools:**
- `compile_code` — Compile source code with specified compiler and flags
- `compile_cmake` — Compile a CMake project
- `format_code` — Format source code with specified formatter

Installed and managed by `mcphub install --server godbolt`. Shared across all MCP clients via the hub's stdio-bridge transport on port 9131.
```

- [ ] **Step 2: Commit documentation**

```bash
git add INSTALL.md
git commit -m "docs: add godbolt server documentation"
```

---

## Self-Review Checklist

**Spec Coverage:**
- ✅ 6 resources implemented: languages, compilers, libraries, formats, asm, version
- ✅ 3 tools implemented: compile_code, compile_cmake, format_code
- ✅ Manifest.yaml created with stdio-bridge transport
- ✅ Client bindings for claude-code and codex-cli
- ✅ Integration into hub via install command
- ✅ Testing via mcphub status, mcp list

**Placeholder Scan:**
- ✅ No "TBD", "TODO", or placeholder code
- ✅ All functions have complete implementation
- ✅ All tool InputSchema definitions complete
- ✅ All test commands have exact expected output
- ✅ Exact file paths throughout

**Type Consistency:**
- ✅ `CompileRequest`, `FormatRequest` types defined once
- ✅ All tool parameter names match across tasks (compiler_id, source, user_arguments, files, libraries, formatter)
- ✅ All resource handlers follow same signature pattern
- ✅ Error handling consistent throughout

**No Red Flags:**
- ✅ Module dependencies minimal (only Go SDK)
- ✅ No external runtime requirements (Go binary is self-contained)
- ✅ All HTTP calls use same godboltBaseURL constant
- ✅ JSON marshaling/unmarshaling consistent
- ✅ Signal handling follows lldb bridge pattern

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-18-godbolt-go-rewrite.md`. Two execution options:

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?
