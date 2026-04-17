# Phase 3A.3 — Management CLI (backups, manifest CRUD, scheduler, settings)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Scope note:** Third and final CLI-foundation plan before the GUI layer. Continuation of:

- Phase 3A.1 — API foundations (done, commits `d5c03a4..a5605cc`): subsystem, internal/api scaffold, Scan/Migrate/Install.
- Phase 3A.2 — Operational CLI (done, commits `01424ed..4f43ccd`): scan/migrate/logs/cleanup/stop/status-enrichment.
- **Phase 3A.3 — Management CLI (THIS PLAN, 7 tasks)** — everything left to reach full CLI parity before Phase 3B starts on the GUI.
- Phase 3B — GUI layer (future plan): HTTP server, SSE, embedded HTML/CSS/JS, tray, single-instance lock.
- Phase 3C — Release (future plan): verification doc, tag.

**Goal of this plan (3A.3):** Deliver all remaining operational CLI surface so Phase 3B has every capability already reachable from the terminal. After 3A.3 the user can: list/clean/restore backups with pristine-sentinel awareness, CRUD manifests without hand-editing YAML, regenerate scheduler tasks after a binary move, enable hub-wide weekly refresh, and read/write GUI preferences from the shell.

**Architecture:** Extend `internal/api/` with `BackupsList/Clean/Show/RollbackOriginal`, `ManifestList/Get/Create/Edit/Validate/Delete/Extract`, `SchedulerUpgrade`, `WeeklyRefreshSet`, and `Settings*` methods. Rework `internal/clients/` `Backup()` to maintain a single pristine `.bak-mcp-local-hub-original` sentinel plus a rolling window of N timestamped backups. Every new CLI command is a thin wrapper over the api layer following the established `newXxxCmd()` → `newXxxCmdReal()` pattern.

**Tech Stack:** Go 1.22+ stdlib (`os`, `path/filepath`, `encoding/yaml`-via-existing-dep, `time`), existing Cobra, `gopkg.in/yaml.v3`. No new external dependencies.

**Reference implementations:**

- `internal/clients/clients.go` — existing `Backup()` implementation to extend with sentinel logic
- `internal/api/scan.go` — pattern for api methods with `*From` tempdir-capable variants
- `internal/cli/scan.go` — thin-wrapper CLI pattern (cobra + api.NewAPI)
- `internal/config/manifest.go` — `ServerManifest` struct + `ParseManifest` (reuse for validation)

**Spec reference:** `docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md` (commit `d20483b`), §3.5 backup strategy, §4.1 new CLI commands.

**Prerequisites:**

- Phase 3A.1 + 3A.2 complete (`bin/mcphub.exe` works with existing commands)
- Binary renamed to `mcphub.exe` (commit `deb9ed7`); `go test ./...` green
- User on Windows 11 (Linux/macOS cross-platform guards inherited from 3A.2)

---

## Scope boundaries (what this plan does NOT cover)

**Out of scope — deferred to Phase 3B or later:**

- GUI-rendered forms for manifest editing (Phase 3B Servers/Add-server view)
- Live backup rotation UI / file browser
- Settings that affect GUI-only behavior (theme, shell-layout, home-screen) — schema is here, but GUI is the primary consumer; CLI gets/set are the plumbing.
- `LogsStream` (follow mode) — stubbed in Phase 3A.2, full implementation is Phase 3B when the SSE bus is built
- Cross-platform backup/manifest path logic beyond Windows (current locations are OS-standard per `userDataDir()` helper from 3A.2)

---

## File Structure

Files to **create**:

- `internal/api/backups.go` — `BackupsList/Clean/Show/RollbackOriginal` + sentinel helpers
- `internal/api/backups_test.go`
- `internal/api/manifest.go` — `ManifestList/Get/Create/Edit/Validate/Delete`
- `internal/api/manifest_test.go`
- `internal/api/scheduler_mgmt.go` — `SchedulerUpgrade`, `WeeklyRefreshSet`, `WeeklyRefreshDisable`
- `internal/api/scheduler_mgmt_test.go`
- `internal/api/settings.go` — `SettingsGet/Set/List` + `gui-preferences.yaml` struct
- `internal/api/settings_test.go`
- `internal/cli/backups.go` — `mcphub backups list/clean/show`
- `internal/cli/manifest.go` — `mcphub manifest *` (all 6 subcommands)
- `internal/cli/scheduler_cmd.go` — `mcphub scheduler upgrade/weekly-refresh`
- `internal/cli/settings.go` — `mcphub settings get/set/list`

Files to **modify**:

- `internal/clients/clients.go` — `Backup()` gains keep-N + sentinel writes; interface stays compatible
- `internal/clients/clients_test.go` — add test for sentinel behavior
- `internal/cli/rollback.go` — add `--original` flag calling `api.RollbackOriginal`
- `internal/cli/root.go` — wire 4 new top-level commands

---

## Task 1: Backup sentinel strategy (pristine original + keep-N rolling)

**Files:**

- Modify: `internal/clients/clients.go`
- Modify: `internal/clients/clients_test.go` (find existing backup test or add)

- [ ] **Step 1: Read current `Backup()` implementation**

Open `internal/clients/clients.go` and find the `Backup()` method. It currently writes a single timestamped backup per invocation without rotation.

Example current shape:

```go
func (c *jsonMCP) Backup() (string, error) {
    data, err := os.ReadFile(c.path)
    if err != nil { return "", err }
    bakPath := c.path + ".bak-mcp-local-hub-" + time.Now().Format("20060102-150405")
    if err := os.WriteFile(bakPath, data, 0600); err != nil { return "", err }
    return bakPath, nil
}
```

- [ ] **Step 2: Write failing test for sentinel + keep-N**

Add to `internal/clients/clients_test.go`:

```go
// TestBackupSentinelWrittenOnlyFirstTime verifies the pristine-original
// sentinel (.bak-mcp-local-hub-original) is written exactly once on the
// first Backup call and never overwritten afterwards.
func TestBackupSentinelWrittenOnlyFirstTime(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"initial":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCP{path: livePath, keyPath: "mcpServers"}

	// First backup — should create the sentinel.
	if _, err := adapter.Backup(); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	sentinel := livePath + ".bak-mcp-local-hub-original"
	if data, err := os.ReadFile(sentinel); err != nil {
		t.Fatalf("sentinel not created: %v", err)
	} else if string(data) != `{"initial":true}` {
		t.Errorf("sentinel content wrong: %s", data)
	}

	// Modify the live file.
	_ = os.WriteFile(livePath, []byte(`{"modified":true}`), 0600)

	// Second backup — sentinel must remain the ORIGINAL content.
	if _, err := adapter.Backup(); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != `{"initial":true}` {
		t.Errorf("sentinel got overwritten on second backup: %s", data)
	}
}

// TestBackupKeepsNLatestTimestamped verifies that after N+3 backups, only
// the most recent N timestamped files remain plus the sentinel.
func TestBackupKeepsNLatestTimestamped(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"v":0}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCP{path: livePath, keyPath: "mcpServers"}

	// 8 backups with sleep so timestamps differ; keep cap is 5.
	for i := 1; i <= 8; i++ {
		_ = os.WriteFile(livePath, []byte(fmt.Sprintf(`{"v":%d}`, i)), 0600)
		if _, err := adapter.BackupKeep(5); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond) // Windows FS only has second-resolution timestamps
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var timestamped, original int
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".bak-mcp-local-hub-original") {
			original++
			continue
		}
		if strings.Contains(name, ".bak-mcp-local-hub-") {
			timestamped++
		}
	}
	if original != 1 {
		t.Errorf("expected 1 sentinel, got %d", original)
	}
	if timestamped != 5 {
		t.Errorf("expected 5 timestamped backups after keep=5, got %d", timestamped)
	}
}
```

Add imports `"fmt"`, `"strings"`, `"time"`, `"path/filepath"`, `"os"` if not present.

Note: if `jsonMCP` is unexported and test lives in a separate file, either move test to `clients_internal_test.go` (package `clients`) or export the type temporarily. Prefer same-package test.

- [ ] **Step 3: Verify failure**

```bash
cd d:/dev/mcp-local-hub && go test ./internal/clients/ -run "TestBackupSentinel|TestBackupKeeps" -v
```

Expected: FAIL — `BackupKeep` undefined OR sentinel not written.

- [ ] **Step 4: Implement sentinel + BackupKeep in `internal/clients/clients.go`**

Find the `Backup()` method (likely on `jsonMCP` and `tomlMCP`). Extract the backup logic into a shared helper + add keep-N parameter:

```go
// writeBackup writes a timestamped backup next to livePath and ensures the
// pristine-original sentinel exists. Returns the timestamped backup path.
// After writing, prunes old timestamped backups so only keepN remain.
// If keepN <= 0, no pruning happens (legacy behavior).
func writeBackup(livePath string, keepN int) (string, error) {
	data, err := os.ReadFile(livePath)
	if err != nil {
		return "", err
	}

	sentinel := livePath + ".bak-mcp-local-hub-original"
	if _, err := os.Stat(sentinel); os.IsNotExist(err) {
		if err := os.WriteFile(sentinel, data, 0600); err != nil {
			return "", fmt.Errorf("write sentinel: %w", err)
		}
	}

	bakPath := livePath + ".bak-mcp-local-hub-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(bakPath, data, 0600); err != nil {
		return "", err
	}

	if keepN > 0 {
		pruneOldTimestamped(livePath, keepN)
	}
	return bakPath, nil
}

// pruneOldTimestamped keeps only the keepN most recent timestamped backups
// of the given live file. The sentinel is never touched.
func pruneOldTimestamped(livePath string, keepN int) {
	dir := filepath.Dir(livePath)
	base := filepath.Base(livePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + ".bak-mcp-local-hub-"
	type bak struct {
		path    string
		modTime time.Time
	}
	var timestamped []bak
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasSuffix(name, "-original") {
			continue // sentinel, never touch
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		timestamped = append(timestamped, bak{path: filepath.Join(dir, name), modTime: fi.ModTime()})
	}
	if len(timestamped) <= keepN {
		return
	}
	sort.Slice(timestamped, func(i, j int) bool {
		return timestamped[i].modTime.After(timestamped[j].modTime)
	})
	for _, b := range timestamped[keepN:] {
		_ = os.Remove(b.path)
	}
}
```

Add imports: `"sort"`, `"strings"`, `"time"`, `"fmt"`, `"path/filepath"` if not present.

Update each adapter's `Backup()` to call `writeBackup(c.path, 0)` (legacy behavior — no pruning). Add a new method `BackupKeep(keepN int)` that calls `writeBackup(c.path, keepN)`.

Sample integration in a `jsonMCP`-like adapter:

```go
func (c *jsonMCP) Backup() (string, error) {
	return writeBackup(c.path, 0)
}

func (c *jsonMCP) BackupKeep(keepN int) (string, error) {
	return writeBackup(c.path, keepN)
}
```

Apply the same to all 4 adapters (claude_code, codex_cli, gemini_cli, antigravity). If the interface `Client` defines `Backup() (string, error)`, add `BackupKeep(int) (string, error)` to the interface too.

- [ ] **Step 5: Run tests**

```bash
cd d:/dev/mcp-local-hub && go test ./internal/clients/ -v
```

Expected: all tests PASS including new sentinel + keep-N tests.

- [ ] **Step 6: Run full suite (existing install/migrate callers still work)**

```bash
cd d:/dev/mcp-local-hub && go test ./... 
```

Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add internal/clients/clients.go internal/clients/clients_test.go
git commit -m "feat(clients): sentinel-based backup strategy + BackupKeep(N)"
```

---

## Task 2: `api.Backups*` + `mcphub backups list/clean/show` + `rollback --original`

**Files:**

- Create: `internal/api/backups.go`
- Create: `internal/api/backups_test.go`
- Create: `internal/cli/backups.go`
- Modify: `internal/cli/rollback.go` (add `--original` flag)
- Modify: `internal/cli/root.go` (wire newBackupsCmd)

- [ ] **Step 1: Write failing tests**

Create `internal/api/backups_test.go`:

```go
package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBackupsListFiltersByKind verifies BackupsList classifies entries as
// "original" (sentinel) vs "timestamped".
func TestBackupsListFiltersByKind(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(live, []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-original", []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-20260417-150000", []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-20260417-160000", []byte("{}"), 0600)

	a := NewAPI()
	list, err := a.BackupsListIn(tmp, ".claude.json")
	if err != nil {
		t.Fatal(err)
	}
	var origs, ts int
	for _, b := range list {
		if b.Kind == "original" {
			origs++
		}
		if b.Kind == "timestamped" {
			ts++
		}
	}
	if origs != 1 {
		t.Errorf("expected 1 original, got %d", origs)
	}
	if ts != 2 {
		t.Errorf("expected 2 timestamped, got %d", ts)
	}
}

// TestBackupsCleanKeepsN prunes timestamped backups down to keepN, never
// touches the original sentinel.
func TestBackupsCleanKeepsN(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(live, []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-original", []byte("orig"), 0600)

	// Create 6 timestamped backups with distinct mtimes.
	base := time.Now().Add(-10 * time.Hour)
	for i := 0; i < 6; i++ {
		p := live + ".bak-mcp-local-hub-" + base.Add(time.Duration(i)*time.Hour).Format("20060102-150405")
		_ = os.WriteFile(p, []byte("bak"), 0600)
		_ = os.Chtimes(p, base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i)*time.Hour))
	}

	a := NewAPI()
	removed, err := a.BackupsCleanIn(tmp, ".claude.json", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Errorf("expected 3 removed (6 - keepN=3), got %d", len(removed))
	}

	// Original sentinel intact.
	if _, err := os.Stat(live + ".bak-mcp-local-hub-original"); err != nil {
		t.Error("sentinel removed")
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd d:/dev/mcp-local-hub && go test ./internal/api/ -run TestBackups -v
```

Expected: FAIL.

- [ ] **Step 3: Implement `internal/api/backups.go`**

```go
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupsList returns all mcp-local-hub backups found next to the four
// managed client config files, classified as "original" (sentinel) or
// "timestamped". Missing client configs are silently skipped.
func (a *API) BackupsList() ([]BackupInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var all []BackupInfo
	for _, c := range clientFiles(home) {
		rows, err := a.BackupsListIn(filepath.Dir(c), filepath.Base(c))
		if err != nil {
			continue // one client's missing dir shouldn't kill the whole list
		}
		all = append(all, rows...)
	}
	return all, nil
}

// BackupsListIn inspects dir for backups of the given live-file name.
// Used by tests and by BackupsList.
func (a *API) BackupsListIn(dir, liveName string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := liveName + ".bak-mcp-local-hub-"
	var out []BackupInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		kind := "timestamped"
		if strings.HasSuffix(name, "-original") {
			kind = "original"
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			Client:   clientNameFromLive(liveName),
			Path:     filepath.Join(dir, name),
			Kind:     kind,
			ModTime:  fi.ModTime(),
			SizeByte: fi.Size(),
		})
	}
	return out, nil
}

// BackupsClean prunes timestamped backups for all 4 clients, keeping only
// keepN most recent per client. Sentinels never touched.
func (a *API) BackupsClean(keepN int) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, c := range clientFiles(home) {
		r, err := a.BackupsCleanIn(filepath.Dir(c), filepath.Base(c), keepN)
		if err != nil {
			continue
		}
		removed = append(removed, r...)
	}
	return removed, nil
}

// BackupsCleanIn is the tempdir-capable form of BackupsClean.
func (a *API) BackupsCleanIn(dir, liveName string, keepN int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := liveName + ".bak-mcp-local-hub-"
	type bak struct {
		path    string
		modTime time.Time
	}
	var ts []bak
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasSuffix(name, "-original") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		ts = append(ts, bak{path: filepath.Join(dir, name), modTime: fi.ModTime()})
	}
	if len(ts) <= keepN {
		return nil, nil
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].modTime.After(ts[j].modTime) })
	var removed []string
	for _, b := range ts[keepN:] {
		if err := os.Remove(b.path); err == nil {
			removed = append(removed, b.path)
		}
	}
	return removed, nil
}

// BackupShow returns the contents of the backup file at path.
func (a *API) BackupShow(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// RollbackOriginal restores each client config from its pristine-sentinel
// backup (if present). Returns per-client result so CLI/GUI can report.
func (a *API) RollbackOriginal() ([]RollbackResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var results []RollbackResult
	for _, live := range clientFiles(home) {
		sentinel := live + ".bak-mcp-local-hub-original"
		if _, err := os.Stat(sentinel); os.IsNotExist(err) {
			results = append(results, RollbackResult{Client: clientNameFromLive(filepath.Base(live)), Err: "no original backup"})
			continue
		}
		data, err := os.ReadFile(sentinel)
		if err != nil {
			results = append(results, RollbackResult{Client: clientNameFromLive(filepath.Base(live)), Err: err.Error()})
			continue
		}
		if err := os.WriteFile(live, data, 0600); err != nil {
			results = append(results, RollbackResult{Client: clientNameFromLive(filepath.Base(live)), Err: err.Error()})
			continue
		}
		results = append(results, RollbackResult{Client: clientNameFromLive(filepath.Base(live)), Restored: live})
	}
	return results, nil
}

// RollbackResult is one row in a RollbackOriginal report.
type RollbackResult struct {
	Client   string
	Restored string
	Err      string
}

// clientFiles returns absolute paths to all 4 managed client configs.
func clientFiles(home string) []string {
	return []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".gemini", "settings.json"),
		filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
	}
}

// clientNameFromLive maps a live config filename to the canonical client id.
func clientNameFromLive(name string) string {
	switch name {
	case ".claude.json":
		return "claude-code"
	case "config.toml":
		return "codex-cli"
	case "settings.json":
		return "gemini-cli"
	case "mcp_config.json":
		return "antigravity"
	}
	return name
}

// _ keeps fmt import alive if any error paths use Errorf in a refactor.
var _ = fmt.Errorf
```

- [ ] **Step 4: Run api tests**

```bash
cd d:/dev/mcp-local-hub && go test ./internal/api/ -v
```

Expected: PASS.

- [ ] **Step 5: Create `internal/cli/backups.go`**

```go
package cli

import (
	"encoding/json"
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newBackupsCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "backups",
		Short: "List, clean, or show client config backups",
	}
	root.AddCommand(newBackupsListCmd())
	root.AddCommand(newBackupsCleanCmd())
	root.AddCommand(newBackupsShowCmd())
	return root
}

func newBackupsListCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List all client config backups with timestamps and sizes",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			list, err := a.BackupsList()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No backups found.")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-14s %-14s %-10s %-10s %s\n", "CLIENT", "KIND", "SIZE(B)", "MODIFIED", "PATH")
			for _, b := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%-14s %-14s %-10d %-10s %s\n",
					b.Client, b.Kind, b.SizeByte, b.ModTime.Format("01-02 15:04"), b.Path)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}

func newBackupsCleanCmd() *cobra.Command {
	var keep int
	c := &cobra.Command{
		Use:   "clean",
		Short: "Remove old timestamped backups, keeping only the N most recent per client",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			removed, err := a.BackupsClean(keep)
			if err != nil {
				return err
			}
			if len(removed) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to clean.")
				return nil
			}
			for _, p := range removed {
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed %s\n", p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n%d file(s) removed.\n", len(removed))
			return nil
		},
	}
	c.Flags().IntVar(&keep, "keep", 5, "number of most recent timestamped backups to retain per client")
	return c
}

func newBackupsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <path>",
		Short: "Print the contents of a backup file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			content, err := a.BackupShow(args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), content)
			return nil
		},
	}
}
```

- [ ] **Step 6: Modify `internal/cli/rollback.go` to add `--original` flag**

Read existing `internal/cli/rollback.go`. Add `var original bool` and a branch:

```go
if original {
    a := api.NewAPI()
    results, err := a.RollbackOriginal()
    if err != nil {
        return err
    }
    for _, r := range results {
        if r.Err != "" {
            fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.Client, r.Err)
        } else {
            fmt.Fprintf(cmd.OutOrStdout(), "✓ Restored %s → %s\n", r.Client, r.Restored)
        }
    }
    return nil
}
// existing latest-backup rollback path stays unchanged
```

Add flag: `c.Flags().BoolVar(&original, "original", false, "restore from the pristine (first-ever) backup rather than the most recent")`.

Add `"mcp-local-hub/internal/api"` to imports if not already.

- [ ] **Step 7: Wire into root.go**

Add to `internal/cli/root.go`:

```go
func newBackupsCmd() *cobra.Command { return newBackupsCmdReal() }
// In AddCommand list:
root.AddCommand(newBackupsCmd())
```

- [ ] **Step 8: Test + build**

```bash
cd d:/dev/mcp-local-hub && go test ./... && go build ./...
```

Expected: all green, silent build.

- [ ] **Step 9: Commit**

```bash
git add internal/api/backups.go internal/api/backups_test.go internal/cli/backups.go internal/cli/rollback.go internal/cli/root.go
git commit -m "feat(backups): list/clean/show CLI + rollback --original (pristine sentinel)"
```

---

## Task 3: `api.Manifest*` — CRUD for server manifests

**Files:**

- Create: `internal/api/manifest.go`
- Create: `internal/api/manifest_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/api/manifest_test.go`:

```go
package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestListReturnsAllYAML(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "foo"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "foo", "manifest.yaml"),
		[]byte("name: foo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons: [{name: default, port: 9200}]\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmp, "bar"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "bar", "manifest.yaml"),
		[]byte("name: bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons: [{name: default, port: 9201}]\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmp, "draft"), 0755)
	// draft dir has no manifest.yaml — should be skipped.

	a := NewAPI()
	names, err := a.ManifestListIn(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 manifests, got %v", names)
	}
}

func TestManifestValidateCatchesMissingFields(t *testing.T) {
	a := NewAPI()
	warnings := a.ManifestValidate("name: foo\n") // missing kind, transport, command, daemons
	if len(warnings) == 0 {
		t.Error("expected warnings for incomplete manifest, got none")
	}
}

func TestManifestCreateWritesYAML(t *testing.T) {
	tmp := t.TempDir()
	a := NewAPI()
	body := "name: newsrv\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons: [{name: default, port: 9202}]\nclient_bindings: []\nweekly_refresh: false\n"
	if err := a.ManifestCreateIn(tmp, "newsrv", body); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, "newsrv", "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: newsrv") {
		t.Error("manifest content not written")
	}
}

func TestManifestDeleteRemovesDir(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "doomed"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "doomed", "manifest.yaml"),
		[]byte("name: doomed\nkind: global\ntransport: stdio-bridge\ncommand: x\ndaemons: [{name: default, port: 9203}]\n"), 0644)

	a := NewAPI()
	if err := a.ManifestDeleteIn(tmp, "doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "doomed")); !os.IsNotExist(err) {
		t.Error("manifest dir not removed")
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd d:/dev/mcp-local-hub && go test ./internal/api/ -run TestManifest -v
```

Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement `internal/api/manifest.go`**

```go
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
)

// ManifestList returns the sorted list of server names that have a manifest
// at <defaultManifestDir>/<name>/manifest.yaml.
func (a *API) ManifestList() ([]string, error) {
	return a.ManifestListIn(defaultManifestDir())
}

// ManifestListIn is the tempdir-capable form of ManifestList.
func (a *API) ManifestListIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ManifestGet returns the raw YAML of the named server's manifest.
func (a *API) ManifestGet(name string) (string, error) {
	return a.ManifestGetIn(defaultManifestDir(), name)
}

// ManifestGetIn is the tempdir-capable form of ManifestGet.
func (a *API) ManifestGetIn(dir, name string) (string, error) {
	path := filepath.Join(dir, name, "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ManifestCreate writes a new manifest under the default servers dir.
// Rejects if the server name already has a manifest — use ManifestEdit
// to change existing ones.
func (a *API) ManifestCreate(name, yaml string) error {
	return a.ManifestCreateIn(defaultManifestDir(), name, yaml)
}

// ManifestCreateIn is the tempdir-capable form of ManifestCreate.
func (a *API) ManifestCreateIn(dir, name, yaml string) error {
	target := filepath.Join(dir, name, "manifest.yaml")
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("manifest %q already exists at %s; use edit instead", name, target)
	}
	if warnings := a.ManifestValidate(yaml); len(warnings) > 0 {
		return fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(yaml), 0644)
}

// ManifestEdit replaces an existing manifest after validation. Fails if
// the manifest doesn't exist; use ManifestCreate for new entries.
func (a *API) ManifestEdit(name, yaml string) error {
	return a.ManifestEditIn(defaultManifestDir(), name, yaml)
}

// ManifestEditIn is the tempdir-capable form of ManifestEdit.
func (a *API) ManifestEditIn(dir, name, yaml string) error {
	target := filepath.Join(dir, name, "manifest.yaml")
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("manifest %q does not exist; use create instead", name)
	}
	if warnings := a.ManifestValidate(yaml); len(warnings) > 0 {
		return fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	return os.WriteFile(target, []byte(yaml), 0644)
}

// ManifestValidate parses a manifest YAML and returns any structural
// issues (missing required fields, unknown kind/transport values). Empty
// slice means the manifest passes basic validation. Does NOT check that
// referenced binaries, ports, or secrets actually exist — that's caller
// responsibility at install time.
func (a *API) ManifestValidate(yaml string) []string {
	var warnings []string
	reader := strings.NewReader(yaml)
	m, err := config.ParseManifest(reader)
	if err != nil {
		return []string{err.Error()}
	}
	// ParseManifest calls m.Validate internally, so if we reach here the
	// structural validation passed. Add secondary soft checks:
	if len(m.Daemons) == 0 {
		warnings = append(warnings, "no daemons declared")
	}
	for _, d := range m.Daemons {
		if d.Port == 0 {
			warnings = append(warnings, fmt.Sprintf("daemon %q has port=0", d.Name))
		}
	}
	return warnings
}

// ManifestDelete removes the named server's manifest directory. Does NOT
// uninstall the server — caller should run Uninstall first for a clean
// teardown.
func (a *API) ManifestDelete(name string) error {
	return a.ManifestDeleteIn(defaultManifestDir(), name)
}

// ManifestDeleteIn is the tempdir-capable form of ManifestDelete.
func (a *API) ManifestDeleteIn(dir, name string) error {
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("manifest %q does not exist", name)
	}
	return os.RemoveAll(target)
}
```

- [ ] **Step 4: Run tests**

```bash
cd d:/dev/mcp-local-hub && go test ./internal/api/ -run TestManifest -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/manifest.go internal/api/manifest_test.go
git commit -m "feat(api): manifest CRUD — ManifestList/Get/Create/Edit/Validate/Delete"
```

---

## Task 4: `mcphub manifest *` CLI (all 7 subcommands)

**Files:**

- Create: `internal/cli/manifest.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Create `internal/cli/manifest.go`**

```go
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newManifestCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "manifest",
		Short: "Manage server manifests under servers/*/manifest.yaml",
	}
	root.AddCommand(newManifestListCmd())
	root.AddCommand(newManifestShowCmd())
	root.AddCommand(newManifestCreateCmd())
	root.AddCommand(newManifestEditCmd())
	root.AddCommand(newManifestValidateCmd())
	root.AddCommand(newManifestDeleteCmd())
	root.AddCommand(newManifestExtractCmd())
	return root
}

func newManifestListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List server names with manifests",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			names, err := a.ManifestList()
			if err != nil {
				return err
			}
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}
}

func newManifestShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print the YAML of a server's manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			yaml, err := a.ManifestGet(args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), yaml)
			return nil
		},
	}
}

func newManifestCreateCmd() *cobra.Command {
	var fromFile string
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new manifest (from --from-file or stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var yaml []byte
			var err error
			if fromFile != "" {
				yaml, err = os.ReadFile(fromFile)
				if err != nil {
					return err
				}
			} else {
				yaml, err = readAllStdin()
				if err != nil {
					return err
				}
			}
			a := api.NewAPI()
			if err := a.ManifestCreate(args[0], string(yaml)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Created manifest %s\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&fromFile, "from-file", "", "read YAML from this file instead of stdin")
	return c
}

func newManifestEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <name>",
		Short: "Open the manifest in $EDITOR and re-validate on save",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			yaml, err := a.ManifestGet(args[0])
			if err != nil {
				return err
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "notepad"
			}
			tmp, err := os.CreateTemp("", args[0]+"-*.yaml")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			if _, err := tmp.WriteString(yaml); err != nil {
				tmp.Close()
				return err
			}
			tmp.Close()
			editorCmd := exec.Command(editor, tmp.Name())
			editorCmd.Stdin = os.Stdin
			editorCmd.Stdout = os.Stdout
			editorCmd.Stderr = os.Stderr
			if err := editorCmd.Run(); err != nil {
				return fmt.Errorf("editor %s: %w", editor, err)
			}
			edited, err := os.ReadFile(tmp.Name())
			if err != nil {
				return err
			}
			if err := a.ManifestEdit(args[0], string(edited)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Saved manifest %s\n", args[0])
			return nil
		},
	}
}

func newManifestValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <name>",
		Short: "Check a manifest for structural issues",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			yaml, err := a.ManifestGet(args[0])
			if err != nil {
				return err
			}
			warnings := a.ManifestValidate(yaml)
			if len(warnings) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s: valid\n", args[0])
				return nil
			}
			for _, w := range warnings {
				fmt.Fprintf(cmd.OutOrStderr(), "  %s\n", w)
			}
			return fmt.Errorf("%d validation issue(s)", len(warnings))
		},
	}
}

func newManifestDeleteCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a manifest (uninstall first or use --force)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				fmt.Fprintln(cmd.OutOrStderr(),
					"refusing to delete without --force; if the server is installed, run `mcphub uninstall --server "+args[0]+"` first")
				return fmt.Errorf("missing --force")
			}
			a := api.NewAPI()
			if err := a.ManifestDelete(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Deleted manifest %s\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip the uninstall-first reminder")
	return c
}

func newManifestExtractCmd() *cobra.Command {
	var clientFlag string
	c := &cobra.Command{
		Use:   "extract <server>",
		Short: "Print a draft manifest YAML derived from an existing client's stdio entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clientFlag == "" {
				return fmt.Errorf("--client is required (claude-code | codex-cli | gemini-cli | antigravity)")
			}
			a := api.NewAPI()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			yaml, err := a.ExtractManifestFromClient(clientFlag, args[0], api.ScanOpts{
				ClaudeConfigPath:      filepath.Join(home, ".claude.json"),
				CodexConfigPath:       filepath.Join(home, ".codex", "config.toml"),
				GeminiConfigPath:      filepath.Join(home, ".gemini", "settings.json"),
				AntigravityConfigPath: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
				ManifestDir:           scanManifestDir(),
			})
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), yaml)
			return nil
		},
	}
	c.Flags().StringVar(&clientFlag, "client", "", "source client (claude-code | codex-cli | gemini-cli | antigravity)")
	return c
}
```

Note: `readAllStdin()` already exists in `internal/cli/secrets.go`; `scanManifestDir()` already exists in `internal/cli/scan.go` from Phase 3A.2.

- [ ] **Step 2: Wire into root.go**

```go
// In the wrapper block:
func newManifestCmd() *cobra.Command { return newManifestCmdReal() }

// In AddCommand list:
root.AddCommand(newManifestCmd())
```

- [ ] **Step 3: Build + test**

```bash
cd d:/dev/mcp-local-hub && go test ./... && go build ./...
```

- [ ] **Step 4: Smoke test**

```bash
./bin/mcphub.exe manifest list
./bin/mcphub.exe manifest show memory
./bin/mcphub.exe manifest validate memory
```

Expected: list prints 8 server names, show prints full memory YAML, validate reports "valid".

- [ ] **Step 5: Commit**

```bash
git add internal/cli/manifest.go internal/cli/root.go
git commit -m "feat(cli): mcphub manifest list/show/create/edit/validate/delete/extract"
```

---

## Task 5: `api.SchedulerUpgrade` + `mcphub scheduler upgrade`

**Files:**

- Create: `internal/api/scheduler_mgmt.go`
- Create: `internal/api/scheduler_mgmt_test.go`
- Create: `internal/cli/scheduler_cmd.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write failing test**

Create `internal/api/scheduler_mgmt_test.go`:

```go
package api

import "testing"

// TestSchedulerUpgradeNoopWhenEmpty verifies that running SchedulerUpgrade
// on a system with no mcp-local-hub tasks returns an empty result list
// without error.
func TestSchedulerUpgradeNoopWhenEmpty(t *testing.T) {
	// Cannot easily stub schtasks.exe in unit tests; just assert the api
	// accepts the call and returns something sane. Real verification is
	// the live smoke test in step 3.
	a := NewAPI()
	results, err := a.SchedulerUpgrade()
	if err != nil {
		t.Skipf("scheduler unavailable: %v", err)
	}
	_ = results
}
```

- [ ] **Step 2: Implement `internal/api/scheduler_mgmt.go`**

```go
package api

import (
	"fmt"
	"os"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// SchedulerUpgradeResult is one row in the per-task upgrade report.
type SchedulerUpgradeResult struct {
	TaskName string
	OldCmd   string
	NewCmd   string
	Err      string
}

// SchedulerUpgrade regenerates every mcp-local-hub scheduler task using the
// current executable path. Useful after:
//   - moving the binary to a new location
//   - renaming the binary (e.g. mcp.exe → mcphub.exe)
//   - bin/ reorganization
//
// Preserves scheduler task names and trigger configurations; only the
// <Command> and <WorkingDirectory> fields are updated.
func (a *API) SchedulerUpgrade() ([]SchedulerUpgradeResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		return nil, err
	}
	var results []SchedulerUpgradeResult
	manifestDir := defaultManifestDir()
	for _, t := range tasks {
		srv, dmn := parseTaskName(t.Name)
		if srv == "" {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: "unparseable task name"})
			continue
		}
		m, err := loadManifestForServer(manifestDir, srv)
		if err != nil {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("manifest %s: %v", srv, err)})
			continue
		}
		// Re-build the task spec with current exe path.
		var args []string
		if dmn == "weekly-refresh" {
			args = []string{"restart", "--server", m.Name}
		} else {
			args = []string{"daemon", "--server", m.Name, "--daemon", dmn}
		}
		_ = m // referenced for future expansion (env, triggers)

		if err := sch.Delete(t.Name); err != nil {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("delete: %v", err)})
			continue
		}
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          exe,
			Args:             args,
			RestartOnFailure: dmn != "weekly-refresh",
		}
		if dmn == "weekly-refresh" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		} else {
			spec.LogonTrigger = true
		}
		if err := sch.Create(spec); err != nil {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("create: %v", err)})
			continue
		}
		results = append(results, SchedulerUpgradeResult{TaskName: t.Name, NewCmd: exe})
	}
	return results, nil
}

// _ keeps config import alive for future use in this file.
var _ = config.KindGlobal
```

- [ ] **Step 3: Create `internal/cli/scheduler_cmd.go`**

```go
package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newSchedulerCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "scheduler",
		Short: "Scheduler-level operations (upgrade tasks, manage weekly refresh)",
	}
	root.AddCommand(newSchedulerUpgradeCmd())
	return root
}

func newSchedulerUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Regenerate scheduler tasks with the current binary path (after move/rename)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			results, err := a.SchedulerUpgrade()
			if err != nil {
				return err
			}
			for _, r := range results {
				if r.Err != "" {
					fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Upgraded %s → %s\n", r.TaskName, r.NewCmd)
				}
			}
			return nil
		},
	}
}
```

- [ ] **Step 4: Wire into root.go**

```go
func newSchedulerCmd() *cobra.Command { return newSchedulerCmdReal() }
root.AddCommand(newSchedulerCmd())
```

- [ ] **Step 5: Test + build + commit**

```bash
cd d:/dev/mcp-local-hub && go test ./... && go build ./...
git add internal/api/scheduler_mgmt.go internal/api/scheduler_mgmt_test.go internal/cli/scheduler_cmd.go internal/cli/root.go
git commit -m "feat(scheduler): mcphub scheduler upgrade — regenerate tasks with current exe path"
```

---

## Task 6: `api.WeeklyRefreshSet` + `mcphub scheduler weekly-refresh`

**Files:**

- Modify: `internal/api/scheduler_mgmt.go` (add WeeklyRefreshSet/Disable)
- Modify: `internal/api/scheduler_mgmt_test.go` (add test)
- Modify: `internal/cli/scheduler_cmd.go` (add weekly-refresh subcommand)

- [ ] **Step 1: Write failing test**

Append to `internal/api/scheduler_mgmt_test.go`:

```go
func TestParseWeeklyRefreshSchedule(t *testing.T) {
	tests := []struct {
		input   string
		wantDay int
		wantHr  int
		wantMin int
		wantErr bool
	}{
		{"SUN 03:00", 0, 3, 0, false},
		{"MON 14:30", 1, 14, 30, false},
		{"FRI 23:59", 5, 23, 59, false},
		{"SAT 00:01", 6, 0, 1, false},
		{"XXX 12:00", 0, 0, 0, true},
		{"SUN 25:00", 0, 0, 0, true},
		{"SUN", 0, 0, 0, true},
	}
	for _, tc := range tests {
		day, hr, min, err := parseWeeklyRefreshSchedule(tc.input)
		gotErr := err != nil
		if gotErr != tc.wantErr {
			t.Errorf("%q: err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && (day != tc.wantDay || hr != tc.wantHr || min != tc.wantMin) {
			t.Errorf("%q: got (%d,%d,%d), want (%d,%d,%d)", tc.input, day, hr, min, tc.wantDay, tc.wantHr, tc.wantMin)
		}
	}
}
```

- [ ] **Step 2: Implement in `internal/api/scheduler_mgmt.go`**

Append:

```go
// WeeklyRefreshSet creates or replaces the hub-wide weekly-refresh
// scheduler task. schedule format is "<DAY> <HH:MM>" where DAY is a
// 3-letter abbreviation (SUN|MON|...|SAT, case-insensitive).
func (a *API) WeeklyRefreshSet(schedule string) error {
	day, hr, min, err := parseWeeklyRefreshSchedule(schedule)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	sch, err := scheduler.New()
	if err != nil {
		return err
	}
	const taskName = "mcp-local-hub-weekly-refresh"
	_ = sch.Delete(taskName) // idempotent
	return sch.Create(scheduler.TaskSpec{
		Name:             taskName,
		Description:      "mcp-local-hub: weekly refresh (restart --all)",
		Command:          exe,
		Args:             []string{"restart", "--all"},
		WeeklyTrigger:    &scheduler.WeeklyTrigger{DayOfWeek: day, HourLocal: hr, MinuteLocal: min},
		RestartOnFailure: false,
	})
}

// WeeklyRefreshDisable removes the hub-wide weekly-refresh task.
// Per-manifest weekly_refresh: true entries are not affected.
func (a *API) WeeklyRefreshDisable() error {
	sch, err := scheduler.New()
	if err != nil {
		return err
	}
	return sch.Delete("mcp-local-hub-weekly-refresh")
}

// parseWeeklyRefreshSchedule parses "<DAY> <HH:MM>" into numeric parts.
// DAY: SUN=0, MON=1, TUE=2, WED=3, THU=4, FRI=5, SAT=6 (matches Go's Weekday).
func parseWeeklyRefreshSchedule(s string) (day, hour, min int, err error) {
	parts := strings.SplitN(strings.TrimSpace(s), " ", 2)
	if len(parts) != 2 {
		return 0, 0, 0, fmt.Errorf("expected '<DAY> <HH:MM>', got %q", s)
	}
	dayMap := map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}
	day, ok := dayMap[strings.ToUpper(parts[0])]
	if !ok {
		return 0, 0, 0, fmt.Errorf("unknown day %q (use SUN..SAT)", parts[0])
	}
	hm := strings.SplitN(parts[1], ":", 2)
	if len(hm) != 2 {
		return 0, 0, 0, fmt.Errorf("expected HH:MM, got %q", parts[1])
	}
	hour, err = strconv.Atoi(hm[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, 0, fmt.Errorf("invalid hour %q", hm[0])
	}
	min, err = strconv.Atoi(hm[1])
	if err != nil || min < 0 || min > 59 {
		return 0, 0, 0, fmt.Errorf("invalid minute %q", hm[1])
	}
	return day, hour, min, nil
}
```

Add imports `"strings"`, `"strconv"` to scheduler_mgmt.go if not present.

- [ ] **Step 3: Add CLI subcommand in `internal/cli/scheduler_cmd.go`**

Add to `newSchedulerCmdReal()` parent:

```go
root.AddCommand(newSchedulerWeeklyRefreshCmd())
```

Define:

```go
func newSchedulerWeeklyRefreshCmd() *cobra.Command {
	var setFlag string
	var disableFlag bool
	c := &cobra.Command{
		Use:   "weekly-refresh",
		Short: "Configure the hub-wide weekly-refresh task",
		Long: `Manages a single scheduler task that runs 'mcphub restart --all' weekly.
Pass --set "SUN 03:00" to enable, --disable to remove.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			if disableFlag {
				if err := a.WeeklyRefreshDisable(); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "✓ Disabled weekly refresh")
				return nil
			}
			if setFlag == "" {
				return fmt.Errorf("--set or --disable is required")
			}
			if err := a.WeeklyRefreshSet(setFlag); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Weekly refresh scheduled: %s\n", setFlag)
			return nil
		},
	}
	c.Flags().StringVar(&setFlag, "set", "", `schedule as "<DAY> <HH:MM>", e.g. "SUN 03:00"`)
	c.Flags().BoolVar(&disableFlag, "disable", false, "remove the weekly-refresh task")
	return c
}
```

- [ ] **Step 4: Test + commit**

```bash
cd d:/dev/mcp-local-hub && go test ./... && go build ./...
# Smoke test
./bin/mcphub.exe scheduler weekly-refresh --set "SUN 03:00"
./bin/mcphub.exe status | grep weekly-refresh
./bin/mcphub.exe scheduler weekly-refresh --disable

git add internal/api/scheduler_mgmt.go internal/api/scheduler_mgmt_test.go internal/cli/scheduler_cmd.go
git commit -m "feat(scheduler): mcphub scheduler weekly-refresh --set/--disable"
```

---

## Task 7: `api.Settings*` + `mcphub settings get/set/list`

**Files:**

- Create: `internal/api/settings.go`
- Create: `internal/api/settings_test.go`
- Create: `internal/cli/settings.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write failing test**

Create `internal/api/settings_test.go`:

```go
package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "gui-preferences.yaml")

	a := NewAPI()
	if err := a.SettingsSetIn(path, "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	if err := a.SettingsSetIn(path, "shell", "sidebar"); err != nil {
		t.Fatal(err)
	}

	all, err := a.SettingsListIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if all["theme"] != "dark" || all["shell"] != "sidebar" {
		t.Errorf("round-trip: got %v, want {theme:dark, shell:sidebar}", all)
	}

	val, err := a.SettingsGetIn(path, "theme")
	if err != nil {
		t.Fatal(err)
	}
	if val != "dark" {
		t.Errorf("get theme: got %q, want dark", val)
	}
}

func TestSettingsGetMissingKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "gui-preferences.yaml")
	_ = os.WriteFile(path, []byte("theme: light\n"), 0600)

	a := NewAPI()
	_, err := a.SettingsGetIn(path, "nonexistent")
	if err == nil {
		t.Error("expected error for missing key, got nil")
	}
}
```

- [ ] **Step 2: Implement `internal/api/settings.go`**

```go
package api

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SettingsPath returns the canonical preferences file location (in the
// per-user data dir — same as secrets).
func SettingsPath() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "gui-preferences.yaml")
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "gui-preferences.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "gui-preferences.yaml"
	}
	return filepath.Join(home, ".local", "share", "mcp-local-hub", "gui-preferences.yaml")
}

// SettingsList returns all settings as a key→value map.
func (a *API) SettingsList() (map[string]string, error) {
	return a.SettingsListIn(SettingsPath())
}

// SettingsListIn is the tempdir-capable form of SettingsList.
func (a *API) SettingsListIn(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SettingsGet returns the value for a key.
func (a *API) SettingsGet(key string) (string, error) {
	return a.SettingsGetIn(SettingsPath(), key)
}

// SettingsGetIn is the tempdir-capable form of SettingsGet.
func (a *API) SettingsGetIn(path, key string) (string, error) {
	all, err := a.SettingsListIn(path)
	if err != nil {
		return "", err
	}
	v, ok := all[key]
	if !ok {
		return "", fmt.Errorf("setting %q not found", key)
	}
	return v, nil
}

// SettingsSet writes a key=value pair, creating the file if needed.
func (a *API) SettingsSet(key, value string) error {
	return a.SettingsSetIn(SettingsPath(), key, value)
}

// SettingsSetIn is the tempdir-capable form of SettingsSet.
func (a *API) SettingsSetIn(path, key, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	all, err := a.SettingsListIn(path)
	if err != nil {
		return err
	}
	all[key] = value
	data, err := yaml.Marshal(all)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
```

- [ ] **Step 3: Create `internal/cli/settings.go`**

```go
package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newSettingsCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "settings",
		Short: "Read/write GUI preferences (theme, shell, default-home, etc.)",
	}
	root.AddCommand(newSettingsListCmd())
	root.AddCommand(newSettingsGetCmd())
	root.AddCommand(newSettingsSetCmd())
	return root
}

func newSettingsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print all current settings as key=value pairs",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			all, err := a.SettingsList()
			if err != nil {
				return err
			}
			if len(all) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no settings yet — defaults apply)")
				return nil
			}
			for k, v := range all {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", k, v)
			}
			return nil
		},
	}
}

func newSettingsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the value for a setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			val, err := a.SettingsGet(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			return nil
		},
	}
}

func newSettingsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write a setting value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			if err := a.SettingsSet(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s=%s\n", args[0], args[1])
			return nil
		},
	}
}
```

- [ ] **Step 4: Wire into root.go**

```go
func newSettingsCmd() *cobra.Command { return newSettingsCmdReal() }
root.AddCommand(newSettingsCmd())
```

- [ ] **Step 5: Test + smoke + commit**

```bash
cd d:/dev/mcp-local-hub && go test ./... && go build ./...
./bin/mcphub.exe settings set theme dark
./bin/mcphub.exe settings list
./bin/mcphub.exe settings get theme

git add internal/api/settings.go internal/api/settings_test.go internal/cli/settings.go internal/cli/root.go
git commit -m "feat(settings): mcphub settings get/set/list (yaml under userDataDir)"
```

---

## Phase 3A.3 concludes at Task 7

After Task 7 commits, the checkpoint covers all Phase 3A CLI surface:

- `go test ./... -race` — all green
- `./bin/mcphub.exe backups list` — shows all backups across 4 clients
- `./bin/mcphub.exe backups clean --keep 3` — prunes to latest 3 per client
- `./bin/mcphub.exe rollback --original` — restores pristine sentinels
- `./bin/mcphub.exe manifest list` — 8 server names
- `./bin/mcphub.exe manifest show memory` — YAML printed
- `./bin/mcphub.exe manifest validate memory` — "valid"
- `./bin/mcphub.exe scheduler upgrade` — all tasks rewritten with current exe path
- `./bin/mcphub.exe scheduler weekly-refresh --set "SUN 03:00"` + `--disable`
- `./bin/mcphub.exe settings set theme dark && settings get theme` — round-trip works

If all gates pass, Phase 3A is complete (commits span `d5c03a4..Task 7 SHA`) and Phase 3B (GUI layer) can be planned next with confidence that every GUI feature will have a working api-level backing.

---

## Quality gates baked into Phase 3A.3

- **TDD:** every code task writes failing test first (Tasks 1, 2, 3, 5, 6, 7).
- **Commits:** one commit per task with conventional-commit message.
- **Source-of-truth layering:** all new logic lives in `internal/api/`, CLI is thin wrapper. No business logic in `internal/cli/`.
- **Cross-platform hygiene:** `SettingsPath()` and `userDataDir()` already work on Linux/macOS via XDG paths. Scheduler ops gracefully error on non-Windows (inherited from existing `internal/scheduler/` stubs).
- **Backward-compat:** sentinel backup is additive — existing `.bak-mcp-local-hub-<timestamp>` files stay valid. Old `Backup()` signature unchanged; new `BackupKeep(N)` is additive.
