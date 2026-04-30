# Installation

## Prerequisites

1. **Go 1.26+** — `go version` must succeed. Required by `go.mod` (uses 1.24+ stdlib APIs such as `strings.SplitSeq`). Tested on 1.26.2 windows/amd64.
2. **Git for Windows** (includes Git Bash; the CLI expects Unix-style shell for some setup commands).
3. **uvx** — Python package runner, needed by Serena. Install via [uv](https://github.com/astral-sh/uv). Do NOT pipe the upstream installer directly into a shell — download it first, inspect it, then run:
   ```powershell
   # 1. Fetch the installer to a file you can audit
   irm https://astral.sh/uv/install.ps1 -OutFile "$env:TEMP\uv-install.ps1"

   # 2. Review the contents — confirm what it does before executing
   notepad "$env:TEMP\uv-install.ps1"

   # 3. Run it only after you're satisfied
   powershell -ExecutionPolicy Bypass -File "$env:TEMP\uv-install.ps1"

   # 4. Verify
   uvx --version
   ```
   Alternatively install via `winget`:
   ```powershell
   winget install --id=astral-sh.uv -e
   ```
   The previous version of this document recommended `irm ... | iex` — that pattern executes remote code with no audit step and is the classic "pipe-to-shell" anti-pattern. Keep the download-inspect-run flow for any third-party installer.
4. **Windows 11** recommended. Windows 10 should work but is untested. Linux/macOS currently fail at `mcphub install` — stubs only.
5. **An MCP client or two** (Claude Code, Codex CLI, Gemini CLI, Cursor, Continue…) already installed on the machine.

## Build

```bash
cd <repo-root>
bash build.sh   # Git Bash / WSL / Linux / macOS
# or, on Windows native:
pwsh ./build.ps1
```

Both scripts embed the current git commit, the build date, and a Windows version resource into the binary (see `mcphub.exe version`). A plain `go build -o mcphub.exe ./cmd/mcphub` also works for dev iteration but leaves version metadata as `dev/unknown`.

On success: `bin/mcphub.exe` appears (~15 MB, includes Windows version resource metadata).

## Setup (canonical install)

Scheduler tasks reference `~/.local/bin/mcphub.exe` by absolute path (Windows Task Scheduler's CreateProcess call doesn't honor PATH — confirmed empirically), and Antigravity relay entries reference the short name (Node's child_process spawner does honor PATH). Both point at the same canonical install. `mcphub setup` puts the binary there and registers PATH:

```bash
./mcphub.exe setup
```

What it does:

- Copies the running binary to `%USERPROFILE%\.local\bin\mcphub.exe` (on Linux/macOS: `~/.local/bin/mcphub`).
- On Windows: appends that directory to `HKCU\Environment\Path` if it isn't already there, then broadcasts `WM_SETTINGCHANGE` so new shells pick up the change. **The shell that ran `setup` won't see the updated PATH — close and reopen it.**
- On Linux/macOS: prints the one-line `export PATH=...` snippet to paste into your shell rc. Does not touch rc files.

Idempotent — running it again when the binary is already at the target and the dir is already on PATH is a no-op (no registry write, no duplicate entries).

If you skip this step, `mcphub install` will detect that `mcphub.exe` isn't on PATH and either prompt to bootstrap (interactive shells) or fail with a pointer back to `mcphub setup` (CI, pipes).

Moving or rebuilding the binary later: run `setup` again from the new location. It copies the new binary over `~/.local/bin/mcphub.exe`, so existing scheduler tasks — which point at that absolute path — keep working without any rewrite. If you need to migrate tasks that still reference an old absolute path (e.g. dev checkout tasks created before setup), run `mcphub scheduler upgrade` once.

## First install

Ten servers ship with manifests: `serena`, `memory`, `sequential-thinking`, `wolfram`, `godbolt`, `paper-search-mcp`, `time`, `gdb`, `lldb`, `perftools`. Each is installed independently. Start with Serena (Phase 1 flagship):

```bash
# Preview what would happen (no side effects)
./mcphub.exe install --server serena --dry-run

# Apply: creates 2 Task Scheduler tasks (claude + codex daemons), writes 4 client configs, starts both daemons
./mcphub.exe install --server serena
```

Expected output:

```
✓ Scheduler task created: mcp-local-hub-serena-claude
✓ Scheduler task created: mcp-local-hub-serena-codex
  backup: C:\Users\<you>\.claude.json.bak-mcp-local-hub-<timestamp>
✓ claude-code → http://localhost:9121/mcp
  backup: C:\Users\<you>\.codex\config.toml.bak-mcp-local-hub-<timestamp>
✓ codex-cli → http://localhost:9122/mcp
  backup: C:\Users\<you>\.gemini\settings.json.bak-mcp-local-hub-<timestamp>
✓ gemini-cli → http://localhost:9121/mcp
  backup: C:\Users\<you>\.gemini\antigravity\mcp_config.json.bak-mcp-local-hub-<timestamp>
✓ antigravity → relay (mcphub.exe relay --server serena --daemon claude)
✓ Started: mcp-local-hub-serena-claude
✓ Started: mcp-local-hub-serena-codex

Install complete.
```

First `✓ Started` triggers `uvx` to download Serena (~30 seconds on a fresh machine). After that Serena processes live on ports 9121 and 9122.

Verify:

```bash
./mcphub.exe status        # 2 tasks; -claude and -codex Running (auto-refresh disabled since v0.x; reinstall prunes any stale `-weekly-refresh` task left from earlier versions)
claude mcp get serena   # Status: ✓ Connected, Type: http, URL: http://localhost:9121/mcp
codex mcp get serena    # enabled: true, transport: streamable_http
```

### Partial install (one daemon only)

```bash
./mcphub.exe install --server serena --daemon codex
```

Creates only the `codex` daemon (port 9122), applies only the `codex-cli` client binding, skips weekly refresh. Useful when trying it out on a single client first.

## Per-client notes

### Claude Code

Writes to `~/.claude.json` (user scope) — the single-file config at your home directory, not `~/.claude/settings.json` (that file holds UI preferences and is ignored for MCP).

HTTP entry shape:
```json
"mcpServers": {
  "serena": {
    "type": "http",
    "url": "http://localhost:9121/mcp"
  }
}
```

**If you had Claude Code open before `install`, restart the Claude session.** Claude reads the MCP list at session start and caches it; `claude mcp get serena` will show Connected immediately, but the current chat session will not see `serena` tools until you start a fresh one.

### Codex CLI

Writes to `~/.codex/config.toml`:
```toml
[mcp_servers.serena]
startup_timeout_sec = 10.0
url = 'http://localhost:9122/mcp'
```

Same session-cache caveat as Claude: restart the Codex CLI after install for its agent to pick up the new MCP. `codex exec` always starts a fresh session, so experiments via `codex exec "use find_symbol..."` bypass the caching issue.

### Gemini CLI

Writes to `~/.gemini/settings.json`:
```json
"mcpServers": {
  "serena": {
    "url": "http://localhost:9121/mcp",
    "type": "http",
    "timeout": 10000
  }
}
```

Namespace for Serena tools inside a Gemini prompt is `mcp_serena_*` (single underscore), not `mcp__serena__*` (double underscore) as in Claude/Codex. Example:
```bash
gemini -p "use mcp_serena_find_symbol with name_path=main" -m gemini-2.5-flash --yolo
```

### Antigravity (Cascade)

Antigravity's Cascade agent silently drops any `mcp_config.json` entry pointing at a loopback HTTP URL. `mcp-local-hub` works around this by writing a **stdio relay** entry instead:

```json
"serena": {
  "command": "<absolute-path-to>\\mcphub.exe",
  "args": ["relay", "--server", "serena", "--daemon", "claude"],
  "disabled": false
}
```

`<absolute-path-to>` is filled in by `mcphub install` using `os.Executable()` — whatever absolute path points at the `mcphub.exe` that ran the install. If you move the binary afterwards, re-run `mcphub install --server serena` so the entry is rewritten with the new path.

Cascade spawns `mcphub.exe relay` as a normal stdio subprocess. The relay translates JSON-RPC between stdin/stdout and the shared HTTP daemon on port 9121. No extra Serena process per Antigravity session — it shares the same daemon as Claude Code and Gemini CLI.

**After install, restart Antigravity** for Cascade to pick up the new entry:
```powershell
Get-Process -Name Antigravity | Stop-Process -Force
Start-Sleep 3
Start-Process "$env:LOCALAPPDATA\Programs\Antigravity\Antigravity.exe"
```

The relay binary path is the canonical `~/.local/bin/mcphub.exe` that `mcphub setup` installs. Moving, rebuilding, or upgrading the binary only requires re-running `mcphub setup` — scheduler tasks and Antigravity client entries keep pointing at the same deterministic path.

## Manifest resolution model

Shipped manifests live under `servers/<name>/manifest.yaml` in the source tree and are embedded into `mcphub.exe` at build time via `//go:embed`. The canonical installed binary at `~/.local/bin/mcphub.exe` resolves manifests from its embedded FS first; CLI commands (`manifest list`, `install`, `scan`, `migrate`, `status`, `relay`) all see the same 10 shipped servers regardless of the invocation's cwd. Dev flow: a newly-added `servers/<name>/manifest.yaml` that has not yet been compiled into the binary is still picked up from disk via a secondary lookup under `defaultManifestDir()` — useful for editing a manifest and immediately testing with `./bin/mcphub.exe manifest get <name>` without a full build cycle.

Write operations (`manifest create` / `edit` / `delete`) still write to disk only — the embedded FS is immutable at runtime.

## Per-server notes (beyond serena)

Phase 2 added 6 global daemons. Each has its own manifest in `servers/<name>/manifest.yaml`.

### memory (port 9123)

Runs `npx -y @modelcontextprotocol/server-memory`. Stores data at
`${HOME}/.local/share/mcp-memory/memory.jsonl` by default. Override the
`MEMORY_FILE_PATH` env var in the manifest (or export it in the shell
that launches `mcphub install`) to relocate. This is the critical
daemon — previously each client spawned its own memory server, causing
concurrent writes to the same JSONL file (data race). The shared
daemon serializes all writes through one subprocess.

### sequential-thinking (port 9124)

Runs `npx -y @modelcontextprotocol/server-sequential-thinking`. Stateless
reasoning helper. No env needed.

### wolfram (port 9132)

Runs `node ${HOME}/.local/mcp-servers/wolframalpha-llm-mcp/build/index.js`.
Clone the Wolfram LLM MCP server into that location and build it:
```bash
mkdir -p ~/.local/mcp-servers
git clone https://github.com/SecretiveShell/MCP-wolfram-alpha.git \
    ~/.local/mcp-servers/wolframalpha-llm-mcp
cd ~/.local/mcp-servers/wolframalpha-llm-mcp
npm install && npm run build
```
`WOLFRAM_LLM_APP_ID` is stored in the encrypted vault:

```bash
mcphub secrets set wolfram_app_id --value <your-app-id>
```

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

### paper-search-mcp (port 9127)

Runs `uvx --from paper-search-mcp python -m paper_search_mcp.server`.
Requires `uvx`. `PAPER_SEARCH_MCP_UNPAYWALL_EMAIL` is stored in the vault:

```bash
mcphub secrets set unpaywall_email --value <your-email>
```

First install may take ~30s as `uvx` downloads `paper-search-mcp`.

### time (port 9128)

Runs `npx -y @mcpcentral/mcp-time`. Trivial, stateless.

### gdb (port 9129)

Runs `uv run --directory ${HOME}/.local/mcp-servers/GDB-MCP python server.py`.
Multi-debugger MCP server (gdb + lldb submodules) with built-in session
management — one daemon serves N concurrent debug sessions identified
by `session_id`. Clone the project at that path:
```bash
git clone https://github.com/pansila/GDB-MCP.git \
    ~/.local/mcp-servers/GDB-MCP
```
Requires `uv` on PATH.

### lldb (port 9130)

Embedded in `mcphub.exe`. LLDB has its own MCP server implementation
but it speaks MCP over a raw TCP socket (`protocol-server start MCP
listen://host:port`), not stdio. The manifest runs `mcphub lldb-bridge
localhost:47000` which:

1. Connects to an LLDB instance already listening on :47000, or
2. Spawns `lldb.exe` (path: `--lldb-path`, defaults to
   `C:\msys64\ucrt64\bin\lldb.exe` on Windows) and waits for it to bind
   :47000, then
3. Forwards stdio↔TCP in both directions until either side closes.

When the stdio-bridge transport in mcphub HTTP-multiplexes this daemon,
multiple Claude / Codex sessions share one LLDB instance — LLDB's
protocol-server itself can only service one TCP client at a time, so
per-session bridges would race. Auto-spawned LLDB is terminated cleanly
on daemon exit. The bridge lives in `internal/lldb/` and can also be
built as a standalone binary — see *Standalone binaries* below.

### perftools (port 9131)

Embedded in `mcphub.exe` — no external dependency beyond what MSYS2/ucrt64 already provides. Manifest runs `mcphub perftools` as the daemon command. Wraps three always-on local analysis tools plus an opt-in benchmarker (`hyperfine`, see below). All operate on the user's **real** build output (post-LTO/PGO/linker-inlining) instead of godbolt's single-file sandbox compile.

**Tools:**

- `clang_tidy` — run clang-tidy with a checks filter against files in a project with a `compile_commands.json`. Returns structured JSON `{diagnostics[{file, line, column, severity, check, message}], raw_stderr, exit_code}`. Catches the dozens of `performance-*` and `bugprone-*` checks that need real build context (transitive includes, preprocessor state, platform macros).
- `hyperfine` — **disabled by default** (opt-in; see "Opting into hyperfine" below). When enabled, benchmarks one or more shell commands with statistical rigor (warmup, outlier detection, min/max runs). Returns hyperfine's `--export-json` verbatim: `{results[{command, mean, stddev, median, min, max, user, system, times[]}]}`. For 2+ commands hyperfine also computes pairwise ratios. Use this to answer "is variant A actually faster than B?" with sub-percent precision.
- `llvm_objdump` — disassemble a function/section of the user's REAL binary (post-LTO/PGO/linker-inlining). Supports Intel/AT&T syntax, source interleave, symbol filtering. Unique vs godbolt: godbolt is compile-only/sandbox/single-file — `llvm_objdump` shows what's in the `.exe` after your whole build pipeline.
- `iwyu` — run include-what-you-use on a source file. Returns per-file `{add[], remove[], full_list[]}` include suggestions plus raw output. Shaves compile time by trimming unused transitive includes.

**Resource:**

- `resource://tools` — JSON catalog of the tools **advertised by this daemon**: the three always-on analyzers (`clang-tidy`, `llvm-objdump`, `include-what-you-use`) when their binary is present, plus `hyperfine` only when the opt-in gate is open (see below). Each entry carries the detected version probed once at startup via `exec.LookPath` + `<bin> --version`. Lets MCP clients skip tools that would fail rather than guessing.

**Prerequisites (tools on PATH):**

On the MSYS2/ucrt64 stack, install via `pacman`:

```powershell
pacman -S mingw-w64-ucrt-x86_64-clang-tools-extra    # clang-tidy
pacman -S mingw-w64-ucrt-x86_64-hyperfine            # hyperfine
pacman -S mingw-w64-ucrt-x86_64-llvm                 # llvm-objdump
pacman -S mingw-w64-ucrt-x86_64-include-what-you-use # include-what-you-use
```

Then make sure `C:\msys64\ucrt64\bin` is on PATH for the mcphub scheduler task. `mcphub perftools` checks at startup and advertises missing tools via `resource://tools` — the server starts regardless; per-tool calls surface a clean "X not installed" error.

**Opting into hyperfine (default: disabled):**

`hyperfine` executes arbitrary shell commands supplied by the MCP client — that's exactly its benchmark contract, but the same surface is a remote-code-execution path for any client that can reach the perftools daemon. To keep reinstall and first-setup safe by default, `hyperfine` is **unregistered** unless you opt in explicitly.

To enable it, set the environment variable

```
MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE=1
```

on the **process that runs `mcphub perftools`**, i.e. the user account that owns the scheduler task `mcp-local-hub-perftools-default`. On Windows the simplest route is a persistent user-level env var:

```powershell
# PowerShell (sets HKCU so the scheduler-launched process inherits it at next logon):
[Environment]::SetEnvironmentVariable("MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE", "1", "User")

# Or via cmd/setx equivalent:
setx MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE 1
```

Log out and back in (or restart the scheduler task) so the daemon picks it up.

Any value other than the literal string `"1"` (including absent, `"true"`, `"yes"`, `"0"`, or trailing whitespace) keeps the gate closed. When closed:

- the tool is **not registered** — `tools/call hyperfine` returns method-not-found from the SDK layer as if the tool never existed
- `resource://tools` and `list_tools` both **hide** `hyperfine` — the "check availability, then call" contract stays consistent ("advertised ⇒ callable")

To confirm the gate state:

```
resource://tools
→ clang-tidy / llvm-objdump / include-what-you-use present; hyperfine absent  (gate closed)
→ all four listed                                                             (gate open)
```

**The complete perf loop in one chat:**

```text
clang_tidy(files=["src/hot.cpp"], project_root="/path/to/repo",
           checks="performance-*")
  → finds: performance-unnecessary-value-param on foo's std::string arg

<edit to pass by const-ref>

compile_code(compiler_id="gcc-13.2", source=..., filters={optOutput: true})
  → godbolt check: vectorized, no spurious copies

<rebuild via user's cmake>

hyperfine(commands=["./build-old/mybin", "./build-new/mybin"],
          warmup=3, min_runs=10)
  → new is 1.28× faster (±0.4%)

llvm_objdump(binary="./build-new/mybin", function="hot_loop")
  → confirm LTO-linked final output still retains the vectorization
```

The Go implementation lives in `internal/perftools/` and can also be built as a standalone binary — see *Standalone binaries* below.

### Standalone binaries (optional)

Three of the bundled servers — `godbolt`, `lldb-bridge`, and `perftools` — live inside
`mcphub.exe` but can also be built as independent binaries for users
who want them without the full hub:

```bash
go build -o godbolt.exe ./cmd/godbolt
go build -o lldb-bridge.exe ./cmd/lldb-bridge
go build -o perftools.exe ./cmd/perftools
```

Each is a thin entry point (`cmd/<name>/main.go`) that imports the same
library package the hub uses (`internal/godbolt`, `internal/lldb`, `internal/perftools`), so
there is zero code duplication between the embedded and standalone
shapes. Behavior is identical to `mcphub godbolt` / `mcphub lldb-bridge` / `mcphub perftools`
— the binaries just skip the hub's scheduler/multiplexer.

When to use standalone binaries:

- You want a compiler-explorer stdio MCP server in another tool that
  doesn't need mcphub.
- You want to run the LLDB bridge from a custom script or a non-Windows
  host where the hub's Task-Scheduler integration is not available.
- You want the perf-analysis toolbox (clang-tidy / hyperfine / llvm-objdump / iwyu) inline in a tool that doesn't need mcphub, provided the underlying binaries are on PATH.

Manifests can target either shape — switch `command: mcphub` to
`command: godbolt` / `command: lldb-bridge` / `command: perftools` if the standalone
binary is on `PATH`.

### context7 (no daemon)

Available at `https://mcp.context7.com/mcp` as a remote HTTPS endpoint.
Codex CLI, Gemini CLI, and Antigravity typically have it pre-configured.
For Claude Code, add it manually:

```bash
claude mcp add --transport http context7 https://mcp.context7.com/mcp
```

## Uninstall & rollback

```bash
# Remove scheduler tasks and client entries
./mcphub.exe uninstall --server serena

# Restore client configs from the latest backup (pre-install state)
./mcphub.exe rollback
```

Backups are named `<config>.bak-mcp-local-hub-YYYYMMDD-HHMMSS` and live next to each client config. Uninstall does NOT delete them — keep as long as you want or clean up manually.

### Install-time atomicity

`install --server X` applies its side effects in order: create scheduler tasks → backup each client config → add the MCP entry → kick off the scheduler task. If any step after the first fails, the installer compensates in reverse: scheduler tasks it just created are deleted, and MCP entries it just added are removed (backups are preserved untouched so you can restore them manually via `rollback` if you need to undo an earlier install of a DIFFERENT server).

This is best-effort atomicity: two concurrent `install` invocations can still step on each other, and a crash between compensating ops leaves partial state. For a deterministic recovery from such a state, run `uninstall --server X` then `install --server X` again.

`uninstall` does not kill already-running Serena Python processes (Task Scheduler deletes the task metadata, not live children). If a daemon is still bound to 9121/9122 after uninstall:
```powershell
Stop-Process -Name python -Force -ErrorAction SilentlyContinue
# or specifically:
Get-Process | Where-Object { $_.Path -like '*uvx*' } | Stop-Process -Force
```

## Secrets

For servers that need API keys (wolfram, paper-search-mcp, any OAuth-bearer server):

```bash
./bin/mcphub.exe secrets init                         # generate .age-key + empty secrets.age
./bin/mcphub.exe secrets set WOLFRAM_APP_ID --value AB123...
./bin/mcphub.exe secrets list                         # shows keys, not values
./bin/mcphub.exe secrets get WOLFRAM_APP_ID           # copies to clipboard by default
./bin/mcphub.exe secrets get WOLFRAM_APP_ID --show    # prints to stdout
./bin/mcphub.exe secrets edit                         # open decrypted vault in $EDITOR
./bin/mcphub.exe secrets migrate --from-client codex-cli   # scan existing config for API keys, interactively import
```

### Where the secret files live

Both files live in the per-user data directory, **independent of where the repo or mcphub.exe is installed**:

| OS | Path |
|---|---|
| Windows | `%LOCALAPPDATA%\mcp-local-hub\` — typically `C:\Users\<you>\AppData\Local\mcp-local-hub\` |
| Linux | `$XDG_DATA_HOME/mcp-local-hub/` — default `~/.local/share/mcp-local-hub/` |
| macOS | `~/Library/Application Support/mcp-local-hub/` |

Two files are stored there:

- `.age-key` — private identity file, ~75 bytes. **Never commit, never email in plaintext.** Treat like an SSH private key.
- `secrets.age` — encrypted vault containing your actual secret values. Opaque ciphertext without `.age-key`.

### Transferring to another machine

1. Install `mcp-local-hub` on the new machine (clone + `./build.sh`)
2. Copy both files from the old machine's data dir to the new machine's data dir (path from the table above):
   - Through a password manager (Bitwarden secure notes, 1Password, etc.)
   - Through an encrypted USB stick
   - Through `scp` / `rsync` / `rclone` with a trusted transport
   - Through a **private** GitHub repository (public repos with `.age-key` is a critical leak)
3. Run `./bin/mcphub.exe secrets list` on the new machine — should print your keys without error. If it errors with "failed to decrypt", the `.age-key` or `secrets.age` didn't copy correctly.

### Manifest env references use prefixes

- `secret:KEY` — look up in encrypted vault
- `file:KEY` — look up in `config.local.yaml` (gitignored)
- `$VAR` — read OS environment variable
- anything else — literal value

### Backup

Losing `.age-key` means the vault is unreadable — there is no recovery path. Keep at least one copy outside the primary machine (password manager is ideal).

## Troubleshooting

### `port 9121 already in use`

Preflight caught another listener on the port Serena wants. Either:
- Another Serena instance is already running (from a previous manual stdio setup) — kill it: `Get-Process -Name python | Where-Object { $_.Path -like '*uvx*' } | Stop-Process`
- A different local service is using 9121 — change the port in `servers/serena/manifest.yaml` and re-install

### `command "uvx" not found on PATH`

Install `uv` (see Prerequisites). Restart your shell afterwards so `PATH` picks up the new binary.

### `error: create task ...: schtasks /Create: exit status 1`

If the error mentions a specific XML element (`(N,M):ElementName:`), it's a schema violation in `scheduler_windows.go`. Please file an issue with the exact message — both known XML bugs (`RestartInterval` flat, `WeeklyTrigger` direct child) are already fixed, any new one would be a regression or Windows version difference.

### Serena installs but client doesn't see it

- Did you restart the client session? (Claude/Codex/Gemini cache MCP list at start — see per-client notes above.)
- Is port 9121 / 9122 actually listening? `powershell "Get-NetTCPConnection -LocalPort 9121"`
- Does `<client> mcp get serena` report Connected? If yes but tools aren't used, it's a prompt-interpretation issue — try `"use mcp__serena__find_symbol with name=main"` instead of `"use serena find_symbol"`.

### Antigravity's `RefreshMcpServers: loading already in progress` loop

Unrelated to mcp-local-hub — this is a third-party bug in `mcp-language-server` (returns exit 1 on graceful shutdown, blocking Antigravity's refresh cycle). Full kill + restart usually clears it:
```powershell
Get-Process -Name Antigravity | Stop-Process -Force
Start-Sleep 3
Start-Process "$env:LOCALAPPDATA\Programs\Antigravity\Antigravity.exe"
```

### Logs

Daemon output lives in `%LOCALAPPDATA%\mcp-local-hub\logs\<server>-<daemon>.log`. Rotates at 10 MB, keeps the last 5 rotations.

Scheduler view: `%SystemRoot%\System32\Tasks\mcp-local-hub-*` are the XML task definitions; `taskschd.msc` opens the GUI.

## Next steps

- Read [docs/phase-1-verification.md](docs/phase-1-verification.md) for the full live-test matrix and the nine post-plan fixes applied during real testing.
- Read [docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md](docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md) for the architectural rationale (global vs workspace-scoped daemons, port pool allocation, transport choice, secrets handling).
- If you want to add a new MCP server beyond Serena, copy `servers/serena/manifest.yaml` to `servers/<your-server>/manifest.yaml`, adjust fields, then `mcphub install --server <your-server>`. Port must be in 9121–9139 (global range) or 9200–9299 (workspace-scoped) and registered in `configs/ports.yaml`.
