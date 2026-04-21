# Phase 3 — Workspace-Scoped `mcp-language-server` Daemons (LAZY MATERIALIZATION) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add workspace-scoped MCP daemon management with **lazy backend materialization**:
- `mcphub register <workspace> [language...]` (language list OPTIONAL — defaults to ALL 9 manifest-declared languages) writes client configs, allocates ports from 9200–9299, creates one scheduler task per `(workspace, language)` pair whose payload is a **lazy proxy** (lightweight Go process that is NOT the heavy LSP backend), and persists the registry. Heavy backends DO NOT start.
- The lazy proxy answers `initialize` + `tools/list` **synthetically** from an embedded static tool catalog — no backend contacted.
- The first `tools/call` for a given `(workspace, language)` triggers **materialization**: spawn backend subprocess (`mcp-language-server --workspace <ws> --lsp <backend>` for 8 languages; `gopls mcp` for Go), proxy subsequent calls to it. Materialization is guarded by a singleflight gate with a retry throttle so concurrent first-calls don't race and failures don't spin.
- Once materialized, the backend lives as long as the lazy proxy does; if the backend crashes, the next `tools/call` re-materializes it (respecting the throttle).
- `mcphub unregister <workspace>` stops the proxy; existing Phase 2 `treekill.go` sweeps the materialized child backend with it.

Nine languages supported: `clangd`, `fortran`, `go`, `javascript`, `python`, `rust`, `typescript`, `vscode-css`, `vscode-html`.

**Architecture:** Build on Phase 2's protocol bridge layer (`internal/daemon/{http_host.go, protocol_bridge.go, synthetic_tools.go, resource_bridge.go, prompt_bridge.go, treekill.go}`), the already-extended manifest (`KindWorkspaceScoped`, `PortPool`, `Languages[]` at `internal/config/manifest.go:14-64`), and the scheduler rollback pattern from `internal/api/install.go:670-724`. A new registry file at `%LOCALAPPDATA%\mcp-local-hub\workspaces.yaml` (Linux/macOS: `$XDG_STATE_HOME` or `~/.local/state/mcp-local-hub/workspaces.yaml`) holds the `(workspace_key, language) → port / task_name / client_entry_names / lifecycle state` mapping. Registry writes are atomic (temp-file + `os.Rename`) and guarded by a cross-process file lock. The lazy proxy is a brand-new Go process (`mcphub daemon workspace-proxy`) that fronts the port, handles synthetic handshake traffic, and materializes the real backend on first `tools/call`.

**Tech Stack:** Go 1.26 (stdlib `os`, `net`, `net/http`, `path/filepath`, `crypto/sha256`, `sync`, `sync/atomic`, `time`), `gopkg.in/yaml.v3` (already in `go.mod`), `github.com/pelletier/go-toml/v2` for codex-cli (already in `go.mod`), `github.com/gofrs/flock` for the cross-process registry lock (new), and `golang.org/x/sync/singleflight` for the per-(workspace,language) materialization gate (new).

**Reference implementations:**

- `internal/api/install.go:651-803` (`executeInstallTo`) — scheduler-task rollback + client-entry rollback stack that workspace `Register` mirrors.
- `internal/api/migrate.go:70-165` (`MigrateFrom`) — shape for legacy-entry detection + report.
- `internal/clients/clients.go:96-108` (`AllClients`) + `internal/clients/codex_cli.go:83-103` (`AddEntry`) — existing writers reused unchanged.
- `internal/daemon/http_host.go` + `internal/daemon/protocol_bridge.go` + `internal/daemon/synthetic_tools.go` — the bridge layer the lazy proxy's materialized path reuses for stdio-to-HTTP.
- `internal/daemon/treekill.go` — Phase 2's process-tree-kill used to sweep the materialized backend when the proxy stops.
- Upstream `mcp-language-server` at `github.com/isaacphi/mcp-language-server` — `--workspace <path> --lsp <cmd>` on stdio; wrap via the Phase 2 stdio-bridge.
- Upstream `gopls mcp` — MCP endpoint; **v1 assumption is stdio mode**; verify during Task 9 (see Gap 1 in self-review).

**Prerequisites:**

- Phase 2 complete: `internal/daemon/{host,http_host,protocol_bridge,synthetic_tools,treekill}.go` exist.
- User MAY have LSP binaries installed, but the lazy path does NOT require them at register time; a missing binary surfaces at first `tools/call` via the `missing` lifecycle state.
- `mcphub` binary itself IS the lazy-proxy runtime, so it MUST be on PATH (already guaranteed by Phase 0–1).
- Windows 11 Task Scheduler (Linux/macOS ship compile-only stubs).

---

## Lazy-mode delta summary — what differs from the archived eager plan

The archived eager plan lives at `.scratch/plans-archive/2026-04-20-mcp-language-server-workspace-scoped-eager.md`. ~60% of that scaffolding carries over verbatim; ~40% diverges. This plan explicitly annotates each task with a **Delta from eager** header:

| Carries over verbatim | Adapted to lazy | Brand new (no eager analog) |
|---|---|---|
| Canonical workspace path + hash key (Task 3) | Manifest schema (Task 1) adds `transport:` field | Tool catalog + golden test (Task 6) |
| Port allocator (Task 5) | Registry (Task 2) adds `last_materialized_at`, `last_tools_call_at`, `last_error` | `BackendLifecycle` interface + two impls (Task 7) |
| Cross-process flock + atomic write | Manifest authoring (Task 1 step 3) | Inflight gate + retry throttle (Task 8) |
| Rollback pattern shape | Register (Task 10) creates lazy proxies, not backends; default all-languages | Lazy proxy (Task 9) |
| Unregister shape | Weekly refresh restarts proxies only | Daemon CLI: `daemon workspace-proxy` subcommand (Task 9 step 7) |
| Legacy detection regex (Task 14) | Migration (Task 14) emits ONE `register <ws>` per detected workspace | 5-state status with `last_used` (Task 15) |
| Status enrichment skeleton | Status (Task 15) adds 5 states + `last_materialized_at` | `--health --force-materialize` flag (Task 16) |

The 6 guardrails and 7 original decisions from the eager plan apply unchanged except:
- **Decision #1** (preflight clear-errors): the lazy proxy is always `mcphub` itself so preflight-the-proxy is trivially satisfied; LSP backend preflight moves to first `tools/call` (not register time). Registration no longer aborts on missing LSP binary — the missing state is observable at first call via status `missing`.
- **Decision #2–#7**: unchanged.

In addition, 6 lazy-specific decisions apply:
1. Default all-languages register.
2. Synthetic initialize + static `tools/list`, versioned + golden-tested.
3. First-call materialization via `golang.org/x/sync/singleflight` (NOT `sync.Once`) + retry throttle (default 2s).
4. Backend lifetime = forever-until-unregister (or proxy crash); re-materialize on crash.
5. Five-state lifecycle model: `configured`, `starting`, `active`, `missing`, `failed`.
6. `BackendLifecycle` interface + manifest `transport:` field (values: `stdio` in v1; `http_listen`, `native_http` reserved).

---

## Naming, ports, and invariants

Same as the eager plan, repeated here for locality:

**Port pool:** 9200–9299 (100 ports, `configs/ports.yaml:35` holds `workspace_scoped: []` empty — the registry is the source of truth).

**Workspace key:** `hex(sha256(canonicalAbsPath))[0:8]` (32 bits; <77k workspaces → <50% collision risk).

**Canonical workspace path (Windows):** `filepath.Abs` + drive-letter lowercase + reject symlinks/reparse points.

**Task naming:** `mcp-local-hub-lsp-<workspaceKey>-<lang>` (e.g. `mcp-local-hub-lsp-3f2a8c91-python`). Shared weekly task: `mcp-local-hub-workspace-weekly-refresh`.

**Client entry naming:** default `<server>-<lang>` (`mcp-language-server-python`, etc.); cross-workspace collision → append `-<4hex>` of workspace key; entry names stable across re-register.

**Registry file location:**
- Windows: `%LOCALAPPDATA%\mcp-local-hub\workspaces.yaml`
- Linux/macOS: `$XDG_STATE_HOME/mcp-local-hub/workspaces.yaml` (fallback `~/.local/state/mcp-local-hub/workspaces.yaml`)
- Backup: `workspaces.yaml.bak` (rolling overwrite on successful mutate).

**Language → LSP binary mapping** (manifest `languages[]`):

| Lang | Backend kind | Transport (v1) | Upstream command |
|---|---|---|---|
| clangd | mcp-language-server | stdio | `clangd` |
| fortran | mcp-language-server | stdio | `fortls` |
| go | gopls-mcp | stdio | `gopls mcp` |
| javascript | mcp-language-server | stdio | `typescript-language-server --stdio` (shared binary, separate entry) |
| python | mcp-language-server | stdio | `pyright-langserver --stdio` |
| rust | mcp-language-server | stdio | `rust-analyzer` |
| typescript | mcp-language-server | stdio | `typescript-language-server --stdio` |
| vscode-css | mcp-language-server | stdio | `vscode-css-language-server --stdio` |
| vscode-html | mcp-language-server | stdio | `vscode-html-language-server --stdio` |

Two backend kinds dispatched by `BackendLifecycle`:
- `mcp-language-server` (8 languages): stdio subprocess `mcp-language-server --workspace <path> --lsp <cmd> [-- <flags>...]`, wrapped by `daemon.NewStdioHost`.
- `gopls-mcp` (go only): stdio subprocess `gopls mcp`, wrapped by `daemon.NewStdioHost`. (If Task 9 verifies gopls rejects stdio, flip manifest `transport: http_listen` and defer to a follow-up task; see Gap 1.)

**Five-state lifecycle (per registry entry):**

| State | Meaning | When |
|---|---|---|
| `configured` | Registry entry exists, lazy proxy scheduler task running, backend NOT spawned | After `register`; after proxy restart before first `tools/call` |
| `starting` | Materialization in-flight (singleflight call active) | During first `tools/call` or retry after crash |
| `active` | Backend materialized and healthy | After successful materialization |
| `missing` | Materialization attempted; `exec.LookPath` of the LSP binary failed | First `tools/call` when binary not on PATH |
| `failed` | Materialization attempted and failed for any reason other than missing binary (handshake error, subprocess died at startup, timeout) | Any failure after the binary was found |

**Retry throttle:** after a materialization failure, the next attempt for the same `(workspaceKey, language)` must wait at least `inflightMinRetryGap` (default 2 seconds, configurable via pkg-scoped var for testing). If a `tools/call` arrives within the throttle window, the proxy returns the cached last error immediately (does not block).

**Registry debounce policy:** `last_tools_call_at` is updated with a coalescing debounce of ≥5s; `last_materialized_at`, `last_error`, and the lifecycle `state` are written immediately on change. Implementation detail: the proxy holds a per-entry in-memory last-write timestamp and only touches the registry when `now - lastWrite >= debounceInterval` or when the lifecycle state field itself changes.

---

## File Structure

**Files to create:**

- `servers/mcp-language-server/manifest.yaml` — workspace-scoped manifest listing the 9 languages + their backend kinds + `transport:` field. Embedded via `servers/embed.go`.
- `internal/api/workspace_path.go` (~60 LOC) — `CanonicalWorkspacePath`, `WorkspaceKey`. **Reused verbatim from eager Task 3.**
- `internal/api/workspace_path_test.go` (~100 LOC) — **verbatim from eager Task 3.**
- `internal/api/workspace_registry.go` (~320 LOC) — `Registry`, `WorkspaceEntry` with additional lazy-mode fields, atomic write + `.bak` + flock.
- `internal/api/workspace_registry_test.go` (~240 LOC) — roundtrip, atomic-write crash, cross-process lock, plus lazy-field serialization.
- `internal/api/port_alloc.go` (~90 LOC) — **verbatim from eager Task 5.**
- `internal/api/port_alloc_test.go` (~80 LOC) — **verbatim from eager Task 5.**
- `internal/api/tool_catalog.go` (~260 LOC) — embedded tool schemas by backend kind (`mcp-language-server`, `gopls-mcp`) with `CatalogVersion`, synthetic `initialize` response factory, synthetic `tools/list` response factory.
- `internal/api/tool_catalog_test.go` (~220 LOC) — golden test vs live upstream (skips on CI without binaries), versioning-drift guard, synthetic handshake envelope test.
- `internal/daemon/backend_lifecycle.go` (~280 LOC) — `BackendLifecycle` interface, `MCPEndpoint` interface, `mcpLanguageServerStdio` impl, `goplsStdio` impl, both reusing `daemon.NewStdioHost` + `protocol_bridge.go`.
- `internal/daemon/backend_lifecycle_test.go` (~180 LOC) — stdio lifecycle integration tests using fake binaries (`sh -c 'echo ...'` / `cmd /c echo`).
- `internal/daemon/inflight.go` (~120 LOC) — singleflight gate + retry throttle state.
- `internal/daemon/inflight_test.go` (~160 LOC) — concurrent-call convergence, retry throttle timing.
- `internal/daemon/lazy_proxy.go` (~360 LOC) — per-port `http.Server`, dispatch: synthetic for `initialize` + `tools/list` + `notifications/*`, materialize-then-forward for `tools/call` + other methods.
- `internal/daemon/lazy_proxy_test.go` (~300 LOC) — synthetic-only traffic doesn't materialize; first tools/call materializes; subsequent initialize still synthetic; failed materialize produces `failed` state and cached error; throttle honored on retry.
- `internal/api/register.go` (~420 LOC) — `Register`, `Unregister`, `PreflightWrapperBinary` (NOT `PreflightLanguages` — the missing-LSP-binary check moves to materialization time), `ResolveEntryName`, `RegisterOpts`, per-language rollback stack.
- `internal/api/register_test.go` (~420 LOC) — full register success, partial failure rollback, idempotent re-register, default-all-languages, full + partial unregister.
- `internal/api/weekly_refresh.go` (~110 LOC) — **semantics verbatim from eager Task 11** (restarts proxies, not backends; re-materialization happens on next `tools/call`).
- `internal/api/weekly_refresh_test.go` (~90 LOC) — **verbatim from eager Task 11.**
- `internal/api/legacy_detect.go` (~180 LOC) — **verbatim from eager Task 12.**
- `internal/api/legacy_detect_test.go` (~150 LOC) — **verbatim from eager Task 12.**
- `internal/api/legacy_migrate.go` (~160 LOC) — `MigrateLegacy` emits ONE `Register(ws, nil, ...)` per detected workspace (default all-languages), NOT one `Register` per language.
- `internal/api/legacy_migrate_test.go` (~120 LOC) — dedup by workspace, default all-languages assertion.
- `internal/cli/register.go` (~280 LOC) — `register` accepts 0 languages (= all), `unregister` accepts partial, `workspaces` prints 5-state + `last_used` column.
- `internal/cli/register_test.go` (~140 LOC) — default-all-languages flag parsing, JSON output golden, 5-state column rendering.
- `internal/cli/migrate_legacy.go` (~80 LOC) — **verbatim from eager Task 12**; underlying impl now emits one Register per workspace.
- `internal/cli/weekly_refresh.go` (~40 LOC) — **verbatim from eager Task 14.**
- `internal/cli/daemon_workspace.go` (new file — separate from `daemon.go` to keep Phase 2 file small) — `daemon workspace-proxy` subcommand: launches the lazy proxy.
- `internal/cli/daemon_workspace_test.go` (~200 LOC) — flag validation + registry-not-found error path.
- `internal/api/e2e_smoke_test.go` (~200 LOC) — register → proxy handshake (no materialization) → tools/call (materialization) → status shows `active` → unregister → cleanup.

**Files to modify:**

- `internal/config/manifest.go` — add `Backend string` + `Transport string` to `LanguageSpec` (line 55-59); extend `Validate()` (line 142-156) to require `port_pool` + non-empty `languages[]` when `kind == workspace-scoped` and to validate `Backend` + `Transport` enum values.
- `internal/config/manifest_test.go` — add `TestParseManifest_WorkspaceScopedSchema`, `TestParseManifest_WorkspaceScopedRejectsMissingPortPool`, `TestParseManifest_LanguageTransportDefault`, `TestParseManifest_LanguageTransportEnum`.
- `internal/api/install.go` — Install/InstallAll refuses/skips workspace-scoped manifests (verbatim from eager Task 14; minor string drift OK).
- `internal/api/types.go:17-35` — extend `DaemonStatus` with `Workspace`, `Language`, `Backend`, **`Lifecycle` (string — 5 states), `LastMaterializedAt`, `LastToolsCallAt`, `LastError` (omitempty)**.
- `internal/api/status_enrich.go` — extend registry-backed enrichment to populate all new fields.
- `internal/cli/status.go` — add `--workspace-scoped` + `--force-materialize` flags; print 5-state + `last_used` column.
- `internal/cli/root.go:22-46` — wire `newRegisterCmd`, `newUnregisterCmd`, `newWorkspacesCmd`, `newMigrateLegacyCmd`, `newWeeklyRefreshCmd`, and the new `daemon workspace-proxy` subcommand (which is a subcommand of the existing `daemon` command, wired in `newDaemonCmd`).
- `go.mod` — add `github.com/gofrs/flock v0.x.y`, `golang.org/x/sync` (for singleflight; `golang.org/x/sync/singleflight` is its own package).

---

## Task breakdown

Total tasks: **16** across 5 milestones. Each task is independently committable.

- **M1 (Manifest + registry foundations)**: Tasks 1, 2, 3
- **M2 (Tool catalog + backend abstraction + lazy proxy)**: Tasks 4, 5, 6, 7
- **M3 (Register + Unregister + CLI)**: Tasks 8, 9, 10
- **M4 (Migration + weekly refresh)**: Tasks 11, 12
- **M5 (Integration polish)**: Tasks 13, 14, 15, 16

---

### Task 1: Manifest schema extension — `Backend` + `Transport` fields + workspace-scoped validation

**Delta from eager:** adds `Transport` field alongside `Backend` and validates its enum (`stdio` | `http_listen` | `native_http`; default `stdio`).

**Files:**
- Modify: `internal/config/manifest.go:55-59` (extend `LanguageSpec`) and `:142-156` (extend `Validate`)
- Modify: `internal/config/manifest_test.go` (4 tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/manifest_test.go`:

```go
func TestParseManifest_WorkspaceScopedSchema(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool:
  start: 9200
  end: 9299
languages:
  - name: python
    backend: mcp-language-server
    transport: stdio
    lsp_command: pyright-langserver
    extra_flags: ["--stdio"]
  - name: go
    backend: gopls-mcp
    transport: stdio
    lsp_command: gopls
    extra_flags: ["mcp"]
weekly_refresh: false
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Kind != KindWorkspaceScoped {
		t.Errorf("Kind = %q, want workspace-scoped", m.Kind)
	}
	if m.PortPool == nil || m.PortPool.Start != 9200 || m.PortPool.End != 9299 {
		t.Errorf("PortPool = %+v, want {9200,9299}", m.PortPool)
	}
	if len(m.Languages) != 2 {
		t.Fatalf("len(Languages) = %d, want 2", len(m.Languages))
	}
	if m.Languages[0].Backend != "mcp-language-server" {
		t.Errorf("Languages[0].Backend = %q", m.Languages[0].Backend)
	}
	if m.Languages[1].Backend != "gopls-mcp" {
		t.Errorf("Languages[1].Backend = %q", m.Languages[1].Backend)
	}
	if m.Languages[0].Transport != "stdio" {
		t.Errorf("Languages[0].Transport = %q, want stdio", m.Languages[0].Transport)
	}
}

func TestParseManifest_LanguageTransportDefault(t *testing.T) {
	// transport omitted -> defaults to "stdio"
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool: {start: 9200, end: 9299}
languages:
  - name: python
    backend: mcp-language-server
    lsp_command: pyright-langserver
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Languages[0].Transport != "stdio" {
		t.Errorf("Transport default = %q, want stdio", m.Languages[0].Transport)
	}
}

func TestParseManifest_LanguageTransportEnum(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
port_pool: {start: 9200, end: 9299}
languages:
  - name: python
    backend: mcp-language-server
    transport: something-unknown
    lsp_command: pyright-langserver
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for unknown transport value")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("error should mention transport: %v", err)
	}
}

func TestParseManifest_WorkspaceScopedRejectsMissingPortPool(t *testing.T) {
	yaml := `
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
languages:
  - name: python
    backend: mcp-language-server
    lsp_command: pyright-langserver
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for workspace-scoped manifest without port_pool")
	}
	if !strings.Contains(err.Error(), "port_pool") {
		t.Errorf("error should mention port_pool: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/config/ -run "TestParseManifest_WorkspaceScoped|TestParseManifest_LanguageTransport" -v
```

Expected: FAIL — `LanguageSpec` has no `Backend`/`Transport`; validation doesn't require them.

- [ ] **Step 3: Add `Backend` + `Transport` fields and extend validation**

In `internal/config/manifest.go`, replace the `LanguageSpec` struct at line 55 with:

```go
// Valid LanguageSpec.Transport values. Kept in manifest alongside language so
// the launcher can dispatch on per-language transport without re-probing the
// upstream binary.
const (
	LanguageTransportStdio      = "stdio"       // v1 default: subprocess stdin/stdout wrapped by daemon.NewStdioHost
	LanguageTransportHTTPListen = "http_listen" // reserved (gopls -listen variant)
	LanguageTransportNativeHTTP = "native_http" // reserved
)

type LanguageSpec struct {
	Name       string   `yaml:"name"`
	Backend    string   `yaml:"backend"`   // "mcp-language-server" or "gopls-mcp"
	Transport  string   `yaml:"transport"` // "stdio" (default) | "http_listen" | "native_http"
	LspCommand string   `yaml:"lsp_command"`
	ExtraFlags []string `yaml:"extra_flags"`
}
```

Then replace `Validate` at line 142:

```go
func (m *ServerManifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest: name is required")
	}
	if m.Kind != KindGlobal && m.Kind != KindWorkspaceScoped {
		return fmt.Errorf("manifest %s: kind must be %q or %q (got %q)", m.Name, KindGlobal, KindWorkspaceScoped, m.Kind)
	}
	if m.Transport != TransportNativeHTTP && m.Transport != TransportStdioBridge {
		return fmt.Errorf("manifest %s: transport must be %q or %q (got %q)", m.Name, TransportNativeHTTP, TransportStdioBridge, m.Transport)
	}
	if m.Command == "" {
		return fmt.Errorf("manifest %s: command is required", m.Name)
	}
	if m.Kind == KindWorkspaceScoped {
		if m.PortPool == nil {
			return fmt.Errorf("manifest %s: port_pool is required for kind=workspace-scoped", m.Name)
		}
		if m.PortPool.Start <= 0 || m.PortPool.End < m.PortPool.Start {
			return fmt.Errorf("manifest %s: port_pool must have start>0 and end>=start (got {%d,%d})", m.Name, m.PortPool.Start, m.PortPool.End)
		}
		if len(m.Languages) == 0 {
			return fmt.Errorf("manifest %s: languages[] must be non-empty for kind=workspace-scoped", m.Name)
		}
		for i := range m.Languages {
			l := &m.Languages[i]
			if l.Name == "" {
				return fmt.Errorf("manifest %s: languages[%d].name is required", m.Name, i)
			}
			if l.Backend != "mcp-language-server" && l.Backend != "gopls-mcp" {
				return fmt.Errorf("manifest %s: languages[%d].backend must be \"mcp-language-server\" or \"gopls-mcp\" (got %q)", m.Name, i, l.Backend)
			}
			if l.Transport == "" {
				l.Transport = LanguageTransportStdio
			}
			if l.Transport != LanguageTransportStdio && l.Transport != LanguageTransportHTTPListen && l.Transport != LanguageTransportNativeHTTP {
				return fmt.Errorf("manifest %s: languages[%d].transport must be %q | %q | %q (got %q)", m.Name, i,
					LanguageTransportStdio, LanguageTransportHTTPListen, LanguageTransportNativeHTTP, l.Transport)
			}
			if l.LspCommand == "" {
				return fmt.Errorf("manifest %s: languages[%d].lsp_command is required", m.Name, i)
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/config/ -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/config/manifest.go internal/config/manifest_test.go
git commit -m "$(cat <<'EOF'
feat(config): manifest LanguageSpec gains Backend + Transport

Adds Backend + Transport (default stdio) to LanguageSpec and tightens
Validate() for kind=workspace-scoped: requires port_pool, non-empty
languages[], and valid Backend + Transport enum values. Groundwork
for Phase 3 (lazy-materialization mcp-language-server).
EOF
)"
```

---

### Task 2: Author `servers/mcp-language-server/manifest.yaml` with `transport: stdio` per language

**Delta from eager:** each language entry now carries `transport: stdio` (default). Otherwise identical to eager Task 2.

**Files:**
- Create: `servers/mcp-language-server/manifest.yaml`
- Create: `internal/config/mcp_language_server_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/config/mcp_language_server_test.go`:

```go
package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseManifest_McpLanguageServerShipped(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	yamlPath := filepath.Join(repoRoot, "servers", "mcp-language-server", "manifest.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read %s: %v", yamlPath, err)
	}
	m, err := ParseManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Kind != KindWorkspaceScoped {
		t.Fatalf("Kind = %q, want workspace-scoped", m.Kind)
	}
	want := map[string]string{
		"clangd": "mcp-language-server", "fortran": "mcp-language-server",
		"go": "gopls-mcp",
		"javascript": "mcp-language-server", "python": "mcp-language-server",
		"rust": "mcp-language-server", "typescript": "mcp-language-server",
		"vscode-css": "mcp-language-server", "vscode-html": "mcp-language-server",
	}
	got := map[string]string{}
	for _, l := range m.Languages {
		got[l.Name] = l.Backend
		if l.Transport != LanguageTransportStdio {
			t.Errorf("language %s: Transport = %q, want stdio in v1", l.Name, l.Transport)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("languages: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for name, backend := range want {
		if got[name] != backend {
			t.Errorf("languages[%s].backend = %q, want %q", name, got[name], backend)
		}
	}
	if m.PortPool.Start != 9200 || m.PortPool.End != 9299 {
		t.Errorf("PortPool = %+v, want {9200,9299}", m.PortPool)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/config/ -run TestParseManifest_McpLanguageServerShipped -v
```

- [ ] **Step 3: Create the manifest**

Create `servers/mcp-language-server/manifest.yaml`:

```yaml
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge

# Default command/transport apply to 8 of 9 languages (the mcp-language-server
# backend). The `go` language overrides these via backend=gopls-mcp — see
# internal/daemon/backend_lifecycle.go for dispatch.
command: mcp-language-server

port_pool:
  start: 9200
  end: 9299

# Phase 3 lazy mode: register() creates scheduler tasks for lazy proxies.
# Heavy backends spawn only on first tools/call. weekly_refresh restarts the
# PROXY; re-materialization of the backend happens on the next tools/call.
weekly_refresh: false

# 9 supported languages. All v1 entries use transport: stdio. The lazy proxy
# dispatches on backend+transport at materialization time; changing to
# http_listen or native_http is a future follow-up.
languages:
  - name: clangd
    backend: mcp-language-server
    transport: stdio
    lsp_command: clangd

  - name: fortran
    backend: mcp-language-server
    transport: stdio
    lsp_command: fortls

  - name: go
    backend: gopls-mcp
    transport: stdio
    lsp_command: gopls
    extra_flags: [mcp]

  - name: javascript
    backend: mcp-language-server
    transport: stdio
    lsp_command: typescript-language-server
    extra_flags: ["--stdio"]

  - name: python
    backend: mcp-language-server
    transport: stdio
    lsp_command: pyright-langserver
    extra_flags: ["--stdio"]

  - name: rust
    backend: mcp-language-server
    transport: stdio
    lsp_command: rust-analyzer

  - name: typescript
    backend: mcp-language-server
    transport: stdio
    lsp_command: typescript-language-server
    extra_flags: ["--stdio"]

  - name: vscode-css
    backend: mcp-language-server
    transport: stdio
    lsp_command: vscode-css-language-server
    extra_flags: ["--stdio"]

  - name: vscode-html
    backend: mcp-language-server
    transport: stdio
    lsp_command: vscode-html-language-server
    extra_flags: ["--stdio"]
```

- [ ] **Step 4: Run test**

```bash
go test ./internal/config/ -run TestParseManifest_McpLanguageServerShipped -v
```

- [ ] **Step 5: Commit**

```bash
git add servers/mcp-language-server/manifest.yaml internal/config/mcp_language_server_test.go
git commit -m "$(cat <<'EOF'
feat(manifest): add mcp-language-server workspace-scoped manifest

9 languages (clangd, fortran, go, javascript, python, rust, typescript,
vscode-css, vscode-html), port pool 9200-9299, transport: stdio for all
entries in v1. go uses gopls-mcp backend; other 8 use mcp-language-server.
EOF
)"
```

---

### Task 3: Canonical workspace path + deterministic key

**Delta from eager:** VERBATIM from eager Task 3.

**Files:** create `internal/api/workspace_path.go` and `internal/api/workspace_path_test.go`. Reuse implementation + tests from the archived plan at `.scratch/plans-archive/2026-04-20-mcp-language-server-workspace-scoped-eager.md` Task 3 (sections Step 1–Step 5). Commit message:

```bash
git add internal/api/workspace_path.go internal/api/workspace_path_test.go
git commit -m "$(cat <<'EOF'
feat(api): canonical workspace path + deterministic workspace key

CanonicalWorkspacePath: abs + clean + reject-symlink + lowercase
Windows drive letter. WorkspaceKey: sha256[:8] hex. Reused downstream
by registry, lazy proxy, and scheduler task naming.
EOF
)"
```

---

### Task 4: Workspace registry — lazy-aware schema + atomic-write + file lock

**Delta from eager:** adds `Lifecycle`, `LastMaterializedAt`, `LastToolsCallAt`, `LastError` fields to `WorkspaceEntry`. Adds constants for the 5 lifecycle states. Otherwise reuses eager Task 4 code for `Load`, `Save`, `Lock`, `Put`, `Get`, `Remove`, `AllocatedPorts`, `ListByWorkspace`, `DefaultRegistryPath`. Adds a `PutLifecycle(key, lang, state, errorMsg)` helper with internal debounce state for `last_tools_call_at`.

**Files:**
- Modify: `go.mod` (add `github.com/gofrs/flock`, `golang.org/x/sync`)
- Create: `internal/api/workspace_registry.go`
- Create: `internal/api/workspace_registry_test.go`

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/gofrs/flock@latest
go get golang.org/x/sync/singleflight@latest
```

- [ ] **Step 2: Write the failing tests**

Create `internal/api/workspace_registry_test.go`. Reuse the four tests from eager plan Task 4 Step 2 (roundtrip empty, roundtrip with entries, atomic-write crash, concurrent lock). Then APPEND these lazy-mode tests:

```go
func TestRegistry_LifecycleFieldsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	now := time.Now().UTC().Truncate(time.Second)
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws/foo",
		Language: "python", Backend: "mcp-language-server", Port: 9200,
		TaskName: "mcp-local-hub-lsp-abcd1234-python",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-python"},
		Lifecycle:     LifecycleActive,
		LastMaterializedAt: now,
		LastToolsCallAt:    now,
		LastError:          "", // healthy
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatal(err)
	}
	got, ok := reg2.Get("abcd1234", "python")
	if !ok {
		t.Fatal("entry missing")
	}
	if got.Lifecycle != LifecycleActive {
		t.Errorf("Lifecycle = %q, want active", got.Lifecycle)
	}
	if !got.LastMaterializedAt.Equal(now) {
		t.Errorf("LastMaterializedAt = %v, want %v", got.LastMaterializedAt, now)
	}
}

func TestRegistry_LastErrorTruncation(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/r.yaml")
	big := strings.Repeat("x", 500)
	reg.PutLifecycle("abcd1234", "python", LifecycleFailed, big)
	e, ok := reg.Get("abcd1234", "python")
	if !ok {
		t.Fatal("missing entry after PutLifecycle")
	}
	if len(e.LastError) > MaxLastErrorBytes {
		t.Errorf("LastError length = %d, want <= %d", len(e.LastError), MaxLastErrorBytes)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/api/ -run "TestRegistry_" -v
```

- [ ] **Step 4: Implement registry**

Create `internal/api/workspace_registry.go`. Start from the eager plan Task 4 Step 4 content, then add:

```go
// Lifecycle enumerates the 5 observable states of a workspace-scoped daemon.
// Written by the lazy proxy; read by Status and CLI output.
const (
	LifecycleConfigured = "configured" // registry entry exists, proxy running, backend NOT spawned
	LifecycleStarting   = "starting"   // materialization in-flight (singleflight call active)
	LifecycleActive     = "active"     // backend materialized and healthy
	LifecycleMissing    = "missing"    // materialization attempted; LSP binary not on PATH
	LifecycleFailed     = "failed"     // materialization attempted; failed for any non-missing-binary reason
)

// MaxLastErrorBytes caps LastError to keep the YAML file compact and
// readable in `workspaces` output. Truncated mid-UTF8 is OK because the
// field is diagnostic-only.
const MaxLastErrorBytes = 200
```

Extend `WorkspaceEntry`:

```go
type WorkspaceEntry struct {
	WorkspaceKey  string            `yaml:"workspace_key"`
	WorkspacePath string            `yaml:"workspace_path"`
	Language      string            `yaml:"language"`
	Backend       string            `yaml:"backend"`
	Port          int               `yaml:"port"`
	TaskName      string            `yaml:"task_name"`
	ClientEntries map[string]string `yaml:"client_entries"`
	WeeklyRefresh bool              `yaml:"weekly_refresh"`

	// Lazy-mode fields. All omitempty so earlier schemas round-trip safely.
	Lifecycle          string    `yaml:"lifecycle,omitempty"`
	LastMaterializedAt time.Time `yaml:"last_materialized_at,omitempty"`
	LastToolsCallAt    time.Time `yaml:"last_tools_call_at,omitempty"`
	LastError          string    `yaml:"last_error,omitempty"`
}
```

Add helper (acquires registry lock internally; used by the proxy for state writes):

```go
// PutLifecycle loads the registry under lock, updates the lifecycle state +
// LastError for (workspaceKey, language), and saves. LastError is truncated
// to MaxLastErrorBytes. If Lifecycle transitions to Active, the caller
// should also set LastMaterializedAt (via PutLifecycleFull below).
func (r *Registry) PutLifecycle(workspaceKey, language, state, lastError string) error {
	unlock, err := r.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.Load(); err != nil {
		return err
	}
	e, ok := r.Get(workspaceKey, language)
	if !ok {
		e = WorkspaceEntry{WorkspaceKey: workspaceKey, Language: language}
	}
	e.Lifecycle = state
	if len(lastError) > MaxLastErrorBytes {
		lastError = lastError[:MaxLastErrorBytes]
	}
	e.LastError = lastError
	r.Put(e)
	return r.Save()
}

// PutLifecycleWithTimestamps is the richer variant used by the proxy at
// materialization edges: state transition + timestamps in one atomic save.
func (r *Registry) PutLifecycleWithTimestamps(workspaceKey, language, state, lastError string, materializedAt, toolsCallAt time.Time) error {
	unlock, err := r.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.Load(); err != nil {
		return err
	}
	e, ok := r.Get(workspaceKey, language)
	if !ok {
		e = WorkspaceEntry{WorkspaceKey: workspaceKey, Language: language}
	}
	e.Lifecycle = state
	if len(lastError) > MaxLastErrorBytes {
		lastError = lastError[:MaxLastErrorBytes]
	}
	e.LastError = lastError
	if !materializedAt.IsZero() {
		e.LastMaterializedAt = materializedAt.UTC()
	}
	if !toolsCallAt.IsZero() {
		e.LastToolsCallAt = toolsCallAt.UTC()
	}
	r.Put(e)
	return r.Save()
}
```

- [ ] **Step 5: Run tests**

```bash
go test ./internal/api/ -run "TestRegistry_" -v
```

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/api/workspace_registry.go internal/api/workspace_registry_test.go
git commit -m "$(cat <<'EOF'
feat(api): workspace registry with lazy-mode lifecycle fields

Registry persists lifecycle (5 states), last_materialized_at,
last_tools_call_at, and last_error (capped at 200 bytes) alongside
the core (workspace_key, language) -> port/task mapping. Atomic
write + rolling .bak + gofrs/flock from eager plan preserved.
EOF
)"
```

---

### Task 5: First-free port allocator

**Delta from eager:** VERBATIM from eager Task 5.

Reuse implementation + tests from archived plan Task 5.

```bash
git commit -m "feat(api): first-free port allocator for workspace-scoped daemons"
```

---

## M2 — Tool catalog + backend abstraction + lazy proxy

### Task 6: Tool catalog — embedded schemas + synthetic initialize/tools-list + golden test

**Delta from eager:** ENTIRELY NEW — no eager analog.

**Files:**
- Create: `internal/api/tool_catalog.go`
- Create: `internal/api/tool_catalog_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/api/tool_catalog_test.go`:

```go
package api

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestToolCatalog_KnownKinds(t *testing.T) {
	for _, kind := range []string{"mcp-language-server", "gopls-mcp"} {
		cat, ok := ToolCatalogForBackend(kind)
		if !ok {
			t.Errorf("missing catalog for backend %q", kind)
			continue
		}
		if cat.CatalogVersion == "" {
			t.Errorf("%s: CatalogVersion empty", kind)
		}
		if len(cat.Tools) == 0 {
			t.Errorf("%s: Tools empty", kind)
		}
	}
}

func TestToolCatalog_SyntheticInitializeShape(t *testing.T) {
	reqID := json.RawMessage(`1`)
	resp, err := SyntheticInitializeResponse(reqID, "mcp-language-server")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", parsed["jsonrpc"])
	}
	if _, ok := parsed["result"]; !ok {
		t.Error("missing result")
	}
	result := parsed["result"].(map[string]any)
	if _, ok := result["capabilities"]; !ok {
		t.Error("missing capabilities")
	}
	si, _ := result["serverInfo"].(map[string]any)
	if si == nil {
		t.Error("missing serverInfo")
	}
}

func TestToolCatalog_SyntheticToolsListEnvelope(t *testing.T) {
	reqID := json.RawMessage(`42`)
	resp, err := SyntheticToolsListResponse(reqID, "mcp-language-server")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Jsonrpc string `json:"jsonrpc"`
		ID      json.RawMessage
		Result  struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Jsonrpc != "2.0" || !bytes.Equal(parsed.ID, reqID) {
		t.Errorf("envelope mismatch: %+v", parsed)
	}
	if len(parsed.Result.Tools) == 0 {
		t.Error("no tools")
	}
}

// TestToolCatalog_GoldenAgainstUpstream spawns the real upstream binary when
// available and compares its tools/list reply to the embedded static catalog.
// Drift -> catalog is out of date; maintainer must bump CatalogVersion.
func TestToolCatalog_GoldenAgainstUpstream(t *testing.T) {
	// Short-circuit when the binary isn't on PATH — CI without LSPs passes.
	bin := "mcp-language-server"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH; skipping live golden test", bin)
	}
	// Minimal handshake: spawn, send initialize, send tools/list, compare.
	upstream, err := captureToolsList(t, bin, []string{"--help"})
	if err != nil {
		t.Skipf("upstream probe failed (acceptable in CI): %v", err)
	}
	cat, _ := ToolCatalogForBackend("mcp-language-server")
	if !strings.EqualFold(upstream.serverName, cat.ServerInfoName) {
		t.Errorf("serverInfo.name drift: upstream=%q catalog=%q (bump CatalogVersion %q and regenerate)",
			upstream.serverName, cat.ServerInfoName, cat.CatalogVersion)
	}
	upstreamNames := map[string]bool{}
	for _, n := range upstream.toolNames {
		upstreamNames[n] = true
	}
	for _, tool := range cat.Tools {
		if !upstreamNames[tool.Name] {
			t.Errorf("catalog has tool %q not present in upstream (bump CatalogVersion %q; live names: %v)",
				tool.Name, cat.CatalogVersion, upstream.toolNames)
		}
	}
	for n := range upstreamNames {
		found := false
		for _, tool := range cat.Tools {
			if tool.Name == n {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("upstream has tool %q missing from catalog (bump CatalogVersion %q; add %q)",
				n, cat.CatalogVersion, n)
		}
	}
}

// captureToolsList is a test-only helper that spawns the binary, performs
// the minimal JSON-RPC handshake over stdio, and returns observed metadata.
// Implementation lives in the test file so non-test binaries don't ship it.
func captureToolsList(t *testing.T, bin string, args []string) (*upstreamProbe, error) {
	// Full implementation added in Step 3. Returning a stub here keeps the
	// failing test readable until ToolCatalog types exist.
	return nil, nil
}

type upstreamProbe struct {
	serverName string
	toolNames  []string
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/api/ -run TestToolCatalog -v
```

Expected: FAIL — undefined types and functions.

- [ ] **Step 3: Implement `tool_catalog.go`**

Create `internal/api/tool_catalog.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
)

// ToolSchema is the embedded representation of one MCP tool entry as it
// appears in a tools/list response. The JSON shape matches the upstream
// MCP spec verbatim so it can be emitted as-is inside the result.tools[].
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolCatalog is a versioned collection of tool schemas for one backend
// kind. CatalogVersion is the maintainer's human hook: bump it whenever
// the upstream tool set changes so the golden test's drift message
// names the right knob to turn.
type ToolCatalog struct {
	CatalogVersion     string
	ServerInfoName     string
	ServerInfoVersion  string
	ProtocolVersion    string
	InstructionsSource string // one-line note about where this was captured from
	Tools              []ToolSchema
}

// ToolCatalogForBackend returns the embedded catalog for the given backend
// kind ("mcp-language-server" | "gopls-mcp"), or (zero, false) if unknown.
// The returned catalog is safe to read but must not be mutated.
func ToolCatalogForBackend(kind string) (ToolCatalog, bool) {
	switch kind {
	case "mcp-language-server":
		return mcpLanguageServerCatalog, true
	case "gopls-mcp":
		return goplsMCPCatalog, true
	}
	return ToolCatalog{}, false
}

// SyntheticInitializeResponse builds a JSON-RPC initialize response using
// the embedded catalog's serverInfo + an empty capabilities envelope that
// advertises tools support. Response id = the request id so the client
// correlates. Caller writes the bytes to their HTTP body.
func SyntheticInitializeResponse(reqID json.RawMessage, kind string) ([]byte, error) {
	cat, ok := ToolCatalogForBackend(kind)
	if !ok {
		return nil, fmt.Errorf("unknown backend kind %q", kind)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result": map[string]any{
			"protocolVersion": cat.ProtocolVersion,
			"serverInfo": map[string]any{
				"name":    cat.ServerInfoName,
				"version": cat.ServerInfoVersion,
			},
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false},
				"prompts":   map[string]any{"listChanged": false},
			},
		},
	}
	return json.Marshal(payload)
}

// SyntheticToolsListResponse builds a JSON-RPC tools/list response from the
// embedded static catalog. Used by the lazy proxy for every client's
// initial tools/list — no backend contacted.
func SyntheticToolsListResponse(reqID json.RawMessage, kind string) ([]byte, error) {
	cat, ok := ToolCatalogForBackend(kind)
	if !ok {
		return nil, fmt.Errorf("unknown backend kind %q", kind)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result":  map[string]any{"tools": cat.Tools},
	}
	return json.Marshal(payload)
}

// --- Embedded catalogs. Update via the maintainer workflow documented at
// the top of the golden test (TestToolCatalog_GoldenAgainstUpstream): run
// upstream, capture tools/list, diff, bump CatalogVersion, paste tools.

// mcpLanguageServerCatalog mirrors the tool set exposed by
// github.com/isaacphi/mcp-language-server as of CatalogVersion "1.0.0".
// Confirm upstream tool names + input schemas during implementation.
var mcpLanguageServerCatalog = ToolCatalog{
	CatalogVersion:     "1.0.0",
	ServerInfoName:     "mcp-language-server",
	ServerInfoVersion:  "unknown",
	ProtocolVersion:    "2024-11-05",
	InstructionsSource: "manual capture via `mcp-language-server --lsp pyright-langserver` handshake 2026-04",
	Tools: []ToolSchema{
		{
			Name:        "definition",
			Description: "Jump to definition of a symbol at a code position.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["file","line","character"]}`),
		},
		{
			Name:        "references",
			Description: "Find references to a symbol at a code position.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["file","line","character"]}`),
		},
		{
			Name:        "diagnostics",
			Description: "List diagnostics for a file or the whole workspace.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"}}}`),
		},
		{
			Name:        "hover",
			Description: "Show hover documentation at a code position.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["file","line","character"]}`),
		},
		{
			Name:        "rename_symbol",
			Description: "Rename a symbol across the workspace.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"},"newName":{"type":"string"}},"required":["file","line","character","newName"]}`),
		},
		{
			Name:        "edit_file",
			Description: "Apply an LSP workspace/edit to a file.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"edits":{"type":"array"}},"required":["file","edits"]}`),
		},
	},
}

// goplsMCPCatalog mirrors `gopls mcp`'s built-in tools as of
// CatalogVersion "1.0.0". Verify during implementation by running
// `gopls mcp -h` or handshaking against a live instance.
var goplsMCPCatalog = ToolCatalog{
	CatalogVersion:     "1.0.0",
	ServerInfoName:     "gopls",
	ServerInfoVersion:  "unknown",
	ProtocolVersion:    "2024-11-05",
	InstructionsSource: "manual capture via `gopls mcp` 2026-04",
	// Placeholder tool set — the TDD cycle in Step 4 fixes these by running
	// the live golden test against a working gopls. Implementation MUST NOT
	// ship with this placeholder unless gopls mcp has been verified.
	Tools: []ToolSchema{
		{
			Name:        "go_definition",
			Description: "Jump to Go definition.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}}}`),
		},
		{
			Name:        "go_references",
			Description: "Find references to a Go symbol.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}}}`),
		},
		{
			Name:        "go_hover",
			Description: "Hover documentation in Go.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}}}`),
		},
	},
}
```

- [ ] **Step 4: Implement the `captureToolsList` helper in the test file**

In `internal/api/tool_catalog_test.go`, replace the `captureToolsList` stub with a real implementation that spawns the binary on stdio, sends `initialize` + `tools/list` JSON-RPC frames, reads the responses, and extracts `serverInfo.name` + each tool's name. Use a 15-second deadline. If any step fails (subprocess exits, malformed JSON, timeout), return an error so the test calls `t.Skipf` cleanly.

```go
func captureToolsList(t *testing.T, bin string, args []string) (*upstreamProbe, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--workspace", t.TempDir(), "--lsp", "pyright-langserver")
	stdin, err := cmd.StdinPipe()
	if err != nil { return nil, err }
	stdout, err := cmd.StdoutPipe()
	if err != nil { return nil, err }
	if err := cmd.Start(); err != nil { return nil, err }
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	send := func(id int, method string, params any) error {
		b, _ := json.Marshal(map[string]any{"jsonrpc":"2.0","id":id,"method":method,"params":params})
		_, err := stdin.Write(append(b, '\n'))
		return err
	}
	if err := send(1, "initialize", map[string]any{"protocolVersion":"2024-11-05","capabilities":map[string]any{},"clientInfo":map[string]any{"name":"test","version":"0"}}); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var initResp struct { Result struct { ServerInfo struct{ Name string } `json:"serverInfo"` } `json:"result"` }
	for scanner.Scan() {
		if err := json.Unmarshal(scanner.Bytes(), &initResp); err == nil && initResp.Result.ServerInfo.Name != "" {
			break
		}
	}
	if err := send(2, "tools/list", map[string]any{}); err != nil { return nil, err }
	var listResp struct { Result struct { Tools []struct{ Name string } `json:"tools"` } `json:"result"` }
	for scanner.Scan() {
		if err := json.Unmarshal(scanner.Bytes(), &listResp); err == nil && len(listResp.Result.Tools) > 0 {
			break
		}
	}
	names := make([]string, 0, len(listResp.Result.Tools))
	for _, t := range listResp.Result.Tools { names = append(names, t.Name) }
	return &upstreamProbe{serverName: initResp.Result.ServerInfo.Name, toolNames: names}, nil
}
```

Add imports: `"bufio"`, `"context"`, `"os/exec"`, `"time"`.

- [ ] **Step 5: Run tests**

```bash
go test ./internal/api/ -run TestToolCatalog -v
```

Expected: first three tests PASS; golden test skips (or passes) depending on local PATH.

- [ ] **Step 6: Commit**

```bash
git add internal/api/tool_catalog.go internal/api/tool_catalog_test.go
git commit -m "$(cat <<'EOF'
feat(api): embedded tool catalog + synthetic initialize/tools-list

Tool schemas for mcp-language-server + gopls-mcp backends are frozen
in code with a CatalogVersion stamp. SyntheticInitializeResponse and
SyntheticToolsListResponse produce RFC-shaped JSON-RPC bodies the
lazy proxy hands back without contacting the heavy backend. Golden
test spawns upstream (when on PATH) and fails if the static catalog
drifts, instructing the maintainer to bump CatalogVersion.
EOF
)"
```

---

### Task 7: `BackendLifecycle` + `MCPEndpoint` interfaces + two stdio implementations

**Delta from eager:** ENTIRELY NEW — no eager analog.

**Files:**
- Create: `internal/daemon/backend_lifecycle.go`
- Create: `internal/daemon/backend_lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/backend_lifecycle_test.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// fakeLSP is a portable stand-in for mcp-language-server: a shell command
// that echoes a canned initialize response then blocks on stdin. Used to
// validate the stdio lifecycle without requiring real LSP binaries on CI.
func fakeLSPCommand(t *testing.T) (cmd string, args []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "echo " + `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}` + " && pause"}
	}
	return "sh", []string{"-c", "echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"capabilities\":{}}}' && cat"}
}

func TestBackendLifecycle_StdioMaterializeStops(t *testing.T) {
	cmd, args := fakeLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmd, WrapperArgs: args, Workspace: t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, err := b.Materialize(ctx)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if b.Kind() != "mcp-language-server" {
		t.Errorf("Kind = %q", b.Kind())
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ep == nil {
		t.Error("endpoint nil")
	}
}

func TestBackendLifecycle_MissingBinarySurfaces(t *testing.T) {
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: "this-binary-does-not-exist-xyz-9999", Workspace: t.TempDir(),
	})
	_, err := b.Materialize(context.Background())
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !IsMissingBinaryErr(err) {
		t.Errorf("error should classify as missing-binary: %v", err)
	}
}

func TestBackendLifecycle_SendRequestAfterStopErrors(t *testing.T) {
	cmd, args := fakeLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmd, WrapperArgs: args, Workspace: t.TempDir(),
	})
	ep, err := b.Materialize(context.Background())
	if err != nil { t.Fatal(err) }
	if err := b.Stop(); err != nil { t.Fatal(err) }
	_, err = ep.SendRequest(context.Background(), &JSONRPCRequest{Method: "tools/call", ID: json.RawMessage(`1`)})
	if err == nil {
		t.Error("SendRequest after Stop must error")
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/daemon/ -run TestBackendLifecycle -v
```

- [ ] **Step 3: Implement interfaces + impls**

Create `internal/daemon/backend_lifecycle.go`:

```go
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// JSONRPCRequest is the minimal request envelope the lazy proxy forwards.
// The proxy reads the entire request body from the HTTP handler, parses
// this shape, rewrites ID if needed, and hands it to MCPEndpoint.SendRequest.
type JSONRPCRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse is the response envelope returned by MCPEndpoint.
type JSONRPCResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// MCPEndpoint is the request/response surface the lazy proxy talks to once
// materialization succeeds. Implementations own the subprocess lifetime
// and multiplex concurrent proxy calls onto the stdio channel.
type MCPEndpoint interface {
	SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error)
	Close() error
}

// BackendLifecycle is the abstraction the lazy proxy uses to spawn the heavy
// backend on first tools/call. Materialize is idempotent: a second call on
// an already-materialized instance MAY return the existing endpoint or
// return an error (impls pick). The lazy proxy's singleflight gate ensures
// only one concurrent caller reaches Materialize for a fresh instance.
type BackendLifecycle interface {
	// Kind identifies the backend flavor for telemetry and routing. One of
	// "mcp-language-server" | "gopls-mcp".
	Kind() string
	// Materialize spawns the subprocess, performs the JSON-RPC initialize
	// handshake against it, and returns a ready MCPEndpoint. ctx bounds
	// startup; a ctx-derived timeout is the caller's responsibility.
	Materialize(ctx context.Context) (MCPEndpoint, error)
	// Stop terminates the subprocess and all derived resources. Safe to
	// call multiple times; safe to call before Materialize.
	Stop() error
}

// errMissingBinary is the sentinel the lazy proxy inspects via
// IsMissingBinaryErr to decide between LifecycleMissing and LifecycleFailed.
var errMissingBinary = errors.New("missing binary")

// IsMissingBinaryErr reports whether err resulted from exec.LookPath
// failing on the wrapper or LSP binary. Used by the lazy proxy's state
// classifier.
func IsMissingBinaryErr(err error) bool {
	return err != nil && errors.Is(err, errMissingBinary)
}

// --- mcp-language-server stdio impl ------------------------------------------

type McpLanguageServerStdioConfig struct {
	WrapperCommand string   // "mcp-language-server"
	WrapperArgs    []string // fully pre-composed, including --workspace / --lsp / flags
	Workspace      string
	Language       string
	LogPath        string
}

type mcpLanguageServerStdio struct {
	cfg  McpLanguageServerStdioConfig
	mu   sync.Mutex
	host *StdioHost
	done atomic.Bool
}

func NewMcpLanguageServerStdio(cfg McpLanguageServerStdioConfig) BackendLifecycle {
	return &mcpLanguageServerStdio{cfg: cfg}
}

func (b *mcpLanguageServerStdio) Kind() string { return "mcp-language-server" }

func (b *mcpLanguageServerStdio) Materialize(ctx context.Context) (MCPEndpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host != nil {
		return &stdioHostEndpoint{host: b.host}, nil
	}
	if _, err := exec.LookPath(b.cfg.WrapperCommand); err != nil {
		return nil, fmt.Errorf("%w: %s", errMissingBinary, b.cfg.WrapperCommand)
	}
	h, err := NewStdioHost(HostConfig{
		Command: b.cfg.WrapperCommand,
		Args:    b.cfg.WrapperArgs,
		LogPath: b.cfg.LogPath,
	})
	if err != nil {
		return nil, err
	}
	if err := h.Start(ctx); err != nil {
		return nil, wrapInitErr(err)
	}
	b.host = h
	return &stdioHostEndpoint{host: h}, nil
}

func (b *mcpLanguageServerStdio) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host == nil || b.done.Load() {
		return nil
	}
	err := b.host.Stop()
	b.done.Store(true)
	return err
}

// --- gopls-mcp stdio impl (v1) -----------------------------------------------

type GoplsMCPStdioConfig struct {
	WrapperCommand string // "gopls"
	ExtraArgs      []string // usually ["mcp"]
	Workspace      string
	LogPath        string
}

type goplsMCPStdio struct {
	cfg  GoplsMCPStdioConfig
	mu   sync.Mutex
	host *StdioHost
}

func NewGoplsMCPStdio(cfg GoplsMCPStdioConfig) BackendLifecycle {
	return &goplsMCPStdio{cfg: cfg}
}

func (b *goplsMCPStdio) Kind() string { return "gopls-mcp" }

func (b *goplsMCPStdio) Materialize(ctx context.Context) (MCPEndpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host != nil {
		return &stdioHostEndpoint{host: b.host}, nil
	}
	if _, err := exec.LookPath(b.cfg.WrapperCommand); err != nil {
		return nil, fmt.Errorf("%w: %s", errMissingBinary, b.cfg.WrapperCommand)
	}
	args := append([]string(nil), b.cfg.ExtraArgs...)
	if len(args) == 0 {
		args = []string{"mcp"}
	}
	h, err := NewStdioHost(HostConfig{
		Command: b.cfg.WrapperCommand, Args: args,
		WorkingDir: b.cfg.Workspace,
		LogPath:    b.cfg.LogPath,
	})
	if err != nil {
		return nil, err
	}
	if err := h.Start(ctx); err != nil {
		return nil, wrapInitErr(err)
	}
	b.host = h
	return &stdioHostEndpoint{host: h}, nil
}

func (b *goplsMCPStdio) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host == nil {
		return nil
	}
	return b.host.Stop()
}

// --- endpoint adapter --------------------------------------------------------

type stdioHostEndpoint struct {
	host   *StdioHost
	closed atomic.Bool
}

func (e *stdioHostEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.closed.Load() {
		return nil, errors.New("endpoint closed")
	}
	body, err := json.Marshal(req)
	if err != nil { return nil, err }
	raw, err := e.host.SendRPC(ctx, body)
	if err != nil { return nil, err }
	var resp JSONRPCResponse
	if err := json.Unmarshal(raw, &resp); err != nil { return nil, err }
	return &resp, nil
}

func (e *stdioHostEndpoint) Close() error {
	e.closed.Store(true)
	return nil
}

// --- helpers ---------------------------------------------------------------

// wrapInitErr preserves the concrete error but annotates it so the lazy
// proxy can distinguish startup handshake failures from missing-binary
// failures. Any error here becomes LifecycleFailed (not Missing) since
// the binary WAS found.
func wrapInitErr(err error) error {
	if err == nil { return nil }
	// Trim extremely long bufio scanner errors.
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	_ = bufio.NewScanner // keep import stable
	_ = io.EOF           // keep import stable
	_ = strings.TrimSpace // keep import stable
	_ = time.Second      // keep import stable
	return fmt.Errorf("backend init: %s", msg)
}
```

Note: `SendRPC` is a method to add to `StdioHost` if it doesn't exist already. It takes a raw JSON-RPC frame (id already rewritten appropriately) and returns the matching response frame. Check `internal/daemon/host.go` for existing helpers before naming; if a different name is already used for the request/response multiplex, reuse it.

- [ ] **Step 4: Run tests**

```bash
go test ./internal/daemon/ -run TestBackendLifecycle -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/backend_lifecycle.go internal/daemon/backend_lifecycle_test.go
git commit -m "$(cat <<'EOF'
feat(daemon): BackendLifecycle interface + two stdio implementations

BackendLifecycle abstracts heavy-backend materialization behind
Materialize/Stop/Kind. Two impls: mcpLanguageServerStdio wraps the
upstream --workspace --lsp binary; goplsMCPStdio wraps `gopls mcp`.
Both reuse daemon.NewStdioHost + ProtocolBridge from Phase 2.
IsMissingBinaryErr classifies exec.LookPath failures for the lazy
proxy's 5-state status model.
EOF
)"
```

---

### Task 8: Inflight gate — singleflight + retry throttle

**Delta from eager:** ENTIRELY NEW.

**Files:**
- Create: `internal/daemon/inflight.go`
- Create: `internal/daemon/inflight_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/inflight_test.go`:

```go
package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInflight_FirstCallMaterializes(t *testing.T) {
	var calls atomic.Int32
	g := NewInflightGate(10 * time.Millisecond)
	fn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		return "ep", nil
	}
	got, err := g.Do(context.Background(), "k1", fn)
	if err != nil { t.Fatal(err) }
	if got.(string) != "ep" || calls.Load() != 1 {
		t.Errorf("Do returned %v, calls=%d", got, calls.Load())
	}
}

func TestInflight_ConcurrentCallsShareOne(t *testing.T) {
	var calls atomic.Int32
	g := NewInflightGate(10 * time.Millisecond)
	fn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return "ep", nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.Do(context.Background(), "k1", fn)
			if err != nil { t.Error(err) }
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 fn call under singleflight, got %d", calls.Load())
	}
}

func TestInflight_FailureReturnsError(t *testing.T) {
	g := NewInflightGate(10 * time.Millisecond)
	boom := errors.New("boom")
	fn := func(ctx context.Context) (any, error) { return nil, boom }
	_, err := g.Do(context.Background(), "k1", fn)
	if !errors.Is(err, boom) { t.Errorf("err = %v, want boom", err) }
}

func TestInflight_RetryThrottleHonored(t *testing.T) {
	g := NewInflightGate(50 * time.Millisecond)
	boom := errors.New("boom")
	callsFn := func(ctx context.Context) (any, error) { return nil, boom }
	// First call fails.
	if _, err := g.Do(context.Background(), "k1", callsFn); !errors.Is(err, boom) { t.Fatal(err) }
	// Immediate retry — must return the cached error WITHOUT calling fn.
	var calls atomic.Int32
	noFn := func(ctx context.Context) (any, error) {
		calls.Add(1)
		return nil, errors.New("should not run")
	}
	_, err := g.Do(context.Background(), "k1", noFn)
	if err == nil { t.Fatal("expected cached error, got nil") }
	if calls.Load() != 0 { t.Errorf("throttle breached: fn called %d times", calls.Load()) }
	// After the throttle window elapses, fn runs again.
	time.Sleep(80 * time.Millisecond)
	calls.Store(0)
	_, err = g.Do(context.Background(), "k1", noFn)
	if err == nil { t.Fatal("expected new error after throttle") }
	if calls.Load() != 1 { t.Errorf("expected 1 fn call after throttle, got %d", calls.Load()) }
}

func TestInflight_SuccessResetsThrottle(t *testing.T) {
	g := NewInflightGate(50 * time.Millisecond)
	// Fail once.
	g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) { return nil, errors.New("x") })
	// Sleep past throttle, then succeed.
	time.Sleep(80 * time.Millisecond)
	g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) { return "ok", nil })
	// Immediately after success, next call must run (no throttle).
	var ran atomic.Int32
	g.Do(context.Background(), "k1", func(ctx context.Context) (any, error) {
		ran.Add(1); return "ok2", nil
	})
	if ran.Load() != 1 { t.Errorf("throttle leaked across success: ran = %d", ran.Load()) }
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/daemon/ -run TestInflight -v
```

- [ ] **Step 3: Implement**

Create `internal/daemon/inflight.go`:

```go
package daemon

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// InflightGate is the lazy-proxy's per-(workspace,language) concurrency
// control: singleflight collapses concurrent first-callers into one
// Materialize call; a per-key failure throttle rejects retries fired
// within minRetryGap of the last failure by returning the cached error
// immediately (without calling fn).
//
// On success, the throttle state for that key is cleared so the next
// Do call runs normally.
type InflightGate struct {
	sf          singleflight.Group
	minRetryGap time.Duration

	mu          sync.Mutex
	lastFailure map[string]failureEntry
}

type failureEntry struct {
	at  time.Time
	err error
}

// NewInflightGate returns a gate with minRetryGap as the minimum gap between
// failed attempts per key. Must be >= 0; zero disables throttling.
func NewInflightGate(minRetryGap time.Duration) *InflightGate {
	if minRetryGap < 0 {
		minRetryGap = 0
	}
	return &InflightGate{
		minRetryGap: minRetryGap,
		lastFailure: map[string]failureEntry{},
	}
}

// Do runs fn exactly once per (in-flight key) and returns its result to all
// concurrent callers. After a failure, further Do calls within minRetryGap
// return the cached error without invoking fn. A successful Do clears the
// failure state for key.
func (g *InflightGate) Do(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	// Fast-path throttle check.
	g.mu.Lock()
	if fe, ok := g.lastFailure[key]; ok {
		if time.Since(fe.at) < g.minRetryGap {
			g.mu.Unlock()
			return nil, fe.err
		}
	}
	g.mu.Unlock()

	v, err, _ := g.sf.Do(key, func() (any, error) {
		res, err := fn(ctx)
		g.mu.Lock()
		defer g.mu.Unlock()
		if err != nil {
			g.lastFailure[key] = failureEntry{at: time.Now(), err: err}
		} else {
			delete(g.lastFailure, key)
		}
		return res, err
	})
	return v, err
}

// Forget drops all inflight + throttle state for key. Used by the lazy proxy
// when the materialized endpoint is explicitly closed (e.g. shutdown), so
// a subsequent restart isn't accidentally throttled.
func (g *InflightGate) Forget(key string) {
	g.sf.Forget(key)
	g.mu.Lock()
	delete(g.lastFailure, key)
	g.mu.Unlock()
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/daemon/ -run TestInflight -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/inflight.go internal/daemon/inflight_test.go
git commit -m "$(cat <<'EOF'
feat(daemon): inflight gate with singleflight + retry throttle

InflightGate collapses concurrent first-callers to one Materialize
via golang.org/x/sync/singleflight and rejects retries within
minRetryGap of the last failure by returning the cached error
immediately (no fn invocation). Success clears the failure state
so the next call runs normally. Used by the lazy proxy to guard
first-call materialization per (workspaceKey, language).
EOF
)"
```

---

### Task 9: Lazy proxy — HTTP listener + synthetic handshake + materialize-on-first-tools-call

**Delta from eager:** ENTIRELY NEW; this is the core lazy runtime.

**Files:**
- Create: `internal/daemon/lazy_proxy.go`
- Create: `internal/daemon/lazy_proxy_test.go`
- Create: `internal/cli/daemon_workspace.go` (cobra subcommand that launches the proxy)

- [ ] **Step 1: Write failing tests**

Create `internal/daemon/lazy_proxy_test.go`. Test cases:
- `TestLazyProxy_InitializeSyntheticNoMaterialize` — POST initialize → 200 with synthetic body; backend.Materialize was not called.
- `TestLazyProxy_ToolsListSyntheticNoMaterialize` — POST tools/list → synthetic body.
- `TestLazyProxy_ToolsCallMaterializesOnce` — POST tools/call → 200; backend.Materialize called exactly once across N concurrent requests.
- `TestLazyProxy_RepeatedInitializeStillSynthetic` — initialize → tools/call → initialize again → third call still answered synthetically (initialize never forwards to materialized backend).
- `TestLazyProxy_MissingBinaryYieldsMissingState` — stub lifecycle returns `errMissingBinary`; registry gets `LifecycleMissing`; HTTP response is a JSON-RPC error.
- `TestLazyProxy_OtherFailureYieldsFailedState` — stub lifecycle returns a random error; registry gets `LifecycleFailed`.
- `TestLazyProxy_ThrottledRetryReturnsCachedError` — two consecutive failed calls within throttle window; backend called once, second gets cached error.

Skeleton:

```go
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLifecycle struct {
	kind           string
	materializeErr error
	materializeCount atomic.Int32
	sendRequestErr error
	sendResult     json.RawMessage
	stopCalls      atomic.Int32
}

func (f *fakeLifecycle) Kind() string { return f.kind }
func (f *fakeLifecycle) Materialize(ctx context.Context) (MCPEndpoint, error) {
	f.materializeCount.Add(1)
	if f.materializeErr != nil { return nil, f.materializeErr }
	return &fakeEndpoint{parent: f}, nil
}
func (f *fakeLifecycle) Stop() error { f.stopCalls.Add(1); return nil }

type fakeEndpoint struct{ parent *fakeLifecycle }
func (e *fakeEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.parent.sendRequestErr != nil { return nil, e.parent.sendRequestErr }
	res := e.parent.sendResult
	if len(res) == 0 { res = json.RawMessage(`{"ok":true}`) }
	return &JSONRPCResponse{Jsonrpc:"2.0", ID: req.ID, Result: res}, nil
}
func (e *fakeEndpoint) Close() error { return nil }

// Implementation details omitted; each test spins up a lazy proxy with a
// fakeLifecycle + in-memory registry path, fires HTTP requests at its
// Handler(), and asserts the observed behavior.
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/daemon/ -run TestLazyProxy -v
```

- [ ] **Step 3: Implement `lazy_proxy.go`**

Create `internal/daemon/lazy_proxy.go`:

```go
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

// LazyProxyConfig describes one lazy-proxy instance.
type LazyProxyConfig struct {
	WorkspaceKey  string
	WorkspacePath string
	Language      string
	BackendKind   string   // "mcp-language-server" | "gopls-mcp"
	Port          int
	Lifecycle     BackendLifecycle
	RegistryPath  string

	// InflightMinRetryGap defaults to 2 seconds when zero.
	InflightMinRetryGap time.Duration
	// ToolsCallDebounce defaults to 5 seconds when zero.
	ToolsCallDebounce time.Duration
}

// LazyProxy is the per-port HTTP proxy that answers synthetic handshake
// traffic from the static tool catalog and lazily materializes the backend
// on first tools/call. One proxy per (workspace, language); the process
// lifetime is bounded by Stop().
type LazyProxy struct {
	cfg    LazyProxyConfig
	gate   *InflightGate
	server *http.Server

	mu       sync.Mutex
	endpoint MCPEndpoint
	closed   atomic.Bool

	debounceMu        sync.Mutex
	lastToolsCallWrite time.Time
}

func NewLazyProxy(cfg LazyProxyConfig) *LazyProxy {
	if cfg.InflightMinRetryGap == 0 {
		cfg.InflightMinRetryGap = 2 * time.Second
	}
	if cfg.ToolsCallDebounce == 0 {
		cfg.ToolsCallDebounce = 5 * time.Second
	}
	return &LazyProxy{
		cfg:  cfg,
		gate: NewInflightGate(cfg.InflightMinRetryGap),
	}
}

// Handler returns the http.Handler for the proxy. Exposed for tests so they
// can fire requests via httptest.NewServer without real port binding.
func (p *LazyProxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", p.handleMCP)
	mux.HandleFunc("/", p.handleMCP) // accept both /mcp and / for compatibility
	return mux
}

// ListenAndServe binds cfg.Port and serves until Stop is called.
func (p *LazyProxy) ListenAndServe() error {
	// Write initial state to registry.
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleConfigured, "")
	p.server = &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", p.cfg.Port),
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return p.server.ListenAndServe()
}

// Stop closes the materialized endpoint (if any), invokes Lifecycle.Stop()
// to tree-kill the backend subprocess, and shuts down the HTTP listener.
func (p *LazyProxy) Stop(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) { return nil }
	p.mu.Lock()
	if p.endpoint != nil { _ = p.endpoint.Close() }
	p.mu.Unlock()
	_ = p.cfg.Lifecycle.Stop()
	p.gate.Forget(p.cfg.WorkspaceKey + "|" + p.cfg.Language)
	if p.server != nil {
		return p.server.Shutdown(ctx)
	}
	return nil
}

// handleMCP is the per-request dispatch. JSON-RPC over POST. A GET opens
// an SSE stream that reuses the Phase 2 bridge machinery when a backend is
// materialized (or returns 204 No Content if not yet materialized).
func (p *LazyProxy) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p.handleSSE(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		writeRPCError(w, nil, -32700, "read body: "+err.Error())
		return
	}
	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	switch req.Method {
	case "initialize":
		resp, err := api.SyntheticInitializeResponse(req.ID, p.cfg.BackendKind)
		if err != nil { writeRPCError(w, req.ID, -32603, err.Error()); return }
		writeJSON(w, resp)
	case "tools/list":
		resp, err := api.SyntheticToolsListResponse(req.ID, p.cfg.BackendKind)
		if err != nil { writeRPCError(w, req.ID, -32603, err.Error()); return }
		writeJSON(w, resp)
	case "notifications/initialized", "ping":
		// Synthetic ack — no backend.
		writeJSON(w, []byte(`{"jsonrpc":"2.0","id":null,"result":{}}`))
	case "tools/call":
		p.handleToolsCall(w, r, &req)
	default:
		// Forward any other request (resources, prompts) to the materialized
		// backend when one exists. If not yet materialized, attempt to
		// materialize; tool-call-like semantics.
		p.handleForward(w, r, &req)
	}
}

func (p *LazyProxy) handleToolsCall(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	ep, err := p.ensureMaterialized(r.Context())
	if err != nil {
		code := -32603
		if IsMissingBinaryErr(err) { code = -32000 /* missing-binary application error */ }
		writeRPCError(w, req.ID, code, err.Error())
		return
	}
	p.debounceWriteToolsCallTimestamp()
	resp, err := ep.SendRequest(r.Context(), req)
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	out, _ := json.Marshal(resp)
	writeJSON(w, out)
}

func (p *LazyProxy) handleForward(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	ep, err := p.ensureMaterialized(r.Context())
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	resp, err := ep.SendRequest(r.Context(), req)
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	out, _ := json.Marshal(resp)
	writeJSON(w, out)
}

func (p *LazyProxy) handleSSE(w http.ResponseWriter, r *http.Request) {
	// v1 minimal: if not materialized, return 204. Once materialized, delegate
	// to the endpoint's SSE path via an adapter on StdioHost.
	p.mu.Lock()
	ep := p.endpoint
	p.mu.Unlock()
	if ep == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Defer to the bridge's SSE writer. Implementation reuses Phase 2 code;
	// the exact hook depends on StdioHost/HTTPHost surface.
	// For v1, write an empty event stream with a keepalive comment.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	fmt.Fprint(w, ": keepalive\n\n")
	if flusher != nil { flusher.Flush() }
	<-r.Context().Done()
}

// ensureMaterialized either returns the cached endpoint or materializes one
// via the inflight gate. Classifies the error to pick Missing vs Failed.
func (p *LazyProxy) ensureMaterialized(ctx context.Context) (MCPEndpoint, error) {
	p.mu.Lock()
	if p.endpoint != nil {
		ep := p.endpoint
		p.mu.Unlock()
		return ep, nil
	}
	p.mu.Unlock()
	key := p.cfg.WorkspaceKey + "|" + p.cfg.Language
	// Mark Starting in the registry.
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleStarting, "")
	v, err := p.gate.Do(ctx, key, func(ctx context.Context) (any, error) {
		return p.cfg.Lifecycle.Materialize(ctx)
	})
	if err != nil {
		state := api.LifecycleFailed
		if IsMissingBinaryErr(err) { state = api.LifecycleMissing }
		_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycle(p.cfg.WorkspaceKey, p.cfg.Language, state, err.Error())
		return nil, err
	}
	ep := v.(MCPEndpoint)
	p.mu.Lock()
	p.endpoint = ep
	p.mu.Unlock()
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycleWithTimestamps(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleActive, "",
		time.Now().UTC(), time.Time{},
	)
	return ep, nil
}

// debounceWriteToolsCallTimestamp coalesces registry writes for the
// LastToolsCallAt field. Only writes when the previous write was >=
// ToolsCallDebounce ago.
func (p *LazyProxy) debounceWriteToolsCallTimestamp() {
	p.debounceMu.Lock()
	now := time.Now()
	due := now.Sub(p.lastToolsCallWrite) >= p.cfg.ToolsCallDebounce
	if due { p.lastToolsCallWrite = now }
	p.debounceMu.Unlock()
	if !due { return }
	_ = api.NewRegistry(p.cfg.RegistryPath).PutLifecycleWithTimestamps(
		p.cfg.WorkspaceKey, p.cfg.Language, api.LifecycleActive, "",
		time.Time{}, now.UTC(),
	)
}

// --- helpers ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	body := map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors use 200 by convention
	_, _ = w.Write(b)
}

// Keep import surface stable.
var _ = bytes.NewBuffer
var _ = io.EOF
var _ = errors.New
```

- [ ] **Step 4: Fill in the test implementations matching the cases enumerated in Step 1**

For each case, construct a `LazyProxy` with a `fakeLifecycle`, point `RegistryPath` at `t.TempDir()+"/r.yaml"`, fire requests via `httptest.NewRecorder()` against `proxy.Handler()`, assert response body and `fakeLifecycle.materializeCount`.

- [ ] **Step 5: Run tests**

```bash
go test ./internal/daemon/ -run TestLazyProxy -v
```

- [ ] **Step 6: Wire the CLI subcommand `daemon workspace-proxy`**

Create `internal/cli/daemon_workspace.go`:

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/servers"

	"github.com/spf13/cobra"
)

func newDaemonWorkspaceProxyCmd() *cobra.Command {
	var workspaceFlag, languageFlag string
	c := &cobra.Command{
		Use:    "workspace-proxy",
		Short:  "Launch the lazy proxy for a workspace-scoped (workspace, language) pair",
		Hidden: true, // invoked by the scheduler task; users don't run this directly
		RunE: func(cmd *cobra.Command, args []string) error {
			if workspaceFlag == "" || languageFlag == "" {
				return fmt.Errorf("--workspace and --language are required")
			}
			canonical, err := api.CanonicalWorkspacePath(workspaceFlag)
			if err != nil { return err }
			wsKey := api.WorkspaceKey(canonical)
			regPath, err := api.DefaultRegistryPath()
			if err != nil { return err }
			reg := api.NewRegistry(regPath)
			if err := reg.Load(); err != nil { return err }
			entry, ok := reg.Get(wsKey, languageFlag)
			if !ok {
				return fmt.Errorf("not registered: workspace %s language %s", canonical, languageFlag)
			}
			f, err := servers.Manifests.Open("mcp-language-server/manifest.yaml")
			if err != nil { return err }
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil { return err }
			var spec config.LanguageSpec
			for _, l := range m.Languages { if l.Name == languageFlag { spec = l; break } }
			if spec.Name == "" { return fmt.Errorf("manifest lacks language %q", languageFlag) }
			logPath := filepath.Join(logBaseDir(), fmt.Sprintf("lsp-%s-%s.log", wsKey, languageFlag))
			lc := buildBackendLifecycle(spec, canonical, logPath)
			proxy := daemon.NewLazyProxy(daemon.LazyProxyConfig{
				WorkspaceKey:  wsKey,
				WorkspacePath: canonical,
				Language:      languageFlag,
				BackendKind:   spec.Backend,
				Port:          entry.Port,
				Lifecycle:     lc,
				RegistryPath:  regPath,
			})
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			go func() { <-ctx.Done(); shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second); defer sc(); _ = proxy.Stop(shutdownCtx) }()
			return proxy.ListenAndServe()
		},
	}
	c.Flags().StringVar(&workspaceFlag, "workspace", "", "workspace path")
	c.Flags().StringVar(&languageFlag, "language", "", "language name (must match manifest entry)")
	return c
}

func buildBackendLifecycle(spec config.LanguageSpec, canonicalWorkspace, logPath string) daemon.BackendLifecycle {
	switch spec.Backend {
	case "gopls-mcp":
		extra := spec.ExtraFlags
		if len(extra) == 0 { extra = []string{"mcp"} }
		return daemon.NewGoplsMCPStdio(daemon.GoplsMCPStdioConfig{
			WrapperCommand: spec.LspCommand, ExtraArgs: extra,
			Workspace: canonicalWorkspace, LogPath: logPath,
		})
	default:
		args := []string{"--workspace", canonicalWorkspace, "--lsp", spec.LspCommand}
		if len(spec.ExtraFlags) > 0 {
			args = append(args, "--")
			args = append(args, spec.ExtraFlags...)
		}
		return daemon.NewMcpLanguageServerStdio(daemon.McpLanguageServerStdioConfig{
			WrapperCommand: "mcp-language-server", WrapperArgs: args,
			Workspace: canonicalWorkspace, Language: spec.Name, LogPath: logPath,
		})
	}
}
```

In `internal/cli/daemon.go`, in `newDaemonCmdReal()`, after the existing command is constructed add `c.AddCommand(newDaemonWorkspaceProxyCmd())` before `return c`.

Ensure `logBaseDir()` exists as a helper (already used for Phase 2 daemons); if absent, factor it out of existing daemon code.

Return to `internal/cli/root.go` if a new toplevel command is needed; it's not — `workspace-proxy` is a subcommand of `daemon`.

- [ ] **Step 7: Run all daemon tests**

```bash
go test ./internal/daemon/ -v
go build ./...
```

- [ ] **Step 8: Commit**

```bash
git add internal/daemon/lazy_proxy.go internal/daemon/lazy_proxy_test.go internal/cli/daemon_workspace.go internal/cli/daemon.go
git commit -m "$(cat <<'EOF'
feat(daemon,cli): lazy proxy + `daemon workspace-proxy` subcommand

LazyProxy listens on the registry-assigned port, answers initialize
and tools/list synthetically from the embedded tool catalog, and
materializes the heavy backend on first tools/call via the inflight
gate. Registry lifecycle transitions {configured -> starting ->
active|missing|failed} are written on state change; last_tools_call_at
is debounced to 5s. `mcphub daemon workspace-proxy --workspace <p>
--language <l>` is the scheduler-task entry point.
EOF
)"
```

---

## M3 — Register + Unregister + CLI

### Task 10: `Register` — lazy-mode orchestrator with per-language rollback

**Delta from eager (eager Task 7):**
- No LSP binary preflight at register time (decision #1 override). Only preflight the wrapper binary (`mcp-language-server`) AND `mcphub` itself. For `gopls-mcp`, preflight `gopls`.
- Default all-languages when `languages` slice is empty (lazy decision #1).
- Scheduler task command changes: `mcphub daemon workspace-proxy --workspace <p> --language <l>`, not `mcphub daemon --workspace <p> --language <l>`. The proxy CLI subcommand is the new invariant.
- No backend materialization at register time.
- Each registered entry starts with `Lifecycle: LifecycleConfigured`.

**Files:**
- Create: `internal/api/register.go` (full file; combines eager Task 6 helpers + Task 7 + Task 8 Unregister)
- Create: `internal/api/register_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/api/register_test.go`. Reuse the structure of eager plan Tasks 6, 7, 8 tests with these adjustments:

- `TestRegister_FullSuccessLazyMode` — invoke `Register(ws, []string{"python","typescript"}, opts)`; assert registry has 2 entries BOTH in `LifecycleConfigured` state; scheduler `Create` was called twice with args `["daemon","workspace-proxy","--workspace",ws,"--language",lang]`.
- `TestRegister_DefaultAllLanguagesWhenEmpty` — invoke `Register(ws, nil, opts)` with a 3-language manifest; assert 3 entries created.
- `TestRegister_NoLspBinaryPreflightAtRegister` — invoke with a language whose `lsp_command` is definitely missing; assert register SUCCEEDS (no preflight) and entry is `LifecycleConfigured`. The missing-binary state only materializes later at tools/call time (covered by the lazy proxy tests).
- `TestRegister_WrapperBinaryPreflightStillEnforced` — swap out the wrapper binary check via a test hook and make it fail; assert register aborts before side effects.
- `TestRegister_PartialFailureRollsBack` — scheduler.Create fails on the 2nd call; registry ends empty. (Simpler than eager — no backends to also kill.)
- `TestRegister_IdempotentReRegisterPreservesPort` — re-register the same (ws, lang); assert port and entry name unchanged.
- `TestUnregister_FullRemovesAllLanguages`, `TestUnregister_PartialKeepsOthers`, `TestUnregister_UnknownWorkspaceErrors` — shapes from eager Task 8.

Use the `newRegisterHarness` test seam pattern from eager Task 7 (fake scheduler + fake clients + temp registry path).

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Create `internal/api/register.go`. Include:

```go
package api

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
	"mcp-local-hub/servers"
)

type RegisterOpts struct {
	WeeklyRefresh bool
	Writer        io.Writer
}

type RegisterReport struct {
	Workspace    string           `json:"workspace"`
	WorkspaceKey string           `json:"workspace_key"`
	Entries      []WorkspaceEntry `json:"entries"`
}

type UnregisterReport struct {
	Workspace    string   `json:"workspace"`
	WorkspaceKey string   `json:"workspace_key"`
	Removed      []string `json:"removed"`
	Warnings     []string `json:"warnings,omitempty"`
}

// Register ensures workspace-scoped lazy proxies exist for each requested
// language in workspacePath. An empty languages slice defaults to every
// language declared in the manifest.
//
// Lazy mode: this function DOES NOT preflight LSP binaries. Missing LSP
// binaries are surfaced later at first tools/call via LifecycleMissing.
// The wrapper binary (mcp-language-server for 8 langs; gopls for go) is
// NOT preflighted either in v1 — the failure surfaces on first call with
// a clear error message. Rationale: eager preflight contradicts the
// lazy contract and forces UX friction at register time for users who
// only use some languages.
//
// Side effects per language (rolled back on later failure):
//   1. Allocate port from registry.
//   2. Create scheduler task whose command is `mcphub daemon
//      workspace-proxy --workspace <ws> --language <lang>`.
//   3. Write managed entries into each client config.
// Registry is saved once at the end.
func (a *API) Register(workspacePath string, languages []string, opts RegisterOpts) (*RegisterReport, error) {
	data, err := loadManifestYAMLEmbedFirst("mcp-language-server")
	if err != nil {
		return nil, fmt.Errorf("load manifest mcp-language-server: %w", err)
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil { return nil, err }
	// Default all-languages when empty.
	if len(languages) == 0 {
		for _, l := range m.Languages { languages = append(languages, l.Name) }
	}
	return a.registerWithManifest(m, workspacePath, languages, opts)
}
```

Reuse helpers from eager Task 6 (`ResolveEntryName`) and from eager Task 7 (`registerWithManifest`, `registerOneLanguage`, test seams `testSchedulerFactory`, `testClientFactory`, `testRegistryPathOverride`, `realSchedulerAdapter`, `fakeScheduler`, `fakeClient`).

In `registerOneLanguage`, replace scheduler task args with:

```go
args := []string{"daemon", "workspace-proxy", "--workspace", canonical, "--language", lang}
```

And the new entry initialization:

```go
return WorkspaceEntry{
	WorkspaceKey:  wsKey,
	WorkspacePath: canonical,
	Language:      lang,
	Backend:       spec.Backend,
	Port:          port,
	TaskName:      taskName,
	ClientEntries: entryNameByClient,
	WeeklyRefresh: opts.WeeklyRefresh,
	Lifecycle:     LifecycleConfigured, // lazy-mode: proxy running, backend NOT spawned
}, nil
```

Drop the eager `PreflightLanguages` helper (not used in v1); keep `MissingBinary` type for the migration path's "which languages have which wrappers" diagnostic if migration chooses to preflight. Keep `ResolveEntryName` verbatim from eager Task 6.

For `Unregister`, reuse eager Task 8 verbatim.

- [ ] **Step 4: Run tests + build**

```bash
go test ./internal/api/ -run "TestRegister_|TestUnregister_|TestResolveEntryName" -v
go build ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/api/register.go internal/api/register_test.go
git commit -m "$(cat <<'EOF'
feat(api): Register/Unregister with lazy-mode semantics

Register allocates port + creates scheduler task pointing at `mcphub
daemon workspace-proxy` + writes client entries. NO LSP binary
preflight at register time — missing-binary surfaces at first
tools/call via LifecycleMissing. Empty languages slice defaults to
all 9 manifest languages. Partial failure rolls back. Unregister
supports full + partial teardown; process-tree-kill on the proxy
sweeps the materialized backend.
EOF
)"
```

---

### Task 11: CLI — `mcphub register`, `unregister`, `workspaces` with 5-state output

**Delta from eager (eager Task 9):**
- `register` accepts 0 positional language args (defaults to all).
- `workspaces` output adds columns: `LIFECYCLE`, `LAST_USED` (derived from `LastToolsCallAt`), `LAST_ERROR` (truncated to 40 chars inline).
- JSON output uses the extended `WorkspaceEntry` verbatim.

**Files:**
- Create: `internal/cli/register.go`
- Create: `internal/cli/register_test.go`
- Modify: `internal/cli/root.go` (wire 3 commands)

- [ ] **Step 1: Write failing tests**

Create `internal/cli/register_test.go`. Adapt eager Task 9 tests:

- `TestRegisterCmd_AcceptsOnlyWorkspaceArgDefaultsAll` — `register <ws>` with no language args must not error with "requires 2+ args".
- `TestRegisterCmd_ExplicitLanguagesStillAccepted` — `register <ws> python typescript` parses the slice correctly.
- `TestWorkspacesCmd_EmptyRegistryPrintsHeader` — header contains `LIFECYCLE` + `LAST_USED`.
- `TestWorkspacesCmd_PopulatedPrintsLifecycleColumn` — seed registry with entries in different states; assert each row shows the state.
- `TestWorkspacesCmd_JSONOutput` — array of `WorkspaceEntry` with extended fields.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Start from the eager Task 9 Step 3 code. Change:

```go
Args: cobra.MinimumNArgs(2), → Args: cobra.MinimumNArgs(1),
```

And in `RunE`:

```go
workspace := args[0]
var languages []string
if len(args) > 1 { languages = args[1:] }
// nil = default-all semantics inside API.Register
```

In `newWorkspacesCmdReal` update the table header:

```go
fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-12s %-6s %-20s %-11s %-19s %s\n",
	"WORKSPACE", "LANG", "PORT", "BACKEND", "LIFECYCLE", "LAST_USED", "PATH")
for _, e := range entries {
	last := "-"
	if !e.LastToolsCallAt.IsZero() { last = e.LastToolsCallAt.Format("2006-01-02 15:04:05") }
	fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-12s %-6d %-20s %-11s %-19s %s\n",
		e.WorkspaceKey, e.Language, e.Port, e.Backend, stateOrDash(e.Lifecycle), last, e.WorkspacePath)
}
```

`stateOrDash` returns `"-"` when empty, else the state as-is.

Wire into `root.go` exactly like eager Task 9 Step 4.

- [ ] **Step 4: Run + commit**

```bash
git add internal/cli/register.go internal/cli/register_test.go internal/cli/root.go
git commit -m "$(cat <<'EOF'
feat(cli): register/unregister/workspaces with 5-state UI

`mcphub register <ws> [lang...]` defaults to all 9 languages when no
language args given. `mcphub workspaces` prints LIFECYCLE + LAST_USED
columns. `unregister` supports full + partial teardown.
EOF
)"
```

---

### Task 12: Block `install` on workspace-scoped manifests

**Delta from eager:** mostly verbatim from eager Task 14 Step 3. Included here as its own task (rather than bundled with weekly-refresh) so it is committed before E2E tests, giving early protection.

Reuse eager Task 14 Step 3's `refuseWorkspaceScopedInstall`, the insertion into `Install`, and the `InstallAll` skip block.

```bash
git commit -m "fix(api): reject workspace-scoped manifests from Install/InstallAll"
```

---

## M4 — Migration + weekly refresh

### Task 13: Weekly refresh — shared task + `WeeklyRefreshAll`

**Delta from eager:** VERBATIM from eager Task 11. The semantic that "weekly refresh restarts the proxy; re-materialization is lazy on next tools/call" follows automatically from the lazy proxy's design, no code change needed beyond what eager produced.

Reuse eager Task 11 Steps 1–6.

```bash
git commit -m "feat(api): shared weekly-refresh task + WeeklyRefreshAll runner"
```

---

### Task 14: Legacy migration — detect + migrate (per-workspace, not per-language)

**Delta from eager:** the detection code (`DetectLegacyLanguageServerEntries`, `legacy_detect.go` + tests) is VERBATIM from eager Task 12 Steps 1–4. The migration code (`MigrateLegacy`) differs:

- Eager emitted one `Register(ws, [lang], opts)` per detected legacy entry.
- Lazy emits one `Register(ws, nil, opts)` per **unique workspace** across detected entries (nil languages = default all). Rationale: once we register any workspace we get ALL languages lazily, so registering per-language would just create redundant overlapping tasks.

**Files:**
- Create: `internal/api/legacy_detect.go` + test — VERBATIM from eager Task 12.
- Create: `internal/api/legacy_migrate.go` — adapted (see below).
- Create: `internal/api/legacy_migrate_test.go` — adapted.
- Create: `internal/cli/migrate_legacy.go` — VERBATIM from eager Task 12 Step 7.

- [ ] **Step 1: Write failing tests**

Create `internal/api/legacy_migrate_test.go`. Tests:

- `TestMigrateLegacy_DryRunMakesNoChanges` — as in eager.
- `TestMigrateLegacy_DedupesByWorkspace` — seed 3 legacy entries for workspace `W1` (python, rust, ts) + 1 for workspace `W2` (go); assert `MigrateLegacy` emits exactly 2 `Register` calls (`W1, nil` and `W2, nil`).
- `TestMigrateLegacy_YesAppliesNonInteractively` — as in eager.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Adapt `MigrateLegacy`'s inner loop:

```go
// Unique workspaces discovered across legacy entries.
seen := map[string]bool{}
var workspaces []string
for _, e := range entries {
	if e.Workspace == "" { continue }
	if !seen[e.Workspace] {
		seen[e.Workspace] = true
		workspaces = append(workspaces, e.Workspace)
	}
}

for _, ws := range workspaces {
	if !opts.Yes {
		fmt.Fprintf(w, "Register all manifest languages for workspace %s? [Y/n] ", ws)
		// ... prompt read as in eager
	}
	if _, err := a.Register(ws, nil, RegisterOpts{Writer: w, WeeklyRefresh: true}); err != nil {
		// record Failed per each entry in this workspace
		for _, e := range entries {
			if e.Workspace == ws {
				report.Failed = append(report.Failed, FailedLegacyEntry{Entry: e, Err: err.Error()})
			}
		}
		continue
	}
	// After Register succeeds, delete every legacy entry for this workspace.
	for _, e := range entries {
		if e.Workspace != ws { continue }
		if adapters := clientsAllForTest(); adapters[e.Client] != nil {
			if err := adapters[e.Client].RemoveEntry(e.EntryName); err != nil {
				report.Failed = append(report.Failed, FailedLegacyEntry{Entry: e, Err: "remove legacy entry: " + err.Error()})
				continue
			}
		}
		report.Applied = append(report.Applied, e)
	}
}
```

- [ ] **Step 4: Run + commit**

```bash
git commit -m "$(cat <<'EOF'
feat(api,cli): legacy migration emits one Register per workspace

Lazy mode means one register call configures ALL languages for a
workspace. Migration now dedupes legacy entries by workspace and
emits a single Register(ws, nil, opts) per workspace — producing
fewer, simpler scheduler tasks than eager's N-per-language approach.
EOF
)"
```

---

## M5 — Integration polish

### Task 15: Status enrichment — 5 states + `last_used` + `--workspace-scoped` filter

**Delta from eager (eager Task 13):**
- `DaemonStatus` gains 4 additional fields beyond eager: `Lifecycle`, `LastMaterializedAt`, `LastToolsCallAt`, `LastError`.
- `enrichStatusWithRegistry` populates all of them.
- `mcphub status --workspace-scoped` prints `LIFECYCLE` + `LAST_USED` columns.

**Files:**
- Modify: `internal/api/types.go` — extend `DaemonStatus`.
- Modify: `internal/api/status_enrich.go` — populate the new fields.
- Modify: `internal/cli/status.go` — new columns + `--workspace-scoped` flag.
- Create: `internal/api/status_workspace_test.go`.

- [ ] **Step 1: Write failing tests**

Test skeleton adapted from eager Task 13 Step 1 — seed a registry entry with `Lifecycle: LifecycleActive, LastToolsCallAt: now, LastError: ""`, invoke `enrichStatusWithRegistry`, assert all fields present.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Extend `DaemonStatus` (types.go)**

Add below the existing fields:

```go
// Workspace-scoped daemon fields (empty for global daemons).
Workspace          string    `json:"workspace,omitempty"`
Language           string    `json:"language,omitempty"`
Backend            string    `json:"backend,omitempty"`
Lifecycle          string    `json:"lifecycle,omitempty"` // one of LifecycleConfigured/.../LifecycleFailed
LastMaterializedAt time.Time `json:"last_materialized_at,omitempty"`
LastToolsCallAt    time.Time `json:"last_tools_call_at,omitempty"`
LastError          string    `json:"last_error,omitempty"`
```

- [ ] **Step 4: Extend `status_enrich.go`**

Populate the new fields from the registry inside `enrichStatusWithRegistry`.

- [ ] **Step 5: Extend `status.go` CLI**

Add `--workspace-scoped` (filter) flag as in eager Task 13 Step 5.

When `--workspace-scoped` is set, print `LIFECYCLE | LAST_USED | LAST_ERROR (truncated 40)` columns.

- [ ] **Step 6: Run + commit**

```bash
git commit -m "$(cat <<'EOF'
feat(api,cli): status shows 5-state lifecycle + last_used

DaemonStatus now includes Workspace, Language, Backend, Lifecycle
(5 states), LastMaterializedAt, LastToolsCallAt, and LastError.
`mcphub status --workspace-scoped` prints LIFECYCLE + LAST_USED +
truncated LAST_ERROR columns.
EOF
)"
```

---

### Task 16: `--force-materialize` health probe flag

**Delta from eager:** NEW — no eager analog.

**Decision:** `mcphub status --health` default stays at **proxy-alive + synthetic handshake** (does NOT force materialization; preserves the lazy contract). `mcphub status --health --force-materialize` is the explicit opt-in that does a real `tools/call` to probe the backend and record the resulting lifecycle state.

**Files:**
- Modify: `internal/cli/status.go` — add `--force-materialize`.
- Modify: `internal/api/install.go` or wherever `StatusWithHealth` lives — accept a `forceMaterialize` bool.
- Modify: `internal/api/health_probe_test.go` — new test `TestHealthProbe_ForceMaterializeTriggersBackend`.

- [ ] **Step 1: Write failing test**

Simulate a lazy proxy with a fake backend: call `StatusWithHealth(ctx, opts)` where `opts.ForceMaterialize == false`; assert fake backend's `Materialize` is NOT called. Call again with `ForceMaterialize == true`; assert `Materialize` IS called and the result populates `DaemonStatus.Lifecycle = LifecycleActive`.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

In `StatusWithHealth`, when `opts.ForceMaterialize && rows[i].TaskName matches lsp-<key>-<lang>`, issue an HTTP POST to `http://localhost:<port>/mcp` with a no-op `tools/call` (pick a tool that is safe; e.g. for `mcp-language-server` use `diagnostics` with the workspace root), read the JSON-RPC response, classify success/failure, update the registry entry, then reload the entry into `rows[i]`.

In the CLI, add:

```go
c.Flags().BoolVar(&forceMaterialize, "force-materialize", false, "for workspace-scoped daemons: send a no-op tools/call to probe backend health (triggers backend materialization). Default health stays at proxy-alive only.")
```

- [ ] **Step 4: Run + commit**

```bash
git commit -m "$(cat <<'EOF'
feat(api,cli): `mcphub status --health --force-materialize`

Default --health preserves the lazy contract (proxy-alive probe
only; backend stays cold). --force-materialize issues a real
tools/call to each workspace-scoped proxy, materializes the
backend, and records the resulting lifecycle state.
EOF
)"
```

---

### Task 17: `weekly-refresh` CLI wiring

**Delta from eager:** VERBATIM from eager Task 14 Step 4.

Create `internal/cli/weekly_refresh.go` from eager content; wire `newWeeklyRefreshCmd` into `root.go`.

```bash
git commit -m "feat(cli): weekly-refresh hidden command (scheduler counterpart)"
```

---

### Task 18: E2E smoke — register → synthetic handshake → tools/call → materialize → status → unregister

**Delta from eager:** NEW — proves the entire lazy flow end-to-end.

**Files:**
- Create: `internal/api/e2e_smoke_test.go`.

- [ ] **Step 1: Write the test**

```go
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
)

// TestE2E_LazyRegisterThenToolsCallMaterializes is the Phase 3 happy-path
// smoke test. It exercises the entire lazy pipeline in-process using a
// httptest.Server wrapped around a real LazyProxy + fakeLifecycle.
func TestE2E_LazyRegisterThenToolsCallMaterializes(t *testing.T) {
	_, restore := newRegisterHarness(t)
	defer restore()

	ws := t.TempDir()
	probeCmd := pickExistingBinary(t)
	m := &config.ServerManifest{
		Name:     "mcp-language-server",
		Kind:     config.KindWorkspaceScoped,
		PortPool: &config.PortPool{Start: 9200, End: 9299},
		Languages: []config.LanguageSpec{
			{Name: "python", Backend: "mcp-language-server", Transport: "stdio", LspCommand: probeCmd},
		},
		ClientBindings: []config.ClientBinding{{Client: "codex-cli", URLPath: "/mcp"}},
	}
	a := NewAPI()
	if _, err := a.registerWithManifest(m, ws, nil /* default-all */, RegisterOpts{Writer: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Assert registry has exactly one entry in LifecycleConfigured.
	regPath, _ := registryPathForTest()
	reg := NewRegistry(regPath)
	_ = reg.Load()
	if len(reg.Workspaces) != 1 {
		t.Fatalf("registry len = %d, want 1", len(reg.Workspaces))
	}
	if reg.Workspaces[0].Lifecycle != LifecycleConfigured {
		t.Errorf("Lifecycle = %q, want configured", reg.Workspaces[0].Lifecycle)
	}

	// Spin up a lazy proxy bound to a fake lifecycle; fire synthetic initialize
	// + tools/list + tools/call; assert state transitions.
	fake := &fakeLifecycle{kind: "mcp-language-server"}
	proxy := daemon.NewLazyProxy(daemon.LazyProxyConfig{
		WorkspaceKey:  reg.Workspaces[0].WorkspaceKey,
		WorkspacePath: reg.Workspaces[0].WorkspacePath,
		Language:      "python",
		BackendKind:   "mcp-language-server",
		Port:          0, // Handler() fires via httptest; we don't bind
		Lifecycle:     fake,
		RegistryPath:  regPath,
	})
	srv := httptest.NewServer(proxy.Handler())
	defer srv.Close()

	postJSON := func(body string) string {
		resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(body))
		if err != nil { t.Fatal(err) }
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	// initialize + tools/list must not materialize.
	_ = postJSON(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	_ = postJSON(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if fake.materializeCount.Load() != 0 {
		t.Fatalf("materialize called %d times during handshake (want 0)", fake.materializeCount.Load())
	}
	// tools/call triggers one materialize.
	_ = postJSON(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hover","arguments":{}}}`)
	if fake.materializeCount.Load() != 1 {
		t.Errorf("materialize count = %d, want 1 after tools/call", fake.materializeCount.Load())
	}

	// Registry now shows LifecycleActive.
	_ = reg.Load()
	if reg.Workspaces[0].Lifecycle != LifecycleActive {
		t.Errorf("post-materialize Lifecycle = %q, want active", reg.Workspaces[0].Lifecycle)
	}

	// Unregister.
	if _, err := a.unregisterWithManifest(m, ws, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	_ = reg.Load()
	if len(reg.Workspaces) != 0 {
		t.Errorf("expected 0 after unregister, got %+v", reg.Workspaces)
	}
}
```

- [ ] **Step 2: Run**

```bash
go test ./internal/api/ -run TestE2E_LazyRegisterThenToolsCallMaterializes -v
go test ./...
go build ./...
```

- [ ] **Step 3: Commit**

```bash
git commit -m "test(api): E2E smoke for lazy register -> handshake -> materialize -> unregister"
```

---

## Self-Review

### Coverage of the 7 original decisions (with lazy-mode overlays)

| # | Decision (and lazy overlay if any) | Task(s) |
|---|---|---|
| 1 | Preflight without auto-install — overlay: LSP preflight moved to first tools/call | Task 10 (`Register` does NOT preflight LSP); Task 9 (lazy proxy surfaces `missing`/`failed` at first call); Task 16 (`--force-materialize` for on-demand probe) |
| 2 | Global managed entries; `<server>-<lang>`; `-<4hex>` on collision | Task 10 uses `ResolveEntryName` (verbatim from eager Task 6) |
| 3 | First-free port + registry-sourced allocation + file lock | Task 4 (Registry.Lock), Task 5 (AllocatePort) |
| 4 | No idle TTL in v1 | Not implemented. Backend lifetime = forever-until-unregister. |
| 5 | ONE shared weekly-refresh task | Task 13 verbatim |
| 6 | Automated legacy migration with dry-run + yes | Task 14 adapted — one Register per workspace (not per language) |
| 7 | Full + partial unregister | Task 10 (Unregister verbatim from eager Task 8) |

### Coverage of the 6 guardrails

| # | Guardrail | Task(s) |
|---|---|---|
| 1 | Registry atomic write + `.bak` + crash-leaves-old-file-intact | Task 4 |
| 2 | Partial register rollback | Task 10 — simpler than eager since no backends to kill |
| 3 | Workspace path identity: abs + lowercase drive + reject symlinks | Task 3 (verbatim) |
| 4 | Task naming uses hash, not raw path | Task 10 (`mcp-local-hub-lsp-<wsKey>-<lang>`) |
| 5 | JavaScript separate entry + port from TypeScript | Task 2 manifest — both present |
| 6 | Go uses `gopls mcp`, not `mcp-language-server --lsp gopls` | Task 2 + Task 7 (GoplsMCPStdio impl) |

### Coverage of the 6 lazy-mode decisions

| # | Lazy decision | Task(s) |
|---|---|---|
| 1 | Default all-languages register | Task 10 (Register defaults languages when nil); Task 11 (CLI accepts `<ws>` alone) |
| 2 | Synthetic initialize + tools/list, versioned + golden-tested | Task 6 (tool_catalog.go + golden test) |
| 3 | Singleflight + retry throttle | Task 8 (inflight.go with 2s default throttle) |
| 4 | Backend forever-until-unregister; re-materialize on crash | Task 7 (BackendLifecycle); Task 9 (lazy proxy re-materializes via gate) |
| 5 | Five-state lifecycle | Task 4 constants, Task 9 transitions, Task 15 status output |
| 6 | BackendLifecycle interface + manifest transport field | Task 1 (Transport), Task 7 (interface + 2 impls) |

### Coverage of the 5 status states

| State | Produced by |
|---|---|
| configured | Task 9 `ListenAndServe` initial PutLifecycle; Task 10 initial WorkspaceEntry |
| starting | Task 9 `ensureMaterialized` (before gate.Do returns) |
| active | Task 9 `ensureMaterialized` success path |
| missing | Task 9 `ensureMaterialized` when `IsMissingBinaryErr(err)` |
| failed | Task 9 `ensureMaterialized` on any other error |

### Coverage of the 9 languages

All 9 appear in `servers/mcp-language-server/manifest.yaml` (Task 2) and asserted by `TestParseManifest_McpLanguageServerShipped`. `go` is `gopls-mcp`; the other 8 are `mcp-language-server`. `javascript` and `typescript` share the same binary name but occupy separate registry rows and separate ports.

### Placeholder scan

Every task specifies: failing-test Step, verify-fail Step, implementation Step with complete code (or verbatim reference to the archived eager plan's specific Step), verify-pass Step, commit Step. No "TBD" or "TODO". `captureToolsList` helper in Task 6 Step 4 is fully specified. The gopls-mcp catalog's tool list in Task 6 Step 3 is flagged as **verify during implementation**; the golden test in Step 4 drives the correction cycle.

### Type consistency

- `WorkspaceEntry` fields (`WorkspaceKey`, `WorkspacePath`, `Language`, `Backend`, `Port`, `TaskName`, `ClientEntries`, `WeeklyRefresh`, `Lifecycle`, `LastMaterializedAt`, `LastToolsCallAt`, `LastError`) spelled identically across Tasks 4, 9, 10, 11, 15, 16, 18.
- `LifecycleConfigured/Starting/Active/Missing/Failed` used consistently across Tasks 4, 9, 15, 16.
- `BackendLifecycle`, `MCPEndpoint`, `JSONRPCRequest`, `JSONRPCResponse` spelled identically in Tasks 7, 9.
- `InflightGate` methods (`NewInflightGate`, `Do`, `Forget`) spelled identically in Tasks 8, 9.
- `LazyProxyConfig` fields (`WorkspaceKey`, `WorkspacePath`, `Language`, `BackendKind`, `Port`, `Lifecycle`, `RegistryPath`, `InflightMinRetryGap`, `ToolsCallDebounce`) fixed in Task 9.

### Gaps for human confirmation before execution

1. **`gopls mcp` stdio-vs-http_listen.** v1 assumes `gopls mcp` (no `-listen` flag) speaks stdio like `mcp-language-server`. Verify by running `gopls mcp -h` before Task 7 Step 3. If gopls rejects stdio (requires `-listen=host:port`), switch the manifest entry to `transport: http_listen` and implement a parallel `goplsMCPHTTPListen` impl that wraps `daemon.NewHTTPHost` (the existing Phase 2 native-http path). Impact: Task 7 adds a second impl, Task 9 remains unchanged because `BackendLifecycle` abstracts the transport difference. **MUST resolve before Task 7 implementation.**

2. **`mcp-language-server` upstream CLI shape.** Plan assumes `mcp-language-server --workspace <p> --lsp <cmd> [-- <flags>...]` on stdio. Confirm via `mcp-language-server --help` before Task 7 Step 3. Fix is local to `buildBackendLifecycle` (Task 9 Step 6) and to `McpLanguageServerStdioConfig` (Task 7).

3. **Tool catalog names (golden test).** The placeholder `mcpLanguageServerCatalog` in Task 4 Step 3 lists 6 tools (definition, references, diagnostics, hover, rename_symbol, edit_file) based on the archived plan's knowledge. Upstream may have drifted. The golden test (`TestToolCatalog_GoldenAgainstUpstream`) is the forcing function to correct it: run during Task 4 Step 5 with the live binary on PATH, observe the actual upstream tool list, paste into the catalog, bump `CatalogVersion`. The `goplsMCPCatalog` placeholder needs the same treatment.

4. **Missing-binary error classification.** `errMissingBinary` (Task 7) is checked via `errors.Is`. If upstream's `exec.ErrNotFound` wrapping differs (e.g. the binary is found but its `--lsp` child is missing), classification lands in `LifecycleFailed` instead of `LifecycleMissing`. This is acceptable v1 behavior — the user sees the raw error in `last_error` — but worth noting.

5. **Registry write frequency under load.** Lazy proxy writes registry on every lifecycle transition + every ≥5s debounced tools/call. Under burst traffic from an active IDE, that's one registry write per 5s, which is bounded and safe. If empirical measurements show contention on the file lock, raise `ToolsCallDebounce` to 30s and drop the `LastToolsCallAt` entirely from the per-call hot path. Plan does not over-optimize preemptively.

6. **`mcp-language-server` binary preflight at register time.** Lazy mode deliberately skips all LSP preflight, including the wrapper binary (`mcp-language-server`, `gopls`). If the wrapper is missing, every first-call materialization fails with `LifecycleMissing`. That is correct UX for lazy mode — the user sees the failure when they try to use the feature — but a user support mental model shift from eager. Consider adding a `mcphub register --check` dry-run flag in a follow-up phase that runs wrapper-binary preflight without side effects.

7. **Antigravity client.** Phase 2 added the antigravity relay adapter. `clientsAllForTest` (eager Task 7) enumerates `codex-cli`, `claude-code`, `gemini-cli` — antigravity is excluded in v1. Workspace-scoped registration does not wire antigravity relay fields because relay presumes one `(server, daemon)` tuple which workspace-scoped entries don't have. Explicitly parking antigravity support for a follow-up phase.

8. **Windows symlink rejection test.** `TestCanonicalWorkspacePath_RejectsSymlink` (Task 3) is `t.Skip`'d on Windows (symlink creation needs Developer Mode). Runtime rejection still enforces the policy; only coverage is Linux/macOS.

9. **Schema versioning beyond v1.** Registry has `version: 1`. Any future schema change must add a migration routine; plan does not include one.

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-04-20-mcp-language-server-workspace-scoped.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — Dispatch a fresh subagent per milestone (M1 → M5); review between milestones; same session. Tightly-ordered deps (manifest → registry → tool catalog → backend lifecycle → lazy proxy → register → migration → status) make serial per-milestone review the right fit. 18 tasks, ~5 milestone checkpoints. The lazy proxy in Task 9 is the highest-risk single task — plan a mid-milestone review on that one.

**2. Inline via `executing-plans`** — Reasonable if the operator prefers one continuous stream and reviews the full 18-task batch at the end.

**Which approach?**
