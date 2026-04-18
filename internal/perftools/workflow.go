package perftools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// workflowMarkdown is the ecosystem-context document exposed as
// resource://workflow. It's the "how and when" companion to the
// individual tool descriptions (which already cover "what").
const workflowMarkdown = `# Perf-Toolbox MCP Server — Workflow Guide

This server wraps four **local** performance-analysis binaries from the
user's MSYS2/ucrt64 (or equivalent) installation: ` + "`clang-tidy`" + `,
` + "`hyperfine`" + `, ` + "`llvm-objdump`" + `, and ` + "`include-what-you-use`" + `. It's the
"real local build" side of the perf-review loop. The companion ` + "`godbolt`" + `
MCP server handles the "single-file compile on godbolt.org sandbox" side.

## Before using any tool — check availability

Different hosts have different subsets of the four tools installed.
**Always consult ` + "`resource://tools`" + ` first** to see what's present:

` + "```" + `
resource://tools
→ {
  "clang-tidy":   {"installed": true,  "version": "20.1.0", "path": "..."},
  "hyperfine":    {"installed": true,  "version": "1.20.0", "path": "..."},
  "llvm-objdump": {"installed": true,  "version": "20.1.0", "path": "..."},
  "include-what-you-use": {"installed": false, "error": "not on PATH: ..."}
}
` + "```" + `

If a tool is ` + "`installed: false`" + `, the handler will return a clean
"not installed" error — no need to preemptively guard. But checking up-front
means you can propose "install iwyu via pacman" instead of calling a failing tool.

## Tool selection (4 tools)

| Tool | Use when |
|------|----------|
| ` + "`clang_tidy`" + ` | Audit real project source for perf / correctness antipatterns. Needs ` + "`compile_commands.json`" + `. |
| ` + "`hyperfine`" + ` | "Is variant A faster than variant B?" Statistical bench of any shell commands. Sub-percent precision. |
| ` + "`llvm_objdump`" + ` | Disassemble user's **real** built binary (post-LTO/PGO/linker-inlining). The authoritative answer to "what's actually in my .exe". |
| ` + "`iwyu`" + ` | Include hygiene: trim unused ` + "`#include`" + `s, add missing ones. Reduces compile time. |

## Canonical workflows

### 1. Full perf-review loop (combines with godbolt)

` + "```" + `
  1. clang_tidy(files=["src/hot.cpp"], project_root=".", checks="performance-*")
     → finds: performance-unnecessary-value-param on foo's std::string arg
  2. <edit source to pass by const-ref>
  3. godbolt.compile_code(source=..., filters={optOutput: true, intel: true})
     → sanity-check asm change and optimization remarks
  4. <rebuild via user's cmake>
  5. hyperfine(commands=["./build-old/mybin", "./build-new/mybin"], warmup=3, min_runs=10)
     → statistical comparison: new is 1.28× faster (±0.4%)
  6. llvm_objdump(binary="./build-new/mybin", function="hot_loop")
     → confirm the LTO-linked final binary retains the vectorization
` + "```" + `

### 2. Include-hygiene audit (compile-time perf)

` + "```" + `
  iwyu(file="src/hot.cpp", extra_args=["-std=c++17", "-Iinclude"])
  → reports[]: each with add[], remove[], full_list[] for the file
` + "```" + `

Interpretation:
- ` + "`status: \"ok\"`" + ` — IWYU parsed the file, reports contain actionable suggestions
- ` + "`status: \"no-suggestions\"`" + ` — IWYU ran clean, file is include-optimal already
- ` + "`status: \"env-failure\"`" + ` — IWYU couldn't find stdlib headers (typical cause: clang's bundled headers don't match host glibc/msvc). Check ` + "`raw_output`" + ` for the ` + "`fatal error:`" + ` line.

### 3. "Did the LTO-linked binary actually vectorize?"

` + "```" + `
  1. godbolt.compile_code(source="<hot loop>", user_arguments="-O3 -march=x86-64-v3",
                          filters={optOutput: true})
     → godbolt claims vectorized
  2. <rebuild real project>
  3. llvm_objdump(binary="./build/mybin", function="hot_loop", intel=true)
     → sanity-check: the real binary's disassembly shows vpaddd/vmovups too
` + "```" + `

Godbolt is a single-file sandbox compile; the real build can differ because of
LTO, PGO, link-time inlining across TUs, or flags that differ from what you
pass to godbolt. ` + "`llvm_objdump`" + ` on the real binary is the authoritative answer.

### 4. Quick A/B speed comparison

` + "```" + `
  hyperfine(
    commands=["./a.out --input bench.dat", "./b.out --input bench.dat"],
    warmup=3, min_runs=10, max_runs=100
  )
  → results[]: {command, mean, stddev, median, min, max, times[]}
` + "```" + `

For N >= 2 commands, hyperfine's own output also computes pairwise ratios.
Warmup runs stabilize filesystem cache / CPU frequency. Default ` + "`min_runs=10`" + `
is enough for commands that run in 100ms+. For sub-10ms commands, bump
` + "`min_runs`" + ` to 50+ to get tight confidence intervals.

## Tool-parameter patterns

### clang_tidy

` + "`checks`" + ` common presets:

- ` + "`performance-*`" + ` — perf antipatterns (most useful for perf review)
- ` + "`bugprone-*`" + ` — latent bugs
- ` + "`readability-*`" + ` — style
- ` + "`performance-*,bugprone-*`" + ` — perf review default
- ` + "`-*,performance-unnecessary-value-param`" + ` — disable all, enable just one check (noise reduction)

` + "`project_root`" + ` must contain a ` + "`compile_commands.json`" + `. Generate via:

- CMake: ` + "`cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON ...`" + `
- Make-based builds: ` + "`bear -- make`" + ` or ` + "`compiledb make`" + `
- Bazel: ` + "`bazel run @hedron_compile_commands//:refresh_all`" + `

` + "`extra_args`" + ` useful flags:

- ` + "`--header-filter=.*`" + ` — report diagnostics in headers too
- ` + "`--quiet`" + ` — suppress "X warnings generated" banners

### hyperfine

- ` + "`warmup: 1-3`" + ` — stabilize caches; essential for commands <100ms
- ` + "`min_runs: 10`" + ` (default) — good for 100ms+ commands
- ` + "`max_runs`" + ` — cap at 100 for short-running or 20 for multi-second commands
- ` + "`extra_args: [\"--prepare\", \"sync && echo 3 > /proc/sys/vm/drop_caches\"]`" + ` — reset OS cache between runs (Linux; obviously not portable)

### llvm_objdump

- ` + "`function`" + ` — **always pass this**. Full .text disassembly of a real binary is multi-MB and overwhelms context.
- ` + "`intel: true`" + ` — Intel syntax instead of AT&T
- ` + "`with_source: true`" + ` — interleave source lines; requires binary built with ` + "`-g`" + `
- ` + "`section: \".text.hot_path\"`" + ` — if your build uses ` + "`__attribute__((section(...)))`" + ` or PGO-generated hot sections

### iwyu

- Requires the same include paths your real build uses. Pass them via ` + "`extra_args: [\"-std=c++17\", \"-Iinclude\", \"-Ithird-party/eigen\"]`" + `.
- Run on one file at a time; iwyu doesn't natively batch (iwyu_tool.py does, but we don't wrap it).
- The ` + "`status`" + ` field in response distinguishes "nothing to suggest" from "environment broken" — check it before treating empty ` + "`reports[]`" + ` as "file is clean".

## Anti-patterns

- **Don't use ` + "`clang_tidy`" + ` without ` + "`compile_commands.json`" + `.** It will run with degraded analysis (no includes resolved) and produce misleading diagnostics. Either generate the compile DB or use ` + "`godbolt.compile_code`" + ` for quick single-file checks.
- **Don't use ` + "`hyperfine`" + ` for correctness checks.** It runs the command many times; if it has side effects (writes files, sends network), those multiply. Use ` + "`godbolt.compile_code filters.execute=true`" + ` for one-shot correctness runs.
- **Don't use ` + "`llvm_objdump`" + ` on the full .text section.** Always pass ` + "`function`" + ` or ` + "`section`" + `. Real binaries produce megabytes of disassembly.
- **Don't expect ` + "`clang_tidy --checks=performance-*`" + ` to find everything.** clang-tidy's pattern matcher misses many perf issues that profilers catch. Complement with ` + "`hyperfine`" + ` + ` + "`llvm_objdump`" + ` for empirical verification.

## Cross-server handoffs

Paired with the ` + "`godbolt`" + ` MCP server (same machine, via ` + "`mcphub godbolt`" + `
or standalone ` + "`godbolt.exe`" + `):

| If you need… | Use (not perftools) |
|--------------|--------------------|
| Single-file compile with experimental flags | ` + "`godbolt.compile_code`" + ` |
| Optimization remarks for a snippet | ` + "`godbolt.compile_code`" + ` with ` + "`filters.optOutput=true`" + ` |
| ` + "`llvm-mca`" + ` throughput analysis | ` + "`godbolt.compile_code`" + ` with ` + "`tools=[{id: \"llvm-mcatrunk\", ...}]`" + ` |
| Lookup an asm instruction | ` + "`godbolt`" + ` resource ` + "`resource://asm/{set}/{opcode}`" + ` |
| Popular flags for a compiler | ` + "`godbolt`" + ` resource ` + "`resource://popularArguments/{compiler_id}`" + ` |

The mental split: **godbolt is the "quick compile + sandbox run + llvm-mca" side,
perftools is the "real project + real binary + real measurement" side.**
`

// getWorkflow handles resource://workflow — the ecosystem-context
// document that tells MCP clients WHEN and HOW to use this server's
// tools, not just WHAT they do.
func (tb *PerfToolbox) getWorkflow(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
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
