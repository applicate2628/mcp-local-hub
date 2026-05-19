# Servers matrix revamp: LSP-bridge integration + per-daemon env overlay — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Per memory rule "Subagents always opus + max", dispatch every subagent with `model: opus`.

**Goal:** Surface the 9 LSP-bridge languages as proper Servers-matrix rows, add a per-daemon env overlay file editable from the GUI, auto-discover binary locations at install time, and replace the supervisor's UNKNOWN_COMMAND `restart` stub with a working `respawn` IPC command.

**Architecture:** Four sequential phases on a single branch / single PR. Phase 1 adds `required_binaries` manifest field + `binary_discovery` package. Phase 2 adds the `daemon_env_overlay` package + extends `mergeDaemonEnv` + removes the `len(d.Env) > 0` spawn gate. Phase 3 adds `ScanEntry.LegacyConflict` + three-rule LSP recognition in scan.go via reverse-lookup against the workspace registry. Phase 4 adds the `respawn` IPC command + three GUI endpoints + frontend Servers.tsx changes.

**Tech Stack:** Go 1.22 (backend, supervisor, CLI), Preact + TypeScript + Vite (frontend), Playwright + Chromium (E2E), `gopkg.in/yaml.v3` (overlay YAML round-trip with comments), `github.com/gofrs/flock` (overlay RMW lock), `golang.org/x/sys/windows` (Windows reparse-point handling).

**Spec:** [docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md](../specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md) (v4 at HEAD `5295c90`).

---

## File Structure

### NEW files

| Path | Responsibility |
|---|---|
| `internal/api/binary_discovery/discover.go` | `Discover(ctx, requiredBinaries, hints) (map[string]string, error)` — hint-walking binary resolver |
| `internal/api/binary_discovery/hints_windows.go` | `DefaultHints() []string` for Windows (glob-aware Python3*) |
| `internal/api/binary_discovery/hints_linux.go` | `DefaultHints() []string` for Linux |
| `internal/api/binary_discovery/hints_darwin.go` | `DefaultHints() []string` for macOS |
| `internal/api/binary_discovery/discover_test.go` | Unit tests with injected synthetic temp-dir hints |
| `internal/api/daemon_env_overlay/normalize.go` | `NormalizeOverlayKey(taskName) string` — prepends `\` if absent |
| `internal/api/daemon_env_overlay/overlay.go` | `Overlay` struct + `Load(path) (*Overlay, error)` |
| `internal/api/daemon_env_overlay/write.go` | `WriteOverlay(path, mutator func(*Overlay) error) error` — flock RMW |
| `internal/api/daemon_env_overlay/read_hardening.go` | Hardened-read pipeline + Windows reparse-point refusal |
| `internal/api/daemon_env_overlay/read_hardening_windows.go` | Windows `CreateFileW` + `FILE_FLAG_OPEN_REPARSE_POINT` set + attribute check |
| `internal/api/daemon_env_overlay/read_hardening_posix.go` | POSIX `os.OpenFile` + `O_NOFOLLOW` |
| `internal/api/daemon_env_overlay/parent_check.go` | `checkStateDirParentReadSafe(dir) error` — symmetric with existing write-side |
| `internal/api/daemon_env_overlay/normalize_test.go` | `NormalizeOverlayKey` idempotency tests |
| `internal/api/daemon_env_overlay/overlay_test.go` | YAML round-trip + comment preservation tests |
| `internal/api/daemon_env_overlay/write_test.go` | Flock + atomic-rename + mutator-error rollback tests |
| `internal/api/daemon_env_overlay/read_hardening_test.go` | Mode().IsRegular() + symlink-refuse tests |
| `internal/api/manifest_lsp_lookup.go` | `ParseEntryName(name string, langs []string) (lang, suffix string)` |
| `internal/api/manifest_lsp_lookup_test.go` | parseEntryName tests for all 9 languages + suffix variants |
| `internal/cli/overlay_quarantine.go` | `mcphub config overlay-quarantine` CLI command (offline, no IPC) |
| `internal/cli/overlay_quarantine_test.go` | Quarantine command tests |

### MODIFIED files

| Path | Change |
|---|---|
| `internal/config/manifest.go` | Add `RequiredBinaries []string` field to `ServerManifest` (line 48) and `LanguageSpec` (line 85) |
| `servers/mcp-language-server/manifest.yaml` | Add `required_binaries` per language |
| `servers/gdb/manifest.yaml` | Add `required_binaries: [gdb]` at server level |
| `servers/lldb/manifest.yaml` | No `required_binaries` (internal bridge — empty/absent) |
| `internal/cli/supervise.go:1456-1506` | Extend `mergeDaemonEnv` signature with `overlayEnv`; remove `if len(d.Env) > 0` gate at lines 1504-1506 |
| `internal/cli/supervise.go:921` | Replace `restart`/`reload` UNKNOWN_COMMAND stub with `respawn` handler |
| `internal/api/types.go:99-106` | Add `LegacyConflict map[string]ClientEntry` field (omitempty) to `ScanEntry` |
| `internal/api/scan.go:240-310` | Integrate three-rule LSP recognition + registry reverse-lookup |
| `internal/gui/server.go` | Add three new handlers: `/api/daemon/env`, `/api/discovery/refresh`, `/api/daemon/respawn` |
| `internal/gui/frontend/src/screens/Servers.tsx` | Active-workspace selector + 9 LSP rows + per-row drawer with env editor + restart button + `${parent_path}` warning chip |

---

## Pre-flight: Verify baseline

- [ ] **Step 0: Verify clean tree on master at v4 commit**

Run:
```bash
git status && git log --oneline -3
```
Expected: clean working tree; HEAD is `5295c90 docs(spec): revise to v4 — fix 3 BLOCKERS + 6 IMPORTANT + 3 MINOR from v3 dual re-review`.

- [ ] **Step 0.1: Verify backend builds green**

Run:
```bash
go build ./... && go vet ./...
```
Expected: no output, exit 0.

- [ ] **Step 0.2: Verify the `len(d.Env) > 0` gate is still at the cited line**

Run:
```bash
grep -n "if len(d.Env) > 0" internal/cli/supervise.go
```
Expected: `1504:		if len(d.Env) > 0 {` (matches spec citation).

---

## Phase 1: Manifest schema + binary_discovery package

### Task 1.1: Add `RequiredBinaries` field to manifest structs

**Files:**
- Modify: `internal/config/manifest.go:48-76` (ServerManifest), `:85-91` (LanguageSpec)
- Modify: `internal/config/manifest_test.go` (add round-trip test)

- [ ] **Step 1.1.1: Write the failing test**

Add to `internal/config/manifest_test.go`:

```go
func TestParseManifestRequiredBinariesServerLevel(t *testing.T) {
    yaml := `name: gdb
kind: global
transport: stdio-bridge
command: gdb
required_binaries:
  - gdb
`
    m, err := ParseManifest(strings.NewReader(yaml))
    if err != nil {
        t.Fatalf("ParseManifest returned error: %v", err)
    }
    if len(m.RequiredBinaries) != 1 || m.RequiredBinaries[0] != "gdb" {
        t.Fatalf("RequiredBinaries = %v, want [gdb]", m.RequiredBinaries)
    }
}

func TestParseManifestRequiredBinariesLanguageLevel(t *testing.T) {
    yaml := `name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
languages:
  - name: clangd
    backend: mcp-language-server
    transport: stdio
    lsp_command: clangd
    required_binaries: [clangd]
`
    m, err := ParseManifest(strings.NewReader(yaml))
    if err != nil {
        t.Fatalf("ParseManifest returned error: %v", err)
    }
    if len(m.Languages) != 1 || len(m.Languages[0].RequiredBinaries) != 1 ||
        m.Languages[0].RequiredBinaries[0] != "clangd" {
        t.Fatalf("Languages[0].RequiredBinaries = %v, want [clangd]", m.Languages[0].RequiredBinaries)
    }
}
```

- [ ] **Step 1.1.2: Run tests to verify they fail with KnownFields(true) rejection**

Run:
```bash
go test ./internal/config/ -run TestParseManifestRequiredBinaries -v
```
Expected: FAIL with `field required_binaries not found in type config.ServerManifest` (and equivalent for LanguageSpec).

- [ ] **Step 1.1.3: Add fields to structs**

Edit `internal/config/manifest.go`:

```go
// ServerManifest at line 48
type ServerManifest struct {
    Name             string            `yaml:"name"`
    Kind             string            `yaml:"kind"`
    Transport        string            `yaml:"transport"`
    Command          string            `yaml:"command"`
    BaseArgs         []string          `yaml:"base_args"`
    BaseArgsTemplate []string          `yaml:"base_args_template"`
    Env              map[string]string `yaml:"env"`
    Daemons          []DaemonSpec      `yaml:"daemons"`
    Languages        []LanguageSpec    `yaml:"languages"`
    PortPool         *PortPool         `yaml:"port_pool"`
    IdleTimeoutMin   int               `yaml:"idle_timeout_min"`
    ClientBindings   []ClientBinding   `yaml:"client_bindings"`
    WeeklyRefresh    bool              `yaml:"weekly_refresh"`
    URL              string            `yaml:"url"`
    Headers          map[string]string `yaml:"headers"`

    // RequiredBinaries names upstream binaries that auto-discovery
    // (internal/api/binary_discovery) walks for at install time.
    // Free-form metadata; Validate() does NOT enforce existence.
    RequiredBinaries []string `yaml:"required_binaries,omitempty"`
}

// LanguageSpec at line 85
type LanguageSpec struct {
    Name             string   `yaml:"name"`
    Backend          string   `yaml:"backend"`
    Transport        string   `yaml:"transport"`
    LspCommand       string   `yaml:"lsp_command"`
    ExtraFlags       []string `yaml:"extra_flags"`
    RequiredBinaries []string `yaml:"required_binaries,omitempty"`
}
```

- [ ] **Step 1.1.4: Run tests to verify they pass**

Run:
```bash
go test ./internal/config/ -run TestParseManifestRequiredBinaries -v
```
Expected: PASS (both tests).

- [ ] **Step 1.1.5: Run full manifest test suite to verify no regression**

Run:
```bash
go test ./internal/config/ -count=1
```
Expected: all PASS.

- [ ] **Step 1.1.6: Commit**

```bash
git add internal/config/manifest.go internal/config/manifest_test.go
git commit -m "feat(manifest): add RequiredBinaries field to ServerManifest + LanguageSpec

Optional free-form metadata field listing upstream binaries that
auto-discovery (Phase 1.3) walks for at install time. Validate()
does NOT enforce existence; the field is purely a hint to the
discovery engine."
```

### Task 1.2: Wire `required_binaries` into shipped manifests

**Files:**
- Modify: `servers/mcp-language-server/manifest.yaml`
- Modify: `servers/gdb/manifest.yaml`

- [ ] **Step 1.2.1: Edit `servers/mcp-language-server/manifest.yaml`**

Add per-language `required_binaries`:

```yaml
languages:
  - name: clangd
    backend: mcp-language-server
    transport: stdio
    lsp_command: clangd
    required_binaries: [clangd]

  - name: fortran
    backend: mcp-language-server
    transport: stdio
    lsp_command: fortls
    required_binaries: [fortls]

  - name: go
    backend: gopls-mcp
    transport: stdio
    lsp_command: gopls
    extra_flags: [mcp]
    required_binaries: [gopls]

  - name: javascript
    backend: mcp-language-server
    transport: stdio
    lsp_command: typescript-language-server
    extra_flags: ["--stdio"]
    required_binaries: [typescript-language-server]

  - name: python
    backend: mcp-language-server
    transport: stdio
    lsp_command: pyright-langserver
    extra_flags: ["--stdio"]
    required_binaries: [pyright-langserver]

  - name: rust
    backend: mcp-language-server
    transport: stdio
    lsp_command: rust-analyzer
    required_binaries: [rust-analyzer]

  - name: typescript
    backend: mcp-language-server
    transport: stdio
    lsp_command: typescript-language-server
    extra_flags: ["--stdio"]
    required_binaries: [typescript-language-server]

  - name: vscode-css
    backend: mcp-language-server
    transport: stdio
    lsp_command: vscode-css-language-server
    extra_flags: ["--stdio"]
    required_binaries: [vscode-css-language-server]

  - name: vscode-html
    backend: mcp-language-server
    transport: stdio
    lsp_command: vscode-html-language-server
    extra_flags: ["--stdio"]
    required_binaries: [vscode-html-language-server]
```

- [ ] **Step 1.2.2: Edit `servers/gdb/manifest.yaml`**

Add at top level (before `command:`):

```yaml
required_binaries:
  - gdb
```

- [ ] **Step 1.2.3: Run manifest parse smoke**

Run:
```bash
go test ./internal/config/ -count=1 && go run ./cmd/mcphub manifest validate
```
Expected: PASS + validation reports manifests OK.

- [ ] **Step 1.2.4: Commit**

```bash
git add servers/mcp-language-server/manifest.yaml servers/gdb/manifest.yaml
git commit -m "feat(manifests): declare required_binaries for LSP + gdb daemons

Per-language declarations for mcp-language-server (clangd, fortls,
gopls, typescript-language-server, pyright-langserver, rust-analyzer,
vscode-{css,html}-language-server) and server-level for gdb.
lldb is omitted intentionally — internal bridge has no external deps."
```

### Task 1.3: Create `internal/api/binary_discovery/` package

**Files:**
- Create: `internal/api/binary_discovery/discover.go`
- Create: `internal/api/binary_discovery/hints_windows.go`
- Create: `internal/api/binary_discovery/hints_linux.go`
- Create: `internal/api/binary_discovery/hints_darwin.go`
- Create: `internal/api/binary_discovery/discover_test.go`

- [ ] **Step 1.3.1: Write the failing tests first**

Create `internal/api/binary_discovery/discover_test.go`:

```go
package binary_discovery

import (
    "context"
    "os"
    "path/filepath"
    "runtime"
    "testing"
)

func TestDiscoverFindsBinaryInFirstHint(t *testing.T) {
    tmp := t.TempDir()
    binName := "clangd"
    if runtime.GOOS == "windows" {
        binName = "clangd.exe"
    }
    binPath := filepath.Join(tmp, binName)
    if err := os.WriteFile(binPath, []byte{}, 0o755); err != nil {
        t.Fatalf("seed binary: %v", err)
    }

    got, err := Discover(context.Background(), []string{"clangd"}, []string{tmp})
    if err != nil {
        t.Fatalf("Discover: %v", err)
    }
    if got["clangd"] != binPath {
        t.Fatalf("got[clangd] = %q, want %q", got["clangd"], binPath)
    }
}

func TestDiscoverReturnsEmptyWhenMissing(t *testing.T) {
    tmp := t.TempDir() // no binary seeded
    got, err := Discover(context.Background(), []string{"missing-tool"}, []string{tmp})
    if err != nil {
        t.Fatalf("Discover: %v", err)
    }
    if got["missing-tool"] != "" {
        t.Fatalf("missing-tool should be empty, got %q", got["missing-tool"])
    }
}

func TestDiscoverWalksHintsInOrder(t *testing.T) {
    first := t.TempDir()
    second := t.TempDir()
    binName := "rust-analyzer"
    if runtime.GOOS == "windows" {
        binName = "rust-analyzer.exe"
    }
    // Only seed the SECOND hint dir.
    secondPath := filepath.Join(second, binName)
    if err := os.WriteFile(secondPath, []byte{}, 0o755); err != nil {
        t.Fatalf("seed: %v", err)
    }

    got, err := Discover(context.Background(), []string{"rust-analyzer"}, []string{first, second})
    if err != nil {
        t.Fatalf("Discover: %v", err)
    }
    if got["rust-analyzer"] != secondPath {
        t.Fatalf("got[rust-analyzer] = %q, want %q", got["rust-analyzer"], secondPath)
    }
}

func TestDefaultHintsNonEmpty(t *testing.T) {
    hints := DefaultHints()
    if len(hints) == 0 {
        t.Fatalf("DefaultHints returned empty list on %s", runtime.GOOS)
    }
}
```

- [ ] **Step 1.3.2: Run tests to verify they fail (package missing)**

Run:
```bash
go test ./internal/api/binary_discovery/ -v
```
Expected: FAIL with `no Go files in d:\dev\mcp-local-hub\internal\api\binary_discovery`.

- [ ] **Step 1.3.3: Implement `discover.go`**

Create `internal/api/binary_discovery/discover.go`:

```go
// Package binary_discovery walks a list of "hint" directories looking
// for upstream binaries declared by manifests' required_binaries field.
// The hints slice is injected via parameter so unit tests can pin to
// a synthetic temp-dir layout; production callers use DefaultHints()
// for the shipped per-OS list.
package binary_discovery

import (
    "context"
    "os"
    "path/filepath"
    "runtime"
)

// Discover walks the hints in order looking for each required binary.
// Returns map[binaryName]absolutePath. Missing binaries map to "".
// On Windows, the search appends ".exe" to each requested binary name.
//
// The function is hint-injection-aware: pass a synthetic tempdir slice
// for tests; pass DefaultHints() in production.
func Discover(ctx context.Context, requiredBinaries []string, hints []string) (map[string]string, error) {
    out := make(map[string]string, len(requiredBinaries))
    for _, bin := range requiredBinaries {
        if err := ctx.Err(); err != nil {
            return out, err
        }
        candidates := []string{bin}
        if runtime.GOOS == "windows" {
            candidates = []string{bin + ".exe", bin}
        }
        out[bin] = "" // default: missing
        for _, dir := range hints {
            for _, c := range candidates {
                p := filepath.Join(os.ExpandEnv(dir), c)
                if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
                    out[bin] = p
                    goto found
                }
            }
        }
    found:
    }
    return out, nil
}
```

- [ ] **Step 1.3.4: Implement `hints_windows.go`**

Create `internal/api/binary_discovery/hints_windows.go`:

```go
//go:build windows

package binary_discovery

import (
    "os"
    "path/filepath"
    "sort"
)

// DefaultHints returns the shipped per-OS list of directories to search.
// The list is intentionally finite and committed to mcphub source.
// Operators override via the env overlay file, NOT by extending this list.
func DefaultHints() []string {
    fixed := []string{
        `C:\msys64\ucrt64\bin`,
        `C:\msys64\mingw64\bin`,
        `C:\msys64\clang64\bin`,
        `C:\Program Files\LLVM\bin`,
        `C:\Program Files\Go\bin`,
        `C:\Program Files\Microsoft Visual Studio\2022\BuildTools\VC\Tools\Llvm\x64\bin`,
        `%USERPROFILE%\.cargo\bin`,
        `%USERPROFILE%\go\bin`,
        `%USERPROFILE%\.local\bin`,
        `%LOCALAPPDATA%\fnm_multishells`,
        `%LOCALAPPDATA%\Programs\fnm`,
        `%LOCALAPPDATA%\nvm`,
        `%APPDATA%\npm`,
    }

    // Python paths are version-agnostic via glob (v4 M-V4-1 fix).
    pythonRoot := os.ExpandEnv(`%LOCALAPPDATA%\Programs\Python`)
    if entries, err := os.ReadDir(pythonRoot); err == nil {
        var pys []string
        for _, e := range entries {
            if e.IsDir() && filepath.Base(e.Name())[:len("Python3")] == "Python3" {
                pys = append(pys, filepath.Join(pythonRoot, e.Name()))
            }
        }
        sort.Strings(pys)
        fixed = append(fixed, pys...)
    }

    return fixed
}
```

- [ ] **Step 1.3.5: Implement `hints_linux.go`**

Create `internal/api/binary_discovery/hints_linux.go`:

```go
//go:build linux

package binary_discovery

func DefaultHints() []string {
    return []string{
        "/usr/local/bin",
        "/usr/bin",
        "$HOME/.local/bin",
        "$HOME/.cargo/bin",
        "$HOME/go/bin",
    }
}
```

- [ ] **Step 1.3.6: Implement `hints_darwin.go`**

Create `internal/api/binary_discovery/hints_darwin.go`:

```go
//go:build darwin

package binary_discovery

func DefaultHints() []string {
    return []string{
        "/opt/homebrew/bin",
        "/usr/local/bin",
        "/opt/local/bin",
        "$HOME/.local/bin",
        "$HOME/.cargo/bin",
        "$HOME/go/bin",
    }
}
```

- [ ] **Step 1.3.7: Run tests to verify they pass**

Run:
```bash
go test ./internal/api/binary_discovery/ -count=1 -v
```
Expected: all 4 tests PASS.

- [ ] **Step 1.3.8: Verify full build**

Run:
```bash
go build ./... && go vet ./...
```
Expected: clean exit 0.

- [ ] **Step 1.3.9: Commit**

```bash
git add internal/api/binary_discovery/
git commit -m "feat(binary_discovery): add hint-walking binary resolver

New package Discover(ctx, requiredBinaries, hints) returns absolute
paths for each binary or empty string when missing. Hints injected
via parameter for test isolation. DefaultHints() per-OS:
- Windows: MSYS2, LLVM, Go, Python3* (glob), cargo, npm, fnm/nvm
- Linux: standard PATH-like dirs
- macOS: Homebrew + MacPorts + Linux list"
```

---

## Phase 2: daemon_env_overlay package + mergeDaemonEnv extension

### Task 2.1: `NormalizeOverlayKey` helper + tests

**Files:**
- Create: `internal/api/daemon_env_overlay/normalize.go`
- Create: `internal/api/daemon_env_overlay/normalize_test.go`

- [ ] **Step 2.1.1: Write failing tests**

Create `internal/api/daemon_env_overlay/normalize_test.go`:

```go
package daemon_env_overlay

import "testing"

func TestNormalizeOverlayKeyPrependsBackslash(t *testing.T) {
    got := NormalizeOverlayKey("mcp-local-hub-memory-default")
    want := `\mcp-local-hub-memory-default`
    if got != want {
        t.Fatalf("got %q, want %q", got, want)
    }
}

func TestNormalizeOverlayKeyIdempotent(t *testing.T) {
    in := `\mcp-local-hub-memory-default`
    got := NormalizeOverlayKey(in)
    if got != in {
        t.Fatalf("got %q, want %q (idempotent)", got, in)
    }
}

func TestNormalizeOverlayKeyEmpty(t *testing.T) {
    if got := NormalizeOverlayKey(""); got != "" {
        t.Fatalf("empty input should stay empty, got %q", got)
    }
}
```

- [ ] **Step 2.1.2: Verify tests fail**

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -v
```
Expected: FAIL — `no Go files` or `NormalizeOverlayKey undefined`.

- [ ] **Step 2.1.3: Implement `normalize.go`**

Create `internal/api/daemon_env_overlay/normalize.go`:

```go
// Package daemon_env_overlay owns the per-daemon env overlay YAML
// file: hardened read, flock-protected RMW write, canonical key
// normalization, and the spawn-time lookup helper.
//
// Storage path: ~/.config/mcp-local-hub/daemon-env-overrides.yaml
// (POSIX) or %LOCALAPPDATA%\mcp-local-hub\daemon-env-overrides.yaml
// (Windows). The file is operator-editable; mcphub install/upgrade
// preserves overlays.
//
// Canonical key form: SupervisorDaemon.TaskName with leading
// backslash, e.g. "\mcp-local-hub-memory-default". Stored keys are
// always canonical. Reads accept either form (operator hand-edit
// might omit the backslash).
package daemon_env_overlay

import "strings"

// NormalizeOverlayKey returns taskName with a leading "\" if it
// doesn't already have one. The empty string is preserved as-is so
// the caller can distinguish "missing key" from "valid bare key".
// This matches the SupervisorDaemon.TaskName canonical form
// documented at internal/api/supervisor_intent.go:25.
func NormalizeOverlayKey(taskName string) string {
    if taskName == "" {
        return ""
    }
    if strings.HasPrefix(taskName, `\`) {
        return taskName
    }
    return `\` + taskName
}
```

- [ ] **Step 2.1.4: Verify tests pass**

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -count=1 -v
```
Expected: 3 PASS.

- [ ] **Step 2.1.5: Commit**

```bash
git add internal/api/daemon_env_overlay/normalize.go internal/api/daemon_env_overlay/normalize_test.go
git commit -m "feat(daemon_env_overlay): add NormalizeOverlayKey canonical-key helper

Prepends leading backslash if absent. Idempotent. Matches
SupervisorDaemon.TaskName canonical form (supervisor_intent.go:25).
Used at every overlay call site (spawn lookup, GUI write, mutator,
orphan detection) per spec v4 I-V4-3."
```

### Task 2.2: `Overlay` struct + YAML round-trip with comment preservation

**Files:**
- Create: `internal/api/daemon_env_overlay/overlay.go`
- Create: `internal/api/daemon_env_overlay/overlay_test.go`

- [ ] **Step 2.2.1: Write failing tests**

Create `internal/api/daemon_env_overlay/overlay_test.go`:

```go
package daemon_env_overlay

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
)

func TestLoadMissingFileReturnsEmptyOverlay(t *testing.T) {
    path := filepath.Join(t.TempDir(), "missing.yaml")
    ov, err := Load(path)
    if err != nil {
        t.Fatalf("Load(missing) returned error: %v", err)
    }
    if ov == nil {
        t.Fatalf("Load(missing) returned nil overlay")
    }
    if len(ov.Daemons) != 0 {
        t.Fatalf("Load(missing).Daemons = %v, want empty", ov.Daemons)
    }
}

func TestLoadParsesRoundTripWithComments(t *testing.T) {
    yaml := `# operator-edited
version: 1
daemons:
  "\\mcp-local-hub-gdb-default":
    env:
      Path: "C:/msys64/ucrt64/bin;${parent_path}"
    source: auto-discovery
    discovered_at: 2026-05-19T14:00:00Z
`
    path := filepath.Join(t.TempDir(), "overlay.yaml")
    if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
        t.Fatalf("seed: %v", err)
    }
    ov, err := Load(path)
    if err != nil {
        t.Fatalf("Load: %v", err)
    }
    if ov.Version != 1 {
        t.Fatalf("Version = %d, want 1", ov.Version)
    }
    row, ok := ov.Daemons[`\mcp-local-hub-gdb-default`]
    if !ok {
        t.Fatalf("expected key found; got Daemons keys = %v", overlayKeys(ov))
    }
    if row.Source != "auto-discovery" {
        t.Fatalf("Source = %q, want auto-discovery", row.Source)
    }
    if strings.TrimSpace(row.Env["Path"]) == "" {
        t.Fatalf("Env[Path] missing")
    }
}

func overlayKeys(ov *Overlay) []string {
    out := make([]string, 0, len(ov.Daemons))
    for k := range ov.Daemons {
        out = append(out, k)
    }
    return out
}
```

- [ ] **Step 2.2.2: Run tests to verify failure**

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -run TestLoad -v
```
Expected: FAIL — `Overlay undefined` / `Load undefined`.

- [ ] **Step 2.2.3: Implement `overlay.go`**

Create `internal/api/daemon_env_overlay/overlay.go`:

```go
package daemon_env_overlay

import (
    "errors"
    "fmt"
    "io"
    "os"

    "gopkg.in/yaml.v3"
)

// Overlay is the parsed daemon-env-overrides.yaml structure.
type Overlay struct {
    Version int                  `yaml:"version"`
    Daemons map[string]DaemonRow `yaml:"daemons"`
}

// DaemonRow is one row keyed by SupervisorDaemon.TaskName (canonical
// leading-backslash form). Source is "auto-discovery" or "operator".
type DaemonRow struct {
    Env           map[string]string `yaml:"env"`
    Source        string            `yaml:"source,omitempty"`         // "auto-discovery" | "operator"
    DiscoveredAt  string            `yaml:"discovered_at,omitempty"`  // RFC3339Nano
    ModifiedAt    string            `yaml:"modified_at,omitempty"`    // RFC3339Nano
}

// MaxOverlaySize is the read-side hard cap (defense in depth).
const MaxOverlaySize = 64 * 1024

// Load parses the YAML overlay at path. Missing file returns an
// empty Overlay and nil error. Parse failures + hardening rejections
// propagate as errors.
//
// Load applies the read-side hardening pipeline from spec v4
// "Read-side hardening" section: symlink/reparse-point refusal,
// non-regular-file refusal, parent-dir DACL check, size cap, UTF-8
// validation.
func Load(path string) (*Overlay, error) {
    if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
        return &Overlay{Version: 1, Daemons: map[string]DaemonRow{}}, nil
    }

    f, err := hardenedOpen(path)
    if err != nil {
        return nil, fmt.Errorf("hardened open %s: %w", path, err)
    }
    defer f.Close()

    fi, err := f.Stat()
    if err != nil {
        return nil, fmt.Errorf("stat handle %s: %w", path, err)
    }
    if !fi.Mode().IsRegular() {
        return nil, fmt.Errorf("%s: not a regular file (mode=%s)", path, fi.Mode())
    }

    raw, err := io.ReadAll(io.LimitReader(f, MaxOverlaySize+1))
    if err != nil {
        return nil, fmt.Errorf("read %s: %w", path, err)
    }
    if len(raw) > MaxOverlaySize {
        return nil, fmt.Errorf("%s exceeds %d bytes cap", path, MaxOverlaySize)
    }

    var ov Overlay
    dec := yaml.NewDecoder(io.NopCloser(bytesReader(raw)))
    dec.KnownFields(true)
    if err := dec.Decode(&ov); err != nil {
        return nil, fmt.Errorf("yaml decode %s: %w", path, err)
    }
    if ov.Daemons == nil {
        ov.Daemons = map[string]DaemonRow{}
    }
    return &ov, nil
}

func bytesReader(b []byte) io.Reader {
    return &byteReader{b: b}
}

type byteReader struct {
    b []byte
    i int
}

func (r *byteReader) Read(p []byte) (int, error) {
    if r.i >= len(r.b) {
        return 0, io.EOF
    }
    n := copy(p, r.b[r.i:])
    r.i += n
    return n, nil
}
```

- [ ] **Step 2.2.4: Implement temporary `hardenedOpen` stub**

Add to `internal/api/daemon_env_overlay/read_hardening.go` (Task 2.4 fills it in):

```go
package daemon_env_overlay

import "os"

// hardenedOpen is replaced in Task 2.4 with the platform-specific
// reparse-point-refusing implementation. The stub is os.Open so
// Task 2.2 tests can pass; Task 2.4 wires the real hardening.
func hardenedOpen(path string) (*os.File, error) {
    return os.Open(path)
}
```

- [ ] **Step 2.2.5: Run tests + verify pass**

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -count=1 -v
```
Expected: 5 PASS (3 from Task 2.1 + 2 new).

- [ ] **Step 2.2.6: Commit**

```bash
git add internal/api/daemon_env_overlay/overlay.go internal/api/daemon_env_overlay/overlay_test.go internal/api/daemon_env_overlay/read_hardening.go
git commit -m "feat(daemon_env_overlay): add Overlay struct + Load() YAML parser

Overlay is the parsed daemon-env-overrides.yaml structure with
{Version, Daemons map[taskName]DaemonRow}. DaemonRow carries
{Env, Source: 'auto-discovery'|'operator', DiscoveredAt, ModifiedAt}.

Load() applies 64 KiB size cap + Mode().IsRegular() check + missing-file
short-circuit (returns empty Overlay). Hardened open is a stub here;
Task 2.4 replaces it with reparse-point refusal + parent-DACL check."
```

### Task 2.3: `WriteOverlay(path, mutator)` flock-protected RMW

**Files:**
- Create: `internal/api/daemon_env_overlay/write.go`
- Create: `internal/api/daemon_env_overlay/write_test.go`

- [ ] **Step 2.3.1: Write failing tests**

Create `internal/api/daemon_env_overlay/write_test.go`:

```go
package daemon_env_overlay

import (
    "errors"
    "os"
    "path/filepath"
    "sync"
    "testing"
)

func TestWriteOverlayAtomicCreateNewFile(t *testing.T) {
    path := filepath.Join(t.TempDir(), "overlay.yaml")
    err := WriteOverlay(path, func(ov *Overlay) error {
        ov.Daemons[`\mcp-local-hub-gdb-default`] = DaemonRow{
            Env:    map[string]string{"Path": "C:/msys64/ucrt64/bin;${parent_path}"},
            Source: "operator",
        }
        return nil
    })
    if err != nil {
        t.Fatalf("WriteOverlay: %v", err)
    }
    ov, err := Load(path)
    if err != nil {
        t.Fatalf("Load: %v", err)
    }
    if got := ov.Daemons[`\mcp-local-hub-gdb-default`].Env["Path"]; got == "" {
        t.Fatalf("Path missing after round-trip")
    }
}

func TestWriteOverlayMutatorErrorRollsBack(t *testing.T) {
    path := filepath.Join(t.TempDir(), "overlay.yaml")
    // Pre-seed an existing file so we can verify it's not modified.
    initial := `version: 1
daemons:
  "\\mcp-local-hub-existing":
    env:
      PATH: /pre-existing
    source: operator
`
    if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
        t.Fatalf("seed: %v", err)
    }
    sentinel := errors.New("mutator-failed")
    err := WriteOverlay(path, func(ov *Overlay) error {
        ov.Daemons["new-key"] = DaemonRow{Env: map[string]string{"FOO": "bar"}}
        return sentinel
    })
    if !errors.Is(err, sentinel) {
        t.Fatalf("expected sentinel, got %v", err)
    }
    after, err := Load(path)
    if err != nil {
        t.Fatalf("Load after rollback: %v", err)
    }
    if _, leaked := after.Daemons["new-key"]; leaked {
        t.Fatalf("mutator change leaked to disk despite error")
    }
    if after.Daemons[`\mcp-local-hub-existing`].Env["PATH"] != "/pre-existing" {
        t.Fatalf("existing row was corrupted")
    }
}

func TestWriteOverlayConcurrentSerializes(t *testing.T) {
    path := filepath.Join(t.TempDir(), "overlay.yaml")
    var wg sync.WaitGroup
    for i := 0; i < 5; i++ {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            key := `\mcp-local-hub-gdb-default`
            envKey := "K" + string(rune('0'+idx))
            _ = WriteOverlay(path, func(ov *Overlay) error {
                row := ov.Daemons[key]
                if row.Env == nil {
                    row.Env = map[string]string{}
                }
                row.Env[envKey] = "v"
                ov.Daemons[key] = row
                return nil
            })
        }(i)
    }
    wg.Wait()
    ov, err := Load(path)
    if err != nil {
        t.Fatalf("Load: %v", err)
    }
    if got := len(ov.Daemons[`\mcp-local-hub-gdb-default`].Env); got != 5 {
        t.Fatalf("expected 5 env keys after concurrent writes, got %d (lost-update bug)", got)
    }
}
```

- [ ] **Step 2.3.2: Verify tests fail**

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -run TestWriteOverlay -v
```
Expected: FAIL — `WriteOverlay undefined`.

- [ ] **Step 2.3.3: Implement `write.go`**

Create `internal/api/daemon_env_overlay/write.go`:

```go
package daemon_env_overlay

import (
    "fmt"
    "os"
    "path/filepath"

    "github.com/gofrs/flock"
    "gopkg.in/yaml.v3"

    "mcp-local-hub/internal/api"
)

// WriteOverlay performs a flock-protected RMW transaction:
//
//   1. Acquire `<path>.lock` via flock (blocks until available).
//   2. Load the current overlay (or empty if missing).
//   3. Invoke mutator(overlay) — caller's edits happen in-memory.
//   4. If mutator returns error: release flock, propagate error,
//      DO NOT touch the on-disk file.
//   5. Else marshal to YAML and route through the existing exported
//      api.SecureWriteClientConfig() for atomic temp+rename + DACL
//      handle-binding (state_file_helper.go:127).
//   6. Release flock.
//
// Mutator-error rollback contract (spec v4 I-V3-4): the mutator MUST
// not perform external side effects before returning error — the
// rollback only covers YAML persistence. External side effects
// cannot be undone.
//
// Concurrent callers serialize on the flock. Lost-update window is
// closed under the documented atomic temp+rename + flock pattern.
func WriteOverlay(path string, mutator func(*Overlay) error) error {
    if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
        return fmt.Errorf("mkdir parent: %w", err)
    }
    lockPath := path + ".lock"
    lk := flock.New(lockPath)
    if err := lk.Lock(); err != nil {
        return fmt.Errorf("flock %s: %w", lockPath, err)
    }
    defer func() { _ = lk.Unlock() }()

    ov, err := Load(path)
    if err != nil {
        return fmt.Errorf("load before write: %w", err)
    }
    if ov.Version == 0 {
        ov.Version = 1
    }

    if err := mutator(ov); err != nil {
        return err // NO disk write on mutator error
    }

    raw, err := yaml.Marshal(ov)
    if err != nil {
        return fmt.Errorf("marshal: %w", err)
    }
    if err := api.SecureWriteClientConfig(path, raw); err != nil {
        return fmt.Errorf("secure write: %w", err)
    }
    return nil
}
```

- [ ] **Step 2.3.4: Run tests + verify pass**

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -count=1 -v
```
Expected: all 8 tests PASS.

- [ ] **Step 2.3.5: Commit**

```bash
git add internal/api/daemon_env_overlay/write.go internal/api/daemon_env_overlay/write_test.go
git commit -m "feat(daemon_env_overlay): add WriteOverlay flock-protected RMW

WriteOverlay(path, mutator func(*Overlay) error) loads + applies
mutator + marshals + routes through api.SecureWriteClientConfig
under a per-file flock. Mutator-error contract: NO disk write,
flock released, error propagated. Concurrent writers serialize.

Test coverage: atomic create, mutator-error rollback (existing
file unchanged), concurrent writers don't lose updates."
```

### Task 2.4: Read-side hardening — Windows reparse-point refusal + parent DACL

**Files:**
- Modify: `internal/api/daemon_env_overlay/read_hardening.go` (replace stub)
- Create: `internal/api/daemon_env_overlay/read_hardening_windows.go`
- Create: `internal/api/daemon_env_overlay/read_hardening_posix.go`
- Create: `internal/api/daemon_env_overlay/parent_check.go`
- Create: `internal/api/daemon_env_overlay/read_hardening_test.go`

- [ ] **Step 2.4.1: Write failing test for non-regular refusal**

Create `internal/api/daemon_env_overlay/read_hardening_test.go`:

```go
package daemon_env_overlay

import (
    "os"
    "path/filepath"
    "testing"
)

func TestLoadRejectsDirectoryAtPath(t *testing.T) {
    dir := t.TempDir()
    pseudoOverlay := filepath.Join(dir, "overlay.yaml")
    if err := os.Mkdir(pseudoOverlay, 0o700); err != nil {
        t.Fatalf("mkdir: %v", err)
    }
    _, err := Load(pseudoOverlay)
    if err == nil {
        t.Fatalf("Load(<directory>) returned nil error; want refusal")
    }
}
```

- [ ] **Step 2.4.2: Verify test fails** (stub `os.Open` accepts directories on some Go versions; the Stat-then-IsRegular check catches it but path may already pass)

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -run TestLoadRejectsDirectory -v
```
Expected: depending on platform, may PASS already (Mode().IsRegular() check) — proceed regardless.

- [ ] **Step 2.4.3: Replace `read_hardening.go` stub with platform dispatch**

Replace `internal/api/daemon_env_overlay/read_hardening.go`:

```go
// Package daemon_env_overlay's hardened open dispatches per-OS.
//
// POSIX: os.OpenFile with O_NOFOLLOW so the kernel refuses symlinks.
// Windows: windows.CreateFileW with FILE_FLAG_OPEN_REPARSE_POINT |
// FILE_FLAG_BACKUP_SEMANTICS SET, then refuse via
// FILE_ATTRIBUTE_REPARSE_POINT check on the handle (pattern at
// internal/api/hub_mcp_state_dacl_windows.go:85-99,192).
//
// The function returns an *os.File so the caller can treat both
// platforms uniformly for Stat / Read.
package daemon_env_overlay

import "os"

// hardenedOpen is replaced by the platform-specific implementation
// in read_hardening_{windows,posix}.go (build-tagged). This file
// exists for godoc.

var _ = os.Open // satisfy import; per-platform file overrides
```

- [ ] **Step 2.4.4: Implement `read_hardening_posix.go`**

Create `internal/api/daemon_env_overlay/read_hardening_posix.go`:

```go
//go:build !windows

package daemon_env_overlay

import (
    "fmt"
    "os"
    "syscall"
)

// hardenedOpen opens path read-only with O_NOFOLLOW so the kernel
// refuses any symlink at the leaf. Returns ErrOverlaySymlink
// (wrapped) when the kernel surfaces ELOOP.
func hardenedOpen(path string) (*os.File, error) {
    f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
    if err != nil {
        return nil, fmt.Errorf("open %s: %w", path, err)
    }
    return f, nil
}
```

- [ ] **Step 2.4.5: Implement `read_hardening_windows.go`**

Create `internal/api/daemon_env_overlay/read_hardening_windows.go`:

```go
//go:build windows

package daemon_env_overlay

import (
    "fmt"
    "os"
    "unsafe"

    "golang.org/x/sys/windows"
)

// hardenedOpen opens path read-only refusing reparse points.
//
// The Win32 flag FILE_FLAG_OPEN_REPARSE_POINT — counter-intuitively —
// SET means "open the reparse point itself, do NOT follow the link".
// We set it so the CreateFile call gives us a handle to the link
// (instead of the target), then immediately refuse if the file
// attributes carry FILE_ATTRIBUTE_REPARSE_POINT.
//
// Pattern reused verbatim from
// internal/api/hub_mcp_state_dacl_windows.go:85-99 (parent-dir open)
// and :192 (attribute check).
func hardenedOpen(path string) (*os.File, error) {
    pathW, err := windows.UTF16PtrFromString(path)
    if err != nil {
        return nil, fmt.Errorf("path to UTF16 %s: %w", path, err)
    }
    h, err := windows.CreateFile(
        pathW,
        windows.GENERIC_READ,
        windows.FILE_SHARE_READ,
        nil,
        windows.OPEN_EXISTING,
        windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
        0,
    )
    if err != nil {
        return nil, fmt.Errorf("CreateFile %s: %w", path, err)
    }
    var bhfi windows.ByHandleFileInformation
    if err := windows.GetFileInformationByHandle(h, &bhfi); err != nil {
        windows.CloseHandle(h)
        return nil, fmt.Errorf("GetFileInformationByHandle %s: %w", path, err)
    }
    if bhfi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
        windows.CloseHandle(h)
        return nil, fmt.Errorf("%s is a reparse point/symlink (refused)", path)
    }
    // Hand the handle to *os.File so the rest of the package can use
    // the standard read/Stat API.
    return os.NewFile(uintptr(unsafe.Pointer(&h)), path), nil
}
```

(Note: a real implementation needs `os.NewFile(uintptr(h), path)` with the raw handle, not a pointer. The exact ceremony depends on `golang.org/x/sys/windows.Handle` ↔ `uintptr` conversion. Reference impl is at `internal/api/hub_mcp_state_dacl_windows.go`.)

- [ ] **Step 2.4.6: Implement `parent_check.go`**

Create `internal/api/daemon_env_overlay/parent_check.go`:

```go
package daemon_env_overlay

import (
    "fmt"
    "os"
    "path/filepath"

    "mcp-local-hub/internal/api"
)

// AllowUnhardenedStateReadEnv is the operator opt-in env var that
// disables the read-side parent-dir DACL strict gate. Symmetric with
// api.AllowUnhardenedStateWriteEnv at
// internal/api/client_write_init.go:105.
const AllowUnhardenedStateReadEnv = "MCPHUB_ALLOW_UNHARDENED_STATE_READ"

// checkStateDirParentReadSafe validates the parent dir's DACL using
// the same default-relax / strict-mode semantics as the existing
// write-side helper at internal/api/state_file_helper.go:139-157.
//
//   - If MCPHUB_REQUIRE_SINGLE_USER_HOME=1 → parent rejection is hard error
//   - Else if AllowUnhardenedStateRead=1 → opt out of all checks
//   - Else if write-bits granted to non-allowlisted principal → reject
//   - Else → proceed (default-relax for legitimate corp hosts)
func checkStateDirParentReadSafe(dir string) error {
    if api.OperatorRequiresSingleUserHome() {
        if err := api.CheckStateDirParentWriteSafe(dir); err != nil {
            return fmt.Errorf("parent %s rejected (strict mode): %w", dir, err)
        }
    }
    if os.Getenv(AllowUnhardenedStateReadEnv) == "1" {
        return nil
    }
    if err := api.CheckStateDirParentWriteSafe(dir); err != nil {
        return fmt.Errorf("parent %s grants write to non-allowlisted principal: %w", dir, err)
    }
    return nil
}

// overlayParentDir returns filepath.Dir(path); extracted for testability.
func overlayParentDir(path string) string {
    return filepath.Dir(path)
}
```

(Note: This requires exporting `OperatorRequiresSingleUserHome` and `CheckStateDirParentWriteSafe` from `internal/api`. Currently these may be lowercase. Sub-step 2.4.7 handles the export.)

- [ ] **Step 2.4.7: Export the parent-DACL helpers from `internal/api`**

Edit `internal/api/state_file_helper.go` (or wherever the helpers live):

```go
// Rename if needed (add uppercase shim):
func OperatorRequiresSingleUserHome() bool { return operatorRequiresSingleUserHome() }
func CheckStateDirParentWriteSafe(dir string) error { return checkStateDirParentWriteSafe(dir) }
```

(Or rename the originals; either works as long as `daemon_env_overlay` can call them.)

- [ ] **Step 2.4.8: Wire `checkStateDirParentReadSafe` into `Load()`**

Edit `internal/api/daemon_env_overlay/overlay.go` `Load()`:

After the `f, err := hardenedOpen(...)` line and before `fi, err := f.Stat()`, add:

```go
    if err := checkStateDirParentReadSafe(overlayParentDir(path)); err != nil {
        f.Close()
        return nil, fmt.Errorf("parent check %s: %w", path, err)
    }
```

- [ ] **Step 2.4.9: Run tests + verify pass**

Run:
```bash
go test ./internal/api/daemon_env_overlay/ -count=1 -v
```
Expected: all 9 tests PASS (8 prior + 1 new directory-refusal).

- [ ] **Step 2.4.10: Run full Go test suite for regression check**

Run:
```bash
go build ./... && go vet ./... && go test ./internal/api/... -count=1 -timeout 5m
```
Expected: all PASS, exit 0.

- [ ] **Step 2.4.11: Commit**

```bash
git add internal/api/daemon_env_overlay/ internal/api/state_file_helper.go
git commit -m "feat(daemon_env_overlay): add Windows reparse-point refusal + parent DACL

POSIX: O_NOFOLLOW. Windows: CreateFileW with FILE_FLAG_OPEN_REPARSE_POINT
| FILE_FLAG_BACKUP_SEMANTICS SET, then GetFileInformationByHandle +
refuse if FILE_ATTRIBUTE_REPARSE_POINT is set. Pattern reused from
hub_mcp_state_dacl_windows.go:85-99,192 (already in repo, correct
polarity).

New env var MCPHUB_ALLOW_UNHARDENED_STATE_READ symmetric with
MCPHUB_ALLOW_UNHARDENED_STATE_WRITE for read-side relax opt-in.
checkStateDirParentReadSafe mirrors the write-side default-relax /
strict-mode semantics from state_file_helper.go:139-157.

Exports OperatorRequiresSingleUserHome + CheckStateDirParentWriteSafe
from internal/api so daemon_env_overlay can call them without
package-internal symbol leak."
```

### Task 2.5: Extend `mergeDaemonEnv` + remove spawn gate

**Files:**
- Modify: `internal/cli/supervise.go:1456-1488` (mergeDaemonEnv signature)
- Modify: `internal/cli/supervise.go:1504-1506` (spawn gate removal)
- Modify: `internal/cli/supervise_test.go` (extended merge tests)

- [ ] **Step 2.5.1: Write failing tests**

Add to `internal/cli/supervise_test.go`:

```go
func TestMergeDaemonEnvOverlayWinsOverManifest(t *testing.T) {
    parent := []string{"PATH=/system"}
    manifest := map[string]string{"Path": "/manifest"}
    overlay := map[string]string{"Path": "/overlay"}
    got := mergeDaemonEnv(parent, manifest, overlay)
    var foundPath string
    for _, kv := range got {
        if strings.HasPrefix(kv, "Path=") {
            foundPath = kv
        }
    }
    if foundPath != "Path=/overlay" {
        t.Fatalf("overlay should win: got %q", foundPath)
    }
}

func TestMergeDaemonEnvBothEmptyReturnsParent(t *testing.T) {
    parent := []string{"PATH=/system"}
    got := mergeDaemonEnv(parent, nil, nil)
    if len(got) != 1 || got[0] != "PATH=/system" {
        t.Fatalf("both empty should preserve parent only: got %v", got)
    }
}

func TestMergeDaemonEnvWindowsCaseInsensitivePATH(t *testing.T) {
    if runtime.GOOS != "windows" {
        t.Skip("Windows case-insensitive only")
    }
    parent := []string{"PATH=/parent"}
    manifest := map[string]string{"path": "/manifest"}
    overlay := map[string]string{"Path": "/overlay"}
    got := mergeDaemonEnv(parent, manifest, overlay)
    // Expect exactly ONE Path-family entry; overlay wins.
    count := 0
    var final string
    for _, kv := range got {
        if strings.EqualFold(strings.SplitN(kv, "=", 2)[0], "path") {
            count++
            final = kv
        }
    }
    if count != 1 {
        t.Fatalf("expected 1 path entry, got %d (%v)", count, got)
    }
    if !strings.HasSuffix(final, "/overlay") {
        t.Fatalf("overlay should win: %q", final)
    }
}
```

- [ ] **Step 2.5.2: Run tests to verify failure**

Run:
```bash
go test ./internal/cli/ -run TestMergeDaemonEnv -v
```
Expected: FAIL — signature mismatch (mergeDaemonEnv takes only 2 args).

- [ ] **Step 2.5.3: Extend `mergeDaemonEnv` signature**

Edit `internal/cli/supervise.go` line 1456-1488. Replace:

```go
// mergeDaemonEnv appends descriptor env overrides in deterministic key order.
func mergeDaemonEnv(parent []string, overrides map[string]string) []string {
    env := append([]string{}, parent...)
    if len(overrides) == 0 {
        return env
    }
    keys := make([]string, 0, len(overrides))
    for k := range overrides {
        keys = append(keys, k)
    }
    sort.Strings(keys)
    for _, k := range keys {
        env = append(env, k+"="+overrides[k])
    }
    return env
}
```

With:

```go
// mergeDaemonEnv combines parent (os.Environ()), manifest descriptor
// env, and per-daemon overlay env into a single env slice for
// exec.Cmd.Env.
//
// Precedence: parent < manifest < overlay (later overrides earlier).
//
// Windows: PATH/Path/path collide case-insensitively; we normalize
// by uppercasing the lookup key. The output preserves the casing
// from the highest-precedence source.
//
// Per spec v4 §"Spawn-time env merge", the old gate
// `if len(d.Env) > 0 { ... }` is GONE — overlay-only rows now spawn
// with their env even when the manifest declared no env.
func mergeDaemonEnv(parent []string, manifest, overlay map[string]string) []string {
    norm := func(k string) string {
        if runtime.GOOS == "windows" {
            return strings.ToUpper(k)
        }
        return k
    }

    // Build a normalized lookup: keep the highest-precedence original key+value.
    merged := map[string]string{}      // normalized key -> "KEY=value"
    keyCanon := map[string]string{}    // normalized key -> last-seen canonical key form

    apply := func(k, v string) {
        n := norm(k)
        merged[n] = k + "=" + v
        keyCanon[n] = k
    }

    for _, kv := range parent {
        i := strings.IndexByte(kv, '=')
        if i < 0 {
            continue
        }
        apply(kv[:i], kv[i+1:])
    }
    manifestKeys := make([]string, 0, len(manifest))
    for k := range manifest {
        manifestKeys = append(manifestKeys, k)
    }
    sort.Strings(manifestKeys)
    for _, k := range manifestKeys {
        apply(k, manifest[k])
    }
    overlayKeys := make([]string, 0, len(overlay))
    for k := range overlay {
        overlayKeys = append(overlayKeys, k)
    }
    sort.Strings(overlayKeys)
    for _, k := range overlayKeys {
        apply(k, overlay[k])
    }

    out := make([]string, 0, len(merged))
    // Deterministic order: sort by normalized key.
    norms := make([]string, 0, len(merged))
    for n := range merged {
        norms = append(norms, n)
    }
    sort.Strings(norms)
    for _, n := range norms {
        out = append(out, merged[n])
    }
    return out
}
```

- [ ] **Step 2.5.4: Update all `mergeDaemonEnv` call sites**

Find existing callers:

Run:
```bash
grep -n "mergeDaemonEnv" internal/cli/
```

For each non-test caller, change `mergeDaemonEnv(parent, d.Env)` to `mergeDaemonEnv(parent, d.Env, nil)` for compile compatibility (Phase 3 will wire the overlay arg properly).

- [ ] **Step 2.5.5: Remove the spawn gate at lines 1504-1506**

Edit `internal/cli/supervise.go`. Find:

```go
        if len(d.Env) > 0 {
            cmd.Env = mergeDaemonEnv(os.Environ(), d.Env)
        }
```

Replace with:

```go
        // v4 B3 fix: spawn gate removed; mergeDaemonEnv handles
        // both-empty fallback internally. The overlay lookup is
        // added in Phase 3 once daemon_env_overlay is wired into
        // the supervisor's startup.
        overlayEnv := loadOverlayForTask(d.TaskName) // returns nil today; Phase 3 wires real overlay
        if len(d.Env) > 0 || len(overlayEnv) > 0 {
            cmd.Env = mergeDaemonEnv(os.Environ(), d.Env, overlayEnv)
        }
```

Add stub function nearby:

```go
// loadOverlayForTask is wired to daemon_env_overlay.LookupOverlay in
// Phase 3. This Phase 2 stub returns nil so the spawn behavior is
// unchanged until Phase 3 wires the overlay file path through.
func loadOverlayForTask(taskName string) map[string]string {
    return nil
}
```

- [ ] **Step 2.5.6: Run tests + verify pass**

Run:
```bash
go test ./internal/cli/ -count=1 -timeout 5m
```
Expected: all PASS including the 3 new merge tests.

- [ ] **Step 2.5.7: Full build + vet**

Run:
```bash
go build ./... && go vet ./...
```
Expected: clean exit 0.

- [ ] **Step 2.5.8: Commit**

```bash
git add internal/cli/supervise.go internal/cli/supervise_test.go
git commit -m "refactor(supervise): extend mergeDaemonEnv with overlayEnv; remove spawn gate

mergeDaemonEnv now takes (parent, manifest, overlay) and applies
precedence parent < manifest < overlay with Windows case-insensitive
PATH/Path/path normalization. The old 'if len(d.Env) > 0' gate at
lines 1504-1506 is removed — overlay-only rows now spawn correctly.

loadOverlayForTask is a Phase-2 stub returning nil; Phase 3 wires
the real daemon_env_overlay.LookupOverlay call once the overlay
file path is threaded through the supervisor startup."
```

### Task 2.6: `mcphub config overlay-quarantine` offline CLI

**Files:**
- Create: `internal/cli/overlay_quarantine.go`
- Create: `internal/cli/overlay_quarantine_test.go`

- [ ] **Step 2.6.1: Write failing test**

Create `internal/cli/overlay_quarantine_test.go`:

```go
package cli

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
)

func TestOverlayQuarantineRenamesFile(t *testing.T) {
    dir := t.TempDir()
    overlayPath := filepath.Join(dir, "daemon-env-overrides.yaml")
    if err := os.WriteFile(overlayPath, []byte("version: 1\ndaemons: {}\n"), 0o600); err != nil {
        t.Fatalf("seed: %v", err)
    }

    if err := overlayQuarantine(overlayPath); err != nil {
        t.Fatalf("overlayQuarantine: %v", err)
    }

    if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
        t.Fatalf("original should be gone after quarantine, got err=%v", err)
    }

    entries, err := os.ReadDir(dir)
    if err != nil {
        t.Fatalf("readdir: %v", err)
    }
    found := false
    for _, e := range entries {
        if strings.HasPrefix(e.Name(), "daemon-env-overrides.yaml.corrupt-") {
            found = true
            break
        }
    }
    if !found {
        t.Fatalf("no .corrupt-<ts> file found in %s", dir)
    }
}

func TestOverlayQuarantineMissingFileIsNoop(t *testing.T) {
    dir := t.TempDir()
    overlayPath := filepath.Join(dir, "daemon-env-overrides.yaml")
    if err := overlayQuarantine(overlayPath); err != nil {
        t.Fatalf("missing file should be no-op, got: %v", err)
    }
}
```

- [ ] **Step 2.6.2: Run test to verify failure**

Run:
```bash
go test ./internal/cli/ -run TestOverlayQuarantine -v
```
Expected: FAIL — `overlayQuarantine undefined`.

- [ ] **Step 2.6.3: Implement `overlay_quarantine.go`**

Create `internal/cli/overlay_quarantine.go`:

```go
package cli

import (
    "fmt"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "time"

    "github.com/gofrs/flock"
    "github.com/spf13/cobra"
)

const maxOverlayQuarantineRetained = 5

func newOverlayQuarantineCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "overlay-quarantine",
        Short: "Rename daemon-env-overrides.yaml aside so supervisor boots with empty overlay",
        Long: `Offline CLI: does NOT require supervisor IPC. Use this when the
supervisor is refusing to spawn due to a corrupt overlay file. The
overlay is renamed to daemon-env-overrides.yaml.corrupt-<RFC3339>;
the 5 newest .corrupt-* files are retained, older are pruned.`,
        RunE: func(cmd *cobra.Command, args []string) error {
            stateDir, err := stateDirPath()
            if err != nil {
                return err
            }
            path := filepath.Join(stateDir, "daemon-env-overrides.yaml")
            return overlayQuarantine(path)
        },
    }
}

// overlayQuarantine renames the overlay file aside and prunes old
// quarantine files. No supervisor IPC; pure file ops + flock.
func overlayQuarantine(path string) error {
    if _, err := os.Stat(path); os.IsNotExist(err) {
        fmt.Println("no overlay to quarantine")
        return nil
    } else if err != nil {
        return fmt.Errorf("stat %s: %w", path, err)
    }

    lockPath := path + ".lock"
    lk := flock.New(lockPath)
    if err := lk.Lock(); err != nil {
        return fmt.Errorf("flock %s: %w", lockPath, err)
    }
    defer func() { _ = lk.Unlock() }()

    ts := time.Now().UTC().Format(time.RFC3339)
    newPath := path + ".corrupt-" + ts
    if err := os.Rename(path, newPath); err != nil {
        return fmt.Errorf("rename: %w", err)
    }

    // Retain 5 newest .corrupt-* files; delete older.
    dir := filepath.Dir(path)
    base := filepath.Base(path)
    entries, err := os.ReadDir(dir)
    if err != nil {
        fmt.Fprintf(os.Stderr, "warn: readdir %s for prune: %v\n", dir, err)
    } else {
        var quarantines []string
        for _, e := range entries {
            if strings.HasPrefix(e.Name(), base+".corrupt-") {
                quarantines = append(quarantines, filepath.Join(dir, e.Name()))
            }
        }
        sort.Sort(sort.Reverse(sort.StringSlice(quarantines)))
        for i, p := range quarantines {
            if i < maxOverlayQuarantineRetained {
                continue
            }
            if rmErr := os.Remove(p); rmErr != nil {
                fmt.Fprintf(os.Stderr, "warn: prune %s: %v\n", p, rmErr)
            }
        }
    }

    fmt.Printf("renamed to %s. Run 'mcphub restart' (or wait for next supervisor cold start) to apply.\n", newPath)
    return nil
}
```

- [ ] **Step 2.6.4: Wire the command into the `config` cobra subcommand**

Find the existing `mcphub config` cobra registration (search):

```bash
grep -rn "newConfigCmd\|mcphub config" internal/cli/
```

Add `cmd.AddCommand(newOverlayQuarantineCmd())` in the discovered `newConfigCmd()` function.

- [ ] **Step 2.6.5: Run tests + verify pass**

Run:
```bash
go test ./internal/cli/ -run TestOverlayQuarantine -count=1 -v
```
Expected: 2 PASS.

- [ ] **Step 2.6.6: Smoke the CLI**

Run:
```bash
go build -o /tmp/mcphub-test ./cmd/mcphub
/tmp/mcphub-test config overlay-quarantine --help
```
Expected: usage help printed; no error.

- [ ] **Step 2.6.7: Commit**

```bash
git add internal/cli/overlay_quarantine.go internal/cli/overlay_quarantine_test.go internal/cli/config.go
git commit -m "feat(cli): add 'mcphub config overlay-quarantine' offline command

Renames daemon-env-overrides.yaml → .corrupt-<RFC3339> via flock,
retains 5 newest .corrupt-* by mtime, prunes older. Standalone CLI:
does NOT require supervisor IPC, so operators can recover when the
supervisor is refusing to spawn due to overlay parse failure.

Mirrors the v0.4.x watchdog quarantine retention pattern."
```

---

## Phase 3: scan.go three-rule recognition + ScanEntry.LegacyConflict

### Task 3.1: `ScanEntry.LegacyConflict` schema field + JSON round-trip

**Files:**
- Modify: `internal/api/types.go:99-106` (ScanEntry struct)
- Modify: `internal/api/types_test.go` (round-trip test)

- [ ] **Step 3.1.1: Write failing test**

Add to `internal/api/types_test.go` (create file if absent):

```go
package api

import (
    "encoding/json"
    "strings"
    "testing"
)

func TestScanEntryLegacyConflictOmitemptyWhenEmpty(t *testing.T) {
    e := ScanEntry{
        Name:           "clangd",
        Status:         "via-hub",
        ClientPresence: map[string]ClientEntry{},
    }
    b, err := json.Marshal(e)
    if err != nil {
        t.Fatalf("marshal: %v", err)
    }
    if strings.Contains(string(b), "legacy_conflict") {
        t.Fatalf("legacy_conflict should be omitted when empty; got %s", b)
    }
}

func TestScanEntryLegacyConflictPopulated(t *testing.T) {
    e := ScanEntry{
        Name:   "clangd",
        Status: "via-hub",
        ClientPresence: map[string]ClientEntry{
            "codex": {Transport: "http", Endpoint: "http://localhost:9201/mcp"},
        },
        LegacyConflict: map[string]ClientEntry{
            "codex": {Transport: "stdio", Endpoint: "/usr/bin/mcp-language-server"},
        },
    }
    b, err := json.Marshal(e)
    if err != nil {
        t.Fatalf("marshal: %v", err)
    }
    var round ScanEntry
    if err := json.Unmarshal(b, &round); err != nil {
        t.Fatalf("round-trip unmarshal: %v", err)
    }
    if round.LegacyConflict["codex"].Endpoint != "/usr/bin/mcp-language-server" {
        t.Fatalf("LegacyConflict lost in round-trip: %+v", round)
    }
}
```

- [ ] **Step 3.1.2: Verify tests fail**

Run:
```bash
go test ./internal/api/ -run TestScanEntryLegacyConflict -v
```
Expected: FAIL — `unknown field LegacyConflict in struct literal`.

- [ ] **Step 3.1.3: Add the field**

Edit `internal/api/types.go:99-106`:

```go
// ScanEntry is one row in the unified "across all clients" view.
type ScanEntry struct {
    Name           string                 `json:"name"`
    Status         string                 `json:"status"`
    ClientPresence map[string]ClientEntry `json:"client_presence"`

    // LegacyConflict (v4 I-V4-2): when a hub-managed entry coexists
    // with a direct-stdio entry for the same (client, server) tuple,
    // ClientPresence[client] holds the canonical (hub) entry and
    // LegacyConflict[client] holds the stdio one. Renderer reads
    // both fields to emit dual-badge cells. Omitempty so existing
    // consumers ignore the field in the common no-conflict case.
    LegacyConflict map[string]ClientEntry `json:"legacy_conflict,omitempty"`

    ManifestExists bool `json:"manifest_exists"`
    CanMigrate     bool `json:"can_migrate"`
    ProcessCount   int  `json:"process_count,omitempty"`
}
```

- [ ] **Step 3.1.4: Verify tests pass**

Run:
```bash
go test ./internal/api/ -run TestScanEntryLegacyConflict -count=1 -v
```
Expected: 2 PASS.

- [ ] **Step 3.1.5: Commit**

```bash
git add internal/api/types.go internal/api/types_test.go
git commit -m "feat(scan): add ScanEntry.LegacyConflict side-channel field

Optional map[string]ClientEntry (omitempty). Populated only when
recognition finds BOTH a hub-managed AND a direct-stdio entry for
the same (client, server) tuple. ClientPresence[client] holds the
canonical (hub-preferred) entry; LegacyConflict[client] holds the
secondary stdio one. Renderer reads both fields to emit dual-badge
cells. Backwards compatible — existing consumers ignore the field
in the common no-conflict case."
```

### Task 3.2: `parseEntryName` helper for LSP recognition

**Files:**
- Create: `internal/api/manifest_lsp_lookup.go`
- Create: `internal/api/manifest_lsp_lookup_test.go`

- [ ] **Step 3.2.1: Write failing tests**

Create `internal/api/manifest_lsp_lookup_test.go`:

```go
package api

import "testing"

var lspLangs = []string{
    "clangd", "fortran", "go", "javascript", "python", "rust",
    "typescript", "vscode-css", "vscode-html",
}

func TestParseEntryNamePlainBase(t *testing.T) {
    lang, suffix := ParseEntryName("mcp-language-server-clangd", lspLangs)
    if lang != "clangd" || suffix != "" {
        t.Fatalf("got (%q, %q), want (clangd, '')", lang, suffix)
    }
}

func TestParseEntryNameShortSuffix(t *testing.T) {
    lang, suffix := ParseEntryName("mcp-language-server-rust-a1b2", lspLangs)
    if lang != "rust" || suffix != "a1b2" {
        t.Fatalf("got (%q, %q), want (rust, a1b2)", lang, suffix)
    }
}

func TestParseEntryNameFullSuffix(t *testing.T) {
    lang, suffix := ParseEntryName("mcp-language-server-typescript-deadbeef", lspLangs)
    if lang != "typescript" || suffix != "deadbeef" {
        t.Fatalf("got (%q, %q), want (typescript, deadbeef)", lang, suffix)
    }
}

func TestParseEntryNameNonLSPReturnsEmpty(t *testing.T) {
    lang, _ := ParseEntryName("some-other-server", lspLangs)
    if lang != "" {
        t.Fatalf("non-LSP entry should return empty, got %q", lang)
    }
}

func TestParseEntryNameHyphenatedLanguage(t *testing.T) {
    lang, suffix := ParseEntryName("mcp-language-server-vscode-html", lspLangs)
    if lang != "vscode-html" || suffix != "" {
        t.Fatalf("got (%q, %q), want (vscode-html, '')", lang, suffix)
    }
}

func TestParseEntryNameHyphenatedWithSuffix(t *testing.T) {
    lang, suffix := ParseEntryName("mcp-language-server-vscode-css-abcd", lspLangs)
    if lang != "vscode-css" || suffix != "abcd" {
        t.Fatalf("got (%q, %q), want (vscode-css, abcd)", lang, suffix)
    }
}
```

- [ ] **Step 3.2.2: Verify tests fail**

Run:
```bash
go test ./internal/api/ -run TestParseEntryName -v
```
Expected: FAIL — `ParseEntryName undefined`.

- [ ] **Step 3.2.3: Implement `manifest_lsp_lookup.go`**

Create `internal/api/manifest_lsp_lookup.go`:

```go
package api

import "strings"

const lspEntryPrefix = "mcp-language-server-"

// ParseEntryName extracts the language and optional collision suffix
// from a client-config entry name produced by `mcphub register`.
//
// Input forms (per internal/api/register.go:722-747 ResolveEntryName):
//
//   mcp-language-server-<lang>               → (lang, "")
//   mcp-language-server-<lang>-<4hex>        → (lang, "<4hex>")
//   mcp-language-server-<lang>-<8hex>        → (lang, "<8hex>") on prefix collision
//
// Where <lang> is one of the 9 manifest language names (langs param).
// vscode-css and vscode-html embed hyphens, so we must match the
// LONGEST language prefix before treating the remainder as suffix.
//
// Returns empty strings if the entry name doesn't start with the LSP
// prefix or doesn't match any known language.
func ParseEntryName(entryName string, langs []string) (lang, suffix string) {
    if !strings.HasPrefix(entryName, lspEntryPrefix) {
        return "", ""
    }
    rest := entryName[len(lspEntryPrefix):]

    // Find the longest matching language prefix.
    bestLang := ""
    for _, candidate := range langs {
        if rest == candidate {
            // Exact match — no suffix.
            return candidate, ""
        }
        if strings.HasPrefix(rest, candidate+"-") {
            if len(candidate) > len(bestLang) {
                bestLang = candidate
            }
        }
    }
    if bestLang == "" {
        return "", ""
    }
    suffix = rest[len(bestLang)+1:] // skip the "-" separator
    return bestLang, suffix
}
```

- [ ] **Step 3.2.4: Verify tests pass**

Run:
```bash
go test ./internal/api/ -run TestParseEntryName -count=1 -v
```
Expected: 6 PASS.

- [ ] **Step 3.2.5: Commit**

```bash
git add internal/api/manifest_lsp_lookup.go internal/api/manifest_lsp_lookup_test.go
git commit -m "feat(api): add ParseEntryName helper for LSP entry-name parsing

Extracts language + optional collision suffix from client-config
entry names produced by ResolveEntryName (register.go:722-747).
Matches longest language prefix first to handle hyphenated languages
(vscode-css, vscode-html) correctly.

Returns empty strings for non-LSP entries or unknown languages."
```

### Task 3.3: Three-rule LSP recognition in `scan.go`

**Files:**
- Modify: `internal/api/scan.go` (extend ScanFrom with LSP categorization pass)
- Modify: `internal/api/scan_test.go` (recognition fixtures)

- [ ] **Step 3.3.1: Write failing test fixtures**

Add to `internal/api/scan_test.go`:

```go
func TestScanRecognizesHubManagedLSP(t *testing.T) {
    // Seed a workspace registry with a clangd registration in codex.
    tmpHome := t.TempDir()
    seedRegistry(t, tmpHome, []WorkspaceEntry{{
        WorkspaceKey:  "a1b2c3d4",
        WorkspacePath: "/proj/main",
        Language:      "clangd",
        Port:          9201,
        ClientEntries: map[string]string{"codex": "mcp-language-server-clangd"},
    }})
    // Seed codex config with the hub URL entry.
    seedCodexConfig(t, tmpHome, map[string]map[string]any{
        "mcp-language-server-clangd": {
            "url": "http://localhost:9201/mcp",
        },
    })

    result, err := ScanFrom(tmpHome)
    if err != nil {
        t.Fatalf("ScanFrom: %v", err)
    }
    var entry *ScanEntry
    for i := range result.Entries {
        if result.Entries[i].Name == "mcp-language-server-clangd" {
            entry = &result.Entries[i]
            break
        }
    }
    if entry == nil {
        t.Fatalf("clangd LSP entry missing from scan; got %v", result.Entries)
    }
    if entry.Status != "via-hub" {
        t.Fatalf("Status = %q, want via-hub", entry.Status)
    }
    if got := entry.ClientPresence["codex"].Transport; got != "http" {
        t.Fatalf("codex transport = %q, want http", got)
    }
    if entry.LegacyConflict != nil {
        t.Fatalf("LegacyConflict should be empty for hub-only entry")
    }
}

func TestScanRecognizesCoexistenceAnomaly(t *testing.T) {
    tmpHome := t.TempDir()
    seedRegistry(t, tmpHome, []WorkspaceEntry{{
        WorkspaceKey:  "a1b2c3d4",
        WorkspacePath: "/proj/main",
        Language:      "rust",
        Port:          9202,
        ClientEntries: map[string]string{"codex": "mcp-language-server-rust"},
    }})
    // Seed codex with BOTH: hub URL entry AND a direct-stdio one
    // under a different name (operator added it manually).
    seedCodexConfig(t, tmpHome, map[string]map[string]any{
        "mcp-language-server-rust": {
            "url": "http://localhost:9202/mcp",
        },
        "rust-langserver-direct": {
            "command": "mcp-language-server",
            "args":    []any{"--lsp", "rust-analyzer", "--workspace", "/proj/main"},
        },
    })

    result, err := ScanFrom(tmpHome)
    if err != nil {
        t.Fatalf("ScanFrom: %v", err)
    }
    // The hub entry should remain at its own row.
    var hubEntry *ScanEntry
    for i := range result.Entries {
        if result.Entries[i].Name == "mcp-language-server-rust" {
            hubEntry = &result.Entries[i]
            break
        }
    }
    if hubEntry == nil {
        t.Fatalf("hub entry missing")
    }
    if hubEntry.ClientPresence["codex"].Transport != "http" {
        t.Fatalf("hub entry codex transport wrong")
    }
    // The direct-stdio entry under operator name surfaces under
    // either: (a) the same logical LSP row if recognition collapses
    // by language, OR (b) a separate Other-MCP row.
    // Per spec v4: when direct-stdio mcp-language-server entries
    // exist, recognition emits LegacyConflict on the canonical row.
    if hubEntry.LegacyConflict == nil || hubEntry.LegacyConflict["codex"].Transport != "stdio" {
        t.Fatalf("LegacyConflict should hold the codex stdio entry; got %+v", hubEntry.LegacyConflict)
    }
}
```

(Test helpers `seedRegistry` and `seedCodexConfig` are assumed to exist; if not, the test step also includes adding them. The implementer reads the scan_test.go to find or add them.)

- [ ] **Step 3.3.2: Verify tests fail**

Run:
```bash
go test ./internal/api/ -run TestScanRecognizes -v
```
Expected: FAIL — LegacyConflict not populated; recognition not implemented.

- [ ] **Step 3.3.3: Implement LSP recognition pass in `scan.go`**

Add to `internal/api/scan.go` after the existing entry-assembly loop (around line 280):

```go
// lspLanguages is the canonical 9-language set from
// servers/mcp-language-server/manifest.yaml.
var lspLanguages = []string{
    "clangd", "fortran", "go", "javascript", "python",
    "rust", "typescript", "vscode-css", "vscode-html",
}

// classifyLSPEntries walks every (entryName, client) pair and
// applies the three-rule recognition algorithm per spec v4
// §"Matrix LSP recognition". Hub-managed entries populate
// ClientPresence; direct-stdio entries on the same (client, lang)
// move to LegacyConflict on the canonical row.
//
// Pre-condition: entries is the in-progress per-name map of
// ScanEntry pointers; reg is the loaded workspace registry (may
// be nil if workspaces.yaml is missing — recognition degrades to
// language-labeling-only without ownership disambiguation).
func classifyLSPEntries(entries map[string]*ScanEntry, reg *Registry) {
    // Index hub-managed LSP entries by language for quick lookup
    // when matching direct-stdio conflicts.
    type lspKey struct {
        lang   string
        client string
    }
    hubByLang := map[lspKey]string{} // entryName

    for entryName, e := range entries {
        for clientName, ce := range e.ClientPresence {
            lang, _ := ParseEntryName(entryName, lspLanguages)
            if lang == "" {
                continue
            }

            // Rule 1: HTTP entry with hub URL → via-hub.
            if ce.Transport == "http" {
                e.Status = "via-hub"
                hubByLang[lspKey{lang, clientName}] = entryName
                continue
            }

            // Rule 2: stdio mcp-language-server invocation → legacy.
            // Rule 3: stdio gopls invocation (Go special-case) → legacy.
            if ce.Transport == "stdio" {
                if isLSPDirectStdio(ce, lang) {
                    // Find the canonical (hub) row for this language.
                    // If one exists for the same client, this entry
                    // becomes a LegacyConflict on it. Otherwise the
                    // current row stays.
                    if canonical, exists := findHubRowForLang(entries, lang, clientName); exists {
                        if canonical.LegacyConflict == nil {
                            canonical.LegacyConflict = map[string]ClientEntry{}
                        }
                        canonical.LegacyConflict[clientName] = ce
                        delete(e.ClientPresence, clientName)
                    } else {
                        e.Status = "legacy"
                    }
                }
            }
        }
    }

    // If reg is non-nil, we could surface workspace ownership labels
    // here. Per spec v4, ownership is informational — recognition
    // correctness does not depend on it.
    _ = reg
}

func isLSPDirectStdio(ce ClientEntry, lang string) bool {
    cmd := ""
    if c, ok := ce.Raw["command"].(string); ok {
        cmd = c
    }
    base := filepath.Base(cmd)
    base = strings.TrimSuffix(base, ".exe")
    if base == "mcp-language-server" {
        return true // Rule 2
    }
    if lang == "go" && base == "gopls" {
        // Rule 3: gopls with "mcp" in args.
        if args, ok := ce.Raw["args"].([]any); ok {
            for _, a := range args {
                if s, ok := a.(string); ok && s == "mcp" {
                    return true
                }
            }
        }
    }
    return false
}

func findHubRowForLang(entries map[string]*ScanEntry, lang, client string) (*ScanEntry, bool) {
    for _, e := range entries {
        canon, _ := ParseEntryName(e.Name, lspLanguages)
        if canon != lang {
            continue
        }
        if ce, ok := e.ClientPresence[client]; ok && ce.Transport == "http" {
            return e, true
        }
    }
    return nil, false
}
```

- [ ] **Step 3.3.4: Wire `classifyLSPEntries` into `ScanFrom`**

In `internal/api/scan.go`'s `ScanFrom` function (around line 280-290 where entries are finalized), add right before the function returns:

```go
    // v4 Phase 3: classify LSP entries — populate via-hub/legacy
    // Status + LegacyConflict for hub+stdio coexistence.
    reg, _ := LoadWorkspaceRegistry(homeDir) // best-effort; nil OK
    classifyLSPEntries(entries, reg)
```

(If `LoadWorkspaceRegistry` has a different name or path, grep for the existing registry loader and use it.)

- [ ] **Step 3.3.5: Run tests + verify pass**

Run:
```bash
go test ./internal/api/ -count=1 -timeout 2m
```
Expected: all PASS including 2 new scan tests.

- [ ] **Step 3.3.6: Full test suite**

Run:
```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 5m -short
```
Expected: clean.

- [ ] **Step 3.3.7: Commit**

```bash
git add internal/api/scan.go internal/api/scan_test.go
git commit -m "feat(scan): three-rule LSP recognition + LegacyConflict population

classifyLSPEntries walks entries after the per-client assembly pass:
- Rule 1: HTTP entry on hub URL → Status='via-hub', ClientPresence holds it
- Rule 2: stdio 'mcp-language-server' with --lsp arg → legacy
- Rule 3: stdio 'gopls mcp' (Go) → legacy

When hub + stdio coexist for same (client, lang), the stdio entry
moves from its own row's ClientPresence to the canonical row's
LegacyConflict[client]. ParseEntryName handles suffix collisions
(short 4hex / full 8hex) via longest-prefix matching to handle
hyphenated languages (vscode-css, vscode-html)."
```

### Task 3.4: Wire `daemon_env_overlay.LookupOverlay` into supervisor spawn

**Files:**
- Modify: `internal/cli/supervise.go` (replace `loadOverlayForTask` stub)

- [ ] **Step 3.4.1: Add `LookupOverlay` helper to `daemon_env_overlay` package**

Add to `internal/api/daemon_env_overlay/overlay.go`:

```go
// LookupOverlay returns the env map for taskName, or nil if no
// overlay row exists. Caller is expected to pass any form
// (canonical or bare); the helper normalizes internally.
func LookupOverlay(ov *Overlay, taskName string) map[string]string {
    if ov == nil {
        return nil
    }
    row, ok := ov.Daemons[NormalizeOverlayKey(taskName)]
    if !ok {
        return nil
    }
    return row.Env
}
```

- [ ] **Step 3.4.2: Replace `loadOverlayForTask` stub**

Edit `internal/cli/supervise.go`. Replace the stub:

```go
func loadOverlayForTask(taskName string) map[string]string {
    return nil
}
```

With:

```go
import (
    "mcp-local-hub/internal/api/daemon_env_overlay"
)

// loadOverlayForTask reads the overlay file once per spawn and
// returns the env map for taskName. Errors are logged but treated
// as "no overlay" — fail-LOUD on parse error happens at supervisor
// startup (Task 3.4.3), not per-spawn.
func loadOverlayForTask(taskName string) map[string]string {
    stateDir, err := stateDirPath()
    if err != nil {
        return nil
    }
    ov, err := daemon_env_overlay.Load(filepath.Join(stateDir, "daemon-env-overrides.yaml"))
    if err != nil {
        // Fail-LOUD at supervisor startup catches parse errors;
        // by the time we reach a spawn the overlay is either nil
        // (missing) or parsed-good.
        return nil
    }
    return daemon_env_overlay.LookupOverlay(ov, taskName)
}
```

- [ ] **Step 3.4.3: Add supervisor-startup fail-LOUD on parse error**

In `internal/cli/supervise.go`'s supervisor startup path (where `supervisor-intent.json` is loaded), add right after intent load:

```go
    // v4 §"Error handling": overlay parse failure is fail-LOUD;
    // supervisor refuses to spawn ANY daemon. Operator runs
    // `mcphub config overlay-quarantine`.
    overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")
    if _, err := daemon_env_overlay.Load(overlayPath); err != nil {
        return fmt.Errorf("daemon-env-overrides.yaml load failed: %w "+
            "(run `mcphub config overlay-quarantine` to rename it aside)", err)
    }
```

- [ ] **Step 3.4.4: Add fail-LOUD test**

Add to `internal/cli/supervise_test.go`:

```go
func TestSuperviseFailsLoudOnCorruptOverlay(t *testing.T) {
    stateDir := t.TempDir()
    overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")
    if err := os.WriteFile(overlayPath, []byte("not valid yaml: [::"), 0o600); err != nil {
        t.Fatalf("seed corrupt: %v", err)
    }
    err := runSuperviseStartupChecks(stateDir) // helper exposing the load check
    if err == nil {
        t.Fatalf("expected fail-LOUD error on corrupt overlay; got nil")
    }
    if !strings.Contains(err.Error(), "overlay-quarantine") {
        t.Fatalf("error should mention recovery command; got %q", err)
    }
}
```

- [ ] **Step 3.4.5: Expose `runSuperviseStartupChecks` for testing**

In `supervise.go`, extract the overlay-load check into a named function:

```go
// runSuperviseStartupChecks runs the fail-LOUD overlay parse check.
// Exposed for testing; production supervise() calls it at startup.
func runSuperviseStartupChecks(stateDir string) error {
    overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")
    if _, err := daemon_env_overlay.Load(overlayPath); err != nil {
        return fmt.Errorf("daemon-env-overrides.yaml load failed: %w "+
            "(run `mcphub config overlay-quarantine` to rename it aside)", err)
    }
    return nil
}
```

And call it from `supervise()` startup instead of inlining.

- [ ] **Step 3.4.6: Run tests + verify pass**

Run:
```bash
go test ./internal/cli/ ./internal/api/... -count=1 -timeout 5m
```
Expected: all PASS.

- [ ] **Step 3.4.7: Commit**

```bash
git add internal/cli/supervise.go internal/cli/supervise_test.go internal/api/daemon_env_overlay/overlay.go
git commit -m "feat(supervise): wire daemon_env_overlay into spawn + fail-LOUD on parse error

loadOverlayForTask reads daemon-env-overrides.yaml + looks up the
canonical-key row via NormalizeOverlayKey. mergeDaemonEnv now sees
the overlay env at every spawn.

Supervisor startup calls runSuperviseStartupChecks which runs the
overlay Load() once. Parse failure aborts startup with a message
pointing the operator at 'mcphub config overlay-quarantine'.
Per-daemon spawn paths assume the overlay parsed-good (else supervisor
never started)."
```

---

## Phase 4: GUI surface — endpoints + Servers.tsx changes

### Task 4.1: Add `respawn` IPC command on supervisor

**Files:**
- Modify: `internal/cli/supervise.go:921` (replace UNKNOWN_COMMAND stub)
- Modify: `internal/cli/supervise_respawn_test.go` (new file with IPC integration tests)

- [ ] **Step 4.1.1: Write failing IPC test**

Create `internal/cli/supervise_respawn_test.go`:

```go
package cli

import (
    "context"
    "testing"
    "time"

    "mcp-local-hub/internal/api"
)

func TestRespawnIPCValidTaskName(t *testing.T) {
    deps := newTestSupervisorDeps(t, []api.SupervisorDaemon{{
        TaskName: `\mcp-local-hub-memory-default`,
        Command:  "echo",
        Args:     []string{"hello"},
    }})
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    resp, err := callSupervisorIPC(ctx, deps, api.IPCRequest{
        Cmd: "respawn",
        Body: map[string]any{
            "task_name": `\mcp-local-hub-memory-default`,
            "force":     false,
        },
    })
    if err != nil {
        t.Fatalf("IPC respawn: %v", err)
    }
    if resp.Error != nil {
        t.Fatalf("expected nil error, got %+v", resp.Error)
    }
}

func TestRespawnIPCUnknownTaskName(t *testing.T) {
    deps := newTestSupervisorDeps(t, nil)
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    resp, err := callSupervisorIPC(ctx, deps, api.IPCRequest{
        Cmd: "respawn",
        Body: map[string]any{
            "task_name": `\nonexistent`,
            "force":     false,
        },
    })
    if err != nil {
        t.Fatalf("IPC call: %v", err)
    }
    if resp.Error == nil || resp.Error.Code != "UNKNOWN_TASK" {
        t.Fatalf("expected UNKNOWN_TASK error, got %+v", resp.Error)
    }
}

func TestRespawnIPCQuarantineRefusedWithoutForce(t *testing.T) {
    deps := newTestSupervisorDeps(t, []api.SupervisorDaemon{{
        TaskName: `\mcp-local-hub-foo-default`,
    }})
    setDaemonState(deps, `\mcp-local-hub-foo-default`, "quarantine")
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    resp, err := callSupervisorIPC(ctx, deps, api.IPCRequest{
        Cmd: "respawn",
        Body: map[string]any{
            "task_name": `\mcp-local-hub-foo-default`,
            "force":     false,
        },
    })
    if err != nil {
        t.Fatalf("IPC call: %v", err)
    }
    if resp.Error == nil || resp.Error.Code != "QUARANTINED" {
        t.Fatalf("expected QUARANTINED error, got %+v", resp.Error)
    }
}
```

(`newTestSupervisorDeps`, `callSupervisorIPC`, `setDaemonState` are test-only helpers; if absent, sub-step 4.1.2 adds them.)

- [ ] **Step 4.1.2: Verify tests fail**

Run:
```bash
go test ./internal/cli/ -run TestRespawnIPC -v
```
Expected: FAIL — `respawn` still returns UNKNOWN_COMMAND.

- [ ] **Step 4.1.3: Replace the stub at line 921**

Edit `internal/cli/supervise.go` around line 916-934. Replace the `case "restart", "reload":` branch with:

```go
    case "respawn":
        return handleRespawn(conn, req, deps)
```

Add the handler function nearby:

```go
// handleRespawn processes the `respawn` IPC command. Body shape:
//
//   {task_name: string, force: bool}
//
// Returns UNKNOWN_TASK if task_name isn't in current intent.
// Returns QUARANTINED if the daemon is in Quarantined state and
// force is false. Otherwise: graceful 5s shutdown → SIGKILL/Job-kill
// → spawn with current intent+overlay. Emits lifecycle events.
func handleRespawn(conn net.Conn, req api.IPCRequest, deps supervisorDeps) error {
    taskName, _ := req.Body["task_name"].(string)
    force, _ := req.Body["force"].(bool)
    taskName = daemon_env_overlay.NormalizeOverlayKey(taskName)

    desc := deps.intent.FindDaemon(taskName)
    if desc == nil {
        return writeIPCFrame(conn, api.IPCResponse{
            ID: req.ID, Final: true,
            Error: &api.IPCErr{Code: "UNKNOWN_TASK", Message: "task not in supervisor-intent.json: " + taskName},
        })
    }

    state := deps.tracker.GetState(taskName)
    if state == "quarantine" && !force {
        return writeIPCFrame(conn, api.IPCResponse{
            ID: req.ID, Final: true,
            Error: &api.IPCErr{Code: "QUARANTINED", Message: "daemon in quarantine; pass force=true to override"},
        })
    }

    // Graceful shutdown 5s, then force-kill, then spawn.
    if err := deps.terminator.TerminateGraceful(taskName, 5*time.Second); err != nil {
        deps.events.Emit(api.SupervisorEvent{Event: "respawn-graceful-timeout", TaskName: taskName})
    }
    if err := deps.spawn(*desc); err != nil {
        return writeIPCFrame(conn, api.IPCResponse{
            ID: req.ID, Final: true,
            Error: &api.IPCErr{Code: "SPAWN_FAILED", Message: err.Error()},
        })
    }
    deps.events.Emit(api.SupervisorEvent{Event: "supervisor-respawn-via-gui", TaskName: taskName})
    return writeIPCFrame(conn, api.IPCResponse{
        ID:    req.ID,
        Final: true,
        Body:  map[string]any{"status": "respawned"},
    })
}
```

(Adjust `deps` field names to match actual `supervisorDeps` shape in `supervise.go`.)

- [ ] **Step 4.1.4: Run tests + verify pass**

Run:
```bash
go test ./internal/cli/ -run TestRespawnIPC -count=1 -v
```
Expected: 3 PASS.

- [ ] **Step 4.1.5: Commit**

```bash
git add internal/cli/supervise.go internal/cli/supervise_respawn_test.go
git commit -m "feat(supervise): replace UNKNOWN_COMMAND stub with respawn IPC handler

Replaces the deferred 'restart'/'reload' stub at supervise.go:921.
The respawn handler accepts {task_name, force: bool}, validates the
task is in supervisor-intent.json (UNKNOWN_TASK on miss), refuses
Quarantined daemons without force (QUARANTINED error), and performs
graceful 5s shutdown → force-kill → spawn with current intent+overlay.

Emits respawn-graceful-timeout (warn) when soft shutdown exceeds 5s
and supervisor-respawn-via-gui (info) on success."
```

### Task 4.2: GUI handlers `/api/daemon/env`, `/api/discovery/refresh`, `/api/daemon/respawn`

**Files:**
- Modify: `internal/gui/server.go` (route registration)
- Create: `internal/gui/daemon_env_handler.go`
- Create: `internal/gui/discovery_refresh_handler.go`
- Create: `internal/gui/daemon_respawn_handler.go`
- Create: `internal/gui/daemon_env_handler_test.go`

- [ ] **Step 4.2.1: Write failing tests**

Create `internal/gui/daemon_env_handler_test.go`:

```go
package gui

import (
    "bytes"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
)

func TestDaemonEnvHandlerRequiresSameOrigin(t *testing.T) {
    s := newTestGUIServer(t)
    req := httptest.NewRequest(http.MethodPost, "/api/daemon/env",
        bytes.NewReader([]byte(`{"task_name":"\\mcp-local-hub-gdb-default","env":{"Path":"x"}}`)))
    req.Header.Set("Origin", "https://evil.com")
    w := httptest.NewRecorder()
    s.ServeHTTP(w, req)
    if w.Code != http.StatusForbidden {
        t.Fatalf("expected 403 cross-origin, got %d", w.Code)
    }
}

func TestDaemonEnvHandlerRejectsUnknownTaskName(t *testing.T) {
    s := newTestGUIServer(t)
    body := map[string]any{
        "task_name": `\nonexistent`,
        "env":       map[string]string{"Path": "x"},
    }
    raw, _ := json.Marshal(body)
    req := httptest.NewRequest(http.MethodPost, "/api/daemon/env", bytes.NewReader(raw))
    req.Host = "127.0.0.1:9125"
    w := httptest.NewRecorder()
    s.ServeHTTP(w, req)
    if w.Code != http.StatusBadRequest {
        t.Fatalf("expected 400 unknown task, got %d (body: %s)", w.Code, w.Body.String())
    }
}

func TestDaemonEnvHandlerWritesOverlayOnSuccess(t *testing.T) {
    s := newTestGUIServer(t)
    seedTestIntent(t, s, []string{`\mcp-local-hub-gdb-default`})
    body := map[string]any{
        "task_name": `\mcp-local-hub-gdb-default`,
        "env":       map[string]string{"Path": "C:/msys64/ucrt64/bin;${parent_path}"},
    }
    raw, _ := json.Marshal(body)
    req := httptest.NewRequest(http.MethodPost, "/api/daemon/env", bytes.NewReader(raw))
    req.Host = "127.0.0.1:9125"
    w := httptest.NewRecorder()
    s.ServeHTTP(w, req)
    if w.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
    }
    // Verify overlay file written.
    ov := readTestOverlay(t, s)
    if got := ov.Daemons[`\mcp-local-hub-gdb-default`].Env["Path"]; got == "" {
        t.Fatalf("Path missing from overlay after handler success")
    }
}
```

- [ ] **Step 4.2.2: Verify tests fail**

Run:
```bash
go test ./internal/gui/ -run TestDaemonEnvHandler -v
```
Expected: FAIL — `/api/daemon/env` returns 404.

- [ ] **Step 4.2.3: Implement `daemon_env_handler.go`**

Create `internal/gui/daemon_env_handler.go`:

```go
package gui

import (
    "encoding/json"
    "fmt"
    "net/http"
    "path/filepath"
    "regexp"
    "strings"

    "mcp-local-hub/internal/api"
    "mcp-local-hub/internal/api/daemon_env_overlay"
)

type daemonEnvRequest struct {
    TaskName string            `json:"task_name"`
    Env      map[string]string `json:"env"`
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s *Server) handleDaemonEnv(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "POST only", http.StatusMethodNotAllowed)
        return
    }
    var req daemonEnvRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIError(w, fmt.Errorf("decode body: %w", err), http.StatusBadRequest, "BAD_REQUEST")
        return
    }
    taskName := daemon_env_overlay.NormalizeOverlayKey(req.TaskName)
    if !s.isKnownTask(taskName) {
        writeAPIError(w, fmt.Errorf("unknown task_name %q", taskName), http.StatusBadRequest, "UNKNOWN_TASK")
        return
    }
    for k, v := range req.Env {
        if !envKeyPattern.MatchString(k) {
            writeAPIError(w, fmt.Errorf("invalid env key %q", k), http.StatusBadRequest, "INVALID_KEY")
            return
        }
        if strings.ContainsAny(v, "\n\x00") || hasControlChar(v) {
            writeAPIError(w, fmt.Errorf("invalid env value for key %q", k), http.StatusBadRequest, "INVALID_VALUE")
            return
        }
    }

    overlayPath := filepath.Join(s.stateDir(), "daemon-env-overrides.yaml")
    err := daemon_env_overlay.WriteOverlay(overlayPath, func(ov *daemon_env_overlay.Overlay) error {
        row := ov.Daemons[taskName]
        if row.Env == nil {
            row.Env = map[string]string{}
        }
        for k, v := range req.Env {
            row.Env[k] = v
        }
        row.Source = "operator"
        ov.Daemons[taskName] = row
        return nil
    })
    if err != nil {
        writeAPIError(w, err, http.StatusInternalServerError, "WRITE_FAILED")
        return
    }

    api.EmitHubMcpEvent(api.HubMcpEvent{
        Type:    "daemon-env-overlay-applied-via-gui",
        Subject: taskName,
    })

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}

func hasControlChar(s string) bool {
    for _, r := range s {
        if r < 0x20 && r != '\t' {
            return true
        }
    }
    return false
}
```

- [ ] **Step 4.2.4: Implement `discovery_refresh_handler.go`**

Create `internal/gui/discovery_refresh_handler.go`:

```go
package gui

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "path/filepath"
    "time"

    "mcp-local-hub/internal/api/binary_discovery"
    "mcp-local-hub/internal/api/daemon_env_overlay"
)

type discoveryRefreshRequest struct {
    Server string `json:"server"`
    Daemon string `json:"daemon"`
}

func (s *Server) handleDiscoveryRefresh(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "POST only", http.StatusMethodNotAllowed)
        return
    }
    var req discoveryRefreshRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIError(w, err, http.StatusBadRequest, "BAD_REQUEST")
        return
    }
    // For simplicity Phase 4 implements 'all' only; per-daemon refresh
    // is a v2 follow-up.
    requiredBinaries := s.gatherRequiredBinaries()
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    found, err := binary_discovery.Discover(ctx, requiredBinaries, binary_discovery.DefaultHints())
    if err != nil {
        writeAPIError(w, err, http.StatusInternalServerError, "DISCOVER_FAILED")
        return
    }
    // Write found bins per task per spec §"Auto-discovery at install".
    overlayPath := filepath.Join(s.stateDir(), "daemon-env-overrides.yaml")
    if err := daemon_env_overlay.WriteOverlay(overlayPath, func(ov *daemon_env_overlay.Overlay) error {
        for taskName, binPath := range s.binPathsToTaskNames(found) {
            row := ov.Daemons[taskName]
            if row.Source == "operator" {
                continue // CAS: skip operator-tagged rows
            }
            if row.Env == nil {
                row.Env = map[string]string{}
            }
            row.Env["Path"] = fmt.Sprintf("%s;${parent_path}", binPath)
            row.Source = "auto-discovery"
            row.DiscoveredAt = time.Now().UTC().Format(time.RFC3339Nano)
            ov.Daemons[taskName] = row
        }
        return nil
    }); err != nil {
        writeAPIError(w, err, http.StatusInternalServerError, "WRITE_FAILED")
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]any{"status": "ok", "found": found})
}

// gatherRequiredBinaries returns the union of required_binaries
// declared across all loaded manifests. Helper used to build the
// discovery list.
func (s *Server) gatherRequiredBinaries() []string {
    return s.requiredBinaries() // assumes server holds manifests via existing seam
}

// binPathsToTaskNames maps binary→absPath into taskName→Path string
// using the manifest's RequiredBinaries declarations and the per-
// daemon SupervisorDaemon.TaskName from intent.
func (s *Server) binPathsToTaskNames(found map[string]string) map[string]string {
    return s.mapBinPathsToTaskNames(found) // implementation detail; routes via existing manifest loader
}
```

- [ ] **Step 4.2.5: Implement `daemon_respawn_handler.go`**

Create `internal/gui/daemon_respawn_handler.go`:

```go
package gui

import (
    "encoding/json"
    "fmt"
    "net/http"

    "mcp-local-hub/internal/api"
    "mcp-local-hub/internal/api/daemon_env_overlay"
)

type daemonRespawnRequest struct {
    TaskName string `json:"task_name"`
    Force    bool   `json:"force"`
}

func (s *Server) handleDaemonRespawn(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "POST only", http.StatusMethodNotAllowed)
        return
    }
    var req daemonRespawnRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIError(w, err, http.StatusBadRequest, "BAD_REQUEST")
        return
    }
    taskName := daemon_env_overlay.NormalizeOverlayKey(req.TaskName)
    if !s.isKnownTask(taskName) {
        writeAPIError(w, fmt.Errorf("unknown task"), http.StatusBadRequest, "UNKNOWN_TASK")
        return
    }
    resp, err := api.CallSupervisorIPC(api.IPCRequest{
        Cmd: "respawn",
        Body: map[string]any{
            "task_name": taskName,
            "force":     req.Force,
        },
    })
    if err != nil {
        writeAPIError(w, err, http.StatusInternalServerError, "IPC_FAILED")
        return
    }
    if resp.Error != nil {
        // Map QUARANTINED → 409.
        if resp.Error.Code == "QUARANTINED" {
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusConflict)
            json.NewEncoder(w).Encode(map[string]any{
                "state":  "quarantined",
                "remedy": "force or unquarantine",
            })
            return
        }
        writeAPIError(w, fmt.Errorf(resp.Error.Message), http.StatusBadRequest, resp.Error.Code)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}
```

- [ ] **Step 4.2.6: Register routes**

Edit `internal/gui/server.go` (wherever mux routes are registered):

```go
    mux.HandleFunc("/api/daemon/env", s.requireSameOrigin(s.handleDaemonEnv))
    mux.HandleFunc("/api/discovery/refresh", s.requireSameOrigin(s.handleDiscoveryRefresh))
    mux.HandleFunc("/api/daemon/respawn", s.requireSameOrigin(s.handleDaemonRespawn))
```

- [ ] **Step 4.2.7: Run tests + verify pass**

Run:
```bash
go test ./internal/gui/ -count=1 -timeout 5m
```
Expected: all PASS.

- [ ] **Step 4.2.8: Commit**

```bash
git add internal/gui/server.go internal/gui/daemon_env_handler.go internal/gui/discovery_refresh_handler.go internal/gui/daemon_respawn_handler.go internal/gui/daemon_env_handler_test.go
git commit -m "feat(gui): add /api/daemon/env, /api/discovery/refresh, /api/daemon/respawn

All three reuse requireSameOrigin middleware (CSRF defense per
internal/gui/csrf.go:81-99). Known-task validation via supervisor-intent.json
lookup; env key/value validation (regex + control-char refusal).

/api/daemon/env writes overlay via WriteOverlay (no IPC; operator
restarts daemon manually via /api/daemon/respawn).

/api/discovery/refresh runs binary_discovery.Discover + writes
overlay with source:auto-discovery (CAS skips source:operator rows).

/api/daemon/respawn dispatches IPC; maps QUARANTINED → HTTP 409."
```

### Task 4.3: Frontend Servers.tsx — active workspace selector + 9 LSP rows + drawer

**Files:**
- Modify: `internal/gui/frontend/src/screens/Servers.tsx`
- Modify: `internal/gui/frontend/src/api.ts` (add three new POST functions)
- Modify: `internal/gui/frontend/src/screens/Servers.css` (styling)

- [ ] **Step 4.3.1: Add API client functions**

Edit `internal/gui/frontend/src/api.ts` — add:

```typescript
export async function applyDaemonEnv(taskName: string, env: Record<string, string>): Promise<void> {
  const res = await fetch('/api/daemon/env', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ task_name: taskName, env }),
  });
  if (!res.ok) throw new Error(`apply env: ${res.status} ${await res.text()}`);
}

export async function refreshDiscovery(server: string = '', daemon: string = 'all'): Promise<{ found: Record<string, string> }> {
  const res = await fetch('/api/discovery/refresh', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ server, daemon }),
  });
  if (!res.ok) throw new Error(`refresh discovery: ${res.status}`);
  return res.json();
}

export async function respawnDaemon(taskName: string, force: boolean = false): Promise<void> {
  const res = await fetch('/api/daemon/respawn', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ task_name: taskName, force }),
  });
  if (res.status === 409) {
    throw new Error('daemon quarantined — pass force=true');
  }
  if (!res.ok) throw new Error(`respawn: ${res.status}`);
}
```

- [ ] **Step 4.3.2: Add LSP rows + active-workspace selector to Servers.tsx**

Edit `internal/gui/frontend/src/screens/Servers.tsx`. Add near the top of the component:

```typescript
const LSP_LANGUAGES = [
  'clangd', 'fortran', 'go', 'javascript', 'python',
  'rust', 'typescript', 'vscode-css', 'vscode-html',
];

function workspaceSelector(workspaces: WorkspaceEntry[], active: string, onChange: (k: string) => void) {
  if (workspaces.length === 0) {
    return <div class="ws-selector empty">(none — register a workspace first)</div>;
  }
  return (
    <select class="ws-selector" value={active} onChange={(e) => onChange((e.target as HTMLSelectElement).value)}>
      {workspaces.map(w => (
        <option key={w.workspace_key} value={w.workspace_key}>{w.workspace_path}</option>
      ))}
    </select>
  );
}
```

In the table rendering, ensure LSP rows always render (even when ClientPresence is empty) — synthesize 9 placeholder rows when no entries match.

- [ ] **Step 4.3.3: Add per-row drawer with env editor + restart button**

Add a drawer component:

```typescript
function EnvDrawer({ entry, taskName, onClose }: { entry: ScanEntry; taskName: string; onClose: () => void }) {
  const [pathValue, setPathValue] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [force, setForce] = useState(false);

  const hasParentPathToken = pathValue.includes('${parent_path}');

  const onApply = async () => {
    setError(null);
    try {
      await applyDaemonEnv(taskName, { Path: pathValue });
    } catch (e) {
      setError(String(e));
    }
  };

  const onRestart = async () => {
    setError(null);
    try {
      await respawnDaemon(taskName, force);
      onClose();
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <div class="env-drawer">
      <h3>{entry.name}</h3>
      <label>
        PATH:
        <textarea value={pathValue} onChange={(e) => setPathValue((e.target as HTMLTextAreaElement).value)} />
      </label>
      {!hasParentPathToken && (
        <div class="warning-chip">
          PATH does not include ${'{parent_path}'} — parent PATH will be DROPPED for this daemon
        </div>
      )}
      <button onClick={onApply}>Apply</button>
      <label>
        <input type="checkbox" checked={force} onChange={(e) => setForce((e.target as HTMLInputElement).checked)} />
        Force restart (required for Quarantined daemons)
      </label>
      <button onClick={onRestart}>Restart daemon to apply</button>
      {error && <div class="error">{error}</div>}
      <button onClick={onClose}>Close</button>
    </div>
  );
}
```

Wire the drawer into the row click handler in the existing Servers.tsx matrix.

- [ ] **Step 4.3.4: Rebuild frontend bundle**

Run:
```bash
cd internal/gui/frontend && npm run build
```
Expected: build completes, writes to `internal/gui/assets/{index.html,app.js,style.css}`.

- [ ] **Step 4.3.5: Run frontend unit + typecheck**

Run:
```bash
cd internal/gui/frontend && npm run typecheck && npm run test
```
Expected: clean.

- [ ] **Step 4.3.6: Commit**

```bash
git add internal/gui/frontend/ internal/gui/assets/
git commit -m "feat(frontend): add active-workspace selector + 9 LSP rows + env drawer

Servers.tsx now renders exactly 9 LSP language rows (one per
manifest language) always, even when no entries exist. Active
workspace selector at top-of-screen, sourced from /api/registry.

Per-row drawer with env editor: textarea for Path, warning chip
when ${parent_path} token absent (PATH-drop foot-gun mitigation),
Apply button → POST /api/daemon/env, Restart button → POST
/api/daemon/respawn with force checkbox for Quarantined override.

Bundle rebuilt via npm run build."
```

### Task 4.4: E2E tests with Playwright

**Files:**
- Create: `internal/gui/e2e/tests/servers-lsp.spec.ts`
- Create: `internal/gui/e2e/tests/servers-env-overlay.spec.ts`
- Create: `internal/gui/e2e/tests/servers-coexistence-anomaly.spec.ts`

- [ ] **Step 4.4.1: Write E2E test for 9-row baseline**

Create `internal/gui/e2e/tests/servers-lsp.spec.ts`:

```typescript
import { test, expect } from '@playwright/test';
import { startHub } from '../fixtures/hub';

test('Servers matrix shows exactly 9 LSP rows', async ({ page }) => {
  const hub = await startHub();
  await page.goto(hub.url + '/#/servers');
  const lspLangs = ['clangd', 'fortran', 'go', 'javascript', 'python', 'rust', 'typescript', 'vscode-css', 'vscode-html'];
  for (const lang of lspLangs) {
    await expect(page.locator(`[data-testid="lsp-row-${lang}"]`)).toBeVisible();
  }
  await hub.stop();
});
```

- [ ] **Step 4.4.2: Write E2E test for env overlay drawer**

Create `internal/gui/e2e/tests/servers-env-overlay.spec.ts`:

```typescript
import { test, expect } from '@playwright/test';
import { startHub } from '../fixtures/hub';

test('per-row drawer shows ${parent_path} warning when token absent', async ({ page }) => {
  const hub = await startHub();
  await page.goto(hub.url + '/#/servers');
  await page.locator('[data-testid="lsp-row-clangd"]').click();
  const drawer = page.locator('.env-drawer');
  await expect(drawer).toBeVisible();
  await drawer.locator('textarea').fill('C:/no-parent');
  await expect(drawer.locator('.warning-chip')).toBeVisible();
  await drawer.locator('textarea').fill('C:/has-parent;${parent_path}');
  await expect(drawer.locator('.warning-chip')).not.toBeVisible();
  await hub.stop();
});
```

- [ ] **Step 4.4.3: Write E2E test for coexistence anomaly**

Create `internal/gui/e2e/tests/servers-coexistence-anomaly.spec.ts`:

```typescript
import { test, expect } from '@playwright/test';
import { startHub, seedCoexistence } from '../fixtures/hub';

test('coexistence anomaly cell renders both [via-hub] and [legacy] badges', async ({ page }) => {
  const hub = await startHub();
  await seedCoexistence(hub, 'codex', 'rust');
  await page.goto(hub.url + '/#/servers');
  const cell = page.locator('[data-testid="lsp-row-rust"] [data-testid="cell-codex"]');
  await expect(cell.locator('.badge-hub')).toBeVisible();
  await expect(cell.locator('.badge-legacy')).toBeVisible();
  await hub.stop();
});
```

- [ ] **Step 4.4.4: Run E2E tests**

Run:
```bash
cd internal/gui/e2e && npm test
```
Expected: all PASS. (May skip on non-Windows due to scheduler dependency per CLAUDE.md.)

- [ ] **Step 4.4.5: Commit**

```bash
git add internal/gui/e2e/tests/
git commit -m "test(e2e): cover servers matrix LSP rows + env drawer + coexistence anomaly

Three new Playwright specs:
- servers-lsp: 9 LSP rows always present
- servers-env-overlay: ${parent_path} warning chip toggles on textarea content
- servers-coexistence-anomaly: dual-badge rendering for hub+legacy same cell"
```

---

## Final verification

- [ ] **Step F.1: Full backend test suite**

Run:
```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 5m
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```
Expected: clean exits.

- [ ] **Step F.2: Sweep test processes (per memory rule)**

Run (PowerShell):
```powershell
Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force
```

- [ ] **Step F.3: Frontend build + unit tests + e2e**

Run:
```bash
cd internal/gui/frontend && npm run build && npm run test && npm run typecheck
cd ../e2e && npm test
```
Expected: clean.

- [ ] **Step F.4: Push branch + open PR**

Run:
```bash
git push -u origin master
gh pr create --title "feat(servers-matrix): LSP recognition + per-daemon env overlay" --body "$(cat <<'EOF'
## Summary

- Auto-discover binary install locations at install + add 'Refresh discovery' GUI button
- Per-daemon env overlay file: edit PATH from GUI, supervisor merges with manifest env at spawn
- Servers matrix recognizes the 9 LSP-bridge languages as proper rows (clangd, fortran, go, javascript, python, rust, typescript, vscode-css, vscode-html)
- New respawn IPC command replaces UNKNOWN_COMMAND stub at supervise.go:921
- Coexistence anomaly (hub + legacy direct-stdio) renders both badges via new ScanEntry.LegacyConflict field

## Test plan

- [ ] go test ./... passes
- [ ] Codex bot review on HEAD commit
- [ ] Manual smoke: PATH edit via GUI + restart triggers daemon spawn with new env
- [ ] Manual smoke: multi-workspace clangd registration shows correct rows
EOF
)"
```

---

## Self-review

**Spec coverage:** I walked v4's Goal, Scope, Architecture summary, Schema change, Canonical key namespace, Data flow, Components, Read-side hardening, Error handling, mcphub config overlay-quarantine, Manifest schema additions, DefaultHints, Security, Observability, Testing strategy, and Migration sections. Every section maps to one or more tasks above:

| Spec section | Tasks |
|---|---|
| Goal / Scope | Phase 1-4 |
| Architecture summary 4 pieces | Phases 1-4 respectively |
| Schema change ScanEntry.LegacyConflict | 3.1 |
| Canonical key namespace + NormalizeOverlayKey | 2.1, 3.4 |
| Spawn-time env merge | 2.5 |
| Auto-discovery at install | 1.3, 4.2 (discovery refresh) |
| Apply env edit from GUI | 4.2 daemon_env_handler |
| Respawn from GUI | 4.1 IPC + 4.2 daemon_respawn_handler |
| Matrix LSP recognition three-rule | 3.2, 3.3 |
| Components table 12 files | covered across tasks |
| Manifest schema additions | 1.1, 1.2 |
| DefaultHints version-agnostic | 1.3 |
| Read-side hardening Windows reparse-point | 2.4 |
| /api/daemon/env auth posture | 4.2 (uses requireSameOrigin) |
| ${parent_path} GUI warning chip | 4.3 |
| Error handling fail-LOUD | 3.4 |
| mcphub config overlay-quarantine | 2.6 |
| Migration / open questions | no implementation task — informational |

**Placeholder scan:** No "TBD" / "TODO" / "fill in later" placeholders. Every code step has actual code. Some platform-specific Windows `unsafe.Pointer` ceremony in Task 2.4 is sketched rather than literal — the implementer reads the existing `hub_mcp_state_dacl_windows.go` pattern as the authoritative model.

**Type consistency:** `Overlay.Daemons map[string]DaemonRow`, `DaemonRow.Env map[string]string`, `ScanEntry.LegacyConflict map[string]ClientEntry`, `NormalizeOverlayKey(taskName) string`, `WriteOverlay(path, mutator func(*Overlay) error) error`, `LookupOverlay(ov, taskName) map[string]string` — types are consistent across tasks.

**Cross-task references:** Task 2.2 stubs `hardenedOpen`; Task 2.4 replaces it. Task 2.5 stubs `loadOverlayForTask`; Task 3.4 wires it. Both transitions are explicit; subsequent task code references the prior stub by name.
