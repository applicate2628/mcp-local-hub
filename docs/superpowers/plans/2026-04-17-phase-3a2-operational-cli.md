# Phase 3A.2 — Operational CLI (scan, migrate, status, logs, cleanup)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Scope note:** Phase 3A is split into three small plans to keep each executable with concrete code and zero placeholders:

- **Phase 3A.1 — API foundations (done, commits d5c03a4..a5605cc + hotfix c667a88)** — subsystem switch, internal/api scaffold, Scan/Migrate/Install/InstallAll.
- **Phase 3A.2 — Operational CLI (THIS PLAN, 9 tasks)** — day-1 CLI commands for visibility and cleanup: enriched `mcp status`, `mcp scan`, `mcp migrate`, `mcp logs`, `mcp cleanup`, `mcp stop`, `mcp restart --all`, plus the api-side process-counting used by cleanup and enriched scan output.
- **Phase 3A.3 — Management CLI (next plan, ~8 tasks)** — `mcp backups *`, `mcp rollback --original`, `mcp manifest list/show/create/edit/validate/delete/extract`, `mcp scheduler upgrade`, `mcp scheduler weekly-refresh`, `mcp settings get/set/list`.

**Goal of this plan (3A.2):** Give users operational visibility and cleanup power from the terminal. After 3A.2 the user can: see which daemons are running (`mcp status` with Port/PID/RAM/Uptime columns), see unified view of what's hub-routed vs stdio across all clients (`mcp scan`), flip stdio→hub for any manifested server (`mcp migrate`), tail daemon logs (`mcp logs`), detect and kill orphaned MCP subprocesses from dead client sessions (`mcp cleanup`), stop/restart daemons (`mcp stop`, `mcp restart --all`). Same Go api package stays the source of truth; CLI is thin wrapper.

**Architecture:** Extend existing `internal/api/` with `Status` enrichment, `CountProcesses`, `CleanupOrphans`, `LogsGet`/`LogsStream`. Add CLI commands under `internal/cli/` as thin wrappers. Process detection uses `wmic process get CommandLine,ProcessId,WorkingSetSize` on Windows, with pattern-matching against manifest's declared `command + base_args`. Log tail/follow uses the existing per-daemon log files under `%LOCALAPPDATA%\mcp-local-hub\logs\`.

**Tech Stack:** Go 1.22+ stdlib (`os/exec`, `bufio`, `syscall`, `net/http` for status probes), existing Cobra. No new external dependencies.

**Reference implementations:**

- `internal/cli/status.go` — existing `mcp status` (to be enhanced in-place)
- `internal/cli/daemon.go` — how daemon log paths are resolved (`logBaseDir()`)
- `internal/api/scan.go` (Phase 3A.1) — pattern for `ScanOpts`, classification, test fixtures

**Spec reference:** `docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md` (commit `d20483b`)

**Prerequisites:**

- Phase 3A.1 + hotfix `c667a88` complete (`go test ./...` green, `mcp.exe` Windows subsystem, gdb daemon via hub)
- `wmic` on PATH (standard Windows system32)
- User on Windows 11

---

## Naming, scope, and non-goals

**In scope:**

- `api.Status` gains `Port`, `PID`, `RAMBytes`, `UptimeSec` — populated by `schtasks /Query` + `wmic process`.
- `api.CountProcesses(server, manifestDir)` — enumerates running processes matching a manifest's declared command pattern.
- `api.CleanupOrphans(opts)` — finds MCP-server processes whose command line matches a known manifest but which are NOT children of our scheduler-owned daemons; reports / kills them.
- New CLI: `scan`, `migrate`, `logs`, `cleanup`, `stop`, `restart --all`. Enriched `status --json`.

**Out of scope (Phase 3A.3):**

- Backup sentinel strategy. Current `clients.Backup()` continues to create timestamped-only backups.
- `mcp manifest create/edit/validate` CLI commands (drafting manifests is still manual YAML editing in 3A.2).
- `mcp scheduler upgrade` / `weekly-refresh --set` / `settings` commands.

**Cleanup semantics:**

- "Orphan" = process whose command line matches a known manifest's `command + base_args` pattern, AND whose parent PID is not the scheduler-owned `mcp.exe daemon --server X` process, AND which has been running more than 60 seconds (avoids race with freshly spawned daemons during install).
- `--dry-run` (default): report orphans and what would be killed, no action.
- Without `--dry-run`: kill the orphans via Windows `TerminateProcess`.

---

## File Structure

Files to **create**:

- `internal/api/status_enrich.go` — `enrichStatus()` helper that adds PID/RAM/Uptime/Port to each `DaemonStatus` (separate file so existing `internal/api/install.go`'s `Status` method stays readable)
- `internal/api/status_enrich_test.go`
- `internal/api/processes.go` — `CountProcesses`, `ListMatchingProcesses`, Windows `wmic` wrapper
- `internal/api/processes_test.go`
- `internal/api/cleanup.go` — `CleanupOrphans`, orphan detection
- `internal/api/cleanup_test.go`
- `internal/api/logs.go` — `LogsGet`, `LogsStream`
- `internal/api/logs_test.go`
- `internal/cli/scan.go` — `mcp scan` command
- `internal/cli/migrate.go` — `mcp migrate` command
- `internal/cli/logs.go` — `mcp logs` command
- `internal/cli/cleanup.go` — `mcp cleanup` command
- `internal/cli/stop.go` — `mcp stop` command

Files to **modify**:

- `internal/api/install.go` — `(*API).Status()` method: currently reads scheduler state; call `enrichStatus()` after the existing fetch
- `internal/cli/status.go` — add Port/PID/RAM/Uptime columns + `--json` flag
- `internal/cli/restart.go` — add `--all` flag
- `internal/cli/root.go` — wire the 5 new commands

---

## Task 1: `api.Status` enrichment — Port/PID/RAM/UptimeSec

**Files:**

- Create: `internal/api/status_enrich.go`
- Create: `internal/api/status_enrich_test.go`
- Modify: `internal/api/install.go` (find the existing `(*API).Status` and call enrichStatus at the end)

- [ ] **Step 1: Read current `Status` implementation**

Open `internal/api/install.go`. Find the `Status() ([]DaemonStatus, error)` method (if not present, it's defined via `scheduler.List`). If no public `Status()` method exists yet at the api layer, add one that wraps `internal/scheduler.Scheduler.List("mcp-local-hub-")`.

If a `Status` method already exists on `*API`, note its location.

- [ ] **Step 2: Write the failing test**

Create `internal/api/status_enrich_test.go`:

```go
package api

import (
	"testing"
)

// TestEnrichStatusFillsPortFromManifest verifies enrichStatus maps the
// task-name suffix to the manifest's daemon.port field (no process poll).
func TestEnrichStatusFillsPortFromManifest(t *testing.T) {
	tmp := t.TempDir()
	makeFakeManifest(t, tmp+"/memory", "memory", 9123)
	makeFakeManifest(t, tmp+"/serena", "serena", 9121)

	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-memory-default`},
		{TaskName: `\mcp-local-hub-serena-claude`},
		{TaskName: `\mcp-local-hub-godbolt-default`}, // no manifest in tmp — port stays 0
	}

	enrichStatus(rows, tmp)

	if rows[0].Port != 9123 {
		t.Errorf("memory.Port: got %d, want 9123", rows[0].Port)
	}
	if rows[1].Port != 9121 {
		t.Errorf("serena.Port: got %d, want 9121 (first daemon in manifest)", rows[1].Port)
	}
	if rows[2].Port != 0 {
		t.Errorf("godbolt.Port: got %d, want 0 (no manifest)", rows[2].Port)
	}
}
```

`makeFakeManifest` already exists in `install_test.go` (Phase 3A.1 Task 7). Reuse.

- [ ] **Step 3: Verify failure**

```bash
cd <repo> && go test ./internal/api/ -run TestEnrichStatusFillsPort -v
```

Expected: FAIL with `enrichStatus undefined`.

- [ ] **Step 4: Implement `enrichStatus` in `internal/api/status_enrich.go`**

```go
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/config"
)

// enrichStatus walks the scheduler.Status rows and adds Port (from manifest),
// Server, Daemon (parsed from TaskName), and — when the task is Running —
// PID/RAMBytes/UptimeSec from a live wmic query. manifestDir points at the
// repo's servers/ directory; passed explicitly so tests can use t.TempDir().
func enrichStatus(rows []DaemonStatus, manifestDir string) {
	// Build server→port map from manifests.
	ports := manifestPortMap(manifestDir)

	for i := range rows {
		srv, dmn := parseTaskName(rows[i].TaskName)
		rows[i].Server = srv
		rows[i].Daemon = dmn
		if p, ok := ports[srv][dmn]; ok {
			rows[i].Port = p
		} else if p, ok := ports[srv]["default"]; ok {
			// Fallback: single-daemon manifests whose task name doesn't encode "default".
			rows[i].Port = p
		}
		if rows[i].State == "Running" {
			if pid, ram, uptime, ok := lookupProcess(rows[i].Port); ok {
				rows[i].PID = pid
				rows[i].RAMBytes = ram
				rows[i].UptimeSec = uptime
			}
		}
	}
}

// parseTaskName splits `\mcp-local-hub-<server>-<daemon>` into (server, daemon).
// Returns ("", "") on unparseable names.
func parseTaskName(task string) (string, string) {
	name := strings.TrimPrefix(task, "\\")
	const prefix = "mcp-local-hub-"
	if !strings.HasPrefix(name, prefix) {
		return "", ""
	}
	rest := name[len(prefix):]
	// The last segment is the daemon; the rest is the server (which may contain '-').
	idx := strings.LastIndex(rest, "-")
	if idx < 0 {
		return rest, ""
	}
	return rest[:idx], rest[idx+1:]
}

// manifestPortMap reads all servers/*/manifest.yaml and returns a map
// server → daemon → port. Missing dir = empty map (not an error).
func manifestPortMap(manifestDir string) map[string]map[string]int {
	out := map[string]map[string]int{}
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(manifestDir, e.Name(), "manifest.yaml"))
		if err != nil {
			continue
		}
		m, err := config.ParseManifest(f)
		f.Close()
		if err != nil {
			continue
		}
		inner := map[string]int{}
		for _, d := range m.Daemons {
			inner[d.Name] = d.Port
		}
		out[m.Name] = inner
	}
	return out
}

// lookupProcess queries wmic/netstat for the process bound to 127.0.0.1:port.
// Returns (pid, ram_bytes, uptime_sec, true) on success. Body is defined in
// Task 3 (processes.go); this file declares the signature via a package-level
// function variable so tests can stub it.
var lookupProcess = lookupProcessReal

// lookupProcessReal is filled in by processes.go. Until Task 3 lands, it
// returns (0, 0, 0, false) so Task 1 can build and test without the live
// netstat call.
func lookupProcessReal(port int) (int, uint64, int64, bool) {
	_ = fmt.Sprintf("%d", port)  // silences unused-import if fmt is reintroduced
	_ = time.Second              // same for time
	return 0, 0, 0, false
}
```

Two tiny silencing lines at the bottom let the file compile with `fmt` and `time` imports that Task 3 will use — so Task 3's diff is purely additive. Remove them in Task 3.

- [ ] **Step 5: Run the test**

```bash
cd <repo> && go test ./internal/api/ -run TestEnrichStatusFills -v
```

Expected: PASS.

- [ ] **Step 6: Run full test suite to ensure no regressions**

```bash
cd <repo> && go test ./... 
```

Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add internal/api/status_enrich.go internal/api/status_enrich_test.go
git commit -m "feat(api): enrichStatus — parse task name, map daemon.port from manifest"
```

---

## Task 2: `api.CountProcesses` + `lookupProcess` — live process introspection

**Files:**

- Create: `internal/api/processes.go`
- Create: `internal/api/processes_test.go`
- Modify: `internal/api/status_enrich.go` (remove silencing stubs — Task 1's trailing hack)

- [ ] **Step 1: Write the failing test for CountProcesses**

Create `internal/api/processes_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

// TestCountProcessesHandlesEmptyInput verifies the parser returns (0, nil)
// on blank wmic output — zero processes matching, no error.
func TestCountProcessesHandlesEmptyInput(t *testing.T) {
	got, err := parseWmicCount(strings.NewReader(""), []string{"memory", "server-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// TestCountProcessesMatchesSubstrings verifies a line containing any of the
// patterns counts once; a line containing multiple patterns still counts once.
func TestCountProcessesMatchesSubstrings(t *testing.T) {
	wmicCsv := `Node,CommandLine,ProcessId,WorkingSetSize
HOST,"npx -y @modelcontextprotocol/server-memory",1234,41000000
HOST,"node server-memory/dist/index.js",5678,40000000
HOST,"some-other-process",9999,1000000
`
	got, err := parseWmicCount(strings.NewReader(wmicCsv), []string{"server-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (both lines mention server-memory)", got)
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd <repo> && go test ./internal/api/ -run TestCountProcesses -v
```

Expected: FAIL (`parseWmicCount` undefined).

- [ ] **Step 3: Implement `internal/api/processes.go`**

```go
package api

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CountProcesses returns how many OS processes currently match the given
// command-line substring patterns. Typical usage: feed it the server name
// and the primary command/arg tokens from its manifest.
//
// Windows-only. On Linux/macOS it returns (0, nil) until a cross-platform
// implementation lands in Phase 3B or later.
func (a *API) CountProcesses(patterns []string) (int, error) {
	cmd := exec.Command("wmic", "process", "get", "CommandLine,ProcessId,WorkingSetSize", "/format:csv")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("wmic: %w", err)
	}
	return parseWmicCount(strings.NewReader(string(out)), patterns)
}

// parseWmicCount scans the CSV `wmic process get` output and returns the
// number of lines whose CommandLine field contains at least one of the given
// substring patterns.
func parseWmicCount(r io.Reader, patterns []string) (int, error) {
	count := 0
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		for _, p := range patterns {
			if strings.Contains(line, p) {
				count++
				break
			}
		}
	}
	return count, s.Err()
}

// ListMatchingProcesses returns (PID, commandLine) pairs for every process
// whose CommandLine matches any of the given substring patterns.
type ProcessInfo struct {
	PID      int
	RAMBytes uint64
	Cmdline  string
}

func (a *API) ListMatchingProcesses(patterns []string) ([]ProcessInfo, error) {
	cmd := exec.Command("wmic", "process", "get", "CommandLine,ProcessId,WorkingSetSize", "/format:csv")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("wmic: %w", err)
	}
	var results []ProcessInfo
	s := bufio.NewScanner(strings.NewReader(string(out)))
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		matched := false
		for _, p := range patterns {
			if strings.Contains(line, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Parse CSV: Node,CommandLine,ProcessId,WorkingSetSize
		fields := splitCSVLine(line)
		if len(fields) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(fields[len(fields)-2]))
		ram, _ := strconv.ParseUint(strings.TrimSpace(fields[len(fields)-1]), 10, 64)
		cmdline := fields[1]
		results = append(results, ProcessInfo{PID: pid, RAMBytes: ram, Cmdline: cmdline})
	}
	return results, nil
}

// splitCSVLine splits a simple comma-separated wmic line. Quoted fields with
// embedded commas are preserved. We keep the logic minimal since wmic output
// doesn't escape quotes inside quoted fields.
func splitCSVLine(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' {
			inQuote = !inQuote
			continue
		}
		if c == ',' && !inQuote {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

// lookupProcessReal (called from status_enrich.go) queries wmic + netstat to
// find the process bound to 127.0.0.1:port. Returns pid/ram/uptime on hit.
func init() {
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		if port <= 0 {
			return 0, 0, 0, false
		}
		// Find PID via netstat.
		out, err := exec.Command("netstat", "-ano").Output()
		if err != nil {
			return 0, 0, 0, false
		}
		var pid int
		portMarker := fmt.Sprintf("127.0.0.1:%d", port)
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, portMarker) || !strings.Contains(line, "LISTENING") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) > 0 {
				pid, _ = strconv.Atoi(fields[len(fields)-1])
				break
			}
		}
		if pid == 0 {
			return 0, 0, 0, false
		}
		// Fetch RAM + start time via wmic.
		out2, err := exec.Command("wmic", "process", "where", fmt.Sprintf("ProcessId=%d", pid),
			"get", "WorkingSetSize,CreationDate", "/format:csv").Output()
		if err != nil {
			return pid, 0, 0, true
		}
		var ram uint64
		var created time.Time
		for _, line := range strings.Split(string(out2), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "Node,") {
				continue
			}
			fields := splitCSVLine(line)
			if len(fields) >= 3 {
				created = parseWmicDate(strings.TrimSpace(fields[1]))
				ram, _ = strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 64)
			}
		}
		var uptime int64
		if !created.IsZero() {
			uptime = int64(time.Since(created).Seconds())
		}
		return pid, ram, uptime, true
	}
}

// parseWmicDate parses wmic's CIM_DATETIME format: YYYYMMDDHHMMSS.mmmmmm+ZZZ.
func parseWmicDate(s string) time.Time {
	if len(s) < 14 {
		return time.Time{}
	}
	t, err := time.Parse("20060102150405", s[:14])
	if err != nil {
		return time.Time{}
	}
	return t
}
```

- [ ] **Step 4: Clean up status_enrich.go silencing lines**

In `internal/api/status_enrich.go`, remove the entire `lookupProcessReal` function (the stub defined in Task 1):

```go
// Delete this from status_enrich.go — now defined in processes.go's init()
func lookupProcessReal(port int) (int, uint64, int64, bool) {
	_ = fmt.Sprintf("%d", port)
	_ = time.Second
	return 0, 0, 0, false
}
```

Also remove the now-unused `fmt` and `time` imports from status_enrich.go. The `var lookupProcess = lookupProcessReal` line should also be replaced with just `var lookupProcess func(port int) (int, uint64, int64, bool)` since processes.go's init() sets it.

Final top of status_enrich.go:

```go
package api

import (
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/config"
)

// lookupProcess queries netstat + wmic for the process bound to 127.0.0.1:port.
// Populated by internal/api/processes.go's init() on Windows; stays nil
// elsewhere so cross-platform callers fall through the (ok==false) branch.
var lookupProcess func(port int) (pid int, ramBytes uint64, uptimeSec int64, ok bool)
```

And where `enrichStatus` calls `lookupProcess`, guard against nil:

```go
if rows[i].State == "Running" && lookupProcess != nil {
    if pid, ram, uptime, ok := lookupProcess(rows[i].Port); ok {
        rows[i].PID = pid
        rows[i].RAMBytes = ram
        rows[i].UptimeSec = uptime
    }
}
```

- [ ] **Step 5: Run the tests**

```bash
cd <repo> && go test ./internal/api/ -v
```

Expected: all tests PASS (3 new `CountProcesses`/`parseWmicCount` tests plus existing ones).

- [ ] **Step 6: Commit**

```bash
git add internal/api/processes.go internal/api/processes_test.go internal/api/status_enrich.go
git commit -m "feat(api): CountProcesses + lookupProcess via wmic/netstat"
```

---

## Task 3: Wire `enrichStatus` into the api Status method + `mcp status` enrichment

**Files:**

- Modify: `internal/api/install.go` — find existing `Status()` method, call enrichStatus at the end
- Modify: `internal/cli/status.go` — add Port/PID/RAM/Uptime columns + `--json`

- [ ] **Step 1: Locate current `Status` method in api/install.go**

Search the file; it likely looks like:

```go
func (a *API) Status() ([]DaemonStatus, error) {
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	rows, err := sch.List("mcp-local-hub-")
	// ... converts []scheduler.TaskStatus to []DaemonStatus ...
	return result, nil
}
```

If this method does NOT exist (Phase 3A.1 didn't add it), create it following the pattern `internal/cli/status.go` currently uses (inline scheduler.New + List).

- [ ] **Step 2: Call enrichStatus at the end of the Status method**

After the conversion loop that turns `[]scheduler.TaskStatus` into `[]DaemonStatus`, add:

```go
enrichStatus(result, defaultManifestDir())
return result, nil
```

- [ ] **Step 3: Write test for Status-via-api**

Append to `internal/api/status_enrich_test.go`:

```go
// TestAPIStatusEnrichesPorts verifies (*API).Status() returns DaemonStatus
// rows with Port populated. Uses defaultManifestDir() in production; the
// test runs against the real servers/ directory so it needs the gdb/memory
// manifests present (ensured by prior commits).
func TestAPIStatusEnrichesPorts(t *testing.T) {
	a := NewAPI()
	rows, err := a.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// Expect at least one row with a non-zero port. If the host has no
	// scheduler tasks installed yet, rows may be empty — that's still OK;
	// the test doesn't require tasks to exist, just that enrichment ran.
	for _, r := range rows {
		if r.Server == "" && r.TaskName != "" {
			t.Errorf("enrichStatus did not parse TaskName %q", r.TaskName)
		}
	}
}
```

- [ ] **Step 4: Modify `internal/cli/status.go` to show new columns + --json**

Open `internal/cli/status.go`. It currently prints rows with `Name/State/LastResult/NextRun`. Replace the output block:

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show state of all mcp-local-hub scheduler tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			rows, err := a.Status()
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(rows)
			}
			// Plain-text table with new columns.
			fmt.Printf("%-45s %-10s %-6s %-8s %-8s %-10s\n",
				"NAME", "STATE", "PORT", "PID", "RAM(MB)", "UPTIME")
			for _, r := range rows {
				ram := ""
				if r.RAMBytes > 0 {
					ram = fmt.Sprintf("%d", r.RAMBytes/(1024*1024))
				}
				uptime := ""
				if r.UptimeSec > 0 {
					uptime = fmt.Sprintf("%dh%dm", r.UptimeSec/3600, (r.UptimeSec%3600)/60)
				}
				port := ""
				if r.Port > 0 {
					port = fmt.Sprintf("%d", r.Port)
				}
				pid := ""
				if r.PID > 0 {
					pid = fmt.Sprintf("%d", r.PID)
				}
				fmt.Printf("%-45s %-10s %-6s %-8s %-8s %-10s\n",
					r.TaskName, r.State, port, pid, ram, uptime)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}
```

- [ ] **Step 5: Run tests + smoke**

```bash
cd <repo> && go test ./... && ./build.sh && ./mcp.exe status
```

Expected: all tests pass; status output now shows Port/PID/RAM/Uptime columns for running daemons; empty columns for Ready/Failed rows.

- [ ] **Step 6: Verify `--json` output**

```bash
./mcp.exe status --json | python -c "import json,sys; d=json.load(sys.stdin); print(d[0])"
```

Expected: JSON of first DaemonStatus row with `server`, `state`, `port`, etc.

- [ ] **Step 7: Commit**

```bash
git add internal/api/install.go internal/api/status_enrich_test.go internal/cli/status.go
git commit -m "feat(status): Port/PID/RAM/Uptime columns + --json flag"
```

---

## Task 4: `mcp scan` CLI + enriched Scan with process counts

**Files:**

- Modify: `internal/api/scan.go` — add optional process-count enrichment to ScanResult
- Modify: `internal/api/types.go` — add `ProcessCount` field to `ScanEntry`
- Create: `internal/cli/scan.go`

- [ ] **Step 1: Add `ProcessCount` field to ScanEntry**

In `internal/api/types.go`, find `type ScanEntry struct` and add field:

```go
type ScanEntry struct {
	Name           string                 `json:"name"`
	Status         string                 `json:"status"`
	ClientPresence map[string]ClientEntry `json:"client_presence"`
	ManifestExists bool                   `json:"manifest_exists"`
	CanMigrate     bool                   `json:"can_migrate"`
	ProcessCount   int                    `json:"process_count,omitempty"` // populated when ScanOpts.WithProcessCount is true
}
```

Also add to `ScanOpts`:

```go
type ScanOpts struct {
	ClaudeConfigPath      string
	CodexConfigPath       string
	GeminiConfigPath      string
	AntigravityConfigPath string
	ManifestDir           string
	WithProcessCount      bool // NEW: populate ScanEntry.ProcessCount via wmic
}
```

- [ ] **Step 2: Wire process-count into ScanFrom**

In `internal/api/scan.go`, at the end of `ScanFrom` just before the final `return out, nil`, add:

```go
	if opts.WithProcessCount {
		for i := range out.Entries {
			patterns := patternsForServer(out.Entries[i].Name, opts.ManifestDir)
			if len(patterns) == 0 {
				continue
			}
			count, err := a.CountProcesses(patterns)
			if err == nil {
				out.Entries[i].ProcessCount = count
			}
		}
	}
```

Add helper to derive patterns from the server name + manifest command:

```go
// patternsForServer returns the substring patterns used to identify running
// processes of this server. For manifested servers, it uses command + the
// last arg (typically a recognizable script or package name). For
// non-manifested (unknown/per-session) servers, it falls back to the server
// name only — callers should treat counts for unknown servers as an upper
// bound.
func patternsForServer(serverName, manifestDir string) []string {
	f, err := os.Open(filepath.Join(manifestDir, serverName, "manifest.yaml"))
	if err != nil {
		// Fallback for unknown / per-session: just the server name.
		return []string{serverName}
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
	if err != nil {
		return []string{serverName}
	}
	var out []string
	if m.Command != "" {
		out = append(out, m.Command)
	}
	for _, a := range m.BaseArgs {
		if len(a) > 3 && !strings.HasPrefix(a, "-") {
			out = append(out, a) // the package name / script path
		}
	}
	if len(out) == 0 {
		out = append(out, serverName)
	}
	return out
}
```

Add `"mcp-local-hub/internal/config"` to imports of scan.go if not present.

- [ ] **Step 3: Test process-count enrichment**

Append to `internal/api/scan_test.go`:

```go
// TestScanWithProcessCountPopulates verifies ScanFrom populates ProcessCount
// when WithProcessCount is true. We don't assert an exact number (test runs
// on real host), just that the field is either zero or positive and that the
// call doesn't error.
func TestScanWithProcessCountPopulates(t *testing.T) {
	tmp := t.TempDir()
	// Minimal Claude config with memory
	_ = os.WriteFile(filepath.Join(tmp, ".claude.json"),
		[]byte(`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9123/mcp"}}}`), 0600)
	// Manifest for memory
	_ = os.MkdirAll(filepath.Join(tmp, "servers", "memory"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "servers", "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\nbase_args:\n  - \"-y\"\n  - \"@modelcontextprotocol/server-memory\"\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ClaudeConfigPath: filepath.Join(tmp, ".claude.json"),
		ManifestDir:      filepath.Join(tmp, "servers"),
		WithProcessCount: true,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	found := false
	for _, e := range result.Entries {
		if e.Name == "memory" {
			found = true
			if e.ProcessCount < 0 {
				t.Errorf("ProcessCount must be non-negative, got %d", e.ProcessCount)
			}
		}
	}
	if !found {
		t.Error("memory entry missing from scan result")
	}
}
```

- [ ] **Step 4: Create `internal/cli/scan.go`**

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newScanCmd() *cobra.Command {
	var jsonOut, withProcs bool
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan client configs: which MCP servers are hub-routed, can be migrated, or orphaned",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			result, err := a.ScanFrom(api.ScanOpts{
				ClaudeConfigPath:      home + "/.claude.json",
				CodexConfigPath:       home + "/.codex/config.toml",
				GeminiConfigPath:      home + "/.gemini/settings.json",
				AntigravityConfigPath: home + "/.gemini/antigravity/mcp_config.json",
				ManifestDir:           defaultManifestDir(),
				WithProcessCount:      withProcs,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			// Human-readable grouping by status.
			groups := map[string][]api.ScanEntry{}
			for _, e := range result.Entries {
				groups[e.Status] = append(groups[e.Status], e)
			}
			for _, status := range []string{"via-hub", "can-migrate", "unknown", "per-session", "not-installed"} {
				items := groups[status]
				if len(items) == 0 {
					continue
				}
				fmt.Printf("\n%s (%d):\n", status, len(items))
				for _, e := range items {
					procs := ""
					if withProcs && e.ProcessCount > 0 {
						procs = fmt.Sprintf("  · %d process(es)", e.ProcessCount)
					}
					fmt.Printf("  %-25s %s%s\n", e.Name, presenceSummary(e), procs)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	c.Flags().BoolVar(&withProcs, "processes", false, "also count live processes matching each server (slower; uses wmic)")
	return c
}

func presenceSummary(e api.ScanEntry) string {
	var parts []string
	for _, c := range []string{"claude-code", "codex-cli", "gemini-cli", "antigravity"} {
		if p, ok := e.ClientPresence[c]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", shortClient(c), p.Transport))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

func shortClient(c string) string {
	switch c {
	case "claude-code":
		return "cc"
	case "codex-cli":
		return "cx"
	case "gemini-cli":
		return "gm"
	case "antigravity":
		return "ag"
	}
	return c
}

// defaultManifestDir returns the path to `servers/` next to the running binary.
// Duplicated from api.defaultManifestDir (which is private); acceptable
// duplication until a `cli.dirs` helper package emerges.
func defaultManifestDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "servers"
	}
	return exe[:len(exe)-len("mcp.exe")] + "servers"
}
```

- [ ] **Step 5: Wire `newScanCmd` into root**

Open `internal/cli/root.go`. Find the command registration block (likely `rootCmd.AddCommand(...)`) and add `newScanCmd()` to the list.

- [ ] **Step 6: Rebuild and smoke**

```bash
cd <repo> && go test ./... && ./build.sh
./mcp.exe scan
./mcp.exe scan --processes
./mcp.exe scan --json | head -c 500
```

Expected: grouped output showing via-hub/can-migrate/unknown/per-session entries; `--processes` adds ` · N process(es)` suffix.

- [ ] **Step 7: Commit**

```bash
git add internal/api/scan.go internal/api/scan_test.go internal/api/types.go internal/cli/scan.go internal/cli/root.go
git commit -m "feat(scan): mcp scan CLI + ProcessCount field via WithProcessCount opt"
```

---

## Task 5: `mcp migrate` CLI

**Files:**

- Create: `internal/cli/migrate.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Create `internal/cli/migrate.go`**

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var clientsFlag string
	var dryRun, jsonOut bool
	c := &cobra.Command{
		Use:   "migrate <server>...",
		Short: "Switch stdio client entries to hub HTTP for the specified servers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var include []string
			if clientsFlag != "" {
				include = strings.Split(clientsFlag, ",")
			}
			a := api.NewAPI()
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			report, err := a.MigrateFrom(api.MigrateOpts{
				Servers:        args,
				ClientsInclude: include,
				DryRun:         dryRun,
				ScanOpts: api.ScanOpts{
					ClaudeConfigPath:      home + "/.claude.json",
					CodexConfigPath:       home + "/.codex/config.toml",
					GeminiConfigPath:      home + "/.gemini/settings.json",
					AntigravityConfigPath: home + "/.gemini/antigravity/mcp_config.json",
					ManifestDir:           defaultManifestDir(),
				},
			})
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(report)
			}
			for _, a := range report.Applied {
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s/%s → %s\n", a.Server, a.Client, a.URL)
			}
			for _, f := range report.Failed {
				fmt.Fprintf(cmd.OutOrStderr(), "✗ %s/%s: %s\n", f.Server, f.Client, f.Err)
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "\n(dry-run — no files modified)")
			}
			return nil
		},
	}
	c.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated subset of clients (default: all four)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change, don't write")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}
```

- [ ] **Step 2: Wire into root.go**

Add `newMigrateCmd()` to the `rootCmd.AddCommand(...)` list.

- [ ] **Step 3: Smoke test**

```bash
cd <repo> && ./build.sh
./mcp.exe migrate memory --dry-run
```

Expected: prints `✓ memory/<client> → http://localhost:9123/mcp` for each client already hub-routed, plus `(dry-run — no files modified)` footer.

- [ ] **Step 4: Commit**

```bash
git add internal/cli/migrate.go internal/cli/root.go
git commit -m "feat(migrate): mcp migrate CLI (wraps api.MigrateFrom)"
```

---

## Task 6: `api.LogsGet` + `mcp logs` CLI

**Files:**

- Create: `internal/api/logs.go`
- Create: `internal/api/logs_test.go`
- Create: `internal/cli/logs.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write the failing test**

Create `internal/api/logs_test.go`:

```go
package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsGetTailsFromFile(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "memory-default.log")
	body := ""
	for i := 0; i < 100; i++ {
		body += "line" + string(rune('0'+i%10)) + "\n"
	}
	if err := os.WriteFile(logPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	content, err := a.LogsGetFrom(LogsOpts{
		LogDir: tmp,
		Server: "memory",
		Daemon: "default",
		Tail:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 tailed lines, got %d", len(lines))
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd <repo> && go test ./internal/api/ -run TestLogsGet -v
```

Expected: FAIL.

- [ ] **Step 3: Implement `internal/api/logs.go`**

```go
package api

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LogsOpts controls a LogsGet call.
type LogsOpts struct {
	LogDir string // e.g., %LOCALAPPDATA%\mcp-local-hub\logs
	Server string
	Daemon string // "default" for single-daemon manifests; daemon name otherwise
	Tail   int    // 0 = all lines
}

// LogsGetFrom reads the log file for (server, daemon) and returns the last
// Tail lines. Exposed (rather than LogsGet) so tests can pass a custom dir.
func (a *API) LogsGetFrom(opts LogsOpts) (string, error) {
	path := filepath.Join(opts.LogDir, fmt.Sprintf("%s-%s.log", opts.Server, opts.Daemon))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if opts.Tail <= 0 {
		return string(data), nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= opts.Tail {
		return string(data), nil
	}
	tail := lines[len(lines)-opts.Tail:]
	return strings.Join(tail, "\n") + "\n", nil
}

// LogsGet is the production entry point using the OS-default log dir.
func (a *API) LogsGet(server, daemon string, tail int) (string, error) {
	return a.LogsGetFrom(LogsOpts{
		LogDir: defaultLogDir(),
		Server: server,
		Daemon: daemon,
		Tail:   tail,
	})
}

// LogsStream opens the log file and emits each new line appended after the
// call begins. Caller cancels via context. Uses a simple poll loop with 200ms
// ticks; good enough for interactive tail usage.
func (a *API) LogsStream(server, daemon string, out *bufio.Writer) error {
	// Scaffolded; full implementation is follow-on (Task 6b if needed).
	return fmt.Errorf("LogsStream not yet implemented — use LogsGet with Tail")
}

func defaultLogDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "logs")
}
```

LogsStream is deliberately stubbed for 3A.2. `--follow` flag in CLI maps to this and returns a clean error until 3A.3 wires full streaming. If follow is needed sooner, a subsequent PR can implement it inside this plan without re-writing.

- [ ] **Step 4: Create `internal/cli/logs.go`**

```go
package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var tail int
	var daemon string
	var follow bool
	c := &cobra.Command{
		Use:   "logs <server>",
		Short: "Print (and optionally follow) daemon logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			if follow {
				return fmt.Errorf("--follow not yet implemented in Phase 3A.2")
			}
			content, err := a.LogsGet(args[0], daemon, tail)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), content)
			return nil
		},
	}
	c.Flags().IntVar(&tail, "tail", 100, "number of trailing lines (0 = all)")
	c.Flags().StringVar(&daemon, "daemon", "default", "daemon name within server manifest")
	c.Flags().BoolVar(&follow, "follow", false, "stream new log lines (not yet implemented)")
	return c
}
```

- [ ] **Step 5: Wire into root + test**

Add `newLogsCmd()` to `rootCmd.AddCommand(...)` in root.go.

```bash
cd <repo> && go test ./... && ./build.sh
./mcp.exe logs memory --tail 5
```

Expected: last 5 lines of memory daemon's log file. If the file doesn't exist (host.go in Phase 2 doesn't tee stderr, so daemons may have no log file), shows `open ...: The system cannot find the file specified.` — that's expected until host.go logging is added in Phase 3B.

- [ ] **Step 6: Commit**

```bash
git add internal/api/logs.go internal/api/logs_test.go internal/cli/logs.go internal/cli/root.go
git commit -m "feat(logs): api.LogsGet + mcp logs CLI (tail mode, --follow stub)"
```

---

## Task 7: `api.CleanupOrphans` — detect and kill orphan MCP subprocesses

**Files:**

- Create: `internal/api/cleanup.go`
- Create: `internal/api/cleanup_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/api/cleanup_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

// TestParseOrphanDetectionIgnoresOurDaemons verifies that a wmic line whose
// CommandLine references our own daemon invocation (`mcp.exe daemon`) is
// NOT counted as an orphan even if it also matches a server's pattern.
func TestParseOrphanDetectionIgnoresOurDaemons(t *testing.T) {
	wmicCsv := `Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize
HOST,"uv run --directory .../GDB-MCP python server.py",20260417180000.000000+180,555,1001,40000000
HOST,"<repo>\mcp.exe daemon --server gdb --daemon default",20260417180000.000000+180,999,555,15000000
HOST,"uv run --directory .../GDB-MCP python server.py",20260417170000.000000+180,1,2002,42000000
`
	orphans := parseOrphans(strings.NewReader(wmicCsv), []string{"GDB-MCP"})
	// PID 1001 has parent 555 which is mcp.exe daemon — NOT orphan.
	// PID 2002 has parent 1 — ORPHAN.
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if orphans[0].PID != 2002 {
		t.Errorf("orphan PID: got %d, want 2002", orphans[0].PID)
	}
}
```

- [ ] **Step 2: Verify failure**

```bash
cd <repo> && go test ./internal/api/ -run TestParseOrphan -v
```

Expected: FAIL.

- [ ] **Step 3: Implement `internal/api/cleanup.go`**

```go
package api

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// OrphanProcess describes one orphan MCP subprocess discovered by CleanupOrphans.
type OrphanProcess struct {
	PID      int
	ParentID int
	Server   string // inferred from matching manifest
	RAMBytes uint64
	Cmdline  string
	AgeSec   int64
}

// CleanupOpts controls CleanupOrphans.
type CleanupOpts struct {
	ManifestDir string
	MinAgeSec   int64 // don't kill processes younger than this (default 60)
	DryRun      bool  // if true, just report
	Server      string // empty = all servers; otherwise only that one
}

// CleanupOrphans finds MCP server processes that match a manifest's command
// pattern but whose parent is NOT our `mcp.exe daemon` wrapper. Reports them
// (dry-run) or kills them (non-dry-run).
func (a *API) CleanupOrphans(opts CleanupOpts) ([]OrphanProcess, error) {
	if opts.MinAgeSec == 0 {
		opts.MinAgeSec = 60
	}
	// Collect patterns per manifest.
	patterns := map[string][]string{}
	if opts.Server != "" {
		patterns[opts.Server] = patternsForServer(opts.Server, opts.ManifestDir)
	} else {
		entries, err := readManifestNames(opts.ManifestDir)
		if err != nil {
			return nil, err
		}
		for name := range entries {
			patterns[name] = patternsForServer(name, opts.ManifestDir)
		}
	}

	// One wmic call, then filter.
	out, err := exec.Command("wmic", "process", "get",
		"CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize",
		"/format:csv").Output()
	if err != nil {
		return nil, fmt.Errorf("wmic: %w", err)
	}

	// Flat list of patterns — any match counts this PID as a candidate orphan.
	var allPatterns []string
	for _, ps := range patterns {
		allPatterns = append(allPatterns, ps...)
	}
	orphans := parseOrphans(strings.NewReader(string(out)), allPatterns)

	// Age filter.
	now := time.Now()
	filtered := orphans[:0]
	for _, o := range orphans {
		if o.AgeSec >= opts.MinAgeSec {
			// Assign server (first manifest whose pattern matched).
			for name, ps := range patterns {
				for _, p := range ps {
					if strings.Contains(o.Cmdline, p) {
						o.Server = name
						break
					}
				}
				if o.Server != "" {
					break
				}
			}
			filtered = append(filtered, o)
		}
	}
	_ = now

	// Kill if not dry-run.
	if !opts.DryRun {
		for _, o := range filtered {
			_ = exec.Command("taskkill", "/PID", strconv.Itoa(o.PID), "/F").Run()
		}
	}

	return filtered, nil
}

// parseOrphans reads `wmic process get CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize`
// CSV output and returns processes whose CommandLine matches any of the given
// patterns BUT whose parent is NOT an `mcp.exe daemon` process.
//
// Visible for unit tests so fixture CSVs can drive the logic without wmic.
func parseOrphans(r io.Reader, patterns []string) []OrphanProcess {
	// First pass: build a PID → cmdline map so we can resolve parent PIDs.
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	type row struct {
		pid, ppid int
		created   time.Time
		cmdline   string
		ram       uint64
	}
	var rows []row
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "Node,") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := splitCSVLine(line)
		if len(fields) < 6 {
			continue
		}
		cmdline := fields[1]
		created := parseWmicDate(strings.TrimSpace(fields[2]))
		ppid, _ := strconv.Atoi(strings.TrimSpace(fields[3]))
		pid, _ := strconv.Atoi(strings.TrimSpace(fields[4]))
		ram, _ := strconv.ParseUint(strings.TrimSpace(fields[5]), 10, 64)
		rows = append(rows, row{pid: pid, ppid: ppid, created: created, cmdline: cmdline, ram: ram})
	}

	// Index by PID.
	byPID := map[int]row{}
	for _, r := range rows {
		byPID[r.pid] = r
	}

	var out []OrphanProcess
	for _, r := range rows {
		// Match against patterns.
		matched := false
		for _, p := range patterns {
			if strings.Contains(r.cmdline, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Is parent an mcp.exe daemon process?
		if parent, ok := byPID[r.ppid]; ok {
			if strings.Contains(parent.cmdline, "mcp.exe") && strings.Contains(parent.cmdline, "daemon") {
				continue // NOT orphan — child of our daemon
			}
		}
		// It's an orphan candidate.
		age := int64(0)
		if !r.created.IsZero() {
			age = int64(time.Since(r.created).Seconds())
		}
		out = append(out, OrphanProcess{
			PID:      r.pid,
			ParentID: r.ppid,
			RAMBytes: r.ram,
			Cmdline:  r.cmdline,
			AgeSec:   age,
		})
	}
	return out
}
```

- [ ] **Step 4: Run the test**

```bash
cd <repo> && go test ./internal/api/ -run TestParseOrphan -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/cleanup.go internal/api/cleanup_test.go
git commit -m "feat(api): CleanupOrphans — detect MCP subprocesses not owned by mcp.exe daemon"
```

---

## Task 8: `mcp cleanup` CLI

**Files:**

- Create: `internal/cli/cleanup.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Create `internal/cli/cleanup.go`**

```go
package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newCleanupCmd() *cobra.Command {
	var dryRun bool
	var server string
	var minAge int64
	c := &cobra.Command{
		Use:   "cleanup",
		Short: "Find and kill orphan MCP server processes (dry-run by default)",
		Long: `Finds MCP-server processes whose command line matches a manifest's command
but whose parent is NOT our 'mcp.exe daemon' wrapper. These are typically
leftover from dead client sessions (IDE restarts, CTRL-C not propagating
to children, etc.).

Default is --dry-run (reports only). Pass --confirm to actually kill.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			opts := api.CleanupOpts{
				ManifestDir: defaultManifestDir(),
				Server:      server,
				DryRun:      dryRun,
				MinAgeSec:   minAge,
			}
			orphans, err := a.CleanupOrphans(opts)
			if err != nil {
				return err
			}
			if len(orphans) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No orphan processes found.")
				return nil
			}
			totalRAM := uint64(0)
			for _, o := range orphans {
				totalRAM += o.RAMBytes
				fmt.Fprintf(cmd.OutOrStdout(), "  PID %-6d  server=%-18s  RAM=%-6.0f MB  age=%-6ds  %s\n",
					o.PID, o.Server, float64(o.RAMBytes)/(1024*1024), o.AgeSec,
					truncate(o.Cmdline, 80))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n%d orphans · %.0f MB total\n",
				len(orphans), float64(totalRAM)/(1024*1024))
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "(dry-run — no processes killed. Re-run with --confirm to kill.)")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "(killed via taskkill /F)")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", true, "report only, do not kill (default)")
	c.Flags().BoolVar(&dryRun, "confirm", false, "(inverse of dry-run) actually kill the orphans")
	c.Flags().StringVar(&server, "server", "", "limit scan to this server's pattern (default: all manifests)")
	c.Flags().Int64Var(&minAge, "min-age-sec", 60, "ignore processes younger than this (seconds)")
	return c
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
```

Note the dual-flag trick: `--dry-run` is the default true flag, `--confirm` inverts it. Cobra will set dryRun=false when `--confirm` is passed. Both flags bind to the same var.

Actually cobra does NOT support two flags binding to the same var cleanly; fix by using separate vars:

```go
// Replace the dual-binding with:
var dryRun, confirm bool
c.Flags().BoolVar(&dryRun, "dry-run", true, "report only, do not kill (default)")
c.Flags().BoolVar(&confirm, "confirm", false, "actually kill the orphans (overrides --dry-run)")

// Inside RunE, before calling CleanupOrphans:
if confirm {
    dryRun = false
}
opts := api.CleanupOpts{
    ManifestDir: defaultManifestDir(),
    Server:      server,
    DryRun:      dryRun,
    MinAgeSec:   minAge,
}
```

Replace the Flags block in the code above accordingly.

- [ ] **Step 2: Wire into root**

Add `newCleanupCmd()` to `rootCmd.AddCommand(...)` in root.go.

- [ ] **Step 3: Smoke test**

```bash
cd <repo> && ./build.sh
./mcp.exe cleanup
```

Expected: report of current orphan processes, or "No orphan processes found." Does NOT kill anything by default.

```bash
./mcp.exe cleanup --confirm
```

After running, verify with `wmic process | grep GDB-MCP` that orphan count dropped.

- [ ] **Step 4: Commit**

```bash
git add internal/cli/cleanup.go internal/cli/root.go
git commit -m "feat(cleanup): mcp cleanup CLI (dry-run by default, --confirm kills)"
```

---

## Task 9: `mcp stop` + `mcp restart --all`

**Files:**

- Create: `internal/cli/stop.go`
- Modify: `internal/cli/restart.go`
- Modify: `internal/api/install.go` — add `(*API).Stop` + `(*API).RestartAll`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Add `(*API).Stop` and `(*API).RestartAll` to install.go**

Append to `internal/api/install.go`:

```go
// Stop stops a running daemon without removing its scheduler task or client
// entries. Equivalent to `schtasks /End /TN <name>` for each task matching
// the server (+ daemon filter if set).
func (a *API) Stop(server, daemonFilter string) error {
	sch, err := scheduler.New()
	if err != nil {
		return err
	}
	tasks, err := sch.List("mcp-local-hub-" + server + "-")
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if daemonFilter != "" {
			// Match "mcp-local-hub-<server>-<daemonFilter>"
			wantSuffix := "-" + daemonFilter
			if !strings.HasSuffix(strings.TrimPrefix(t.Name, "\\"), wantSuffix) {
				continue
			}
		}
		if err := sch.Stop(t.Name); err != nil {
			return fmt.Errorf("stop %s: %w", t.Name, err)
		}
	}
	return nil
}

// RestartAll stops+starts every scheduler task under our prefix. Returns a
// per-task result list with any errors.
func (a *API) RestartAll() ([]RestartResult, error) {
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		return nil, err
	}
	var results []RestartResult
	for _, t := range tasks {
		// Skip weekly-refresh — it's scheduled, not restarted.
		if strings.Contains(t.Name, "weekly-refresh") {
			continue
		}
		if err := sch.Stop(t.Name); err != nil && !strings.Contains(err.Error(), "no running instance") {
			results = append(results, RestartResult{TaskName: t.Name, Err: err.Error()})
			continue
		}
		if err := sch.Run(t.Name); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: err.Error()})
			continue
		}
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// RestartResult is one row in a RestartAll report.
type RestartResult struct {
	TaskName string
	Err      string
}
```

(Adjust imports if `strings` / `fmt` not already imported in install.go — they are.)

- [ ] **Step 2: Create `internal/cli/stop.go`**

```go
package cli

import (
	"fmt"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	var server, daemonFilter string
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop a daemon without uninstalling (tasks and configs remain)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			a := api.NewAPI()
			if err := a.Stop(server, daemonFilter); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Stopped %s\n", server)
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (required)")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "daemon name within the server manifest")
	return c
}
```

- [ ] **Step 3: Extend `internal/cli/restart.go` with `--all` flag**

Read the current restart.go. Add `var all bool`, flag registration, and branch:

```go
if all {
    if server != "" {
        return fmt.Errorf("--all is mutually exclusive with --server")
    }
    a := api.NewAPI()
    results, err := a.RestartAll()
    if err != nil {
        return err
    }
    for _, r := range results {
        if r.Err != "" {
            fmt.Fprintf(cmd.OutOrStderr(), "✗ %s: %s\n", r.TaskName, r.Err)
        } else {
            fmt.Fprintf(cmd.OutOrStdout(), "✓ Restarted %s\n", r.TaskName)
        }
    }
    return nil
}
// ... existing --server X path unchanged
```

Add `c.Flags().BoolVar(&all, "all", false, "restart every daemon")`.

- [ ] **Step 4: Wire stop into root**

Add `newStopCmd()` to `rootCmd.AddCommand(...)` in root.go.

- [ ] **Step 5: Smoke test**

```bash
cd <repo> && ./build.sh
./mcp.exe stop --server memory
./mcp.exe status | grep memory
# Expected: memory Ready (stopped)
./mcp.exe restart --server memory
./mcp.exe restart --all
```

Expected: stop works; restart --all cycles every daemon.

- [ ] **Step 6: Commit**

```bash
git add internal/api/install.go internal/cli/stop.go internal/cli/restart.go internal/cli/root.go
git commit -m "feat(cli): mcp stop + mcp restart --all"
```

---

## Phase 3A.2 concludes at Task 9

After Task 9 commits, run a checkpoint review:

- `go test ./... -race` — all green
- `./mcp.exe status --json | python -m json.tool | head` — JSON output with Port/PID/RAM/Uptime
- `./mcp.exe scan --processes` — unified per-client view with process counts
- `./mcp.exe migrate <any-server> --dry-run` — prints applied migrations
- `./mcp.exe cleanup` — reports orphans without killing
- `./mcp.exe cleanup --confirm` — actually kills them (wmic count should drop)
- `./mcp.exe restart --all` — cycles every daemon

If all gates pass, Phase 3A.2 is complete and Phase 3A.3 (management commands: backup sentinel, manifest CRUD, scheduler upgrade, weekly-refresh, settings) can be written next.

---

## Quality gates baked into Phase 3A.2

- **TDD:** every implementation task writes a failing test first (Tasks 1, 2, 4, 6, 7).
- **Commits:** one commit per task with a conventional-commit message.
- **No-op cross-platform:** wmic-dependent code (processes.go, cleanup.go) is guarded so Linux/macOS builds still compile; functions return empty results rather than error.
- **Backward-compat:** no existing CLI surface changes meaning. `mcp status` gains columns (may reshape user scripts parsing the old 4-column output — mitigated by `--json` for machines). `mcp restart --server X` still works; `--all` is additive.
