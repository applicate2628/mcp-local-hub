# Native-code debugging through mcphub (Windows / Linux)

mcphub ships **two native debugger MCP servers** — there is no longer any
external GDB-MCP python dependency. Both spawn the debugger by an absolute path
(resolved by the shared `internal/toolchain` detector) via Go `exec`, which works
inside the console-less daemon where an external python `subprocess.run(['gdb'])`
fails.

| MCP server | Backed by | How it works |
|---|---|---|
| **`mcp__lldb`** (`servers/lldb`, `internal/lldb`) | LLDB's own built-in MCP server (`lldb -> protocol-server start MCP`) | mcphub's `lldb-bridge` is a thin stdio↔TCP proxy; it auto-spawns `lldb.exe` and exposes LLDB's full surface (`command`, `debugger_list`). The single `command` tool IS the full LLDB REPL. |
| **`mcp__gdb`** (`servers/gdb`, `internal/gdb`) | gdb in GDB/MI mode (`gdb --interpreter=mi3`) | mcphub's native `gdb-bridge` drives gdb over MI in-process. Tools: `gdb_start`, `gdb_command` (run any MI or CLI command — `break`, `run`, `step`, `bt`, `info locals`, …), `gdb_terminate`, `gdb_list_sessions`, `debugger_status`. |

## Which debugger for which build

The debug-info format decides the debugger, not the compiler vendor:

| Build | Debug format | Debugger |
|---|---|---|
| **icx-cl `/clang:-gdwarf-4 -fuse-ld=lld`** | DWARF (in the PE) | **gdb 17.2 OR lldb** — both read it. *(verified: `Breakpoint 1, add (a=2, b=0) at test.cpp:2` — function, arg values, source+line)* |
| **icx-cl default / `/Zi`** | PDB / CodeView (MSVC) | **lldb** (reads PDB); gdb does not |
| **icx `-gdwarf-4 -fuse-ld=lld`** | DWARF | gdb / lldb |
| **g++ / clang (MinGW, ucrt64/clang64)** | DWARF | **gdb** (natural fit) / lldb |
| **MSVC `cl /Zi`** | PDB | **lldb** (or WinDbg/VS); gdb does not read PDB |
| **Linux (gcc/clang `-g`)** | DWARF | **gdb** / lldb |

Key gotcha (Windows / icx-cl): **plain `-gdwarf-4` is silently ignored** by the
`clang-cl`-style icx-cl driver (`warning: unknown argument ignored in clang-cl`).
You MUST use the `/clang:` passthrough: `icx-cl /clang:-gdwarf-4`. lld-link then
emits the `.debug_*` PE sections (it warns the section names are >8 chars —
harmless).

## Recipes

### icx-cl source-level debugging with gdb (Windows)

```text
# 1. compile with DWARF (via run_in_oneapi_env so the VS+oneAPI env is present)
mcp__oneapi-run/run_in_oneapi_env  command=icx-cl
  args=[ /clang:-gdwarf-4, -fuse-ld=lld, test.cpp, -o, test.exe ]
  cwd=C:\path\to\src

# 2. debug via the native gdb bridge
mcp__gdb/gdb_start                                  -> { session_id: "gdb-1" }
mcp__gdb/gdb_command  session_id=gdb-1  command="file C:/path/to/src/test.exe"
mcp__gdb/gdb_command  session_id=gdb-1  command="break add"
mcp__gdb/gdb_command  session_id=gdb-1  command="run"
mcp__gdb/gdb_command  session_id=gdb-1  command="info args"     # a=.., b=..
mcp__gdb/gdb_command  session_id=gdb-1  command="next"          # step to next line
mcp__gdb/gdb_command  session_id=gdb-1  command="info locals"   # sum=..
mcp__gdb/gdb_command  session_id=gdb-1  command="bt"
mcp__gdb/gdb_terminate session_id=gdb-1
```

`gdb_command` accepts BOTH gdb CLI (`break`, `run`, `info locals`) and raw GDB/MI
(`-break-insert`, `-exec-run`); `run`/`continue`/`step` block until the program
stops and the result carries the stop reason + frame.

### icx-cl debugging with lldb (Windows, any build incl. default PDB)

```text
mcp__lldb/command  command="target create C:/path/to/src/test.exe"
mcp__lldb/command  command="breakpoint set -f test.cpp -l 4"
mcp__lldb/command  command="run"
mcp__lldb/command  command="frame variable"     # a=7, b=35, sum=42
mcp__lldb/command  command="bt"
```

For a DWARF build (`icx -gdwarf-4`) lldb gives line-level breakpoints + locals
with values; on the default PDB build it still resolves symbols, breakpoints, and
backtraces.

### Linux

gdb and lldb are normally already on the system `PATH`; the daemon inherits it,
and the toolchain detector is a no-op. Build `-g` (DWARF) and use either bridge
the same way as above (`mcp__gdb` / `mcp__lldb`).

## Toolchain detection + override

The supervisor puts the debugger toolchain dir on the gdb/lldb daemon `PATH`, and
the native bridges resolve the binary by absolute path, both via
`internal/toolchain`:

- **Windows**: probes MSYS2 sub-environments in order `ucrt64`, `clang64`,
  `mingw64` under `%MSYS2_ROOT%` (default `C:\msys64`) for `gdb.exe` / `lldb.exe`.
- **Non-standard install / Linux / custom LLVM**: set
  `MCPHUB_DEBUGGER_TOOLCHAIN_DIR` to the bin dir(s) holding the debuggers
  (`os.PathListSeparator`-separated); it wins over detection.

`mcp__gdb/debugger_status` reports `{available, gdb_path, version}` so you can
confirm which gdb is being driven without starting a session.

## Why not the old external GDB-MCP

The previous `servers/gdb` ran the external GDB-MCP python server under `uv run`.
On Windows it failed two ways: its availability gate is a bare
`subprocess.run(['gdb','--version'])` that fails inside the console-less mcphub
daemon even when gdb is installed and on PATH; and its LLDB submodule needs the
lldb **python bindings** (absent from the uv venv). The native Go bridge avoids
both. LLDB has always been served by mcphub's own native lldb bridge.

## Terms and Abbreviations

- **DWARF** — the debug-info format gdb reads natively (and lldb reads); standard
  on Linux/MinGW, emit-able on Windows from clang-cl/icx-cl via `/clang:-gdwarf`.
- **PDB / CodeView** — Microsoft's debug-info format (MSVC, default icx-cl/`cl`);
  lldb reads it, gdb does not.
- **GDB/MI** — gdb's machine interface (`--interpreter=mi3`), the structured
  protocol the native gdb bridge drives.
- **icx-cl** — the `clang-cl`-style (MSVC-driver-compatible) Intel oneAPI C++
  compiler; **icx** — its GNU-style driver counterpart.
- **MSYS2 ucrt64** — the UCRT-based MinGW environment shipping gdb/lldb on this
  host (`C:\msys64\ucrt64\bin`); UCRT is the same Universal C Runtime MSVC uses.
