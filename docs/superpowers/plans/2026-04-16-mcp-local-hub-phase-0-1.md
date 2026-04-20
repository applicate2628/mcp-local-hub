# mcp-local-hub Phase 0 + Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Go-based `mcp-local-hub` CLI that can install Serena as three shared daemons (claude-code/codex/antigravity contexts) serving all four MCP clients on this workstation, replacing per-session stdio subprocesses.

**Architecture:** Single Go binary with OS-abstracted scheduler (Windows first, Linux/macOS stubs), age-encrypted secrets, YAML manifests per MCP server, per-client config writers that preserve backups, and a `daemon` subcommand launched by the OS scheduler that execs the target MCP server with resolved env + tee'd logs.

**Tech Stack:** Go 1.22+, `github.com/spf13/cobra` (CLI), `gopkg.in/yaml.v3` (manifest parsing), `github.com/BurntSushi/toml` (Codex config), `filippo.io/age` (encryption), `golang.org/x/sys/windows` (Task Scheduler integration), `github.com/atotto/clipboard` (secret clipboard copy), standard `testing` package for tests.

**Spec reference:** `docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md` (relative to repo root)

**Prerequisites before Task 1:**
- Go 1.22+ installed (`go version` must succeed)
- Git for Windows installed
- User decides final GitHub repo path (the plan uses placeholder `github.com/dima/mcp-local-hub` — replace in Task 1 with actual path)
- User on Windows 11 (Windows adapter is first-class; Linux/macOS are compile-only stubs in this plan)

---

## File Structure

The plan produces this directory layout (all paths relative to repo root):

```
mcp-local-hub/
├── README.md
├── LICENSE
├── .gitignore
├── go.mod, go.sum
├── cmd/mcp/main.go                         # CLI entry point, subcommand routing
├── internal/
│   ├── config/
│   │   ├── manifest.go                     # ServerManifest struct + YAML parser
│   │   ├── manifest_test.go
│   │   ├── ports.go                        # ports.yaml reader + conflict detection
│   │   └── ports_test.go
│   ├── secrets/
│   │   ├── vault.go                        # age-based encrypt/decrypt
│   │   ├── vault_test.go
│   │   ├── resolver.go                     # secret: / file: / env: / literal resolution
│   │   └── resolver_test.go
│   ├── scheduler/
│   │   ├── scheduler.go                    # Scheduler interface + common types
│   │   ├── scheduler_windows.go            # schtasks-based implementation
│   │   ├── scheduler_windows_test.go
│   │   ├── scheduler_linux.go              # systemd-user units (compile-only stub)
│   │   └── scheduler_darwin.go             # launchd agents (compile-only stub)
│   ├── clients/
│   │   ├── clients.go                      # Client interface
│   │   ├── claude_code.go                  # ~/.claude/settings.json
│   │   ├── claude_code_test.go
│   │   ├── codex_cli.go                    # ~/.codex/config.toml
│   │   ├── codex_cli_test.go
│   │   ├── gemini_cli.go                   # ~/.gemini/settings.json
│   │   ├── gemini_cli_test.go
│   │   ├── antigravity.go                  # ~/.gemini/antigravity/mcp_config.json
│   │   └── antigravity_test.go
│   ├── daemon/
│   │   ├── launcher.go                     # child-process launch + log tee
│   │   ├── launcher_test.go
│   │   ├── logrotate.go                    # 10MB rotation, keep last 5
│   │   └── bridge.go                       # stdio-bridge via supergateway spawn
│   └── cli/
│       ├── root.go                         # cobra root command
│       ├── install.go
│       ├── uninstall.go
│       ├── status.go
│       ├── rollback.go
│       ├── restart.go
│       ├── daemon.go                       # invoked by scheduler
│       └── secrets.go                      # all `mcp secrets *` subcommands
├── servers/
│   ├── _template/
│   │   ├── manifest.yaml
│   │   └── README.md
│   └── serena/
│       ├── manifest.yaml
│       └── README.md
├── configs/
│   ├── config.example.yaml                 # template
│   └── ports.yaml                          # central port registry
├── secrets.age                             # committed empty vault (base64 "empty")
└── docs/
    └── (existing spec at ../specs/ stays in place)
```

---

## Phase 0 — Foundation Tasks

### Task 1: Initialize Go project and commit scaffold

**Files:**
- Create: `go.mod`, `.gitignore`, `README.md`, `LICENSE`, `cmd/mcp/main.go`

- [ ] **Step 1: cd into the pre-seeded repo directory and initialize git**

The repo directory `d:\dev\mcp-local-hub\` already exists with `docs/superpowers/{specs,plans}/` populated (this spec and plan). Do not `mkdir` — just enter and initialize git.

```bash
cd /d/dev/mcp-local-hub
git init
```

- [ ] **Step 2: Initialize Go module**

Run: `go mod init github.com/dima/mcp-local-hub` (replace `github.com/dima/mcp-local-hub` with the actual GitHub username/repo path the user confirms).

Expected output: `go: creating new go.mod: module github.com/dima/mcp-local-hub`

- [ ] **Step 3: Create `.gitignore`**

```gitignore
# Identity (NEVER commit)
.age-key

# Local config (gitignored per §3.8 of spec)
config.local.yaml

# Build outputs
/mcp
/mcp.exe
/bin/

# Go
*.test
*.prof
coverage.out

# Editor
.vscode/
.idea/
*.swp
```

- [ ] **Step 4: Create minimal `cmd/mcp/main.go`**

```go
package main

import "fmt"

func main() {
	fmt.Println("mcp-local-hub: CLI entry point — Task 1 stub")
}
```

- [ ] **Step 5: Create `README.md`**

```markdown
# mcp-local-hub

Local shared-daemon manager for MCP (Model Context Protocol) servers.

See [design spec](docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md) for architecture.

Status: under construction (Phase 0-1 implementation).
```

- [ ] **Step 6: Create minimal `LICENSE` (MIT placeholder)**

```
MIT License

Copyright (c) 2026 Dmitry

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

- [ ] **Step 7: Verify build**

Run: `go build -o mcp.exe ./cmd/mcp`
Expected: no output, `mcp.exe` exists in repo root.

Run: `.\mcp.exe`
Expected output: `mcp-local-hub: CLI entry point — Task 1 stub`

- [ ] **Step 8: Commit**

```bash
git add .gitignore go.mod README.md LICENSE cmd/mcp/main.go docs/
git commit -m "chore: initialize Go project scaffold with pre-seeded design/plan docs"
```

---

### Task 2: Add cobra CLI framework with subcommand stubs

**Files:**
- Modify: `cmd/mcp/main.go`
- Create: `internal/cli/root.go`

- [ ] **Step 1: Add cobra dependency**

Run: `go get github.com/spf13/cobra@latest`
Expected output: `go: added github.com/spf13/cobra v1.x.y`

- [ ] **Step 2: Write `internal/cli/root.go`**

```go
package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCmd builds the top-level `mcp` command with all subcommand stubs attached.
// Subcommand implementations are filled in by later tasks; this task only wires the tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "mcp",
		Short:         "Local shared-daemon manager for MCP servers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newInstallCmd())
	root.AddCommand(newUninstallCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newRestartCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newSecretsCmd())
	return root
}

// Stub constructors — each returns a cobra.Command that prints "not implemented yet".
// Later tasks replace each RunE with real logic.
func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		RunE:  stub("install"),
	}
}
func newUninstallCmd() *cobra.Command {
	return &cobra.Command{Use: "uninstall", Short: "Uninstall server(s)", RunE: stub("uninstall")}
}
func newStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show daemon status", RunE: stub("status")}
}
func newRestartCmd() *cobra.Command {
	return &cobra.Command{Use: "restart", Short: "Restart daemon(s)", RunE: stub("restart")}
}
func newRollbackCmd() *cobra.Command {
	return &cobra.Command{Use: "rollback", Short: "Restore pre-install client configs", RunE: stub("rollback")}
}
func newDaemonCmd() *cobra.Command {
	return &cobra.Command{Use: "daemon", Short: "Run a daemon (invoked by scheduler)", RunE: stub("daemon")}
}
func newSecretsCmd() *cobra.Command {
	return &cobra.Command{Use: "secrets", Short: "Manage encrypted secrets", RunE: stub("secrets")}
}

func stub(name string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		cmd.Printf("mcp %s: not implemented yet\n", name)
		return nil
	}
}
```

- [ ] **Step 3: Rewrite `cmd/mcp/main.go`**

```go
package main

import (
	"fmt"
	"os"

	"github.com/dima/mcp-local-hub/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Verify build and subcommand stubs respond**

Run: `go build -o mcp.exe ./cmd/mcp`
Expected: no output.

Run: `.\mcp.exe --help`
Expected: help text listing `install`, `uninstall`, `status`, `restart`, `rollback`, `daemon`, `secrets`.

Run: `.\mcp.exe install`
Expected output: `mcp install: not implemented yet`

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/cli/root.go cmd/mcp/main.go
git commit -m "feat: wire cobra CLI with subcommand stubs"
```

---

### Task 3: Manifest struct + YAML parser

**Files:**
- Create: `internal/config/manifest.go`, `internal/config/manifest_test.go`

- [ ] **Step 1: Add YAML dependency**

Run: `go get gopkg.in/yaml.v3`

- [ ] **Step 2: Write failing test `internal/config/manifest_test.go`**

```go
package config

import (
	"strings"
	"testing"
)

func TestParseManifest_SerenaMinimal(t *testing.T) {
	yaml := `
name: serena
kind: global
transport: native-http
command: uvx
base_args: [--refresh, --from, git+https://github.com/oraios/serena, serena, start-mcp-server]
daemons:
  - name: claude
    context: claude-code
    port: 9121
    extra_args: [--context, claude-code, --transport, streamable-http]
client_bindings:
  - client: claude-code
    daemon: claude
    url_path: /mcp
weekly_refresh: true
`
	m, err := ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "serena" {
		t.Errorf("Name = %q, want serena", m.Name)
	}
	if m.Kind != "global" {
		t.Errorf("Kind = %q, want global", m.Kind)
	}
	if len(m.Daemons) != 1 {
		t.Fatalf("len(Daemons) = %d, want 1", len(m.Daemons))
	}
	if m.Daemons[0].Port != 9121 {
		t.Errorf("Daemons[0].Port = %d, want 9121", m.Daemons[0].Port)
	}
	if !m.WeeklyRefresh {
		t.Error("WeeklyRefresh = false, want true")
	}
}

func TestParseManifest_MissingName(t *testing.T) {
	yaml := `kind: global`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention 'name', got: %v", err)
	}
}

func TestParseManifest_InvalidKind(t *testing.T) {
	yaml := `
name: foo
kind: nonsense
transport: native-http
command: echo
`
	_, err := ParseManifest(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for invalid kind, got nil")
	}
}
```

- [ ] **Step 3: Run tests — confirm they fail because `ParseManifest` does not exist**

Run: `go test ./internal/config/...`
Expected: compile error "undefined: ParseManifest".

- [ ] **Step 4: Implement `internal/config/manifest.go`**

```go
package config

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Kind enumerates daemon types. Only these two values are valid in manifest.kind.
const (
	KindGlobal           = "global"
	KindWorkspaceScoped  = "workspace-scoped"
)

// Transport enumerates how the server speaks MCP. Only these are valid.
const (
	TransportNativeHTTP  = "native-http"
	TransportStdioBridge = "stdio-bridge"
)

// ServerManifest is the parsed form of a `servers/<name>/manifest.yaml` file.
type ServerManifest struct {
	Name            string          `yaml:"name"`
	Kind            string          `yaml:"kind"`
	Transport       string          `yaml:"transport"`
	Command         string          `yaml:"command"`
	BaseArgs        []string        `yaml:"base_args"`
	BaseArgsTemplate []string       `yaml:"base_args_template"`
	Env             map[string]string `yaml:"env"`
	Daemons         []DaemonSpec    `yaml:"daemons"`
	Languages       []LanguageSpec  `yaml:"languages"`
	PortPool        *PortPool       `yaml:"port_pool"`
	IdleTimeoutMin  int             `yaml:"idle_timeout_min"`
	ClientBindings  []ClientBinding `yaml:"client_bindings"`
	WeeklyRefresh   bool            `yaml:"weekly_refresh"`
}

type DaemonSpec struct {
	Name      string   `yaml:"name"`
	Context   string   `yaml:"context"`
	Port      int      `yaml:"port"`
	ExtraArgs []string `yaml:"extra_args"`
}

type LanguageSpec struct {
	Name       string   `yaml:"name"`
	LspCommand string   `yaml:"lsp_command"`
	ExtraFlags []string `yaml:"extra_flags"`
}

type PortPool struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

type ClientBinding struct {
	Client  string `yaml:"client"`
	Daemon  string `yaml:"daemon"`
	URLPath string `yaml:"url_path"`
}

// ParseManifest reads YAML from r and returns a validated ServerManifest.
// Returns an error if required fields are missing or kind/transport values are unknown.
func ParseManifest(r io.Reader) (*ServerManifest, error) {
	var m ServerManifest
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks required fields and enum values. Called automatically by ParseManifest.
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
	return nil
}
```

- [ ] **Step 5: Run tests — confirm all pass**

Run: `go test ./internal/config/...`
Expected: `ok github.com/dima/mcp-local-hub/internal/config`

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/config/manifest.go internal/config/manifest_test.go
git commit -m "feat(config): manifest YAML parser with validation"
```

---

### Task 4: Port registry (ports.yaml reader + conflict detection)

**Files:**
- Create: `internal/config/ports.go`, `internal/config/ports_test.go`, `configs/ports.yaml`

- [ ] **Step 1: Write failing test `internal/config/ports_test.go`**

```go
package config

import (
	"strings"
	"testing"
)

func TestParsePortRegistry(t *testing.T) {
	yaml := `
global:
  - server: serena
    daemon: claude
    port: 9121
  - server: serena
    daemon: codex
    port: 9122
  - server: memory
    daemon: shared
    port: 9140
workspace_scoped:
  - server: mcp-language-server
    pool_start: 9200
    pool_end: 9299
`
	r, err := ParsePortRegistry(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParsePortRegistry: %v", err)
	}
	if len(r.Global) != 3 {
		t.Fatalf("len(Global) = %d, want 3", len(r.Global))
	}
	if r.Global[0].Port != 9121 {
		t.Errorf("Global[0].Port = %d, want 9121", r.Global[0].Port)
	}
	if len(r.WorkspaceScoped) != 1 {
		t.Fatalf("len(WorkspaceScoped) = %d, want 1", len(r.WorkspaceScoped))
	}
}

func TestPortRegistry_DetectConflictGlobal(t *testing.T) {
	yaml := `
global:
  - server: a
    daemon: x
    port: 9121
  - server: b
    daemon: y
    port: 9121
`
	_, err := ParsePortRegistry(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "9121") {
		t.Errorf("error should mention conflicting port 9121, got: %v", err)
	}
}

func TestPortRegistry_DetectPoolOverlap(t *testing.T) {
	yaml := `
global:
  - server: a
    daemon: x
    port: 9250
workspace_scoped:
  - server: b
    pool_start: 9200
    pool_end: 9299
`
	_, err := ParsePortRegistry(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected overlap error between global port 9250 and workspace pool 9200-9299")
	}
}
```

- [ ] **Step 2: Run test — confirm it fails**

Run: `go test ./internal/config/...`
Expected: compile error "undefined: ParsePortRegistry".

- [ ] **Step 3: Implement `internal/config/ports.go`**

```go
package config

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// PortRegistry is the parsed form of configs/ports.yaml — the central authority
// for which port each daemon uses. Conflicts between global ports and workspace
// pools are detected at parse time.
type PortRegistry struct {
	Global          []GlobalPortEntry    `yaml:"global"`
	WorkspaceScoped []WorkspacePoolEntry `yaml:"workspace_scoped"`
}

type GlobalPortEntry struct {
	Server string `yaml:"server"`
	Daemon string `yaml:"daemon"`
	Port   int    `yaml:"port"`
}

type WorkspacePoolEntry struct {
	Server    string `yaml:"server"`
	PoolStart int    `yaml:"pool_start"`
	PoolEnd   int    `yaml:"pool_end"`
}

// ParsePortRegistry reads YAML and returns a validated registry.
// Validation ensures no two global entries share a port and no global port falls
// inside any workspace pool range.
func ParsePortRegistry(r io.Reader) (*PortRegistry, error) {
	var reg PortRegistry
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("port registry decode: %w", err)
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return &reg, nil
}

// Validate checks for conflicts:
// - two global entries with the same port
// - a global port that falls inside any workspace pool
// - overlapping workspace pools
func (r *PortRegistry) Validate() error {
	seen := map[int]string{}
	for _, g := range r.Global {
		if owner, ok := seen[g.Port]; ok {
			return fmt.Errorf("port %d conflict: %s and %s/%s", g.Port, owner, g.Server, g.Daemon)
		}
		seen[g.Port] = fmt.Sprintf("%s/%s", g.Server, g.Daemon)
	}
	for _, w := range r.WorkspaceScoped {
		if w.PoolStart > w.PoolEnd {
			return fmt.Errorf("server %s: pool_start %d > pool_end %d", w.Server, w.PoolStart, w.PoolEnd)
		}
		for _, g := range r.Global {
			if g.Port >= w.PoolStart && g.Port <= w.PoolEnd {
				return fmt.Errorf("global port %d (%s/%s) falls inside workspace pool %d-%d (%s)",
					g.Port, g.Server, g.Daemon, w.PoolStart, w.PoolEnd, w.Server)
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test — confirm all pass**

Run: `go test ./internal/config/...`
Expected: `ok`

- [ ] **Step 5: Create initial `configs/ports.yaml`**

```yaml
# Central port registry for mcp-local-hub.
# Edited by `mcp install` and read at every daemon start for conflict detection.
# Ranges (per spec §3.5):
#   9121-9139 — global daemons
#   9140-9199 — reserved for future global daemons
#   9200-9299 — workspace-scoped daemons

global: []
workspace_scoped: []
```

- [ ] **Step 6: Commit**

```bash
git add internal/config/ports.go internal/config/ports_test.go configs/ports.yaml
git commit -m "feat(config): port registry with conflict detection"
```

---

### Task 5: age-encrypted vault (secret storage)

**Files:**
- Create: `internal/secrets/vault.go`, `internal/secrets/vault_test.go`

- [ ] **Step 1: Add age dependency**

Run: `go get filippo.io/age`

- [ ] **Step 2: Write failing test `internal/secrets/vault_test.go`**

```go
package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVault_InitSetGet(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")

	if err := InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("identity file missing: %v", err)
	}
	if _, err := os.Stat(vaultPath); err != nil {
		t.Fatalf("vault file missing: %v", err)
	}

	v, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if err := v.Set("API_KEY", "super-secret-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := v.Get("API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "super-secret-value" {
		t.Errorf("Get = %q, want super-secret-value", got)
	}

	// Reopen vault with same identity — value should persist.
	v2, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault reopen: %v", err)
	}
	got2, err := v2.Get("API_KEY")
	if err != nil {
		t.Fatalf("Get reopen: %v", err)
	}
	if got2 != "super-secret-value" {
		t.Errorf("persisted value = %q, want super-secret-value", got2)
	}
}

func TestVault_List(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)

	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("A", "1")
	v.Set("B", "2")
	v.Set("C", "3")

	keys := v.List()
	if len(keys) != 3 {
		t.Fatalf("List = %v (len %d), want 3 keys", keys, len(keys))
	}
}

func TestVault_Delete(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)

	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("A", "1")
	if err := v.Delete("A"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Get("A"); err == nil {
		t.Error("expected error for deleted key, got nil")
	}
}

func TestVault_WrongIdentity(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)

	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("X", "1")

	// Create a second identity and try to open the vault with it.
	wrongKey := filepath.Join(dir, ".age-key-wrong")
	wrongVault := filepath.Join(dir, "wrong.age")
	_ = InitVault(wrongKey, wrongVault)

	if _, err := OpenVault(wrongKey, vaultPath); err == nil {
		t.Error("OpenVault with wrong identity should fail, got nil error")
	}
}
```

- [ ] **Step 3: Run test — confirm failure**

Run: `go test ./internal/secrets/...`
Expected: compile errors — undefined `InitVault`, `OpenVault`.

- [ ] **Step 4: Implement `internal/secrets/vault.go`**

```go
package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"filippo.io/age"
)

// Vault is an age-encrypted key/value store persisted in a single file.
// The on-disk format is JSON, age-encrypted with the user's identity.
// In-memory state is kept in `data` and flushed to disk on every mutation.
type Vault struct {
	identity  age.Identity
	recipient age.Recipient
	vaultPath string
	data      map[string]string
}

// InitVault generates a new X25519 identity at keyPath and writes an empty
// encrypted vault to vaultPath. Fails if either file already exists.
func InitVault(keyPath, vaultPath string) error {
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("identity file already exists: %s", keyPath)
	}
	if _, err := os.Stat(vaultPath); err == nil {
		return fmt.Errorf("vault file already exists: %s", vaultPath)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generate identity: %w", err)
	}
	// Write identity file — plain text so launcher can read it.
	// Filesystem permissions are the only protection in MVP.
	if err := os.WriteFile(keyPath, []byte(id.String()+"\n"), 0600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	v := &Vault{
		identity:  id,
		recipient: id.Recipient(),
		vaultPath: vaultPath,
		data:      map[string]string{},
	}
	return v.save()
}

// OpenVault reads the identity from keyPath and opens the encrypted vault at vaultPath.
func OpenVault(keyPath, vaultPath string) (*Vault, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read identity: %w", err)
	}
	ids, err := age.ParseIdentities(bytes.NewReader(keyBytes))
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no identity in %s", keyPath)
	}
	id := ids[0]

	x25519, ok := id.(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("identity is not X25519 (unsupported)")
	}
	v := &Vault{
		identity:  id,
		recipient: x25519.Recipient(),
		vaultPath: vaultPath,
		data:      map[string]string{},
	}
	if err := v.load(); err != nil {
		return nil, err
	}
	return v, nil
}

// Set stores value under key and persists the vault to disk.
func (v *Vault) Set(key, value string) error {
	v.data[key] = value
	return v.save()
}

// Get returns the value for key or an error if not present.
func (v *Vault) Get(key string) (string, error) {
	val, ok := v.data[key]
	if !ok {
		return "", fmt.Errorf("secret %q not found", key)
	}
	return val, nil
}

// Delete removes key and persists the vault to disk.
func (v *Vault) Delete(key string) error {
	if _, ok := v.data[key]; !ok {
		return fmt.Errorf("secret %q not found", key)
	}
	delete(v.data, key)
	return v.save()
}

// List returns all secret keys, sorted alphabetically.
func (v *Vault) List() []string {
	keys := make([]string, 0, len(v.data))
	for k := range v.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// save encrypts v.data as JSON and writes to v.vaultPath.
func (v *Vault) save() error {
	raw, err := json.Marshal(v.data)
	if err != nil {
		return fmt.Errorf("marshal vault: %w", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, v.recipient)
	if err != nil {
		return fmt.Errorf("age encrypt init: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("age encrypt write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("age encrypt close: %w", err)
	}
	return os.WriteFile(v.vaultPath, buf.Bytes(), 0600)
}

// load reads v.vaultPath, decrypts with v.identity, unmarshals JSON into v.data.
func (v *Vault) load() error {
	raw, err := os.ReadFile(v.vaultPath)
	if err != nil {
		return fmt.Errorf("read vault: %w", err)
	}
	if len(raw) == 0 {
		return nil // empty vault after init
	}
	r, err := age.Decrypt(bytes.NewReader(raw), v.identity)
	if err != nil {
		return fmt.Errorf("age decrypt: %w", err)
	}
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read decrypted: %w", err)
	}
	if len(plaintext) == 0 {
		return nil
	}
	if err := json.Unmarshal(plaintext, &v.data); err != nil {
		return fmt.Errorf("unmarshal vault: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run tests — confirm all pass**

Run: `go test ./internal/secrets/...`
Expected: `ok github.com/dima/mcp-local-hub/internal/secrets`

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/secrets/vault.go internal/secrets/vault_test.go
git commit -m "feat(secrets): age-encrypted vault with CRUD"
```

---

### Task 6: Secret resolver (prefix-based value resolution)

**Files:**
- Create: `internal/secrets/resolver.go`, `internal/secrets/resolver_test.go`

- [ ] **Step 1: Write failing test `internal/secrets/resolver_test.go`**

```go
package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolver_Secret(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("API_KEY", "xyz123")

	r := NewResolver(v, nil)
	got, err := r.Resolve("secret:API_KEY")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "xyz123" {
		t.Errorf("Resolve = %q, want xyz123", got)
	}
}

func TestResolver_File(t *testing.T) {
	local := map[string]string{"email": "user@example.com"}
	r := NewResolver(nil, local)
	got, err := r.Resolve("file:email")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "user@example.com" {
		t.Errorf("Resolve = %q, want user@example.com", got)
	}
}

func TestResolver_Env(t *testing.T) {
	t.Setenv("MCP_TEST_VAR", "env-value")
	r := NewResolver(nil, nil)
	got, err := r.Resolve("$MCP_TEST_VAR")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "env-value" {
		t.Errorf("Resolve = %q, want env-value", got)
	}
}

func TestResolver_Literal(t *testing.T) {
	r := NewResolver(nil, nil)
	got, err := r.Resolve("plain-text")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "plain-text" {
		t.Errorf("Resolve = %q, want plain-text", got)
	}
}

func TestResolver_SecretMissing(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)

	r := NewResolver(v, nil)
	if _, err := r.Resolve("secret:NONEXISTENT"); err == nil {
		t.Error("expected error for missing secret, got nil")
	}
}

func TestResolver_EnvMissing(t *testing.T) {
	// Ensure variable is not set
	os.Unsetenv("MCP_DEFINITELY_NOT_SET")
	r := NewResolver(nil, nil)
	if _, err := r.Resolve("$MCP_DEFINITELY_NOT_SET"); err == nil {
		t.Error("expected error for missing env var, got nil")
	}
}
```

- [ ] **Step 2: Run test — confirm failure**

Run: `go test ./internal/secrets/...`
Expected: undefined `NewResolver`.

- [ ] **Step 3: Implement `internal/secrets/resolver.go`**

```go
package secrets

import (
	"fmt"
	"os"
	"strings"
)

// Resolver turns manifest env values (which may contain prefixes like `secret:`,
// `file:`, or `$VAR`) into plaintext for use in a child process environment.
// Resolution order per spec §3.8:
//   secret:<key> → vault.Get
//   file:<key>   → local config map
//   $VAR         → os.Getenv (fails if unset)
//   <literal>    → returned as-is
type Resolver struct {
	vault *Vault
	local map[string]string
}

// NewResolver builds a Resolver. Either argument may be nil if the caller knows
// that prefix is not in use; in that case, matching-prefix lookups return errors.
func NewResolver(v *Vault, local map[string]string) *Resolver {
	return &Resolver{vault: v, local: local}
}

// Resolve returns the resolved value for a manifest-style reference string.
func (r *Resolver) Resolve(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "secret:"):
		if r.vault == nil {
			return "", fmt.Errorf("resolve %q: vault not available", ref)
		}
		key := strings.TrimPrefix(ref, "secret:")
		return r.vault.Get(key)
	case strings.HasPrefix(ref, "file:"):
		key := strings.TrimPrefix(ref, "file:")
		if r.local == nil {
			return "", fmt.Errorf("resolve %q: local config not available", ref)
		}
		v, ok := r.local[key]
		if !ok {
			return "", fmt.Errorf("resolve %q: key %q not in local config", ref, key)
		}
		return v, nil
	case strings.HasPrefix(ref, "$"):
		name := strings.TrimPrefix(ref, "$")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("resolve %q: environment variable %q not set", ref, name)
		}
		return v, nil
	default:
		return ref, nil
	}
}

// ResolveMap resolves every value in a manifest env map and returns a new map.
// If any resolution fails, an error is returned referencing the offending key.
func (r *Resolver) ResolveMap(env map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		resolved, err := r.Resolve(v)
		if err != nil {
			return nil, fmt.Errorf("env[%s]: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests — confirm all pass**

Run: `go test ./internal/secrets/...`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/secrets/resolver.go internal/secrets/resolver_test.go
git commit -m "feat(secrets): prefix-based value resolver"
```

---

### Task 7: Scheduler interface + Linux/macOS stubs

**Files:**
- Create: `internal/scheduler/scheduler.go`, `internal/scheduler/scheduler_linux.go`, `internal/scheduler/scheduler_darwin.go`

- [ ] **Step 1: Write `internal/scheduler/scheduler.go`**

```go
package scheduler

// TaskSpec describes a scheduled task the hub wants the OS to manage.
// Scheduler backends translate this into Windows Task Scheduler, systemd user units,
// or launchd agents.
type TaskSpec struct {
	// Name is a unique identifier. Used for create/delete/run operations.
	// Convention: "mcp-local-hub-<server>-<daemon>" for daemon tasks, "mcp-local-hub-refresh" for weekly refresh.
	Name string

	// Description is a human-readable summary shown by the OS scheduler UI.
	Description string

	// Command is the program to run. Typically the absolute path to the `mcp` binary.
	Command string

	// Args are passed verbatim to the command. Typically: ["daemon", "--server", "serena", "--daemon", "claude"].
	Args []string

	// WorkingDir is the process's cwd at launch. Usually the repo root.
	WorkingDir string

	// Env is added to the process environment at launch.
	Env map[string]string

	// Trigger determines when the task fires. Only one of LogonTrigger or WeeklyTrigger is used.
	LogonTrigger bool
	// WeeklyTrigger fires every week on the named day at the given time.
	// DayOfWeek uses Go's time.Weekday (Sunday=0 .. Saturday=6). HourLocal+MinuteLocal are 24h local time.
	WeeklyTrigger *WeeklyTrigger

	// RestartOnFailure enables automatic retry. The backend configures a fixed policy:
	// retry every 60 seconds, max 3 attempts (per spec §3.3.1).
	RestartOnFailure bool
}

type WeeklyTrigger struct {
	DayOfWeek    int // 0=Sunday .. 6=Saturday
	HourLocal    int
	MinuteLocal  int
}

// TaskStatus summarizes what the OS scheduler currently thinks of a task.
type TaskStatus struct {
	Name       string
	State      string // "Ready", "Running", "Disabled", "Unknown"
	LastResult int    // exit code of last run, or -1 if never run
	NextRun    string // human-readable, backend-specific
}

// Scheduler is the OS-abstracted interface for managing mcp-local-hub daemon tasks.
// Implementations live in scheduler_<os>.go files selected by build tags.
type Scheduler interface {
	// Create registers a new task. If a task with the same name already exists,
	// Create returns an error — callers must Delete first for idempotence.
	Create(spec TaskSpec) error

	// Delete removes a task by name. Returns nil if the task does not exist.
	Delete(name string) error

	// Run triggers an immediate one-off execution of a task.
	Run(name string) error

	// Stop terminates a currently-running task. No-op if not running.
	Stop(name string) error

	// Status reports the current state of a task.
	Status(name string) (TaskStatus, error)

	// List returns all tasks whose Name starts with prefix (e.g., "mcp-local-hub-").
	List(prefix string) ([]TaskStatus, error)
}

// New returns the platform-appropriate Scheduler implementation for the current OS.
// Defined per-OS in scheduler_<os>.go.
func New() (Scheduler, error) {
	return newPlatformScheduler()
}
```

- [ ] **Step 2: Write Linux stub `internal/scheduler/scheduler_linux.go`**

```go
//go:build linux

package scheduler

import "fmt"

// linuxScheduler is a stub that compiles but returns "not implemented" for all operations.
// Full systemd-user-unit integration is out of scope for Phase 0-1 of this plan.
type linuxScheduler struct{}

func newPlatformScheduler() (Scheduler, error) {
	return nil, fmt.Errorf("linux scheduler not yet implemented (Phase 0-1 is Windows-first)")
}

func (linuxScheduler) Create(TaskSpec) error          { return fmt.Errorf("not implemented") }
func (linuxScheduler) Delete(string) error            { return fmt.Errorf("not implemented") }
func (linuxScheduler) Run(string) error               { return fmt.Errorf("not implemented") }
func (linuxScheduler) Stop(string) error              { return fmt.Errorf("not implemented") }
func (linuxScheduler) Status(string) (TaskStatus, error) {
	return TaskStatus{}, fmt.Errorf("not implemented")
}
func (linuxScheduler) List(string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("not implemented")
}
```

- [ ] **Step 3: Write macOS stub `internal/scheduler/scheduler_darwin.go`**

```go
//go:build darwin

package scheduler

import "fmt"

// darwinScheduler is a stub that compiles but returns "not implemented".
// Full launchd agent integration is out of scope for Phase 0-1.
type darwinScheduler struct{}

func newPlatformScheduler() (Scheduler, error) {
	return nil, fmt.Errorf("darwin scheduler not yet implemented (Phase 0-1 is Windows-first)")
}

func (darwinScheduler) Create(TaskSpec) error          { return fmt.Errorf("not implemented") }
func (darwinScheduler) Delete(string) error            { return fmt.Errorf("not implemented") }
func (darwinScheduler) Run(string) error               { return fmt.Errorf("not implemented") }
func (darwinScheduler) Stop(string) error              { return fmt.Errorf("not implemented") }
func (darwinScheduler) Status(string) (TaskStatus, error) {
	return TaskStatus{}, fmt.Errorf("not implemented")
}
func (darwinScheduler) List(string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("not implemented")
}
```

- [ ] **Step 4: Verify cross-platform compile**

Run: `go build ./internal/scheduler/...`
Expected: no output.

Run: `GOOS=linux go build ./internal/scheduler/...` (from Git Bash).
Expected: no output — Linux stub compiles.

Run: `GOOS=darwin go build ./internal/scheduler/...`
Expected: no output — macOS stub compiles.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_linux.go internal/scheduler/scheduler_darwin.go
git commit -m "feat(scheduler): interface + Linux/macOS stubs"
```

---

### Task 8: Windows scheduler implementation (schtasks-based)

**Files:**
- Create: `internal/scheduler/scheduler_windows.go`, `internal/scheduler/scheduler_windows_test.go`

- [ ] **Step 1: Write failing test `internal/scheduler/scheduler_windows_test.go`**

```go
//go:build windows

package scheduler

import (
	"strings"
	"testing"
)

func TestBuildCreateXML_Logon(t *testing.T) {
	spec := TaskSpec{
		Name:             "mcp-local-hub-test-logon",
		Description:      "test logon task",
		Command:          `C:\path\mcp.exe`,
		Args:             []string{"daemon", "--server", "serena"},
		WorkingDir:       `C:\repo`,
		LogonTrigger:     true,
		RestartOnFailure: true,
	}
	xml := buildCreateXML(spec, "USERNAME")

	if !strings.Contains(xml, "<LogonTrigger>") {
		t.Error("expected <LogonTrigger> in XML")
	}
	if !strings.Contains(xml, `<Command>C:\path\mcp.exe</Command>`) {
		t.Errorf("Command path not found in XML: %s", xml)
	}
	if !strings.Contains(xml, "<Arguments>daemon --server serena</Arguments>") {
		t.Errorf("Arguments not properly joined: %s", xml)
	}
	if !strings.Contains(xml, "<RestartInterval>PT60S</RestartInterval>") {
		t.Errorf("Restart policy not set: %s", xml)
	}
	if !strings.Contains(xml, "<RestartCount>3</RestartCount>") {
		t.Errorf("Restart count not set: %s", xml)
	}
}

func TestBuildCreateXML_Weekly(t *testing.T) {
	spec := TaskSpec{
		Name:        "mcp-local-hub-refresh",
		Description: "weekly",
		Command:     `C:\path\mcp.exe`,
		Args:        []string{"restart", "--all"},
		WeeklyTrigger: &WeeklyTrigger{
			DayOfWeek:   0, // Sunday
			HourLocal:   3,
			MinuteLocal: 0,
		},
	}
	xml := buildCreateXML(spec, "USERNAME")
	if !strings.Contains(xml, "<WeeklyTrigger>") {
		t.Error("expected <WeeklyTrigger>")
	}
	if !strings.Contains(xml, "<DaysOfWeek><Sunday /></DaysOfWeek>") {
		t.Errorf("Sunday not set: %s", xml)
	}
	if !strings.Contains(xml, "T03:00:00") {
		t.Errorf("03:00 time not set: %s", xml)
	}
}
```

- [ ] **Step 2: Run test — confirm failure**

Run: `go test ./internal/scheduler/... -run TestBuildCreateXML`
Expected: undefined `buildCreateXML`.

- [ ] **Step 3: Implement `internal/scheduler/scheduler_windows.go`**

```go
//go:build windows

package scheduler

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

// windowsScheduler shells out to `schtasks.exe` for all operations.
// We build a Task Scheduler XML document per spec, pipe it to `schtasks /Create /XML`,
// and parse the output of `/Query` for Status/List.
type windowsScheduler struct {
	username string // e.g., "USERNAME"
}

func newPlatformScheduler() (Scheduler, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("user.Current: %w", err)
	}
	// u.Username is typically "MACHINE\\user" on Windows — strip the domain.
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return &windowsScheduler{username: name}, nil
}

// dayNames maps Go weekday ints to Task Scheduler XML element names.
var dayNames = map[int]string{
	0: "Sunday",
	1: "Monday",
	2: "Tuesday",
	3: "Wednesday",
	4: "Thursday",
	5: "Friday",
	6: "Saturday",
}

// buildCreateXML serializes a TaskSpec into a Task Scheduler XML document.
// Exposed (lowercase) for testing within the same package.
func buildCreateXML(spec TaskSpec, userName string) string {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-16"?>`)
	buf.WriteString("\n")
	buf.WriteString(`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`)
	buf.WriteString("\n  <RegistrationInfo>\n")
	buf.WriteString(fmt.Sprintf("    <Description>%s</Description>\n", xmlEscape(spec.Description)))
	buf.WriteString(fmt.Sprintf("    <Author>%s</Author>\n", xmlEscape(userName)))
	buf.WriteString(fmt.Sprintf("    <Date>%s</Date>\n", time.Now().Format("2006-01-02T15:04:05")))
	buf.WriteString("  </RegistrationInfo>\n")

	// Triggers
	buf.WriteString("  <Triggers>\n")
	if spec.LogonTrigger {
		buf.WriteString("    <LogonTrigger>\n")
		buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", xmlEscape(userName)))
		buf.WriteString("      <Enabled>true</Enabled>\n")
		buf.WriteString("    </LogonTrigger>\n")
	}
	if spec.WeeklyTrigger != nil {
		wt := spec.WeeklyTrigger
		day := dayNames[wt.DayOfWeek]
		buf.WriteString("    <WeeklyTrigger>\n")
		buf.WriteString(fmt.Sprintf("      <StartBoundary>2026-01-04T%02d:%02d:00</StartBoundary>\n", wt.HourLocal, wt.MinuteLocal))
		buf.WriteString("      <Enabled>true</Enabled>\n")
		buf.WriteString("      <ScheduleByWeek>\n")
		buf.WriteString(fmt.Sprintf("        <DaysOfWeek><%s /></DaysOfWeek>\n", day))
		buf.WriteString("        <WeeksInterval>1</WeeksInterval>\n")
		buf.WriteString("      </ScheduleByWeek>\n")
		buf.WriteString("    </WeeklyTrigger>\n")
	}
	buf.WriteString("  </Triggers>\n")

	// Principal — run as current user, interactive (needs session)
	buf.WriteString("  <Principals>\n")
	buf.WriteString("    <Principal id=\"Author\">\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", xmlEscape(userName)))
	buf.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	buf.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	buf.WriteString("    </Principal>\n")
	buf.WriteString("  </Principals>\n")

	// Settings — restart policy + sane defaults
	buf.WriteString("  <Settings>\n")
	if spec.RestartOnFailure {
		buf.WriteString("    <RestartInterval>PT60S</RestartInterval>\n")
		buf.WriteString("    <RestartCount>3</RestartCount>\n")
	}
	buf.WriteString("    <AllowHardTerminate>true</AllowHardTerminate>\n")
	buf.WriteString("    <StartWhenAvailable>false</StartWhenAvailable>\n")
	buf.WriteString("    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>\n")
	buf.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	buf.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	buf.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	buf.WriteString("    <IdleSettings>\n      <StopOnIdleEnd>false</StopOnIdleEnd>\n      <RestartOnIdle>false</RestartOnIdle>\n    </IdleSettings>\n")
	buf.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\n")
	buf.WriteString("    <Enabled>true</Enabled>\n")
	buf.WriteString("    <Hidden>false</Hidden>\n")
	buf.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n")
	buf.WriteString("    <WakeToRun>false</WakeToRun>\n")
	buf.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n") // no timeout
	buf.WriteString("    <Priority>7</Priority>\n")
	buf.WriteString("  </Settings>\n")

	// Actions
	buf.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n")
	buf.WriteString(fmt.Sprintf("      <Command>%s</Command>\n", xmlEscape(spec.Command)))
	if len(spec.Args) > 0 {
		buf.WriteString(fmt.Sprintf("      <Arguments>%s</Arguments>\n", xmlEscape(strings.Join(spec.Args, " "))))
	}
	if spec.WorkingDir != "" {
		buf.WriteString(fmt.Sprintf("      <WorkingDirectory>%s</WorkingDirectory>\n", xmlEscape(spec.WorkingDir)))
	}
	buf.WriteString("    </Exec>\n  </Actions>\n")
	buf.WriteString("</Task>\n")

	return buf.String()
}

func xmlEscape(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}

// Create writes the XML to a temp file and invokes `schtasks /Create /XML`.
func (w *windowsScheduler) Create(spec TaskSpec) error {
	xmlDoc := buildCreateXML(spec, w.username)
	tmp, err := os.CreateTemp("", "mcp-local-hub-task-*.xml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	// Task Scheduler requires UTF-16 LE with BOM. Re-encode.
	utf16 := utf8ToUTF16WithBOM(xmlDoc)
	if _, err := tmp.Write(utf16); err != nil {
		tmp.Close()
		return fmt.Errorf("write xml: %w", err)
	}
	tmp.Close()

	cmd := exec.Command("schtasks", "/Create", "/TN", spec.Name, "/XML", tmp.Name(), "/F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Create: %w: %s", err, string(out))
	}
	return nil
}

// utf8ToUTF16WithBOM converts a UTF-8 string to UTF-16 LE with a BOM, which
// is what Task Scheduler's /XML flag requires.
func utf8ToUTF16WithBOM(s string) []byte {
	var out bytes.Buffer
	out.WriteByte(0xFF)
	out.WriteByte(0xFE) // UTF-16 LE BOM
	for _, r := range s {
		if r <= 0xFFFF {
			out.WriteByte(byte(r))
			out.WriteByte(byte(r >> 8))
		} else {
			// surrogate pair
			r -= 0x10000
			hi := 0xD800 + (r >> 10)
			lo := 0xDC00 + (r & 0x3FF)
			out.WriteByte(byte(hi))
			out.WriteByte(byte(hi >> 8))
			out.WriteByte(byte(lo))
			out.WriteByte(byte(lo >> 8))
		}
	}
	return out.Bytes()
}

func (w *windowsScheduler) Delete(name string) error {
	cmd := exec.Command("schtasks", "/Delete", "/TN", name, "/F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// If the task does not exist, schtasks returns exit 1 with "ERROR: The system cannot find the file specified."
		// Treat that as success (idempotent delete).
		if strings.Contains(string(out), "cannot find") || strings.Contains(string(out), "does not exist") {
			return nil
		}
		return fmt.Errorf("schtasks /Delete: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Run(name string) error {
	cmd := exec.Command("schtasks", "/Run", "/TN", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Run: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Stop(name string) error {
	cmd := exec.Command("schtasks", "/End", "/TN", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// "ERROR: There is no running instance of the task." → nil
		if strings.Contains(string(out), "no running instance") {
			return nil
		}
		return fmt.Errorf("schtasks /End: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Status(name string) (TaskStatus, error) {
	cmd := exec.Command("schtasks", "/Query", "/TN", name, "/V", "/FO", "LIST")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return TaskStatus{}, fmt.Errorf("schtasks /Query: %w: %s", err, string(out))
	}
	return parseTaskQueryOutput(string(out), name), nil
}

func (w *windowsScheduler) List(prefix string) ([]TaskStatus, error) {
	cmd := exec.Command("schtasks", "/Query", "/V", "/FO", "LIST")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("schtasks /Query: %w: %s", err, string(out))
	}
	// Split into records separated by blank lines; each record has "TaskName:" line.
	records := strings.Split(string(out), "\r\n\r\n")
	var results []TaskStatus
	for _, r := range records {
		status := parseTaskQueryOutput(r, "")
		if status.Name != "" && strings.HasPrefix(strings.TrimPrefix(status.Name, "\\"), prefix) {
			results = append(results, status)
		}
	}
	return results, nil
}

// parseTaskQueryOutput extracts key fields from schtasks /Query /V /FO LIST output.
func parseTaskQueryOutput(out string, nameHint string) TaskStatus {
	status := TaskStatus{Name: nameHint, LastResult: -1}
	for _, line := range strings.Split(out, "\r\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TaskName:") {
			status.Name = strings.TrimSpace(strings.TrimPrefix(line, "TaskName:"))
		} else if strings.HasPrefix(line, "Status:") {
			status.State = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		} else if strings.HasPrefix(line, "Last Result:") {
			fmt.Sscanf(strings.TrimPrefix(line, "Last Result:"), " %d", &status.LastResult)
		} else if strings.HasPrefix(line, "Next Run Time:") {
			status.NextRun = strings.TrimSpace(strings.TrimPrefix(line, "Next Run Time:"))
		}
	}
	return status
}
```

- [ ] **Step 4: Run XML-building tests — confirm pass**

Run: `go test ./internal/scheduler/... -run TestBuildCreateXML`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/scheduler_windows.go internal/scheduler/scheduler_windows_test.go
git commit -m "feat(scheduler): Windows Task Scheduler backend"
```

---

---

### Task 9: Client adapter interface

**Files:**
- Create: `internal/clients/clients.go`

- [ ] **Step 1: Write `internal/clients/clients.go`**

```go
package clients

// MCPEntry describes one MCP server entry in a client's config.
// The hub uses this to add/update/remove entries idempotently.
type MCPEntry struct {
	Name    string            // server name, e.g., "serena"
	URL     string            // full URL, e.g., "http://localhost:9121/mcp"
	Headers map[string]string // optional HTTP headers
	Env     map[string]string // only used by stdio entries (for rollback); URL entries leave this nil
}

// Client is the OS-/format-abstracted interface for a single MCP client config file.
// Implementations live in one file per client.
type Client interface {
	// Name returns a stable identifier ("claude-code", "codex-cli", "gemini-cli", "antigravity")
	// used in manifest client_bindings.
	Name() string

	// ConfigPath returns the absolute path to the config file this client reads.
	// Used for display, backup, and existence checks.
	ConfigPath() string

	// Exists reports whether the config file is present. If false, AddEntry/RemoveEntry
	// are no-ops and Backup returns ErrClientNotInstalled.
	Exists() bool

	// Backup copies the current config to a sibling file ending in ".bak-mcp-local-hub-<timestamp>"
	// and returns the path. Overwrites any previous backup with the same timestamp-second.
	Backup() (string, error)

	// Restore copies the named backup over the live config, overwriting current content.
	Restore(backupPath string) error

	// AddEntry adds or replaces the MCP server entry named entry.Name.
	// Creates parent `mcpServers` / `[mcp_servers.*]` section if missing.
	AddEntry(entry MCPEntry) error

	// RemoveEntry removes the MCP server entry with the given name.
	// Returns nil if the entry does not exist (idempotent).
	RemoveEntry(name string) error

	// GetEntry returns the current value of the named entry, or nil if missing.
	GetEntry(name string) (*MCPEntry, error)
}

// ErrClientNotInstalled signals the client's config file does not exist on this machine.
type ErrClientNotInstalled struct{ Client string }

func (e *ErrClientNotInstalled) Error() string {
	return "client not installed: " + e.Client
}
```

- [ ] **Step 2: Build check**

Run: `go build ./internal/clients/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add internal/clients/clients.go
git commit -m "feat(clients): define Client interface"
```

---

### Task 10: Claude Code client adapter

**Files:**
- Create: `internal/clients/claude_code.go`, `internal/clients/claude_code_test.go`

- [ ] **Step 1: Write failing test `internal/clients/claude_code_test.go`**

```go
package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func setupClaudeConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeCode_AddEntry_CreatesField(t *testing.T) {
	path := setupClaudeConfig(t, `{"other":"field"}`)
	c := &claudeCode{path: path}

	err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %v", parsed["mcpServers"])
	}
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want http://localhost:9121/mcp", serena["url"])
	}
	// Original field preserved
	if parsed["other"] != "field" {
		t.Error("original field dropped")
	}
}

func TestClaudeCode_AddEntry_Replaces(t *testing.T) {
	path := setupClaudeConfig(t, `{"mcpServers":{"serena":{"url":"http://old"}}}`)
	c := &claudeCode{path: path}
	_ = c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})

	entry, _ := c.GetEntry("serena")
	if entry == nil || entry.URL != "http://localhost:9121/mcp" {
		t.Errorf("entry not replaced: %v", entry)
	}
}

func TestClaudeCode_RemoveEntry(t *testing.T) {
	path := setupClaudeConfig(t, `{"mcpServers":{"serena":{"url":"http://x"},"other":{"url":"http://y"}}}`)
	c := &claudeCode{path: path}
	_ = c.RemoveEntry("serena")

	entry, _ := c.GetEntry("serena")
	if entry != nil {
		t.Errorf("serena still present: %v", entry)
	}
	other, _ := c.GetEntry("other")
	if other == nil {
		t.Error("other entry should still be present")
	}
}

func TestClaudeCode_BackupRestore(t *testing.T) {
	original := `{"mcpServers":{"serena":{"url":"http://old"}}}`
	path := setupClaudeConfig(t, original)
	c := &claudeCode{path: path}

	bak, err := c.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Mutate live file
	_ = c.AddEntry(MCPEntry{Name: "serena", URL: "http://new"})
	// Restore
	if err := c.Restore(bak); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("after restore = %q, want %q", got, original)
	}
}
```

- [ ] **Step 2: Run — confirm failure**

Run: `go test ./internal/clients/...`
Expected: undefined `claudeCode`.

- [ ] **Step 3: Implement `internal/clients/claude_code.go`**

```go
package clients

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// NewClaudeCode returns a Client bound to the current user's ~/.claude/settings.json.
func NewClaudeCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &claudeCode{path: filepath.Join(home, ".claude", "settings.json")}, nil
}

type claudeCode struct {
	path string
}

func (c *claudeCode) Name() string       { return "claude-code" }
func (c *claudeCode) ConfigPath() string { return c.path }

func (c *claudeCode) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

func (c *claudeCode) Backup() (string, error) {
	if !c.Exists() {
		return "", &ErrClientNotInstalled{Client: c.Name()}
	}
	ts := time.Now().Format("20060102-150405")
	bak := c.path + ".bak-mcp-local-hub-" + ts
	in, err := os.Open(c.path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	return bak, nil
}

func (c *claudeCode) Restore(backupPath string) error {
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// readJSON / writeJSON keep unknown top-level fields untouched by round-tripping
// through map[string]any.
func (c *claudeCode) readJSON() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *claudeCode) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Append trailing newline to match Claude Code's own formatting preference.
	return os.WriteFile(c.path, append(out, '\n'), 0600)
}

func (c *claudeCode) AddEntry(entry MCPEntry) error {
	m, err := c.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{"url": entry.URL}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return c.writeJSON(m)
}

func (c *claudeCode) RemoveEntry(name string) error {
	m, err := c.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcpServers"] = servers
	return c.writeJSON(m)
}

func (c *claudeCode) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	return &MCPEntry{Name: name, URL: url}, nil
}
```

- [ ] **Step 4: Run tests — confirm pass**

Run: `go test ./internal/clients/...`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/clients/claude_code.go internal/clients/claude_code_test.go
git commit -m "feat(clients): Claude Code adapter"
```

---

### Task 11: Codex CLI client adapter (TOML)

**Files:**
- Create: `internal/clients/codex_cli.go`, `internal/clients/codex_cli_test.go`

- [ ] **Step 1: Add TOML dependency**

Run: `go get github.com/pelletier/go-toml/v2`

Rationale: `go-toml/v2` preserves unknown fields across unmarshal/marshal, which is essential for a config file the hub shares with other sections (`[projects]`, `[notice]`, etc.).

- [ ] **Step 2: Write failing test `internal/clients/codex_cli_test.go`**

```go
package clients

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupCodexConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexCLI_AddEntry_ReplaceStdioBlock(t *testing.T) {
	initial := `[mcp_servers.serena]
command = "uvx"
args = ["--from", "git+...", "serena", "start-mcp-server"]
startup_timeout_sec = 30.0

[mcp_servers.other]
command = "echo"
args = ["hi"]
`
	path := setupCodexConfig(t, initial)
	c := &codexCLI{path: path}

	err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9122/mcp"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)
	if !strings.Contains(s, `url = "http://localhost:9122/mcp"`) {
		t.Errorf("URL not set: %s", s)
	}
	if strings.Contains(s, `command = "uvx"`) && strings.Contains(s[strings.Index(s, "[mcp_servers.serena]"):strings.Index(s, "[mcp_servers.other]")], "command") {
		t.Error("old command line not removed from serena block")
	}
	// Other section preserved
	if !strings.Contains(s, "[mcp_servers.other]") {
		t.Error("other section dropped")
	}
}

func TestCodexCLI_RemoveEntry(t *testing.T) {
	initial := `[mcp_servers.serena]
url = "http://localhost:9122/mcp"

[mcp_servers.memory]
url = "http://localhost:9140/mcp"
`
	path := setupCodexConfig(t, initial)
	c := &codexCLI{path: path}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "serena") {
		t.Errorf("serena not removed: %s", raw)
	}
	if !strings.Contains(string(raw), "memory") {
		t.Error("memory also removed (should be preserved)")
	}
}
```

- [ ] **Step 3: Run — confirm failure**

Run: `go test ./internal/clients/... -run TestCodexCLI`
Expected: undefined `codexCLI`.

- [ ] **Step 4: Implement `internal/clients/codex_cli.go`**

```go
package clients

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// NewCodexCLI returns a Client bound to ~/.codex/config.toml.
func NewCodexCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &codexCLI{path: filepath.Join(home, ".codex", "config.toml")}, nil
}

type codexCLI struct {
	path string
}

func (c *codexCLI) Name() string       { return "codex-cli" }
func (c *codexCLI) ConfigPath() string { return c.path }

func (c *codexCLI) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

func (c *codexCLI) Backup() (string, error) {
	if !c.Exists() {
		return "", &ErrClientNotInstalled{Client: c.Name()}
	}
	ts := time.Now().Format("20060102-150405")
	bak := c.path + ".bak-mcp-local-hub-" + ts
	in, err := os.Open(c.path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return bak, err
}

func (c *codexCLI) Restore(backupPath string) error {
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// readTOML / writeTOML round-trip through map[string]any so unknown sections survive.
func (c *codexCLI) readTOML() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *codexCLI) writeTOML(m map[string]any) error {
	out, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, out, 0600)
}

func (c *codexCLI) AddEntry(entry MCPEntry) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Replace the entry wholesale — this drops any stdio-era fields like `command`/`args`.
	entryMap := map[string]any{
		"url":                 entry.URL,
		"startup_timeout_sec": 10.0,
	}
	if len(entry.Headers) > 0 {
		entryMap["http_headers"] = entry.Headers
	}
	servers[entry.Name] = entryMap
	m["mcp_servers"] = servers
	return c.writeTOML(m)
}

func (c *codexCLI) RemoveEntry(name string) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcp_servers"] = servers
	return c.writeTOML(m)
}

func (c *codexCLI) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	return &MCPEntry{Name: name, URL: url}, nil
}
```

- [ ] **Step 5: Run tests — confirm pass**

Run: `go test ./internal/clients/...`
Expected: `ok`

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/clients/codex_cli.go internal/clients/codex_cli_test.go
git commit -m "feat(clients): Codex CLI adapter (TOML)"
```

---

### Task 12: Gemini CLI + Antigravity adapters (parallel JSON format)

**Files:**
- Create: `internal/clients/gemini_cli.go`, `internal/clients/gemini_cli_test.go`, `internal/clients/antigravity.go`, `internal/clients/antigravity_test.go`

Both Gemini CLI and Antigravity use the same JSON schema (`mcpServers.<name>.httpUrl`). This task implements both as thin wrappers around a shared helper.

- [ ] **Step 1: Extract JSON helper into `internal/clients/json_mcp.go`**

```go
package clients

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// jsonMCPClient is a reusable struct that handles JSON-format MCP configs
// with the `mcpServers.<name>.httpUrl` schema shared by Gemini CLI and Antigravity.
// clientName and urlField distinguish the two (field name is "httpUrl" for both,
// kept parameterized in case a future client uses a different field).
type jsonMCPClient struct {
	path       string
	clientName string
	urlField   string // "httpUrl" for both known cases
}

func (j *jsonMCPClient) Name() string       { return j.clientName }
func (j *jsonMCPClient) ConfigPath() string { return j.path }

func (j *jsonMCPClient) Exists() bool {
	_, err := os.Stat(j.path)
	return err == nil
}

func (j *jsonMCPClient) Backup() (string, error) {
	if !j.Exists() {
		return "", &ErrClientNotInstalled{Client: j.clientName}
	}
	ts := time.Now().Format("20060102-150405")
	bak := j.path + ".bak-mcp-local-hub-" + ts
	in, err := os.Open(j.path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return bak, err
}

func (j *jsonMCPClient) Restore(backupPath string) error {
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(j.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func (j *jsonMCPClient) readJSON() (map[string]any, error) {
	data, err := os.ReadFile(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", j.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (j *jsonMCPClient) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(j.path, append(out, '\n'), 0600)
}

func (j *jsonMCPClient) AddEntry(entry MCPEntry) error {
	m, err := j.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		j.urlField: entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return j.writeJSON(m)
}

func (j *jsonMCPClient) RemoveEntry(name string) error {
	m, err := j.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcpServers"] = servers
	return j.writeJSON(m)
}

func (j *jsonMCPClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := j.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw[j.urlField].(string)
	return &MCPEntry{Name: name, URL: url}, nil
}
```

- [ ] **Step 2: Write `internal/clients/gemini_cli.go`**

```go
package clients

import (
	"os"
	"path/filepath"
)

// NewGeminiCLI returns a Client bound to ~/.gemini/settings.json.
func NewGeminiCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "settings.json"),
		clientName: "gemini-cli",
		urlField:   "httpUrl",
	}, nil
}
```

- [ ] **Step 3: Write `internal/clients/antigravity.go`**

```go
package clients

import (
	"os"
	"path/filepath"
)

// NewAntigravity returns a Client bound to ~/.gemini/antigravity/mcp_config.json.
func NewAntigravity() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		clientName: "antigravity",
		urlField:   "httpUrl",
	}, nil
}
```

- [ ] **Step 4: Write shared test `internal/clients/json_mcp_test.go`**

```go
package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newJSONClientForTest(t *testing.T, initial string) *jsonMCPClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &jsonMCPClient{path: path, clientName: "test", urlField: "httpUrl"}
}

func TestJSONMCP_AddReplacesStdio(t *testing.T) {
	j := newJSONClientForTest(t, `{
  "mcpServers": {
    "serena": {
      "command": "uvx",
      "args": ["--from", "git+...", "serena", "start-mcp-server"],
      "disabled": false
    }
  }
}`)
	if err := j.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9123/mcp"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(j.path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["httpUrl"] != "http://localhost:9123/mcp" {
		t.Errorf("httpUrl = %v, want http://localhost:9123/mcp", serena["httpUrl"])
	}
	if _, ok := serena["command"]; ok {
		t.Error("old command field not removed")
	}
}

func TestJSONMCP_RemoveEntry_Idempotent(t *testing.T) {
	j := newJSONClientForTest(t, `{"mcpServers":{}}`)
	if err := j.RemoveEntry("nonexistent"); err != nil {
		t.Errorf("remove of nonexistent should be nil, got %v", err)
	}
}
```

- [ ] **Step 5: Run all client tests**

Run: `go test ./internal/clients/...`
Expected: `ok`

- [ ] **Step 6: Commit**

```bash
git add internal/clients/json_mcp.go internal/clients/json_mcp_test.go internal/clients/gemini_cli.go internal/clients/antigravity.go
git commit -m "feat(clients): Gemini CLI + Antigravity adapters (shared JSON helper)"
```

---

### Task 13: Log rotation utility

**Files:**
- Create: `internal/daemon/logrotate.go`, `internal/daemon/logrotate_test.go`

- [ ] **Step 1: Write failing test**

```go
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateIfLarge_RotatesWhenOverLimit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	big := make([]byte, 11*1024*1024) // 11 MB
	os.WriteFile(logPath, big, 0600)

	if err := RotateIfLarge(logPath, 10*1024*1024, 5); err != nil {
		t.Fatalf("RotateIfLarge: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	var rotated int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "test.log.") {
			rotated++
		}
	}
	if rotated != 1 {
		t.Errorf("expected 1 rotated file, got %d", rotated)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Error("original log should be removed after rotation")
	}
}

func TestRotateIfLarge_SkipsSmall(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	os.WriteFile(logPath, []byte("small"), 0600)

	if err := RotateIfLarge(logPath, 10*1024*1024, 5); err != nil {
		t.Fatalf("RotateIfLarge: %v", err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Error("small log should not be rotated")
	}
}

func TestRotateIfLarge_PrunesOldestWhenOverCount(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	// Create 6 pre-existing rotated files
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "test.log.2026010"+string(rune('0'+i))+"-000000")
		os.WriteFile(p, []byte("x"), 0600)
	}
	big := make([]byte, 11*1024*1024)
	os.WriteFile(logPath, big, 0600)

	if err := RotateIfLarge(logPath, 10*1024*1024, 5); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var rotated int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "test.log.") {
			rotated++
		}
	}
	if rotated != 5 {
		t.Errorf("expected 5 rotated files after prune, got %d", rotated)
	}
}
```

- [ ] **Step 2: Run — confirm failure**

Run: `go test ./internal/daemon/...`

- [ ] **Step 3: Implement `internal/daemon/logrotate.go`**

```go
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RotateIfLarge moves logPath to "<logPath>.<timestamp>" if its size exceeds maxSize.
// After rotation, prunes oldest rotated siblings so no more than keepCount remain.
// Returns nil if the file doesn't exist or is smaller than maxSize.
func RotateIfLarge(logPath string, maxSize int64, keepCount int) error {
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxSize {
		return nil
	}
	ts := time.Now().Format("20060102-150405")
	rotated := logPath + "." + ts
	if err := os.Rename(logPath, rotated); err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	return pruneOldRotations(logPath, keepCount)
}

// pruneOldRotations deletes rotated siblings beyond keepCount, keeping the newest.
// Rotation files are identified by the prefix `filepath.Base(logPath)+"."`.
func pruneOldRotations(logPath string, keepCount int) error {
	dir := filepath.Dir(logPath)
	base := filepath.Base(logPath) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type rotatedFile struct {
		path string
		mod  time.Time
	}
	var rotations []rotatedFile
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rotations = append(rotations, rotatedFile{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	if len(rotations) <= keepCount {
		return nil
	}
	sort.Slice(rotations, func(i, j int) bool {
		return rotations[i].mod.After(rotations[j].mod) // newest first
	})
	for _, r := range rotations[keepCount:] {
		_ = os.Remove(r.path)
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/daemon/...`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/logrotate.go internal/daemon/logrotate_test.go
git commit -m "feat(daemon): size-based log rotation"
```

---

### Task 14: Process launcher (exec child, tee output, propagate exit)

**Files:**
- Create: `internal/daemon/launcher.go`, `internal/daemon/launcher_test.go`

- [ ] **Step 1: Write failing test**

```go
package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLaunch_EchoOutputIntoLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")
	spec := LaunchSpec{
		Command: func() string {
			if runtime.GOOS == "windows" {
				return "cmd"
			}
			return "sh"
		}(),
		Args: func() []string {
			if runtime.GOOS == "windows" {
				return []string{"/C", "echo hello-launcher"}
			}
			return []string{"-c", "echo hello-launcher"}
		}(),
		LogPath: logPath,
	}
	code, err := Launch(spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "hello-launcher") {
		t.Errorf("log missing output, got: %q", raw)
	}
}

func TestLaunch_PropagatesExitCode(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")
	spec := LaunchSpec{
		Command: func() string {
			if runtime.GOOS == "windows" {
				return "cmd"
			}
			return "sh"
		}(),
		Args: func() []string {
			if runtime.GOOS == "windows" {
				return []string{"/C", "exit 7"}
			}
			return []string{"-c", "exit 7"}
		}(),
		LogPath: logPath,
	}
	code, _ := Launch(spec)
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}
```

- [ ] **Step 2: Run — confirm failure**

Run: `go test ./internal/daemon/... -run TestLaunch`

- [ ] **Step 3: Implement `internal/daemon/launcher.go`**

```go
package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// LaunchSpec describes one child-process invocation.
type LaunchSpec struct {
	// Command is the program to run (looked up on PATH unless absolute).
	Command string
	// Args are passed verbatim to the child process.
	Args []string
	// Env adds/overrides environment for the child. Hub secrets are resolved before this struct.
	Env map[string]string
	// WorkingDir is optional; empty means inherit.
	WorkingDir string
	// LogPath receives both stdout and stderr. Rotated (if large) before each launch.
	LogPath string
	// MaxLogSize controls rotation threshold in bytes. Zero means 10 MB default.
	MaxLogSize int64
	// LogKeep controls how many rotated siblings to retain. Zero means 5 default.
	LogKeep int
}

// Launch executes the spec, tees stdout+stderr into a log file, and returns the
// child's exit code. This is a blocking call — intended to be used as the entire
// body of a scheduler-triggered `mcp daemon` invocation.
func Launch(spec LaunchSpec) (int, error) {
	if spec.Command == "" {
		return -1, errors.New("Launch: Command is required")
	}
	if spec.LogPath == "" {
		return -1, errors.New("Launch: LogPath is required")
	}
	maxSize := spec.MaxLogSize
	if maxSize == 0 {
		maxSize = 10 * 1024 * 1024
	}
	keep := spec.LogKeep
	if keep == 0 {
		keep = 5
	}
	if err := os.MkdirAll(filepath_Dir(spec.LogPath), 0755); err != nil {
		return -1, fmt.Errorf("mkdir log dir: %w", err)
	}
	if err := RotateIfLarge(spec.LogPath, maxSize, keep); err != nil {
		// Rotation failure is non-fatal — we still try to launch.
		fmt.Fprintf(os.Stderr, "warn: rotate failed: %v\n", err)
	}
	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return -1, fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Stdout = io.MultiWriter(logFile, os.Stdout)
	cmd.Stderr = io.MultiWriter(logFile, os.Stderr)
	cmd.Dir = spec.WorkingDir
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// filepath_Dir is a tiny local wrapper to avoid importing path/filepath here;
// kept as a single-purpose helper so this file has no conditional deps.
func filepath_Dir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/daemon/...`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/launcher.go internal/daemon/launcher_test.go
git commit -m "feat(daemon): process launcher with log tee"
```

---

### Task 15: stdio-bridge wrapper (supergateway spawner)

**Files:**
- Create: `internal/daemon/bridge.go`

- [ ] **Step 1: Write `internal/daemon/bridge.go`**

```go
package daemon

import (
	"fmt"
	"strings"
)

// BuildBridgeSpec wraps a stdio MCP server invocation into a supergateway call
// that exposes it as HTTP on `port`. Returns a LaunchSpec ready for Launch().
//
// supergateway is a well-maintained community MCP stdio↔HTTP bridge.
// We spawn it via `npx -y supergateway --stdio "<inner cmd>" --port <N>`.
//
// innerCmd + innerArgs form the stdio server's own command line. supergateway
// expects this as a single shell-quoted string in the --stdio argument.
// Env is applied to the inner command's process (supergateway forwards it).
func BuildBridgeSpec(innerCmd string, innerArgs []string, port int, env map[string]string, logPath string) LaunchSpec {
	// Shell-quote the inner command line for supergateway.
	// Simple strategy: wrap each token in double quotes if it contains whitespace.
	quoted := make([]string, 0, len(innerArgs)+1)
	quoted = append(quoted, shellQuote(innerCmd))
	for _, a := range innerArgs {
		quoted = append(quoted, shellQuote(a))
	}
	stdioArg := strings.Join(quoted, " ")
	return LaunchSpec{
		Command:    "npx",
		Args:       []string{"-y", "supergateway", "--stdio", stdioArg, "--port", fmt.Sprintf("%d", port)},
		Env:        env,
		LogPath:    logPath,
		MaxLogSize: 10 * 1024 * 1024,
		LogKeep:    5,
	}
}

// shellQuote conservatively wraps a token in double quotes when it contains
// whitespace or shell metacharacters. Backslashes on Windows paths are preserved.
func shellQuote(s string) string {
	if s == "" {
		return `""`
	}
	needs := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '"' || r == '\'' || r == '&' || r == '|' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	// Escape internal double quotes by doubling them (cmd.exe-compatible).
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./internal/daemon/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add internal/daemon/bridge.go
git commit -m "feat(daemon): stdio-bridge wrapper via supergateway"
```

---

### Task 16: `mcp secrets init/set/get/list/delete` subcommands

**Files:**
- Create: `internal/cli/secrets.go`
- Modify: `internal/cli/root.go` (replace stub `newSecretsCmd`)

- [ ] **Step 1: Write `internal/cli/secrets.go`**

```go
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dima/mcp-local-hub/internal/secrets"
	"github.com/spf13/cobra"
)

// Repo-relative paths — resolved at every call so the binary is portable.
func defaultKeyPath() string   { return filepath.Join(".", ".age-key") }
func defaultVaultPath() string { return filepath.Join(".", "secrets.age") }

func newSecretsCmdReal() *cobra.Command {
	root := &cobra.Command{Use: "secrets", Short: "Manage encrypted secrets"}
	root.AddCommand(newSecretsInitCmd())
	root.AddCommand(newSecretsSetCmd())
	root.AddCommand(newSecretsGetCmd())
	root.AddCommand(newSecretsListCmd())
	root.AddCommand(newSecretsDeleteCmd())
	return root
}

func newSecretsInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Generate identity and empty vault",
		RunE: func(cmd *cobra.Command, args []string) error {
			keyPath := defaultKeyPath()
			vaultPath := defaultVaultPath()
			if err := secrets.InitVault(keyPath, vaultPath); err != nil {
				return err
			}
			cmd.Printf("✓ Wrote %s (keep safe, gitignored)\n", keyPath)
			cmd.Printf("✓ Wrote %s (safe to commit; encrypted)\n", vaultPath)
			return nil
		},
	}
}

func newSecretsSetCmd() *cobra.Command {
	var valueFlag string
	var fromStdin bool
	c := &cobra.Command{
		Use:   "set <key>",
		Short: "Create or replace a secret value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			var value string
			switch {
			case valueFlag != "":
				value = valueFlag
			case fromStdin:
				b, err := readAllStdin()
				if err != nil {
					return err
				}
				value = strings.TrimRight(string(b), "\r\n")
			default:
				// Interactive prompt with hidden input.
				v, err := promptHidden(cmd.ErrOrStderr(), "Enter value for "+key+": ")
				if err != nil {
					return err
				}
				value = v
			}
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			if err := v.Set(key, value); err != nil {
				return err
			}
			cmd.Printf("✓ Stored %s\n", key)
			return nil
		},
	}
	c.Flags().StringVar(&valueFlag, "value", "", "provide value on command line (non-interactive)")
	c.Flags().BoolVar(&fromStdin, "from-stdin", false, "read value from stdin")
	return c
}

func newSecretsGetCmd() *cobra.Command {
	var show bool
	c := &cobra.Command{
		Use:   "get <key>",
		Short: "Retrieve a secret (clipboard by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			val, err := v.Get(args[0])
			if err != nil {
				return err
			}
			if show {
				cmd.Println(val)
				return nil
			}
			if err := copyToClipboard(val); err != nil {
				return fmt.Errorf("clipboard: %w (use --show to print instead)", err)
			}
			cmd.Printf("✓ Copied %s to clipboard\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&show, "show", false, "print value to stdout instead of clipboard")
	return c
}

func newSecretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List secret keys (not values)",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			keys := v.List()
			if len(keys) == 0 {
				cmd.Println("(vault is empty)")
				return nil
			}
			for _, k := range keys {
				cmd.Println(k)
			}
			return nil
		},
	}
}

func newSecretsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <key>",
		Short: "Remove a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			if err := v.Delete(args[0]); err != nil {
				return err
			}
			cmd.Printf("✓ Deleted %s\n", args[0])
			return nil
		},
	}
}

func readAllStdin() ([]byte, error) {
	r := bufio.NewReader(os.Stdin)
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if errors.Is(err, os.ErrInvalid) || err.Error() == "EOF" {
				break
			}
			return out, nil
		}
	}
	return out, nil
}
```

- [ ] **Step 2: Add clipboard + hidden-input helpers `internal/cli/interactive.go`**

```go
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/atotto/clipboard"
	"golang.org/x/term"
)

// copyToClipboard places value on the OS clipboard.
// Non-Windows platforms also supported via github.com/atotto/clipboard.
func copyToClipboard(value string) error {
	return clipboard.WriteAll(value)
}

// promptHidden writes prompt to w and reads a line from stdin with input hidden.
func promptHidden(w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	b, err := term.ReadPassword(0) // fd 0 = stdin
	fmt.Fprintln(w)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
```

- [ ] **Step 3: Add dependencies**

Run:
```
go get github.com/atotto/clipboard
go get golang.org/x/term
```

- [ ] **Step 4: Replace stub in `internal/cli/root.go`**

Find `func newSecretsCmd() *cobra.Command` and replace its body with:

```go
func newSecretsCmd() *cobra.Command {
	return newSecretsCmdReal()
}
```

- [ ] **Step 5: Integration test — roundtrip via CLI**

```bash
cd <repo>
go build -o mcp.exe ./cmd/mcp
.\mcp.exe secrets init
# expected: two ✓ lines
.\mcp.exe secrets set TEST --value hello
.\mcp.exe secrets list
# expected: TEST
.\mcp.exe secrets get TEST --show
# expected: hello
.\mcp.exe secrets delete TEST
.\mcp.exe secrets list
# expected: (vault is empty)
```

Expected: all commands succeed.

- [ ] **Step 6: Cleanup generated files + commit**

```bash
# Remove test identity created during manual verification
rm -f .age-key secrets.age
git add go.mod go.sum internal/cli/secrets.go internal/cli/interactive.go internal/cli/root.go
git commit -m "feat(cli): secrets init/set/get/list/delete"
```

---

### Task 17: `mcp secrets edit` — open decrypted vault in $EDITOR

**Files:**
- Modify: `internal/cli/secrets.go` (add new subcommand), `internal/secrets/vault.go` (add bulk export/import)

- [ ] **Step 1: Write failing test `internal/secrets/vault_bulk_test.go`**

```go
package secrets

import (
	"path/filepath"
	"testing"
)

func TestVault_ExportYAML_ImportYAML_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("A", "1")
	v.Set("B", "two with spaces")

	raw, err := v.ExportYAML()
	if err != nil {
		t.Fatalf("ExportYAML: %v", err)
	}

	// Wipe and reimport.
	for _, k := range v.List() {
		_ = v.Delete(k)
	}
	if err := v.ImportYAML(raw); err != nil {
		t.Fatalf("ImportYAML: %v", err)
	}
	if got, _ := v.Get("A"); got != "1" {
		t.Errorf("A = %q, want 1", got)
	}
	if got, _ := v.Get("B"); got != "two with spaces" {
		t.Errorf("B = %q, want 'two with spaces'", got)
	}
}
```

- [ ] **Step 2: Implement bulk helpers in `internal/secrets/vault.go`** — append these methods to the file:

```go
// ExportYAML returns the current vault contents as YAML bytes (for editor workflow).
func (v *Vault) ExportYAML() ([]byte, error) {
	return yaml.Marshal(v.data)
}

// ImportYAML replaces the entire vault with the contents of raw (YAML key/value map).
// The backing file is re-encrypted with the current identity.
func (v *Vault) ImportYAML(raw []byte) error {
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("import yaml: %w", err)
	}
	if m == nil {
		m = map[string]string{}
	}
	v.data = m
	return v.save()
}
```

Add `"gopkg.in/yaml.v3"` to the `import` block at the top of vault.go (aliased as `yaml` since the package name is `yaml`).

- [ ] **Step 3: Run vault tests**

Run: `go test ./internal/secrets/...`
Expected: `ok`

- [ ] **Step 4: Add `edit` subcommand in `internal/cli/secrets.go`** — add to `newSecretsCmdReal()` and define:

```go
func newSecretsEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open decrypted vault in $EDITOR and re-encrypt on save",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			yamlBytes, err := v.ExportYAML()
			if err != nil {
				return err
			}
			tmp, err := os.CreateTemp("", "mcp-secrets-*.yaml")
			if err != nil {
				return err
			}
			tmpPath := tmp.Name()
			defer func() {
				// Best-effort secure wipe: overwrite with zeros, then delete.
				zeros := make([]byte, 4096)
				if f, err := os.OpenFile(tmpPath, os.O_WRONLY, 0600); err == nil {
					_, _ = f.Write(zeros)
					f.Close()
				}
				_ = os.Remove(tmpPath)
			}()
			if _, err := tmp.Write(yamlBytes); err != nil {
				tmp.Close()
				return err
			}
			tmp.Close()

			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "notepad" // Windows fallback
			}
			c := exec.Command(editor, tmpPath)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("editor: %w", err)
			}
			updated, err := os.ReadFile(tmpPath)
			if err != nil {
				return err
			}
			if err := v.ImportYAML(updated); err != nil {
				return err
			}
			cmd.Println("✓ Re-encrypted secrets.age")
			return nil
		},
	}
}
```

Also add `"os/exec"` to the imports of `secrets.go` and register the subcommand:

```go
// inside newSecretsCmdReal():
root.AddCommand(newSecretsEditCmd())
```

- [ ] **Step 5: Build + manual test**

```bash
go build -o mcp.exe ./cmd/mcp
.\mcp.exe secrets init
.\mcp.exe secrets set A --value one
$env:EDITOR = "notepad"
.\mcp.exe secrets edit
# notepad opens with "A: one" — add "B: two", save, close
.\mcp.exe secrets list
# expected: A, B
.\mcp.exe secrets get B --show
# expected: two
```

- [ ] **Step 6: Commit**

```bash
git add internal/secrets/vault.go internal/secrets/vault_bulk_test.go internal/cli/secrets.go
git commit -m "feat(cli): secrets edit via \$EDITOR"
```

---

### Task 18: `mcp secrets migrate --from-client <name>`

**Files:**
- Create: `internal/secrets/migrate.go`, `internal/secrets/migrate_test.go`
- Modify: `internal/cli/secrets.go` (add subcommand)

- [ ] **Step 1: Write failing test `internal/secrets/migrate_test.go`**

```go
package secrets

import (
	"strings"
	"testing"
)

func TestScanCodexConfig_FindsApiKey(t *testing.T) {
	toml := `[mcp_servers.wolfram.env]
WOLFRAM_LLM_APP_ID = "EXAMPLE_APP_ID_123"
`
	candidates := ScanConfigText(toml)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(candidates), candidates)
	}
	if candidates[0].Key != "WOLFRAM_LLM_APP_ID" {
		t.Errorf("Key = %q, want WOLFRAM_LLM_APP_ID", candidates[0].Key)
	}
	if candidates[0].Value != "EXAMPLE_APP_ID_123" {
		t.Errorf("Value = %q, want EXAMPLE_APP_ID_123", candidates[0].Value)
	}
}

func TestScanConfigText_SkipsPlaceholders(t *testing.T) {
	toml := `
KEY_1 = "CONTEXT7_API_KEY"
KEY_2 = "your-api-key-here"
`
	candidates := ScanConfigText(toml)
	for _, c := range candidates {
		if strings.ToUpper(c.Value) == c.Key {
			t.Errorf("placeholder %q should be skipped", c.Value)
		}
		if strings.Contains(strings.ToLower(c.Value), "your-api-key") {
			t.Errorf("placeholder-style %q should be skipped", c.Value)
		}
	}
}
```

- [ ] **Step 2: Implement `internal/secrets/migrate.go`**

```go
package secrets

import (
	"regexp"
	"strings"
)

// Candidate is a plausible secret value found in an MCP client config file.
// Callers typically show candidates to the user interactively and let them
// approve each import into the vault.
type Candidate struct {
	Key   string // e.g., "WOLFRAM_LLM_APP_ID"
	Value string // e.g., "EXAMPLE_APP_ID_123"
}

// secretLineRe matches lines that look like `KEY = "VALUE"` (TOML) or `"KEY": "VALUE"` (JSON).
// The key is captured as group 1, the value as group 2.
var secretLineRe = regexp.MustCompile(`(?m)["\s]*([A-Z][A-Z0-9_]*(?:API_KEY|TOKEN|SECRET|APP_ID|PASSWORD|PASS))["\s]*[:=]\s*"([^"]+)"`)

// ScanConfigText examines raw TOML or JSON text and returns secret-shaped
// key/value pairs. Placeholder values (equal to their own key, containing
// common scaffolding strings like "your-", "example", "changeme") are filtered.
func ScanConfigText(text string) []Candidate {
	var out []Candidate
	for _, match := range secretLineRe.FindAllStringSubmatch(text, -1) {
		key := match[1]
		value := match[2]
		if isPlaceholder(key, value) {
			continue
		}
		out = append(out, Candidate{Key: key, Value: value})
	}
	return out
}

func isPlaceholder(key, value string) bool {
	lv := strings.ToLower(value)
	if strings.EqualFold(value, key) {
		return true
	}
	for _, needle := range []string{"your-", "your_", "example", "changeme", "replace-me", "<", ">", "xxx"} {
		if strings.Contains(lv, needle) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run test**

Run: `go test ./internal/secrets/... -run TestScanConfigText`
Expected: `ok`

- [ ] **Step 4: Wire `migrate --from-client` subcommand in `internal/cli/secrets.go`**

Add to `newSecretsCmdReal()`:
```go
root.AddCommand(newSecretsMigrateCmd())
```

Define:
```go
func newSecretsMigrateCmd() *cobra.Command {
	var fromClient string
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Import hardcoded secrets from a client config",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := clientConfigPath(fromClient)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			candidates := secrets.ScanConfigText(string(data))
			if len(candidates) == 0 {
				cmd.Println("No candidates found.")
				return nil
			}
			v, err := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			if err != nil {
				return err
			}
			in := bufio.NewReader(os.Stdin)
			imported := 0
			for _, cand := range candidates {
				cmd.Printf("Found %s = %s (from %s)\n", cand.Key, maskValue(cand.Value), path)
				cmd.Print("Import? [y/N]: ")
				line, _ := in.ReadString('\n')
				line = strings.TrimSpace(strings.ToLower(line))
				if line == "y" || line == "yes" {
					if err := v.Set(cand.Key, cand.Value); err != nil {
						return err
					}
					imported++
				}
			}
			cmd.Printf("✓ Imported %d secrets. Original file NOT modified — run `mcp install` to apply.\n", imported)
			return nil
		},
	}
	c.Flags().StringVar(&fromClient, "from-client", "", "client name: claude-code | codex-cli | gemini-cli | antigravity")
	_ = c.MarkFlagRequired("from-client")
	return c
}

func clientConfigPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch name {
	case "claude-code":
		return filepath.Join(home, ".claude", "settings.json"), nil
	case "codex-cli":
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "gemini-cli":
		return filepath.Join(home, ".gemini", "settings.json"), nil
	case "antigravity":
		return filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), nil
	default:
		return "", fmt.Errorf("unknown client %q (expected claude-code | codex-cli | gemini-cli | antigravity)", name)
	}
}

func maskValue(v string) string {
	if len(v) <= 4 {
		return "***"
	}
	return v[:2] + strings.Repeat("*", len(v)-4) + v[len(v)-2:]
}
```

- [ ] **Step 5: Commit**

```bash
git add internal/secrets/migrate.go internal/secrets/migrate_test.go internal/cli/secrets.go
git commit -m "feat(cli): secrets migrate from client config"
```

---

### Task 19: `mcp install --dry-run` (manifest → planned actions)

**Files:**
- Create: `internal/cli/install.go`

This task implements the planning phase of install: read manifest, resolve bindings, describe what would happen, without making changes.

- [ ] **Step 1: Implement `internal/cli/install.go`**

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dima/mcp-local-hub/internal/clients"
	"github.com/dima/mcp-local-hub/internal/config"
	"github.com/spf13/cobra"
)

// Plan describes the side effects that `mcp install --server X` would produce.
// Returned by BuildPlan and rendered by `install --dry-run`.
type Plan struct {
	Server        string
	SchedulerTasks []ScheduledTaskPlan
	ClientUpdates  []ClientUpdatePlan
}

type ScheduledTaskPlan struct {
	Name    string
	Command string
	Args    []string
	Trigger string // human-readable
}

type ClientUpdatePlan struct {
	Client string
	Path   string
	Action string // "add" | "replace"
	URL    string
}

func newInstallCmdReal() *cobra.Command {
	var server string
	var dryRun bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			manifestPath := filepath.Join("servers", server, "manifest.yaml")
			f, err := os.Open(manifestPath)
			if err != nil {
				return fmt.Errorf("open %s: %w", manifestPath, err)
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil {
				return err
			}
			plan, err := BuildPlan(m)
			if err != nil {
				return err
			}
			if dryRun {
				return printPlan(cmd, plan)
			}
			return fmt.Errorf("real install not yet implemented — use --dry-run (Task 20 wires this)")
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (matches servers/<name>/manifest.yaml)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without making changes")
	return c
}

// BuildPlan translates a manifest into concrete intended actions.
func BuildPlan(m *config.ServerManifest) (*Plan, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	p := &Plan{Server: m.Name}
	// Scheduler tasks — one per daemon (global) or lazy (workspace-scoped).
	for _, d := range m.Daemons {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-" + d.Name,
			Command: exe,
			Args:    []string{"daemon", "--server", m.Name, "--daemon", d.Name},
			Trigger: "At logon",
		})
	}
	if m.WeeklyRefresh {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-weekly-refresh",
			Command: exe,
			Args:    []string{"restart", "--server", m.Name},
			Trigger: "Weekly Sun 03:00",
		})
	}
	// Client updates — one per binding.
	for _, b := range m.ClientBindings {
		daemon, ok := findDaemon(m, b.Daemon)
		if !ok {
			return nil, fmt.Errorf("binding references unknown daemon %q", b.Daemon)
		}
		path, err := clientConfigPath(b.Client)
		if err != nil {
			return nil, err
		}
		urlPath := b.URLPath
		if urlPath == "" {
			urlPath = "/mcp"
		}
		url := fmt.Sprintf("http://localhost:%d%s", daemon.Port, urlPath)
		p.ClientUpdates = append(p.ClientUpdates, ClientUpdatePlan{
			Client: b.Client,
			Path:   path,
			Action: "add/replace",
			URL:    url,
		})
	}
	return p, nil
}

func findDaemon(m *config.ServerManifest, name string) (config.DaemonSpec, bool) {
	for _, d := range m.Daemons {
		if d.Name == name {
			return d, true
		}
	}
	return config.DaemonSpec{}, false
}

func printPlan(cmd *cobra.Command, p *Plan) error {
	cmd.Printf("Install plan for server %q (dry-run):\n\n", p.Server)
	cmd.Printf("  Scheduler tasks to create (%d):\n", len(p.SchedulerTasks))
	for _, t := range p.SchedulerTasks {
		cmd.Printf("    • %s  [%s]\n        %s %v\n", t.Name, t.Trigger, t.Command, t.Args)
	}
	cmd.Printf("\n  Client configs to update (%d):\n", len(p.ClientUpdates))
	for _, u := range p.ClientUpdates {
		cmd.Printf("    • %s (%s)\n        %s  →  %s\n", u.Client, u.Path, u.Action, u.URL)
	}
	cmd.Println("\nNo changes made.")
	_ = clients.Client(nil) // keep import live for later tasks
	return nil
}
```

- [ ] **Step 2: Wire into root**

In `internal/cli/root.go`, replace `newInstallCmd` body with:
```go
func newInstallCmd() *cobra.Command {
	return newInstallCmdReal()
}
```

- [ ] **Step 3: Build + verify dry-run produces sensible output for a not-yet-existing manifest**

Run: `go build -o mcp.exe ./cmd/mcp`
Run: `.\mcp.exe install --server serena --dry-run`
Expected: error "open servers/serena/manifest.yaml" (file doesn't exist yet — that's fine; real run happens in Phase 1 task 29).

- [ ] **Step 4: Commit**

```bash
git add internal/cli/install.go internal/cli/root.go
git commit -m "feat(cli): install --dry-run with manifest planner"
```

---

### Task 20: `mcp install` (real install — scheduler + client configs)

**Files:**
- Modify: `internal/cli/install.go`

- [ ] **Step 1: Extend `newInstallCmdReal()` with real execution path**

Replace the `return fmt.Errorf("real install not yet implemented ...")` line with a call to `executeInstall(cmd, m, plan)`, and add the function:

```go
func executeInstall(cmd *cobra.Command, m *config.ServerManifest, p *Plan) error {
	sch, err := scheduler.New()
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	repoDir, err := os.Getwd()
	if err != nil {
		return err
	}
	// 1. Create scheduler tasks.
	for _, t := range p.SchedulerTasks {
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          t.Command,
			Args:             t.Args,
			WorkingDir:       repoDir,
			RestartOnFailure: true,
		}
		if t.Trigger == "At logon" {
			spec.LogonTrigger = true
		} else if t.Trigger == "Weekly Sun 03:00" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		}
		// Delete any previous instance so Create is idempotent.
		_ = sch.Delete(spec.Name)
		if err := sch.Create(spec); err != nil {
			return fmt.Errorf("create task %s: %w", spec.Name, err)
		}
		cmd.Printf("✓ Scheduler task created: %s\n", spec.Name)
	}
	// 2. Backup + update client configs.
	allClients := mustAllClients()
	for _, u := range p.ClientUpdates {
		client := allClients[u.Client]
		if client == nil {
			return fmt.Errorf("unknown client %q in binding", u.Client)
		}
		if !client.Exists() {
			cmd.Printf("⚠ Client %s not installed on this machine — skipping\n", u.Client)
			continue
		}
		bak, err := client.Backup()
		if err != nil {
			return fmt.Errorf("backup %s: %w", u.Client, err)
		}
		cmd.Printf("  backup: %s\n", bak)
		if err := client.AddEntry(clients.MCPEntry{Name: m.Name, URL: u.URL}); err != nil {
			return fmt.Errorf("add entry to %s: %w", u.Client, err)
		}
		cmd.Printf("✓ %s → %s\n", u.Client, u.URL)
	}
	// 3. Start daemons immediately (without waiting for next logon).
	for _, t := range p.SchedulerTasks {
		// Skip weekly refresh — it's triggered on schedule, not on install.
		if t.Trigger != "At logon" {
			continue
		}
		if err := sch.Run(t.Name); err != nil {
			cmd.Printf("⚠ failed to start %s immediately: %v (will start at next logon)\n", t.Name, err)
		} else {
			cmd.Printf("✓ Started: %s\n", t.Name)
		}
	}
	cmd.Println("\nInstall complete.")
	return nil
}

func mustAllClients() map[string]clients.Client {
	result := map[string]clients.Client{}
	for _, factory := range []func() (clients.Client, error){
		clients.NewClaudeCode, clients.NewCodexCLI, clients.NewGeminiCLI, clients.NewAntigravity,
	} {
		c, err := factory()
		if err != nil {
			continue
		}
		result[c.Name()] = c
	}
	return result
}
```

Add imports to the top of the file: `"github.com/dima/mcp-local-hub/internal/scheduler"`.

- [ ] **Step 2: Build**

Run: `go build -o mcp.exe ./cmd/mcp`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/install.go
git commit -m "feat(cli): install executes scheduler + client updates"
```

---

### Task 21: `mcp uninstall` (reverse install)

**Files:**
- Create: `internal/cli/uninstall.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write `internal/cli/uninstall.go`**

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dima/mcp-local-hub/internal/config"
	"github.com/dima/mcp-local-hub/internal/scheduler"
	"github.com/spf13/cobra"
)

func newUninstallCmdReal() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove an installed MCP server (scheduler + client bindings)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			manifestPath := filepath.Join("servers", server, "manifest.yaml")
			f, err := os.Open(manifestPath)
			if err != nil {
				return err
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil {
				return err
			}
			sch, err := scheduler.New()
			if err != nil {
				return err
			}
			// Delete all tasks that begin with our prefix.
			prefix := "mcp-local-hub-" + m.Name
			tasks, err := sch.List(prefix)
			if err != nil {
				return err
			}
			for _, t := range tasks {
				if err := sch.Delete(t.Name); err != nil {
					cmd.Printf("⚠ delete %s: %v\n", t.Name, err)
				} else {
					cmd.Printf("✓ Deleted task: %s\n", t.Name)
				}
			}
			// Remove client entries.
			allClients := mustAllClients()
			for _, b := range m.ClientBindings {
				client := allClients[b.Client]
				if client == nil || !client.Exists() {
					continue
				}
				if err := client.RemoveEntry(m.Name); err != nil {
					cmd.Printf("⚠ remove %s from %s: %v\n", m.Name, b.Client, err)
					continue
				}
				cmd.Printf("✓ Removed %s from %s\n", m.Name, b.Client)
			}
			cmd.Println("Uninstall complete. Client config backups (.bak-mcp-local-hub-*) remain on disk.")
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	return c
}
```

- [ ] **Step 2: Wire into root**

In `internal/cli/root.go`, replace `newUninstallCmd` body:
```go
func newUninstallCmd() *cobra.Command { return newUninstallCmdReal() }
```

- [ ] **Step 3: Build + commit**

```bash
go build -o mcp.exe ./cmd/mcp
git add internal/cli/uninstall.go internal/cli/root.go
git commit -m "feat(cli): uninstall (reverse of install)"
```

---

### Task 22: `mcp rollback` (restore latest client backups)

**Files:**
- Create: `internal/cli/rollback.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write `internal/cli/rollback.go`**

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func newRollbackCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback",
		Short: "Restore the latest mcp-local-hub backup for each client",
		RunE: func(cmd *cobra.Command, args []string) error {
			allClients := mustAllClients()
			restored := 0
			for name, c := range allClients {
				if !c.Exists() {
					continue
				}
				bak, err := findLatestBackup(c.ConfigPath())
				if err != nil {
					cmd.Printf("⚠ %s: %v\n", name, err)
					continue
				}
				if bak == "" {
					cmd.Printf("  %s: no backup found, skipping\n", name)
					continue
				}
				if err := c.Restore(bak); err != nil {
					cmd.Printf("⚠ %s restore: %v\n", name, err)
					continue
				}
				cmd.Printf("✓ %s restored from %s\n", name, bak)
				restored++
			}
			cmd.Printf("\nRolled back %d clients. Scheduler tasks untouched — run `mcp uninstall --server <name>` for each to remove tasks.\n", restored)
			return nil
		},
	}
}

// findLatestBackup locates the newest `<configPath>.bak-mcp-local-hub-*` sibling.
func findLatestBackup(configPath string) (string, error) {
	dir := filepath.Dir(configPath)
	base := filepath.Base(configPath) + ".bak-mcp-local-hub-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base) {
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	if len(backups) == 0 {
		return "", nil
	}
	sort.Strings(backups) // lexicographic == chronological due to timestamp format
	return backups[len(backups)-1], nil
}

func _unused_error_wrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("rollback: %w", err)
}
```

- [ ] **Step 2: Wire into root**

```go
func newRollbackCmd() *cobra.Command { return newRollbackCmdReal() }
```

- [ ] **Step 3: Build + commit**

```bash
go build -o mcp.exe ./cmd/mcp
git add internal/cli/rollback.go internal/cli/root.go
git commit -m "feat(cli): rollback restores latest client backups"
```

---

### Task 23: `mcp status` and `mcp restart`

**Files:**
- Create: `internal/cli/status.go`, `internal/cli/restart.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write `internal/cli/status.go`**

```go
package cli

import (
	"github.com/dima/mcp-local-hub/internal/scheduler"
	"github.com/spf13/cobra"
)

func newStatusCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show scheduler task state for all mcp-local-hub daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			sch, err := scheduler.New()
			if err != nil {
				return err
			}
			tasks, err := sch.List("mcp-local-hub-")
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				cmd.Println("No mcp-local-hub tasks installed.")
				return nil
			}
			cmd.Printf("%-45s  %-10s  %-12s  %s\n", "NAME", "STATE", "LAST RESULT", "NEXT RUN")
			for _, t := range tasks {
				cmd.Printf("%-45s  %-10s  %-12d  %s\n", t.Name, t.State, t.LastResult, t.NextRun)
			}
			return nil
		},
	}
}
```

- [ ] **Step 2: Write `internal/cli/restart.go`**

```go
package cli

import (
	"github.com/dima/mcp-local-hub/internal/scheduler"
	"github.com/spf13/cobra"
)

func newRestartCmdReal() *cobra.Command {
	var server string
	var all bool
	c := &cobra.Command{
		Use:   "restart",
		Short: "Restart daemon(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			sch, err := scheduler.New()
			if err != nil {
				return err
			}
			prefix := "mcp-local-hub-"
			if server != "" {
				prefix = "mcp-local-hub-" + server
			}
			if !all && server == "" {
				return cmd.Help()
			}
			tasks, err := sch.List(prefix)
			if err != nil {
				return err
			}
			for _, t := range tasks {
				// Skip weekly-refresh tasks when scope is --all (they trigger themselves).
				if all && (containsSuffix(t.Name, "-weekly-refresh")) {
					continue
				}
				_ = sch.Stop(t.Name)
				if err := sch.Run(t.Name); err != nil {
					cmd.Printf("⚠ run %s: %v\n", t.Name, err)
					continue
				}
				cmd.Printf("✓ Restarted %s\n", t.Name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "restart only daemons for this server")
	c.Flags().BoolVar(&all, "all", false, "restart all mcp-local-hub daemons")
	return c
}

func containsSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
```

- [ ] **Step 3: Wire into root**

```go
func newStatusCmd() *cobra.Command  { return newStatusCmdReal() }
func newRestartCmd() *cobra.Command { return newRestartCmdReal() }
```

- [ ] **Step 4: Build + commit**

```bash
go build -o mcp.exe ./cmd/mcp
git add internal/cli/status.go internal/cli/restart.go internal/cli/root.go
git commit -m "feat(cli): status + restart subcommands"
```

---

### Task 24: `mcp daemon` subcommand (invoked by scheduler)

**Files:**
- Create: `internal/cli/daemon.go`
- Modify: `internal/cli/root.go`

This is the runtime target: the scheduler launches `mcp daemon --server serena --daemon claude`, and this code reads the manifest, resolves env, and exec's the actual MCP server with tee'd logs.

- [ ] **Step 1: Write `internal/cli/daemon.go`**

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dima/mcp-local-hub/internal/config"
	"github.com/dima/mcp-local-hub/internal/daemon"
	"github.com/dima/mcp-local-hub/internal/secrets"
	"github.com/spf13/cobra"
)

func newDaemonCmdReal() *cobra.Command {
	var server, daemonName string
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run a daemon (invoked by scheduler, not by humans)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" || daemonName == "" {
				return fmt.Errorf("--server and --daemon are required")
			}
			manifestPath := filepath.Join("servers", server, "manifest.yaml")
			f, err := os.Open(manifestPath)
			if err != nil {
				return err
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil {
				return err
			}
			var spec *config.DaemonSpec
			for i := range m.Daemons {
				if m.Daemons[i].Name == daemonName {
					spec = &m.Daemons[i]
					break
				}
			}
			if spec == nil {
				return fmt.Errorf("no daemon %q in %s manifest", daemonName, server)
			}
			// Resolve env.
			vault, _ := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			resolver := secrets.NewResolver(vault, nil) // TODO config.local.yaml in later task
			env, err := resolver.ResolveMap(m.Env)
			if err != nil {
				return err
			}
			// Build launch spec.
			logPath := filepath.Join(logBaseDir(), server+"-"+daemonName+".log")
			args := append([]string{}, m.BaseArgs...)
			args = append(args, spec.ExtraArgs...)
			if m.Transport == config.TransportNativeHTTP {
				args = append(args, "--port", fmt.Sprintf("%d", spec.Port))
				ls := daemon.LaunchSpec{
					Command: m.Command,
					Args:    args,
					Env:     env,
					LogPath: logPath,
				}
				code, err := daemon.Launch(ls)
				if err != nil {
					return err
				}
				os.Exit(code)
			} else if m.Transport == config.TransportStdioBridge {
				ls := daemon.BuildBridgeSpec(m.Command, args, spec.Port, env, logPath)
				code, err := daemon.Launch(ls)
				if err != nil {
					return err
				}
				os.Exit(code)
			} else {
				return fmt.Errorf("unsupported transport %q", m.Transport)
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonName, "daemon", "", "daemon name within the server manifest")
	return c
}

// logBaseDir returns the per-OS directory for daemon logs.
// Windows: %LOCALAPPDATA%\mcp-local-hub\logs
// Linux/macOS: $XDG_STATE_HOME/mcp-local-hub/logs (or ~/.local/state/mcp-local-hub/logs)
func logBaseDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "logs")
}
```

- [ ] **Step 2: Wire into root**

```go
func newDaemonCmd() *cobra.Command { return newDaemonCmdReal() }
```

- [ ] **Step 3: Build + commit**

```bash
go build -o mcp.exe ./cmd/mcp
git add internal/cli/daemon.go internal/cli/root.go
git commit -m "feat(cli): daemon subcommand — scheduler entry point"
```

---

## Phase 1 — Serena Migration

### Task 25: Create Serena manifest

**Files:**
- Create: `servers/serena/manifest.yaml`, `servers/serena/README.md`

- [ ] **Step 1: Write `servers/serena/manifest.yaml`**

```yaml
name: serena
kind: global
transport: native-http
command: uvx
base_args:
  - --refresh
  - --from
  - git+https://github.com/oraios/serena
  - serena
  - start-mcp-server
  - --transport
  - streamable-http
env: {}
daemons:
  - name: claude
    context: claude-code
    port: 9121
    extra_args: [--context, claude-code]
  - name: codex
    context: codex
    port: 9122
    extra_args: [--context, codex]
  - name: antigravity
    context: antigravity
    port: 9123
    extra_args: [--context, antigravity]
client_bindings:
  - client: claude-code
    daemon: claude
    url_path: /mcp
  - client: codex-cli
    daemon: codex
    url_path: /mcp
  - client: antigravity
    daemon: antigravity
    url_path: /mcp
  - client: gemini-cli
    daemon: antigravity
    url_path: /mcp
weekly_refresh: true
```

- [ ] **Step 2: Write `servers/serena/README.md`**

```markdown
# Serena (MCP server)

Three daemons, one per client context:

| Context       | Port | Clients            |
|---------------|------|--------------------|
| claude-code   | 9121 | Claude Code        |
| codex         | 9122 | Codex CLI          |
| antigravity   | 9123 | Antigravity, Gemini CLI |

Upstream: https://github.com/oraios/serena

Install: `mcp install --server serena`
```

- [ ] **Step 3: Update `configs/ports.yaml` with the three Serena entries**

```yaml
global:
  - server: serena
    daemon: claude
    port: 9121
  - server: serena
    daemon: codex
    port: 9122
  - server: serena
    daemon: antigravity
    port: 9123
workspace_scoped: []
```

- [ ] **Step 4: Validate manifest parses + ports.yaml has no conflicts**

Create a simple test in `internal/config/serena_test.go`:

```go
package config

import (
	"os"
	"testing"
)

func TestSerenaManifestParses(t *testing.T) {
	f, err := os.Open("../../servers/serena/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseManifest(f)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "serena" {
		t.Errorf("Name = %q", m.Name)
	}
	if len(m.Daemons) != 3 {
		t.Errorf("len(Daemons) = %d, want 3", len(m.Daemons))
	}
	if len(m.ClientBindings) != 4 {
		t.Errorf("len(ClientBindings) = %d, want 4", len(m.ClientBindings))
	}
}

func TestPortsRegistryValid(t *testing.T) {
	f, err := os.Open("../../configs/ports.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := ParsePortRegistry(f); err != nil {
		t.Fatalf("ParsePortRegistry: %v", err)
	}
}
```

- [ ] **Step 5: Run test**

Run: `go test ./internal/config/...`
Expected: `ok`

- [ ] **Step 6: Commit**

```bash
git add servers/serena/manifest.yaml servers/serena/README.md configs/ports.yaml internal/config/serena_test.go
git commit -m "feat(serena): manifest + port registry entries"
```

---

### Task 26: Pre-install verification: uvx available, ports free, clients exist

**Files:**
- Create: `internal/cli/preflight.go`

- [ ] **Step 1: Write `internal/cli/preflight.go`**

```go
package cli

import (
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/dima/mcp-local-hub/internal/config"
)

// Preflight verifies install preconditions. Returns first error found.
// Called by install before any side effects.
func Preflight(m *config.ServerManifest) error {
	// 1. Command available.
	if _, err := exec.LookPath(m.Command); err != nil {
		return fmt.Errorf("command %q not found on PATH: %w", m.Command, err)
	}
	// 2. Ports free (for global daemons).
	for _, d := range m.Daemons {
		if portInUse(d.Port) {
			return fmt.Errorf("port %d already in use (needed for daemon %s/%s)", d.Port, m.Name, d.Name)
		}
	}
	return nil
}

// portInUse returns true if a listener on the given port accepts connections.
func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
```

- [ ] **Step 2: Integrate into `newInstallCmdReal()`**

In `internal/cli/install.go`, inside the `RunE` function, after `config.ParseManifest` and before `BuildPlan`:

```go
if err := Preflight(m); err != nil {
    return err
}
```

- [ ] **Step 3: Build + commit**

```bash
go build -o mcp.exe ./cmd/mcp
git add internal/cli/preflight.go internal/cli/install.go
git commit -m "feat(install): preflight checks (command, ports)"
```

---

### Task 27: End-to-end manual verification — install Serena

**Files:** none new — this is a verification task.

- [ ] **Step 1: Backup current client configs outside the tool**

```powershell
cp ~\.claude\settings.json ~\.claude\settings.json.preinstall
cp ~\.codex\config.toml ~\.codex\config.toml.preinstall
cp ~\.gemini\settings.json ~\.gemini\settings.json.preinstall
cp ~\.gemini\antigravity\mcp_config.json ~\.gemini\antigravity\mcp_config.json.preinstall
```

- [ ] **Step 2: Dry-run**

Run: `.\mcp.exe install --server serena --dry-run`
Expected output: plan listing 4 tasks (3 daemons + 1 weekly refresh) and 4 client updates (9121, 9122, 9123, 9123).

- [ ] **Step 3: Real install**

Run: `.\mcp.exe install --server serena`
Expected: 4 "✓ Scheduler task created" lines, 4 "backup:" + "✓ claude-code → ..." lines, 3 "✓ Started" lines.

- [ ] **Step 4: Check daemon responds**

Wait 5 seconds for uvx to bootstrap, then:
```powershell
curl -s -o $null -w "%{http_code}" http://localhost:9121/mcp
```
Expected: a number (200, 400, 405 — anything that isn't connection-refused counts).

Repeat for 9122 and 9123.

- [ ] **Step 5: Open Claude Code, run a Serena tool**

Open Claude Code. Ask it to use a Serena tool like `find_symbol`. Observe that:
- The tool call succeeds
- In a second terminal, `.\mcp.exe status` shows all 4 tasks in "Running" state
- `Get-Process | Where-Object { $_.ProcessName -like '*python*' -or $_.ProcessName -like '*uv*' }` shows exactly 3 Serena processes (not more), regardless of how many Claude Code sessions are open

- [ ] **Step 6: Run same verification for Codex CLI, Antigravity, Gemini CLI**

For each: open a fresh session, run a Serena tool, confirm the tool responds and no new Serena process is spawned.

- [ ] **Step 7: Rollback dry-run**

Run: `.\mcp.exe uninstall --server serena`
Expected: 4 deleted tasks, 4 removed entries.

- [ ] **Step 8: Restore client configs from `.bak-mcp-local-hub-*` backups if needed**

Run: `.\mcp.exe rollback`
Expected: 4 "✓ restored" lines.

- [ ] **Step 9: Final reinstall (assuming verification passed)**

Run: `.\mcp.exe install --server serena`

- [ ] **Step 10: Commit verification notes to `docs/phase-1-verification.md`**

```markdown
# Phase 1 Verification — 2026-XX-XX

- All 3 Serena daemons started via Task Scheduler at logon: ✓
- Claude Code, Codex CLI, Antigravity, Gemini CLI each call Serena tools successfully: ✓
- Only 3 Serena processes observed regardless of open sessions: ✓
- Rollback restored original configs bitwise: ✓
```

```bash
git add docs/phase-1-verification.md
git commit -m "docs: Phase 1 verification notes"
```

---

## Out of scope for this plan (deferred to follow-up plans)

- **Phase 2:** memory daemon + stdio-bridge real integration (supergateway tested end-to-end, JSONL race confirmed eliminated)
- **Phase 3:** workspace-scoped daemons + `mcp register`/`unregister` for mcp-language-server
- **Phase 4+:** sequential-thinking, wolfram, paper-search-mcp as additional manifests
- **Linux/macOS scheduler:** real implementations replacing the current stubs
- **`mcp secrets rotate --restart-dependents`:** exists in spec §3.8 but deferred (not required for Phase 0-1)
- **`mcp secrets rename`, `verify`, `where`, `export/import`:** deferred

## Self-review notes

- Every `spec §` referenced in task descriptions maps to a task above.
- Task 25's Serena manifest matches spec §3.7 example exactly.
- Client adapter field names (`url` for Claude/Codex, `httpUrl` for Gemini/Antigravity) match spec §3.6 table.
- Port assignments (9121/9122/9123) match spec §3.5.
- `supergateway` named in Task 15 matches spec §3.4.2 chosen bridge.
- Secrets vault uses age per spec §3.8.
- Phase 1 "Gemini CLI shares Antigravity daemon" per user's choice during brainstorming.

---

**Plan complete and saved to `d:\dev\mcp-local-hub\docs\superpowers\plans\2026-04-16-mcp-local-hub-phase-0-1.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

**Which approach?**
