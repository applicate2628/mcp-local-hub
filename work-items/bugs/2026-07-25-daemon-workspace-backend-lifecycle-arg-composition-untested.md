---
title: buildWorkspaceBackendLifecycle (the function that composes every LSP language's actual spawn argv) has zero test coverage
severity: low
found-by: backend-engineer, cmake-language-support work-item (adjacent finding during exhaustive consumer sweep)
affected-surface: internal/cli/daemon_workspace.go buildWorkspaceBackendLifecycle
context: adjacent-finding
status: open
---

## What happened

While investigating a possible `cmake` addition to the `mcp-language-server` manifest
(work-item: `feat/cmake-language-support`, branch off `origin/master@1889cff6`;
the cmake-via-`mcp-language-server` wiring itself was subsequently reverted after
protocol-level measurement showed the neocmakelsp/mcp-language-server tool catalog
would advertise dead tools — see this work-item's status notes), I traced how a
`config.LanguageSpec` (backend/lsp_command/extra_flags) becomes the actual
subprocess argv via `internal/cli/daemon_workspace.go:353-383`
(`buildWorkspaceBackendLifecycle`). For the `mcp-language-server` backend branch
(used by 8 of the current 9 manifest languages — everything except `go`), the
composed argv is:

```go
args := []string{"-workspace", canonicalWorkspace, "-lsp", spec.LspCommand}
if len(spec.ExtraFlags) > 0 {
    args = append(args, "--")
    args = append(args, spec.ExtraFlags...)
}
```

i.e. `mcp-language-server -workspace <ws> -lsp <cmd> -- <extra_flags...>`. This is
the SINGLE function that decides the real spawn shape for every `mcp-language-server`-
backend language (clangd, fortran, javascript, python, rust, typescript, vscode-css,
vscode-html) plus the `gopls-mcp` branch for `go`. This finding is independent of the
cmake pivot — it applies to the manifest as it stands today, at 9 languages.

## Confirmed gap

`grep -rln "buildWorkspaceBackendLifecycle" internal/cli/*_test.go` returns nothing.
There is no `daemon_workspace_test.go` at all. Nothing asserts:

- the `-workspace`/`-lsp` positional shape,
- that `extra_flags` land AFTER a literal `--` separator (vs. being merged before it,
  or the separator being omitted),
- that a language with NO `extra_flags` (clangd, rust) omits the `--` entirely,
- the `gopls-mcp` branch's default `extra: []string{"mcp"}` fallback when
  `spec.ExtraFlags` is empty,
- the `default:` case returning `nil` for an unrecognized `spec.Backend`.

Every one of these is currently verified only by manual code reading or by a live
process spawn — no live spawn was performed this session (explicit operator
constraint: do not spawn real mcphub processes against the live fleet). So the
argv-composition step for all 9 shipped languages is presently unverified by any
automated test.

## Why this wasn't fixed in this work-item

Out of the approved change surface: this work-item's scope was scoped to the
manifest + hardcoded-consumer sweep, and after the pivot, to a standalone
`internal/cmakegraph` package. Neither touches `internal/cli`, and this function's
logic is already language-agnostic and needed no edit either way. Per the
adjacent-findings protocol, this is filed rather than fixed inline.

## Suggested fix direction (not decided — needs planner/architect sign-off if picked up)

Add `internal/cli/daemon_workspace_test.go` with a table-driven test over
`buildWorkspaceBackendLifecycle` covering:
1. `mcp-language-server` backend, no `ExtraFlags` (e.g. clangd/rust shape) →
   argv has no `--`.
2. `mcp-language-server` backend, with `ExtraFlags` (e.g. python/javascript's
   `--stdio`) → argv is `-workspace <ws> -lsp <cmd> -- <flags...>`.
3. `gopls-mcp` backend, empty `ExtraFlags` → defaults to `["mcp"]`.
4. `gopls-mcp` backend, explicit `ExtraFlags` → uses them verbatim (no default).
5. Unknown `spec.Backend` → returns `nil`.

This is a pure-function test (no process spawn, no scheduler, no live host
state) — safe to add without touching any live surface.
