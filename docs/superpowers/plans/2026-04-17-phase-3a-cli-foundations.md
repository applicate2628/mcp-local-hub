# Phase 3A.1 — API Foundations (subsystem, scan, migrate, install refactor)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Scope note:** Phase 3 as a whole is too large for a single plan (subsystem change + full CLI parity + GUI + release hardening). This plan is split as follows:

- **Phase 3A.1 — API Foundations (THIS PLAN, 8 tasks)** — Windows subsystem switch + AttachConsole, `internal/api/` scaffolding, `api.Scan`/`api.ExtractManifestFromClient`/`api.Migrate`, install/uninstall refactor, `install --all` flag. Delivers the source-of-truth package and the migration-scanning capability at the Go API level; `mcp scan` / `mcp migrate` CLI wrappers come in 3A.2.
- **Phase 3A.2 — Remaining CLI parity (next plan, ~12 tasks)** — `api.Status` enrichment, `api.Logs*`, backup sentinel + `api.BackupsList/Clean/Show/Rollback/RollbackOriginal`, all new CLI commands (`scan`, `migrate`, `logs`, `backups *`, `manifest *`, `scheduler upgrade`, `scheduler weekly-refresh`, `stop`, `settings *`), `mcp status` column enrichment, `mcp restart --all`. Written after 3A.1 commits so line numbers and signatures are concrete.
- **Phase 3B — GUI layer (future plan)** — `internal/gui/` HTTP + SSE + embedded UI + tray + single-instance lock + `mcp gui` command.
- **Phase 3C — Release hardening (future plan)** — Phase 3 verification doc, README/INSTALL updates, smoke-test matrix, tag.

Splitting this way guarantees every task in every plan has a concrete test + implementation with exact code rather than bullet placeholders.

**Goal of this plan (3A.1):** Deliver (a) a single-binary `mcp.exe` built with `-H windowsgui` + `AttachConsole` that runs CLI output cleanly in real terminals while staying silent under Scheduler, (b) a new `internal/api/` package that owns the Scan/Migrate/Install/Uninstall/InstallAll/ExtractManifestFromClient operations, and (c) a refactored `internal/cli/install.go` that is a thin wrapper over `api.Install`. After 3A.1, existing CLI commands keep working, but all their logic lives in the new api package so 3A.2 and 3B can build on it.

**Architecture:** One Go binary compiled with `-H windowsgui` using `AttachConsole(ATTACH_PARENT_PROCESS)` at init to preserve CLI output in real terminals while staying silent under Scheduler/Explorer. A new `internal/api/` package owns every operation; `internal/cli/install.go` is refactored to be a thin wrapper over it. All existing tests keep passing.

**Tech Stack:** Go 1.22+ (stdlib `net/http`, `encoding/json`, `os/exec`, `syscall`), existing deps only: Cobra, go-toml/v2, filippo.io/age, yaml.v3. No new external dependencies in Phase 3A.

**Reference implementations:**

- `internal/daemon/relay.go` — lifecycle patterns and JSON parsing style
- `internal/daemon/host.go` — subprocess management patterns
- `internal/cli/install.go` — existing install-flow to refactor into `internal/api/`

**Spec reference:** `docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md` (commit `d20483b`)

**Prerequisites:**

- Phase 2 complete (`docs/phase-2-verification.md`): all 9 daemons running, `internal/daemon/host.go` stable, native Go stdio-host replacing supergateway
- User on Windows 11 (Linux/macOS stubs remain untouched in Phase 3A; GUI HTTP server is Phase 3B)
- `claude`, `codex`, `gemini`, and Antigravity configs present for E2E smoke tests

---

## Naming, scope, and phases

**Phase 3A · CLI parity + foundations (Tasks 1-20)** — after completion, `mcp.exe` is a single binary with complete CLI for scan/migrate/manifest/backup/scheduler operations. Everything the GUI will later expose is reachable from the terminal.

**Phase 3B · GUI layer (Tasks 21-34)** — adds HTTP server, embedded web UI, tray icon, single-instance lock. Uses only `internal/api/` primitives that already exist after 3A.

**Phase 3C · Release hardening (Task 35)** — verification doc, README + INSTALL updates, smoke-test pass.

Stopping after 3A delivers a substantially more useful CLI on its own. The Implementation Plan document sequences tasks 1→35 but execution can pause anywhere a checkpoint review makes sense.

---

## File Structure

Files that will be **created** (new):

- `cmd/mcp/console_windows.go` — AttachConsole logic, `//go:build windows`
- `cmd/mcp/console_other.go` — noop stub, `//go:build !windows`
- `internal/api/api.go` — package doc, shared types (`ScanEntry`, `DaemonStatus`, `BackupInfo`, `MigrationPlan`)
- `internal/api/scan.go` — `Scan()` + `ExtractManifestFromClient()`
- `internal/api/scan_test.go`
- `internal/api/migrate.go` — `Migrate()` including atomic apply
- `internal/api/migrate_test.go`
- `internal/api/manifest.go` — `ManifestList/Get/Create/Edit/Validate/Delete`
- `internal/api/manifest_test.go`
- `internal/api/status.go` — `Status()` with PID/RAM/Uptime + `Logs*`
- `internal/api/status_test.go`
- `internal/api/backups.go` — sentinel-aware `Backup/List/Clean/Show/Rollback` + `RollbackOriginal`
- `internal/api/backups_test.go`
- `internal/api/scheduler.go` — `SchedulerUpgrade`, `WeeklyRefreshSet`
- `internal/api/events.go` — SSE event bus, `Publish(Event)`, `Subscribe() <-chan Event`
- `internal/api/events_test.go`
- `internal/api/settings.go` — GUI preferences read/write
- `internal/cli/scan.go` — `mcp scan`
- `internal/cli/migrate.go` — `mcp migrate`
- `internal/cli/logs.go` — `mcp logs`
- `internal/cli/backups.go` — `mcp backups *`
- `internal/cli/manifest.go` — `mcp manifest *`
- `internal/cli/scheduler_cmd.go` — `mcp scheduler *`
- `internal/cli/settings.go` — `mcp settings *`
- `internal/cli/gui.go` — `mcp gui`
- `internal/cli/stop.go` — `mcp stop`
- `internal/gui/server.go` — HTTP server bootstrap
- `internal/gui/handlers.go` — all endpoints
- `internal/gui/sse.go` — `/api/events` SSE handler
- `internal/gui/lock.go` — named-mutex + pidport-file single-instance lock
- `internal/gui/lock_test.go`
- `internal/gui/browser.go` — Chrome/Edge app-mode launcher with fallback chain
- `internal/gui/watcher.go` — fsnotify on 4 client configs
- `internal/gui/assets.go` — `//go:embed` for HTML/CSS/JS/SVG-sprite/icon PNGs
- `internal/gui/assets/index.html` — SPA shell, sidebar + main area
- `internal/gui/assets/app.css` — theme variables, shell layout, all screen styles
- `internal/gui/assets/app.js` — router + SSE subscriber + API client + screen controllers
- `internal/gui/assets/icons.svg` — SVG-sprite with 18 symbols
- `internal/gui/assets/tray-healthy.png`, `tray-partial.png`, `tray-down.png`, `tray-error.png` — 16×16 tray icons
- `internal/tray/tray.go` — systray integration, menu construction, icon-state mapping
- `internal/tray/tray_test.go`

Files that will be **modified**:

- `build.sh` / `build.ps1` — add `-H windowsgui` ldflag, verify single binary output
- `cmd/mcp/main.go` — call console-init, detect "no args from Explorer" for auto-gui
- `internal/clients/clients.go` — `Backup()` now takes `(keepN int)`, adds sentinel path, refactored to call `internal/api/backups`
- `internal/cli/root.go` — wire the new commands
- `internal/cli/install.go` — move logic to `internal/api/install.go`, add `--all` flag
- `internal/cli/restart.go` — add `--all` flag
- `internal/cli/status.go` — add Port/PID/RAM/Uptime columns + `--json` flag
- `internal/cli/rollback.go` — add `--original` flag
- `internal/scheduler/scheduler_windows.go` — no behavior change, just audit (tasks use `os.Executable()` already so subsystem swap is automatic)
- `.gitignore` — add `secrets.age`, `gui-preferences.yaml` per your policy decision (decide in Task 12)
- `README.md`, `INSTALL.md` — Phase 3 sections
- `docs/phase-3-verification.md` — new verification doc (Task 35)

Files that will be **removed** or **deprecated**:

- `internal/daemon/bridge.go` — already deprecated in Phase 2; can remove in Phase 3 final cleanup (Task 35)

---

# Phase 3A · CLI parity + foundations

## Task 1: Windows subsystem switch + AttachConsole init

**Files:**

- Create: `cmd/mcp/console_windows.go`
- Create: `cmd/mcp/console_other.go`
- Modify: `cmd/mcp/main.go`
- Modify: `build.sh`
- Modify: `build.ps1`

- [ ] **Step 1: Write `cmd/mcp/console_windows.go`**

```go
//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// ATTACH_PARENT_PROCESS is the sentinel understood by AttachConsole.
const attachParentProcess = ^uint32(0) // (DWORD)-1

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole       = kernel32.NewProc("AttachConsole")
	procGetStdHandle        = kernel32.NewProc("GetStdHandle")
	procSetStdHandle        = kernel32.NewProc("SetStdHandle")
)

// attachParentConsoleIfAvailable tries to attach this Windows-subsystem
// process to its parent's console (cmd.exe, PowerShell, etc.). When the
// parent has a console, stdin/stdout/stderr are rewired so plain fmt.Print
// calls work. When there is no parent console (Scheduler, Explorer
// double-click, detached spawn), this returns quietly.
func attachParentConsoleIfAvailable() {
	ret, _, _ := procAttachConsole.Call(uintptr(attachParentProcess))
	if ret == 0 {
		return
	}
	// Re-open the standard file descriptors against the console handles.
	reopen(syscall.Stdin, "CONIN$", os.O_RDONLY, &os.Stdin)
	reopen(syscall.Stdout, "CONOUT$", os.O_WRONLY, &os.Stdout)
	reopen(syscall.Stderr, "CONOUT$", os.O_WRONLY, &os.Stderr)
}

func reopen(stdHandle syscall.Handle, name string, mode int, target **os.File) {
	f, err := os.OpenFile("\\\\.\\"+name, mode, 0)
	if err != nil {
		return
	}
	*target = f
	_ = stdHandle
	_ = unsafe.Pointer(nil)
}
```

- [ ] **Step 2: Write `cmd/mcp/console_other.go`**

```go
//go:build !windows

package main

// attachParentConsoleIfAvailable is a no-op on non-Windows platforms;
// the OS already hands us stdin/stdout/stderr correctly.
func attachParentConsoleIfAvailable() {}
```

- [ ] **Step 3: Update `cmd/mcp/main.go` to call the attach function first**

Current file body after the `//go:generate` comment currently starts with `package main` then imports then `var (...)` then `func main()`. Change `main()` so the FIRST thing it does is attach to parent console. Replace:

```go
func main() {
	cli.SetBuildInfo(version, commit, buildDate)
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

With:

```go
func main() {
	attachParentConsoleIfAvailable()
	cli.SetBuildInfo(version, commit, buildDate)

	// Explorer double-click: no args and no parent console ⇒ auto-launch GUI.
	// (Detect by checking whether os.Args has any command and stdout is a
	// pipe/console. If neither, route to `gui`.)
	if shouldAutoLaunchGUI() {
		os.Args = append(os.Args, "gui")
	}

	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// shouldAutoLaunchGUI returns true when we were started with no command-line
// arguments AND we have no console attached — the hallmark of an Explorer
// double-click on a Windows-subsystem binary.
func shouldAutoLaunchGUI() bool {
	if len(os.Args) > 1 {
		return false
	}
	// If AttachConsole succeeded, os.Stdout is now attached; file.Stat will
	// report a character device for a console.
	fi, err := os.Stdout.Stat()
	if err != nil {
		return true
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}
```

- [ ] **Step 4: Update `build.sh` to use Windows subsystem**

Change the `go build` line from:

```bash
go build -trimpath -ldflags "${LDFLAGS}" -o mcp.exe ./cmd/mcp
```

To:

```bash
go build -trimpath -ldflags "${LDFLAGS} -H windowsgui" -o mcp.exe ./cmd/mcp
```

- [ ] **Step 5: Update `build.ps1` identically**

Change the build line to append ` -H windowsgui` to `$ldflags` before the `go build` invocation.

- [ ] **Step 6: Rebuild and smoke-test**

```bash
cd <repo> && ./build.sh && ./mcp.exe version
```

Expected: version prints to terminal (AttachConsole worked against Git Bash). File size ~12 MB unchanged materially.

- [ ] **Step 7: Verify Scheduler-invoked daemon no longer flashes console**

```bash
./mcp.exe restart --server memory
sleep 3
# No console window should have flashed on screen
./mcp.exe status
```

Expected: memory daemon still Running, no visual flash during restart.

- [ ] **Step 8: Commit**

```bash
git add cmd/mcp/console_windows.go cmd/mcp/console_other.go cmd/mcp/main.go build.sh build.ps1
git commit -m "feat(build): switch mcp.exe to Windows subsystem + AttachConsole"
```

---

## Task 2: Pre-Task-3 scaffold — `internal/api/` package

**Files:**

- Create: `internal/api/api.go`
- Create: `internal/api/types.go`
- Create: `internal/api/api_test.go`

- [ ] **Step 1: Create `internal/api/api.go` package doc**

```go
// Package api is the single source of truth for operations exposed through
// the mcp-local-hub CLI and GUI frontends. Every command the user runs (via
// cobra) or every HTTP endpoint the GUI calls dispatches into one function
// here; they never reach directly into internal/clients, internal/scheduler,
// internal/config, or internal/secrets.
//
// This enforces CLI ≡ UI parity structurally: if a capability is in api, it
// is reachable from both frontends by construction; if it is not, neither
// frontend can expose it.
package api

// API is the orchestration handle held by cli and gui. A single instance is
// created per process via NewAPI. Methods are safe for concurrent use unless
// noted otherwise.
type API struct {
	stateMu sync.RWMutex
	state   *State

	bus *EventBus
}

// NewAPI constructs a fresh API with an initialized state and event bus.
func NewAPI() *API {
	return &API{
		state: &State{Daemons: make(map[string]DaemonStatus)},
		bus:   newEventBus(),
	}
}
```

- [ ] **Step 2: Create `internal/api/types.go` with shared structs**

```go
package api

import (
	"sync"
	"time"
)

// State is the snapshot of what the API knows about the running system.
// Accessed only under API.stateMu.
type State struct {
	Daemons map[string]DaemonStatus // key: "<server>.<daemon>"
	LastScan *ScanResult
	LastSync time.Time
}

// DaemonStatus enriches the scheduler-task view with process stats.
type DaemonStatus struct {
	Server     string    `json:"server"`
	Daemon     string    `json:"daemon"`
	TaskName   string    `json:"task_name"`
	State      string    `json:"state"`      // "Running" | "Ready" | "Failed" | "Stopped"
	Port       int       `json:"port"`
	LastResult int32     `json:"last_result"`
	NextRun    time.Time `json:"next_run"`
	PID        int       `json:"pid,omitempty"`
	RAMBytes   uint64    `json:"ram_bytes,omitempty"`
	UptimeSec  int64     `json:"uptime_sec,omitempty"`
}

// ScanEntry is one row in the unified "across all clients" view.
type ScanEntry struct {
	Name           string                 `json:"name"`
	Status         string                 `json:"status"` // "via-hub" | "can-migrate" | "unknown" | "per-session" | "not-installed"
	ClientPresence map[string]ClientEntry `json:"client_presence"`
	ManifestExists bool                   `json:"manifest_exists"`
	CanMigrate     bool                   `json:"can_migrate"`
}

// ClientEntry captures the shape of how one MCP server is configured inside
// one client config.
type ClientEntry struct {
	Transport string            `json:"transport"` // "http" | "stdio" | "relay" | "absent"
	Endpoint  string            `json:"endpoint"`  // URL for http, command for stdio, etc.
	Raw       map[string]any    `json:"raw"`       // the original JSON/TOML fragment
}

// ScanResult bundles a full scan with timestamp for caching / SSE.
type ScanResult struct {
	At      time.Time   `json:"at"`
	Entries []ScanEntry `json:"entries"`
}

// BackupInfo describes one file in the backup area.
type BackupInfo struct {
	Client   string    `json:"client"`
	Path     string    `json:"path"`
	Kind     string    `json:"kind"` // "original" | "timestamped"
	ModTime  time.Time `json:"mod_time"`
	SizeByte int64     `json:"size_byte"`
}
```

Additionally add the `sync` import atop `api.go` since `State` lives in `types.go` but the `stateMu` field is declared in `api.go`. Move the `import "sync"` above `type API`.

- [ ] **Step 3: Write minimal test that exercises `NewAPI`**

Create `internal/api/api_test.go`:

```go
package api

import "testing"

func TestNewAPIReturnsWorkingInstance(t *testing.T) {
	a := NewAPI()
	if a == nil {
		t.Fatal("NewAPI returned nil")
	}
	if a.state == nil {
		t.Error("state is nil")
	}
	if a.state.Daemons == nil {
		t.Error("Daemons map is nil")
	}
	if a.bus == nil {
		t.Error("bus is nil")
	}
}
```

- [ ] **Step 4: Create a stub for `internal/api/events.go` so the test compiles**

```go
package api

import "sync"

// EventBus is a simple in-process SSE broker. Populated in Task 22; this
// stub exists so the api package compiles before the bus is implemented.
type EventBus struct {
	mu sync.Mutex
}

func newEventBus() *EventBus { return &EventBus{} }
```

- [ ] **Step 5: Run test**

```bash
cd <repo> && go test ./internal/api/ -v
```

Expected: `TestNewAPIReturnsWorkingInstance` PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/
git commit -m "feat(api): package scaffold — API handle, State, shared types, event-bus stub"
```

---

## Task 3: `api.Scan` — unified view of all MCP servers across clients

**Files:**

- Create: `internal/api/scan.go`
- Create: `internal/api/scan_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/api/scan_test.go`:

```go
package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestScanClassifiesEntries verifies the three key classifications:
// via-hub (HTTP entry pointing at our daemon), can-migrate (stdio entry
// matching one of our manifest names), unknown (stdio entry with no
// manifest), per-session (well-known per-session server like gdb).
func TestScanClassifiesEntries(t *testing.T) {
	tmp := t.TempDir()

	// Fake Claude Code config with 3 entries.
	claudeCfg := map[string]any{
		"mcpServers": map[string]any{
			"memory":   map[string]any{"type": "http", "url": "http://localhost:9123/mcp"},
			"filesystem": map[string]any{"command": "npx", "args": []string{"-y", "@x/filesystem"}},
			"my-thing": map[string]any{"command": "python", "args": []string{"my.py"}},
			"gdb":      map[string]any{"command": "uv", "args": []string{"run", "server.py"}},
		},
	}
	claudePath := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(claudeCfg)
	_ = os.WriteFile(claudePath, b, 0600)

	// Manifest dir with memory + filesystem.
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)
	_ = os.MkdirAll(filepath.Join(manifestDir, "filesystem"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "filesystem", "manifest.yaml"),
		[]byte("name: filesystem\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9130\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ClaudeConfigPath: claudePath,
		CodexConfigPath:  "", GeminiConfigPath: "", AntigravityConfigPath: "",
		ManifestDir:      manifestDir,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	byName := map[string]ScanEntry{}
	for _, e := range result.Entries {
		byName[e.Name] = e
	}

	if got := byName["memory"].Status; got != "via-hub" {
		t.Errorf("memory.Status: got %q, want via-hub", got)
	}
	if got := byName["filesystem"].Status; got != "can-migrate" {
		t.Errorf("filesystem.Status: got %q, want can-migrate", got)
	}
	if got := byName["my-thing"].Status; got != "unknown" {
		t.Errorf("my-thing.Status: got %q, want unknown", got)
	}
	if got := byName["gdb"].Status; got != "per-session" {
		t.Errorf("gdb.Status: got %q, want per-session", got)
	}
}
```

- [ ] **Step 2: Verify the test fails**

```bash
cd <repo> && go test ./internal/api/ -run TestScanClassifiesEntries -v
```

Expected: FAIL with `a.ScanFrom undefined` / `ScanOpts undefined`.

- [ ] **Step 3: Implement `internal/api/scan.go`**

```go
package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScanOpts provides per-client config paths so tests can point at temp dirs.
// Production callers pass "" for each to use the OS default discovery.
type ScanOpts struct {
	ClaudeConfigPath      string
	CodexConfigPath       string
	GeminiConfigPath      string
	AntigravityConfigPath string
	ManifestDir           string
}

// perSessionServers are MCP servers whose state is session-bound and cannot
// be meaningfully shared across clients. Hardcoded because the list is small,
// well-known, and rarely changes; if it grows, move to a config file.
var perSessionServers = map[string]bool{
	"gdb":        true,
	"lldb":       true,
	"playwright": true,
}

// ScanFrom builds a unified cross-client view. Exposed (rather than Scan) so
// tests can pass arbitrary paths.
func (a *API) ScanFrom(opts ScanOpts) (*ScanResult, error) {
	entries := map[string]*ScanEntry{}

	// Read each client config; missing files are skipped (client not installed).
	if opts.ClaudeConfigPath != "" {
		if err := scanClaude(entries, opts.ClaudeConfigPath); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
	}
	// Codex, Gemini, Antigravity follow in Task 4 — kept minimal here.

	// Build a set of manifest names so we can mark can-migrate.
	manifestNames, err := readManifestNames(opts.ManifestDir)
	if err != nil {
		return nil, fmt.Errorf("manifests: %w", err)
	}

	// Classify.
	for name, e := range entries {
		e.Name = name
		e.ManifestExists = manifestNames[name]
		e.CanMigrate = e.ManifestExists && !perSessionServers[name]
		e.Status = classify(e, name, manifestNames)
	}

	out := &ScanResult{At: time.Now()}
	for _, e := range entries {
		out.Entries = append(out.Entries, *e)
	}
	return out, nil
}

func scanClaude(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["claude-code"] = shapeClaudeEntry(raw)
	}
	return nil
}

func shapeClaudeEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func readManifestNames(dir string) (map[string]bool, error) {
	names := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return names, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
			names[e.Name()] = true
		}
	}
	return names, nil
}

func classify(e *ScanEntry, name string, manifestNames map[string]bool) string {
	if perSessionServers[name] {
		return "per-session"
	}
	hasHub := false
	hasStdio := false
	for _, c := range e.ClientPresence {
		if c.Transport == "http" && strings.Contains(c.Endpoint, "localhost") {
			hasHub = true
		}
		if c.Transport == "stdio" {
			hasStdio = true
		}
	}
	if hasHub && !hasStdio {
		return "via-hub"
	}
	if hasStdio && manifestNames[name] {
		return "can-migrate"
	}
	if hasStdio {
		return "unknown"
	}
	return "not-installed"
}
```

- [ ] **Step 4: Run the test**

```bash
cd <repo> && go test ./internal/api/ -run TestScanClassifiesEntries -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/scan.go internal/api/scan_test.go
git commit -m "feat(api): Scan() — classify cross-client MCP entries (claude adapter first)"
```

---

## Task 4: Extend `Scan` to Codex, Gemini, and Antigravity configs

**Files:**

- Modify: `internal/api/scan.go`
- Modify: `internal/api/scan_test.go`

- [ ] **Step 1: Write the test for multi-client classification**

Add to `internal/api/scan_test.go`:

```go
// TestScanCoversAllFourClients seeds a Codex TOML, Gemini JSON, and
// Antigravity JSON with "memory" entries of different transports and checks
// each is represented in the ClientPresence map with the correct transport tag.
func TestScanCoversAllFourClients(t *testing.T) {
	tmp := t.TempDir()

	// Codex (TOML)
	codexPath := filepath.Join(tmp, "config.toml")
	_ = os.WriteFile(codexPath, []byte(`[mcp_servers.memory]
url = "http://localhost:9123/mcp"
`), 0600)

	// Gemini (JSON w/ mcpServers { url + type: http })
	geminiPath := filepath.Join(tmp, "settings.json")
	_ = os.WriteFile(geminiPath, []byte(`{"mcpServers":{"memory":{"url":"http://localhost:9123/mcp","type":"http"}}}`), 0600)

	// Antigravity — relay (stdio with command=mcp.exe args=[relay, --server, memory])
	agPath := filepath.Join(tmp, "mcp_config.json")
	_ = os.WriteFile(agPath, []byte(`{"mcpServers":{"memory":{"command":"D:/dev/mcp.exe","args":["relay","--server","memory","--daemon","default"],"disabled":false}}}`), 0600)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		CodexConfigPath:       codexPath,
		GeminiConfigPath:      geminiPath,
		AntigravityConfigPath: agPath,
		ManifestDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var memEntry *ScanEntry
	for i := range result.Entries {
		if result.Entries[i].Name == "memory" {
			memEntry = &result.Entries[i]
		}
	}
	if memEntry == nil {
		t.Fatal("no memory entry found")
	}
	if got := memEntry.ClientPresence["codex-cli"].Transport; got != "http" {
		t.Errorf("codex-cli.Transport: got %q, want http", got)
	}
	if got := memEntry.ClientPresence["gemini-cli"].Transport; got != "http" {
		t.Errorf("gemini-cli.Transport: got %q, want http", got)
	}
	if got := memEntry.ClientPresence["antigravity"].Transport; got != "relay" {
		t.Errorf("antigravity.Transport: got %q, want relay", got)
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd <repo> && go test ./internal/api/ -run TestScanCoversAllFourClients -v
```

Expected: FAIL — `codex-cli` key never populated.

- [ ] **Step 3: Add Codex, Gemini, Antigravity scanners**

Append to `scan.go`:

```go
// Extend ScanFrom to call these (insert the three new calls right after scanClaude):

// In ScanFrom, after `scanClaude(...)`:
//
//   if opts.CodexConfigPath != "" {
//       if err := scanCodex(entries, opts.CodexConfigPath); err != nil { ... }
//   }
//   if opts.GeminiConfigPath != "" {
//       if err := scanGemini(entries, opts.GeminiConfigPath); err != nil { ... }
//   }
//   if opts.AntigravityConfigPath != "" {
//       if err := scanAntigravity(entries, opts.AntigravityConfigPath); err != nil { ... }
//   }

func scanCodex(entries map[string]*ScanEntry, path string) error {
	// Codex config is TOML with nested [mcp_servers.<name>] tables.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Use existing internal/config or parse via go-toml/v2 — here minimal
	// line-scan parser to avoid cyclic dep. In practice reuse config.ParseCodex.
	// For plan brevity, call into go-toml directly:
	var root map[string]any
	if err := tomlUnmarshal(data, &root); err != nil {
		return err
	}
	srv, _ := root["mcp_servers"].(map[string]any)
	for name, raw := range srv {
		m, _ := raw.(map[string]any)
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["codex-cli"] = shapeCodexEntry(m)
	}
	return nil
}

func shapeCodexEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanGemini(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["gemini-cli"] = shapeGeminiEntry(raw)
	}
	return nil
}

func shapeGeminiEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	if url, ok := raw["httpUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanAntigravity(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["antigravity"] = shapeAntigravityEntry(raw)
	}
	return nil
}

func shapeAntigravityEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["serverUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	// Detect our own relay shape: command=*mcp.exe args[0]=="relay".
	if cmd, ok := raw["command"].(string); ok {
		if args, ok := raw["args"].([]any); ok && len(args) > 0 {
			if first, _ := args[0].(string); first == "relay" && strings.HasSuffix(strings.ToLower(cmd), "mcp.exe") {
				return ClientEntry{Transport: "relay", Endpoint: cmd, Raw: raw}
			}
		}
		return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
	}
	return ClientEntry{Transport: "absent", Raw: raw}
}

// tomlUnmarshal wraps the TOML library used elsewhere in the repo.
func tomlUnmarshal(data []byte, v any) error {
	// The repo already imports github.com/pelletier/go-toml/v2 via the
	// clients package. Reuse it here.
	return toml.Unmarshal(data, v)
}
```

Add the import for `github.com/pelletier/go-toml/v2` to `scan.go` (aliased `toml`).

- [ ] **Step 4: Wire the three new calls in ScanFrom**

Replace the body of `ScanFrom` with:

```go
func (a *API) ScanFrom(opts ScanOpts) (*ScanResult, error) {
	entries := map[string]*ScanEntry{}

	if opts.ClaudeConfigPath != "" {
		if err := scanClaude(entries, opts.ClaudeConfigPath); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
	}
	if opts.CodexConfigPath != "" {
		if err := scanCodex(entries, opts.CodexConfigPath); err != nil {
			return nil, fmt.Errorf("codex: %w", err)
		}
	}
	if opts.GeminiConfigPath != "" {
		if err := scanGemini(entries, opts.GeminiConfigPath); err != nil {
			return nil, fmt.Errorf("gemini: %w", err)
		}
	}
	if opts.AntigravityConfigPath != "" {
		if err := scanAntigravity(entries, opts.AntigravityConfigPath); err != nil {
			return nil, fmt.Errorf("antigravity: %w", err)
		}
	}

	manifestNames, err := readManifestNames(opts.ManifestDir)
	if err != nil {
		return nil, fmt.Errorf("manifests: %w", err)
	}
	for name, e := range entries {
		e.Name = name
		e.ManifestExists = manifestNames[name]
		e.CanMigrate = e.ManifestExists && !perSessionServers[name]
		e.Status = classify(e, name, manifestNames)
	}

	out := &ScanResult{At: time.Now()}
	for _, e := range entries {
		out.Entries = append(out.Entries, *e)
	}
	return out, nil
}
```

- [ ] **Step 5: Run the tests**

```bash
cd <repo> && go test ./internal/api/ -v
```

Expected: both TestScanClassifiesEntries and TestScanCoversAllFourClients PASS.

- [ ] **Step 6: Add `Scan()` wrapper with OS-default paths**

Append to `scan.go`:

```go
// Scan is the production entry point: it resolves client config paths from
// OS defaults and calls ScanFrom.
func (a *API) Scan() (*ScanResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return a.ScanFrom(ScanOpts{
		ClaudeConfigPath:      filepath.Join(home, ".claude.json"),
		CodexConfigPath:       filepath.Join(home, ".codex", "config.toml"),
		GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
		AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		ManifestDir:           defaultManifestDir(),
	})
}

// defaultManifestDir returns the path to `servers/` next to the running binary.
func defaultManifestDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "servers"
	}
	return filepath.Join(filepath.Dir(exe), "servers")
}
```

- [ ] **Step 7: Commit**

```bash
git add internal/api/scan.go internal/api/scan_test.go
git commit -m "feat(api): Scan() covers Codex TOML, Gemini JSON, Antigravity relay-entry detection"
```

---

## Task 5: `api.ExtractManifestFromClient` — draft a manifest from an existing stdio entry

**Files:**

- Modify: `internal/api/scan.go`
- Create: `internal/api/scan_extract_test.go`

- [ ] **Step 1: Write the failing test**

```go
package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractManifestFromClientPreservesCommandAndEnv asserts that the
// extracted manifest has command/base_args/env matching the original stdio
// entry, plus a port auto-picked and all-four client bindings as defaults.
func TestExtractManifestFromClientPreservesCommandAndEnv(t *testing.T) {
	tmp := t.TempDir()
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"fetch": map[string]any{
				"command": "uvx",
				"args":    []string{"--from", "mcp-server-fetch", "mcp-server-fetch"},
				"env":     map[string]any{"CACHE_DIR": "/tmp/fetch"},
			},
		},
	}
	path := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(cfg)
	_ = os.WriteFile(path, b, 0600)

	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("claude-code", "fetch", ScanOpts{
		ClaudeConfigPath: path,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yaml, "name: fetch") {
		t.Error("missing name: fetch")
	}
	if !strings.Contains(yaml, "command: uvx") {
		t.Error("missing command: uvx")
	}
	if !strings.Contains(yaml, "CACHE_DIR") {
		t.Error("env CACHE_DIR lost")
	}
	if !strings.Contains(yaml, "client_bindings:") {
		t.Error("missing client_bindings")
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd <repo> && go test ./internal/api/ -run TestExtractManifestFromClient -v
```

Expected: FAIL.

- [ ] **Step 3: Implement Extract**

Append to `scan.go`:

```go
// ExtractManifestFromClient reads a stdio entry from the specified client
// config and renders a draft manifest.yaml suitable for the GUI "Create
// manifest" flow. The draft always includes bindings for all four clients;
// users edit as desired before saving.
func (a *API) ExtractManifestFromClient(client, serverName string, opts ScanOpts) (string, error) {
	var raw map[string]any

	switch client {
	case "claude-code":
		if opts.ClaudeConfigPath == "" {
			return "", fmt.Errorf("ClaudeConfigPath empty")
		}
		data, err := os.ReadFile(opts.ClaudeConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]
	default:
		return "", fmt.Errorf("extract not yet supported for client %q (extend here when needed)", client)
	}
	if raw == nil {
		return "", fmt.Errorf("server %q not found in client %q config", serverName, client)
	}

	cmd, _ := raw["command"].(string)
	var args []string
	if arr, ok := raw["args"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				args = append(args, s)
			}
		}
	}
	envMap := map[string]string{}
	if envAny, ok := raw["env"].(map[string]any); ok {
		for k, v := range envAny {
			if s, ok := v.(string); ok {
				envMap[k] = s
			}
		}
	}

	// Pick next free port in 9121-9139 range not already used by other manifests.
	port, err := pickNextFreePort(opts.ManifestDir)
	if err != nil {
		return "", err
	}

	return renderDraftManifestYAML(serverName, cmd, args, envMap, port), nil
}

func pickNextFreePort(manifestDir string) (int, error) {
	used := map[int]bool{}
	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(manifestDir, e.Name(), "manifest.yaml"))
		if err != nil {
			continue
		}
		// Minimal YAML scrape — we do not want to pull go-yaml just for this.
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			const p = "port:"
			if strings.HasPrefix(line, p) {
				var n int
				fmt.Sscanf(line, "port: %d", &n)
				if n > 0 {
					used[n] = true
				}
			}
		}
	}
	for p := 9121; p <= 9139; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in 9121-9139 range")
}

func renderDraftManifestYAML(name, cmd string, args []string, env map[string]string, port int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintln(&b, "kind: global")
	fmt.Fprintln(&b, "transport: stdio-bridge")
	fmt.Fprintf(&b, "command: %s\n", cmd)
	if len(args) > 0 {
		fmt.Fprintln(&b, "base_args:")
		for _, a := range args {
			fmt.Fprintf(&b, "  - %q\n", a)
		}
	}
	if len(env) > 0 {
		fmt.Fprintln(&b, "env:")
		for k, v := range env {
			fmt.Fprintf(&b, "  %s: %q\n", k, v)
		}
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "daemons:")
	fmt.Fprintln(&b, "  - name: default")
	fmt.Fprintf(&b, "    port: %d\n", port)
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "client_bindings:")
	for _, c := range []string{"claude-code", "codex-cli", "gemini-cli", "antigravity"} {
		fmt.Fprintf(&b, "  - client: %s\n", c)
		fmt.Fprintln(&b, "    daemon: default")
		fmt.Fprintln(&b, "    url_path: /mcp")
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "weekly_refresh: false")
	return b.String()
}
```

- [ ] **Step 4: Run the test**

```bash
cd <repo> && go test ./internal/api/ -v
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/scan.go internal/api/scan_extract_test.go
git commit -m "feat(api): ExtractManifestFromClient — render draft manifest from existing stdio entry"
```

---

## Task 6: Move existing install/uninstall logic into `internal/api/install.go`

**Files:**

- Create: `internal/api/install.go`
- Modify: `internal/cli/install.go`
- Modify: `internal/cli/uninstall.go`

- [ ] **Step 1: Create `internal/api/install.go` by moving the body of `executeInstall`**

Copy the whole `executeInstall` function and `BuildPlan`/`Plan`/`ClientUpdatePlan`/`ScheduledTaskPlan` types from `internal/cli/install.go` into a new file `internal/api/install.go`. Rename `executeInstall` to `Install`, make it a method on `*API`. Keep the signatures but change `*cobra.Command` printf-style output to `io.Writer` parameter so CLI and GUI can both pipe results.

```go
package api

import (
	// existing imports: os, fmt, io, mcp-local-hub/internal/config, ...
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// InstallOpts controls an install invocation.
type InstallOpts struct {
	Server       string
	DaemonFilter string // empty = all daemons in the manifest
	DryRun       bool
	Writer       io.Writer // progress output destination
}

// Install performs the full install flow for one server.
func (a *API) Install(opts InstallOpts) error {
	// ... copied body of BuildPlan + executeInstall, adjusted to use opts.Writer
}

// (BuildPlan etc. unchanged, just moved.)
```

Full body is too long to inline; the engineer should copy from `internal/cli/install.go` verbatim and adjust only the output sink and the receiver.

- [ ] **Step 2: Rewrite `internal/cli/install.go` to thin wrapper**

```go
package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	var server, daemonFilter string
	var dryRun bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install a server: create scheduler tasks + client configs, start daemons",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			a := api.NewAPI()
			return a.Install(api.InstallOpts{
				Server:       server,
				DaemonFilter: daemonFilter,
				DryRun:       dryRun,
				Writer:       cmd.OutOrStdout(),
			})
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "install only this daemon")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print plan without applying")
	return c
}
```

- [ ] **Step 3: Similarly thin-wrap uninstall**

`internal/cli/uninstall.go` becomes a thin wrapper calling `a.Uninstall(server)`. Move its existing logic into `internal/api/install.go` as `func (a *API) Uninstall(server string) error`.

- [ ] **Step 4: Run full test suite**

```bash
cd <repo> && go test ./... -v
```

Expected: all green. Existing install_test.go may need tweaks to point at `api.Install()` instead of the CLI helper — apply minimal adjustments until the test suite is clean.

- [ ] **Step 5: Commit**

```bash
git add internal/api/install.go internal/cli/install.go internal/cli/uninstall.go internal/cli/install_test.go
git commit -m "refactor(api): move Install/Uninstall logic from cli to api package"
```

---

## Task 7: `api.InstallAll` + `mcp install --all` flag

**Files:**

- Modify: `internal/api/install.go`
- Modify: `internal/cli/install.go`

- [ ] **Step 1: Add the failing test**

Create a section in `internal/api/install_test.go`:

```go
// TestInstallAllInstallsEverything spawns a tempdir with two fake manifests
// and asserts Install is invoked for each.
func TestInstallAllInstallsEverything(t *testing.T) {
	tmp := t.TempDir()
	makeFakeManifest(t, filepath.Join(tmp, "foo"), "foo", 9130)
	makeFakeManifest(t, filepath.Join(tmp, "bar"), "bar", 9131)

	a := &API{stateMu: sync.RWMutex{}, state: &State{Daemons: map[string]DaemonStatus{}}, bus: newEventBus()}
	var buf bytes.Buffer
	results := a.InstallAllFrom(InstallAllOpts{
		ManifestDir: tmp,
		DryRun:      true,
		Writer:      &buf,
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func makeFakeManifest(t *testing.T, dir, name string, port int) {
	_ = os.MkdirAll(dir, 0755)
	body := fmt.Sprintf(`name: %s
kind: global
transport: stdio-bridge
command: echo
daemons:
  - name: default
    port: %d
client_bindings: []
weekly_refresh: false
`, name, port)
	_ = os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0644)
}
```

- [ ] **Step 2: Verify failure**

Expected: FAIL — `InstallAllFrom` and `InstallAllOpts` undefined.

- [ ] **Step 3: Implement `InstallAll`**

Append to `internal/api/install.go`:

```go
// InstallAllOpts controls a bulk install.
type InstallAllOpts struct {
	ManifestDir string
	DryRun      bool
	Writer      io.Writer
}

// InstallResult is one row in an InstallAll report.
type InstallResult struct {
	Server string
	Err    error
}

// InstallAllFrom installs every manifest under ManifestDir. Continues past
// individual failures and returns the per-server result list so callers can
// display a summary.
func (a *API) InstallAllFrom(opts InstallAllOpts) []InstallResult {
	var results []InstallResult
	entries, err := os.ReadDir(opts.ManifestDir)
	if err != nil {
		return []InstallResult{{Err: err}}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(opts.ManifestDir, e.Name(), "manifest.yaml")); err != nil {
			continue
		}
		err := a.Install(InstallOpts{
			Server: e.Name(),
			DryRun: opts.DryRun,
			Writer: opts.Writer,
		})
		results = append(results, InstallResult{Server: e.Name(), Err: err})
	}
	return results
}

// InstallAll is the production entry point using defaultManifestDir().
func (a *API) InstallAll(dryRun bool, w io.Writer) []InstallResult {
	return a.InstallAllFrom(InstallAllOpts{
		ManifestDir: defaultManifestDir(),
		DryRun:      dryRun,
		Writer:      w,
	})
}
```

- [ ] **Step 4: Add `--all` flag to the CLI**

In `internal/cli/install.go`, add `var all bool`, a `c.Flags().BoolVar(&all, "all", false, "install every manifest under servers/")`, and branch in RunE:

```go
if all {
    if server != "" || daemonFilter != "" {
        return fmt.Errorf("--all is mutually exclusive with --server/--daemon")
    }
    a := api.NewAPI()
    results := a.InstallAll(dryRun, cmd.OutOrStdout())
    for _, r := range results {
        if r.Err != nil {
            fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %v\n", r.Server, r.Err)
        } else {
            fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", r.Server)
        }
    }
    return nil
}
// ...existing --server path unchanged
```

- [ ] **Step 5: Run tests**

```bash
cd <repo> && go test ./... -v
```

Expected: all PASS including new test.

- [ ] **Step 6: Smoke-test `mcp install --all --dry-run`**

```bash
./mcp.exe install --all --dry-run
```

Expected: prints plan summaries for all 7 installed manifests.

- [ ] **Step 7: Commit**

```bash
git add internal/api/install.go internal/api/install_test.go internal/cli/install.go
git commit -m "feat(install): add --all flag, InstallAll API"
```

---

## Task 8: `api.Migrate` — atomic migration of stdio entries to HTTP

**Files:**

- Create: `internal/api/migrate.go`
- Create: `internal/api/migrate_test.go`

- [ ] **Step 1: Write the failing test**

```go
package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateReplacesStdioWithHTTPForOneClient verifies that a single-
// client migration rewrites the expected config entry without touching
// other clients.
func TestMigrateReplacesStdioWithHTTPForOneClient(t *testing.T) {
	tmp := t.TempDir()

	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(`{"mcpServers":{"memory":{"command":"npx","args":["-y","@x/memory"]}}}`), 0600)

	codexPath := filepath.Join(tmp, "config.toml")
	_ = os.WriteFile(codexPath, []byte(`[mcp_servers.memory]
command = "npx"
args = ["-y", "@x/memory"]
`), 0600)

	// Fake manifest so ManifestExists is true
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte(`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9123
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
weekly_refresh: false
`), 0644)

	a := NewAPI()
	_, err := a.MigrateFrom(MigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"claude-code"}, // only migrate claude-code
		ScanOpts: ScanOpts{
			ClaudeConfigPath: claudePath,
			CodexConfigPath:  codexPath,
			ManifestDir:      manifestDir,
		},
	})
	if err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}

	// Claude is now http
	data, _ := os.ReadFile(claudePath)
	var claudeCfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	_ = json.Unmarshal(data, &claudeCfg)
	if claudeCfg.MCPServers["memory"]["type"] != "http" {
		t.Errorf("claude memory.type: want http, got %v", claudeCfg.MCPServers["memory"]["type"])
	}

	// Codex is unchanged (still has command=npx)
	cod, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(cod), `command = "npx"`) {
		t.Errorf("codex was unexpectedly migrated")
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd <repo> && go test ./internal/api/ -run TestMigrateReplacesStdioWithHTTPForOneClient -v
```

Expected: FAIL.

- [ ] **Step 3: Implement `internal/api/migrate.go`**

```go
package api

import (
	"fmt"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// MigrateOpts controls a migration invocation.
type MigrateOpts struct {
	Servers        []string // server names to migrate
	ClientsInclude []string // empty means all four clients
	DryRun         bool
	ScanOpts       ScanOpts
}

// MigrateReport holds per-client success/failure after a migration.
type MigrateReport struct {
	Applied []AppliedMigration `json:"applied"`
	Failed  []FailedMigration  `json:"failed"`
}

type AppliedMigration struct {
	Server string `json:"server"`
	Client string `json:"client"`
	URL    string `json:"url"`
}
type FailedMigration struct {
	Server string `json:"server"`
	Client string `json:"client"`
	Err    string `json:"err"`
}

// MigrateFrom rewrites stdio entries to hub-HTTP entries (or relay for
// Antigravity) for the specified (server, client) pairs. Idempotent.
func (a *API) MigrateFrom(opts MigrateOpts) (*MigrateReport, error) {
	report := &MigrateReport{}
	allClients := clients.MustAllClients() // helper returning the 4 adapters

	includedClient := func(c string) bool {
		if len(opts.ClientsInclude) == 0 {
			return true
		}
		for _, x := range opts.ClientsInclude {
			if x == c {
				return true
			}
		}
		return false
	}

	for _, server := range opts.Servers {
		// Load the server's manifest to know which daemon port and which
		// clients are expected.
		m, err := loadManifestForServer(opts.ScanOpts.ManifestDir, server)
		if err != nil {
			report.Failed = append(report.Failed, FailedMigration{Server: server, Err: err.Error()})
			continue
		}
		for _, binding := range m.ClientBindings {
			if !includedClient(binding.Client) {
				continue
			}
			adapter := allClients[binding.Client]
			if adapter == nil || !adapter.Exists() {
				continue
			}
			daemonPort, ok := findDaemonPort(m, binding.Daemon)
			if !ok {
				continue
			}
			url := fmt.Sprintf("http://localhost:%d%s", daemonPort, binding.URLPath)
			if opts.DryRun {
				report.Applied = append(report.Applied, AppliedMigration{Server: server, Client: binding.Client, URL: url})
				continue
			}
			if _, err := adapter.Backup(); err != nil {
				report.Failed = append(report.Failed, FailedMigration{Server: server, Client: binding.Client, Err: err.Error()})
				continue
			}
			entry := clients.MCPEntry{
				Name:        server,
				URL:         url,
				RelayServer: server,
				RelayDaemon: binding.Daemon,
				// RelayExePath populated in Install path; for migrate we
				// rely on os.Executable when binding.Client == antigravity.
			}
			if binding.Client == "antigravity" {
				if exe, err := os.Executable(); err == nil {
					entry.RelayExePath = exe
				}
			}
			if err := adapter.AddEntry(entry); err != nil {
				report.Failed = append(report.Failed, FailedMigration{Server: server, Client: binding.Client, Err: err.Error()})
				continue
			}
			report.Applied = append(report.Applied, AppliedMigration{Server: server, Client: binding.Client, URL: url})
		}
	}
	return report, nil
}

func loadManifestForServer(dir, name string) (*config.ServerManifest, error) {
	f, err := os.Open(filepath.Join(dir, name, "manifest.yaml"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return config.ParseManifest(f)
}

func findDaemonPort(m *config.ServerManifest, daemonName string) (int, bool) {
	for _, d := range m.Daemons {
		if d.Name == daemonName {
			return d.Port, true
		}
	}
	return 0, false
}
```

(Adjust imports and helpers as needed.)

- [ ] **Step 4: Run the test**

```bash
cd <repo> && go test ./internal/api/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/migrate.go internal/api/migrate_test.go
git commit -m "feat(api): Migrate() — switch stdio entries to hub HTTP, atomic per client"
```

---

## Phase 3A.1 concludes at Task 8

After Task 8 commits, run a checkpoint review:

- `go test ./... -race` — all green, no regressions in existing packages
- `go build -trimpath -ldflags "... -H windowsgui" -o mcp.exe ./cmd/mcp` — single binary ~12 MB, no console window on `./mcp.exe restart --server memory` (watch for visual flash)
- `./mcp.exe install --server memory --dry-run` — plan still printed correctly (install code moved into api but CLI still wraps it)
- `./mcp.exe install --all --dry-run` — new flag prints plans for all 7 servers
- Manual sanity: verify `mcp.exe status` from cmd.exe attaches to parent console, output prints, prompt returns

If all gates pass, write the Phase 3A.2 plan next. That plan's checkpoint references the line numbers and signatures this plan produces.

---

## What Phase 3A.2 will deliver (preview, not part of this plan)

Phase 3A.2 is a separate document written after 3A.1 commits. Preview of its ~12 tasks:

- `api.Status` enrichment (PID, RAM via `GetProcessMemoryInfo`, Uptime, Port via `netstat` lookup) + `mcp status` column display + `--json` flag
- `api.LogsGet` (tail N lines) + `api.LogsStream` (channel-based follow) + `mcp logs <server> [--tail N] [--follow]`
- Backup sentinel rework: `clients.Backup()` takes keep-N, writes `.bak-mcp-local-hub-original` exactly once, prunes timestamped backups beyond keep threshold
- `api.BackupsList/Clean/Show` + `mcp backups list/clean/show` CLI
- `api.Rollback(original bool)` + `mcp rollback --original` flag
- `mcp scan` CLI (wraps existing `api.Scan`)
- `mcp migrate <server>...` CLI (wraps existing `api.MigrateFrom`)
- `mcp manifest list/show/create/edit/validate/delete/extract` CLI + underlying `api.Manifest*` methods
- `mcp scheduler upgrade` (regenerates scheduler tasks with current exe path) + `mcp scheduler weekly-refresh --set "SUN 03:00"` / `--disable` (hub-wide weekly task replacing per-manifest opt-in)
- `mcp stop --server X` (stops without uninstalling) + `mcp restart --all`
- `mcp settings get/set/list` reading `%LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml` (the same file Phase 3B GUI writes)

## What Phases 3B and 3C will deliver (preview)

**Phase 3B — GUI layer (future plan):**

- `internal/gui/server.go` HTTP skeleton on 127.0.0.1 + handler routing
- `internal/api/events.go` real event bus + state poller goroutine (replaces stub from Task 2)
- `internal/gui/handlers.go` all HTTP API endpoints documented in spec §4.3
- `internal/gui/watcher.go` fsnotify on four client configs
- Embedded HTML/CSS/JS assets + SVG icon sprite via `//go:embed`
- Servers / Migration / Dashboard / Add-server / Logs / Secrets / Settings / About screens
- `internal/tray/` systray integration (icon state + menu + toast notifications)
- Single-instance lock: named mutex + pidport handshake with stale-reboot recovery
- `mcp gui` command: HTTP + tray + browser launch (Chrome `--app` → Edge `--app` → default browser)

**Phase 3C — Release hardening (future plan):**

- `docs/phase-3-verification.md` — full verification matrix run across spec §8.4 checklist
- README + INSTALL updates (Phase 3 section, tray screenshots, `mcp gui` usage)
- Subsystem verification doc (`docs/phase-3-subsystem-verification.md`) with empirical launch matrix results
- Final commit + version bump to 0.3.0 + tag

---

## Summary of quality gates baked into Phase 3A.1

- **TDD:** every task writes a failing test first, then a minimal impl, then verifies green.
- **Commits:** one commit per task with a conventional-commit message.
- **No bundled tasks:** each task changes a focused surface and can be reviewed independently.
- **Source-of-truth layering:** after Task 6, install/uninstall logic lives in `internal/api/` only; CLI wrappers are thin. This shape is what both Phase 3A.2 and Phase 3B will rely on.
- **Backward-compat:** Phase 3A.1 does NOT change any CLI surface observable to users. `mcp install --server X` still works exactly as before (implemented via api.Install now). The `--all` flag is additive. This keeps the checkpoint review low-risk.
- **Verification:** Task 35 runs the full smoke matrix from spec §8.4 before declaring Phase 3 done.
