package godbolt

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// workflowMarkdown is the ecosystem-context document exposed as
// resource://workflow. It's the "how and when" companion to the
// individual tool descriptions (which already cover "what"). Written
// as markdown because LLM clients read markdown natively.
const workflowMarkdown = `# Godbolt MCP Server — Workflow Guide

This server proxies the public Compiler Explorer API at godbolt.org. It excels at
inspecting compiler output for a **short, self-contained code snippet** with a
**single set of flags**. It's paired with the ` + "`perftools`" + ` MCP server, which
handles the "real local build" side of the performance-review loop.

## Tool selection (3 tools)

| Tool | Use when |
|------|----------|
| ` + "`compile_code`" + ` | A single source file. C/C++/Rust/Go/any godbolt-supported language. This is 90% of typical use. |
| ` + "`compile_cmake`" + ` | User has a CMakeLists.txt project and you need the CMake configure-and-build flow, not a single-TU compile. |
| ` + "`format_code`" + ` | Running source through a godbolt-hosted formatter (clang-format, rustfmt, gofmt, etc.). Rare. |

## Resource map (when to consult)

- **Before ` + "`compile_code`" + `** with an unfamiliar language → ` + "`resource://compilers/{language_id}`" + ` to get valid ` + "`compiler_id`" + ` values.
- **Unfamiliar toolchain (nvcc, icpx, embedded cross-compiler)** → ` + "`resource://popularArguments/{compiler_id}`" + ` for curated flag suggestions.
- **Need to link a library** → ` + "`resource://libraries/{language_id}`" + ` lists godbolt-hosted libraries with versions.
- **Looking up an assembly instruction** → ` + "`resource://asm/{instruction_set}/{opcode}`" + ` (e.g. ` + "`resource://asm/x86/vpaddd`" + `).
- **One-off format query** → ` + "`resource://formats`" + ` lists formatter ids for ` + "`format_code`" + `.

## Canonical workflows

### 1. "Did my loop vectorize?"

` + "```" + `
compile_code(
  compiler_id="gcc-13.2",
  source="<hot loop>",
  user_arguments="-O3 -march=x86-64-v3",
  filters={"optOutput": true, "intel": true}
)
` + "```" + `

Response contains ` + "`asm[]`" + ` (for eyeballing SIMD instructions) and ` + "`optOutput[]`" + `
with structured LLVM remarks. If vectorization failed, ` + "`optOutput[]`" + ` typically
contains ` + "`{Name: \"loop-vectorize\", Args: [{String: \"loop not vectorized: unsafe dependency\"}]}`" + `
— no more guessing why.

### 2. "What throughput will this hot loop achieve?"

` + "```" + `
compile_code(
  compiler_id="clang-17",
  source="<hot loop>",
  user_arguments="-O3 -march=skylake",
  tools=[{"id": "llvm-mcatrunk", "args": "-mcpu=skylake -timeline -iterations=100"}]
)
` + "```" + `

Response gains a ` + "`tools[]`" + ` field with per-tool output. ` + "`llvm-mca`" + ` reports IPC,
uOps/cycle, Block RThroughput, port-pressure tables — moves the conversation from
"it vectorized" to "the bottleneck is port 5 at 2.5 uOps/cycle".

### 3. "Audit this struct's layout"

` + "```" + `
compile_code(
  compiler_id="gcc-13.2",
  source="struct Foo { ... };",
  user_arguments="-g -O2",
  tools=[{"id": "pahole", "args": ""}]
)
` + "```" + `

pahole reports padding holes, cacheline boundaries, member ordering — the
bread-and-butter of data-oriented perf work.

### 4. "Run it quickly to sanity-check correctness"

` + "```" + `
compile_code(
  compiler_id=...,
  source=...,
  filters={"execute": true},
  execute_parameters={"stdin": "42\n", "args": []}
)
` + "```" + `

Response contains ` + "`execResult.{stdout, stderr, code}`" + `. Useful for functional
checks before diving into asm. **NOT suitable for benchmarking** — godbolt's
execute runs on a shared sandbox VM with second-scale timing noise. For real
speed measurement, use ` + "`perftools.hyperfine`" + ` locally.

### 5. "Compare two source variants side-by-side"

Call ` + "`compile_code`" + ` twice with the same compiler and flags but different source;
diff the ` + "`asm[]`" + ` arrays client-side. godbolt does not expose a built-in diff
endpoint.

## Tool-parameter patterns

### filters (object, all boolean)

- ` + "`execute`" + ` — run the binary after compile; adds ` + "`execResult`" + ` to response
- ` + "`optOutput`" + ` — structured LLVM optimization remarks; adds ` + "`optOutput[]`" + `
- ` + "`intel`" + ` — Intel asm syntax (default is AT&T)
- ` + "`demangle`" + ` — render C++ symbols readable (usually on by default)
- ` + "`labels`" + ` — show asm labels without instruction bodies (quick flow-only view)
- ` + "`directives`" + ` — show ` + "`.section`" + ` / ` + "`.type`" + ` / etc.
- ` + "`commentOnly`" + ` — keep asm comments
- ` + "`libraryCode`" + ` — expand library-inlined code
- ` + "`trim`" + ` — strip unused sections
- ` + "`binary`" + `/` + "`binaryObject`" + ` — dump linked binary or object form

### tools (array of {id, args})

Run godbolt-hosted analyzers over the compile output. Most useful for perf:

- ` + "`{id: \"llvm-mcatrunk\", args: \"-mcpu=<uarch>\"}`" + ` — throughput/port-pressure
- ` + "`{id: \"pahole\", args: \"\"}`" + ` — struct layout (needs ` + "`-g`" + ` in user_arguments)
- ` + "`{id: \"readelf\", args: \"-a\"}`" + ` — binary inspection (when filters.binary=true)

**Not** the place for ` + "`clang-tidy`" + ` / ` + "`cppcheck`" + ` / ` + "`iwyu`" + ` — those
need the user's real project context (includes, compile_commands.json), not
godbolt's sandbox. Use ` + "`perftools.clang_tidy`" + ` / ` + "`perftools.iwyu`" + ` for those.

### execute_parameters (object)

Only meaningful when ` + "`filters.execute=true`" + `:

- ` + "`stdin`" + ` — string piped to the process
- ` + "`args`" + ` — array of argv strings

## Anti-patterns (don't do this)

- **Benchmarking via godbolt.execute.** Shared VM, seconds of noise. Use ` + "`perftools.hyperfine`" + ` locally.
- **Single-file compile to audit a whole project.** godbolt has no access to your includes, your compile_commands.json, your LTO. Use ` + "`perftools.clang_tidy`" + ` against your real project.
- **Inspecting post-linker asm.** godbolt is compile-only, single-file, sandbox. For "what's actually in my .exe" → ` + "`perftools.llvm_objdump`" + ` on your real binary.
- **Running long-running code.** Godbolt sandboxes have strict CPU/time limits.

## Cross-server handoffs

When godbolt alone isn't enough, the ` + "`perftools`" + ` MCP server (same machine, via
` + "`mcphub perftools`" + ` or standalone ` + "`perftools.exe`" + `) provides the "real local
build" side:

| If you need… | Use (not godbolt) |
|--------------|-------------------|
| Real compile_commands.json-aware static analysis | ` + "`perftools.clang_tidy`" + ` |
| Post-LTO/PGO/linker-inlining asm | ` + "`perftools.llvm_objdump`" + ` |
| Statistical microbenchmark A vs B | ` + "`perftools.hyperfine`" + ` |
| Cross-TU include hygiene | ` + "`perftools.iwyu`" + ` |

Full perf-review loop (godbolt + perftools):

1. ` + "`perftools.clang_tidy`" + ` → audit real project for perf antipatterns
2. ` + "`<edit>`" + ` → apply fix
3. ` + "`godbolt.compile_code`" + ` with ` + "`filters.optOutput=true`" + ` → sanity-check asm and remarks
4. ` + "`<rebuild via user's cmake>`" + `
5. ` + "`perftools.hyperfine`" + ` against old vs new binary → statistical speed comparison
6. ` + "`perftools.llvm_objdump`" + ` on final binary → confirm the LTO-linked output retains the vectorization seen on godbolt
`

// getWorkflow handles resource://workflow — the ecosystem-context
// document that tells MCP clients WHEN and HOW to use this server's
// tools, not just WHAT they do. The individual tool descriptions
// cover "what"; this resource covers workflows, tool-selection
// tradeoffs, anti-patterns, and cross-server handoffs.
func (gs *GodboltServer) getWorkflow(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "text/markdown",
				Text:     workflowMarkdown,
			},
		},
	}, nil
}

// workflowTool is the tool-transport twin of getWorkflow. MCP clients
// that surface resources to the user/agent natively can read
// resource://workflow directly; clients that only wire tools (e.g.
// Claude Code CLI's agent view) need the same content behind a tool
// call. Same markdown body, just returned in a CallToolResult instead
// of a ReadResourceResult.
func (gs *GodboltServer) workflowTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: workflowMarkdown}},
	}, nil
}
