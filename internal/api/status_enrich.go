package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/config"
)

// selfPIDFn returns the current process PID. Test seam for DM-2: tests
// can override it to control which PID counts as "self" without
// spawning a real subprocess. Production = os.Getpid.
var selfPIDFn = os.Getpid

// lookupProcess queries netstat + wmic for the process bound to 127.0.0.1:port.
// Populated by internal/api/processes.go's init() on Windows (Task 2); stays
// nil elsewhere so cross-platform callers fall through the (ok==false) branch.
//
// This single-port variant is O(netstat+wmic) per call — expensive for
// status/scan which probe many ports at once. Prefer lookupProcessBatch
// when the set of ports is known up front; it amortizes both subprocess
// invocations across all ports.
var lookupProcess func(port int) (pid int, ramBytes uint64, uptimeSec int64, ok bool)

// lookupProcessBatch returns per-port process info for every port in
// the input list, making exactly one netstat and one wmic subprocess
// call. On non-Windows hosts (where lookupProcessBatch is nil — the
// Windows init populates it) callers should skip enrichment.
//
// Result keying: port → (pid, ram, uptime, alive). Ports not listening
// are absent from the map; callers check existence via map lookup.
var lookupProcessBatch func(ports []int) map[int]struct {
	PID       int
	RAMBytes  uint64
	UptimeSec int64
}

// enrichStatus walks the scheduler.Status rows and adds Port (from manifest),
// Server, Daemon (parsed from TaskName), and — when the task is Running —
// PID/RAMBytes/UptimeSec from a live wmic query. manifestDir points at the
// repo's servers/ directory; passed explicitly so tests can use t.TempDir().
//
// Workspace-scoped rows (TaskName matches `mcp-local-hub-lsp-<key>-<lang>`)
// get their Port and the 5-state lifecycle fields from the workspace
// registry when registryPath is non-empty; pass "" in tests that do not
// need the workspace-scoped overlay.
func enrichStatus(rows []DaemonStatus, manifestDir string) {
	enrichStatusWithRegistry(rows, manifestDir, "")
}

// enrichStatusWithRegistry is the richer variant used by StatusWithHealth.
// registryPath, when non-empty, points at the workspace registry YAML; rows
// whose TaskName matches the lazy-proxy pattern are enriched with
// Workspace, Language, Backend, Lifecycle, and the LastMaterializedAt /
// LastToolsCallAt / LastError fields. Empty registryPath (or a missing
// file) skips the overlay silently so existing global-daemon callers see
// stable output.
func enrichStatusWithRegistry(rows []DaemonStatus, manifestDir, registryPath string) {
	ports := manifestPortMap(manifestDir)

	// Workspace-scoped overlay: preload the registry once so we don't reload
	// per-row. Keys are normalized by trimming the scheduler's leading '\'
	// so rows emitted as `\mcp-...` still match registry entries stored as
	// `mcp-...`.
	var wsEntries map[string]WorkspaceEntry
	if registryPath != "" {
		reg := NewRegistry(registryPath)
		if err := reg.Load(); err == nil {
			wsEntries = make(map[string]WorkspaceEntry, len(reg.Workspaces))
			for _, e := range reg.Workspaces {
				wsEntries[strings.TrimPrefix(e.TaskName, "\\")] = e
			}
		}
	}

	// First pass: populate Server/Daemon/Port.
	var wantedPorts []int
	for i := range rows {
		// Workspace-scoped row? Populate lifecycle overlay from registry and
		// short-circuit the manifest-port lookup (workspace-scoped ports live
		// in the registry, not the manifest). The manifest-port lookup would
		// miss anyway since "lsp-<key>" is not a real server slug.
		if wsKey, lang, ok := parseLazyProxyTaskName(rows[i].TaskName); ok {
			// Mark the row with the structural workspace-scoped flag BEFORE
			// the registry overlay so consumers (notably the GUI Logs
			// picker) get a truthful signal even when registry loading or
			// enrichment fails and Workspace/Language/Lifecycle end up
			// empty. See DaemonStatus.IsWorkspaceScoped in types.go.
			rows[i].IsWorkspaceScoped = true
			rows[i].Server = "mcp-language-server"
			rows[i].Daemon = "lsp-" + wsKey + "-" + lang
			if wsEntries != nil {
				// Task Scheduler emits names with a leading '\'; the registry
				// stores them without. Normalize both when looking up.
				normalized := strings.TrimPrefix(rows[i].TaskName, "\\")
				if e, had := wsEntries[normalized]; had {
					rows[i].Workspace = e.WorkspacePath
					rows[i].Language = e.Language
					rows[i].Backend = e.Backend
					rows[i].Lifecycle = e.Lifecycle
					rows[i].LastMaterializedAt = e.LastMaterializedAt
					rows[i].LastToolsCallAt = e.LastToolsCallAt
					rows[i].LastError = e.LastError
					if e.Port != 0 {
						rows[i].Port = e.Port
					}
				}
			}
			if rows[i].Port != 0 {
				wantedPorts = append(wantedPorts, rows[i].Port)
			}
			continue
		}
		srv, dmn := parseTaskName(rows[i].TaskName)
		rows[i].Server = srv
		rows[i].Daemon = dmn
		// Mark scheduler-maintenance rows so GUI consumers can filter them
		// out of daemon-only surfaces (Logs picker, Dashboard cards). All
		// three weekly-refresh naming conventions parse to
		// daemon == "weekly-refresh":
		//   - "mcp-local-hub-weekly-refresh"           → ("", "weekly-refresh")
		//   - "mcp-local-hub-workspace-weekly-refresh" → ("workspace", "weekly-refresh")
		//   - "mcp-local-hub-<server>-weekly-refresh"  → (<server>, "weekly-refresh")
		// Use the parse output as the single source of truth; the JS side
		// reads the is_maintenance flag instead of re-implementing the
		// match.
		if dmn == "weekly-refresh" {
			rows[i].IsMaintenance = true
		}
		if p, ok := ports[srv][dmn]; ok {
			rows[i].Port = p
		} else if p, ok := ports[srv]["default"]; ok {
			rows[i].Port = p
		}
		if rows[i].Port != 0 {
			wantedPorts = append(wantedPorts, rows[i].Port)
		}
	}

	// Batch process-info lookup. Previously each row invoked
	// `netstat -ano` + `wmic process get …` separately — for 11 daemons
	// that's 22+ subprocess launches and ~7s of wall-time. One shared
	// netstat + one wmic (filtered to the PIDs netstat found) amortizes
	// both: measured 11 daemons drops from ~7 s to ~0.7 s.
	var batch map[int]struct {
		PID       int
		RAMBytes  uint64
		UptimeSec int64
	}
	if lookupProcessBatch != nil && len(wantedPorts) > 0 {
		batch = lookupProcessBatch(wantedPorts)
	}

	// Second pass: fill PID/RAM/Uptime + derive state.
	selfPID := selfPIDFn()
	for i := range rows {
		// DM-1 (extends the lazy-proxy escape hatch): any row with Port=0
		// means we don't know which port to probe. Two real causes:
		//   1) Workspace-scoped lazy-proxy whose registry entry couldn't
		//      be resolved (corrupt / stale registry, orphan task).
		//   2) Global daemon whose manifest isn't in this binary's embed
		//      (dev binary running alongside an installed mcphub.exe
		//      whose servers/ has more entries — the dev `go run`
		//      binary's status query then finds task names it can't
		//      resolve to ports).
		// Without this guard, deriveState sees alive=false and transforms
		// the raw scheduler "Running" into "Starting" forever — the
		// operator sees a permanently "Starting" daemon that is actually
		// healthy. The lazy-proxy reasoning generalizes: if we don't know
		// the port we can't probe alive, so the raw scheduler state is
		// the most honest signal we have. The IsLazyProxyTaskName check
		// is now redundant but harmless — keep the comment-anchor for
		// the original lazy-proxy fix while the broader Port==0 guard
		// catches the new orphan / manifest-asymmetry cases.
		if rows[i].Port == 0 {
			continue
		}
		alive := false
		if batch != nil && rows[i].Port != 0 {
			if info, ok := batch[rows[i].Port]; ok {
				// DM-2: skip self-PID. If the running mcphub process is
				// the PID listening on rows[i].Port, that means the
				// GUI itself bound that port (e.g., wolfram manifest
				// declares 9125 and mcphub-GUI's --port also defaults
				// to 9125). The daemon is NOT actually alive — counting
				// the GUI as the daemon would falsely report Running
				// with the GUI's PID/RAM/uptime. Treat as alive=false
				// so deriveState produces a truthful state for the
				// daemon row instead of a misleading false positive.
				if info.PID != selfPID {
					rows[i].PID = info.PID
					rows[i].RAMBytes = info.RAMBytes
					rows[i].UptimeSec = info.UptimeSec
					alive = true
				}
			}
		} else if lookupProcess != nil && rows[i].Port != 0 {
			// Fallback to per-row lookup when the batch form isn't
			// populated (shouldn't happen on Windows; covers unit-test
			// harnesses that set lookupProcess but not the batch).
			if pid, ram, uptime, ok := lookupProcess(rows[i].Port); ok {
				if pid != selfPID {
					rows[i].PID = pid
					rows[i].RAMBytes = ram
					rows[i].UptimeSec = uptime
					alive = true
				}
			}
		}
		rows[i].State = deriveState(rows[i].State, alive, rows[i].NextRun)
	}
}

// deriveState maps (raw scheduler task state, daemon port-listening?, NextRun text)
// to a user-meaningful status. Windows Task Scheduler's "Ready" covers both
// "waiting for next trigger" and "logon-triggered daemon whose process died
// hours ago" — the raw state alone cannot tell those apart. By folding in
// port liveness and trigger presence we get four actionable labels:
//
//	Running   — port bound; daemon alive (raw state irrelevant)
//	Starting  — scheduler currently executing the task's launch action,
//	            port not yet bound
//	Scheduled — task idle, no live daemon, but trigger will fire later
//	            (weekly-refresh-style tasks)
//	Stopped   — task idle with no future trigger and no daemon (logon-only
//	            daemon that has exited; user needs to `mcphub restart`)
//
// Anything unexpected (Disabled, Queued, etc.) falls through unchanged.
func deriveState(raw string, alive bool, nextRun string) string {
	if alive {
		return "Running"
	}
	switch raw {
	case "Running":
		return "Starting"
	case "Ready":
		if hasFutureTrigger(nextRun) {
			return "Scheduled"
		}
		return "Stopped"
	}
	return raw
}

// hasFutureTrigger returns true when schtasks reports a concrete next-run
// timestamp. Empty string or the literal "N/A" both mean "no upcoming
// trigger" (e.g. logon-triggered daemon tasks that only fire at user
// logon — schtasks emits "N/A" for NextRun in that case).
func hasFutureTrigger(nextRun string) bool {
	s := strings.TrimSpace(nextRun)
	return s != "" && !strings.EqualFold(s, "N/A")
}

// parseLazyProxyTaskName recognizes the workspace-scoped lazy-proxy
// TaskName convention `mcp-local-hub-lsp-<workspaceKey>-<language>` (with
// optional leading backslash) and returns (workspaceKey, language, true).
// Any other shape returns ("", "", false) — including the hub-wide
// weekly-refresh task (`mcp-local-hub-workspace-weekly-refresh`) and the
// global per-server/per-daemon tasks parsed by parseTaskName.
//
// WorkspaceKey is always 8 hex characters (api.WorkspaceKey); rejecting
// other lengths keeps a future `lsp-*` global-server name from matching
// this pattern accidentally.
// IsLazyProxyTaskName returns true iff the given scheduler task name
// matches the `mcp-local-hub-lsp-<workspaceKey>-<language>` pattern.
// Use this for filtering status rows by structure rather than by
// registry-derived fields (Lifecycle, Language), which can be empty
// when registry loading or enrichment fails — in that scenario a
// field-based filter silently drops genuine workspace-scoped rows.
func IsLazyProxyTaskName(task string) bool {
	_, _, ok := parseLazyProxyTaskName(task)
	return ok
}

func parseLazyProxyTaskName(task string) (workspaceKey, language string, ok bool) {
	name := strings.TrimPrefix(task, "\\")
	const prefix = "mcp-local-hub-lsp-"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := name[len(prefix):]
	// workspaceKey is exactly 8 hex chars; the language follows the '-'.
	if len(rest) < 8+1 || rest[8] != '-' {
		return "", "", false
	}
	key := rest[:8]
	for i := 0; i < 8; i++ {
		c := key[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", "", false
		}
	}
	lang := rest[9:]
	if lang == "" {
		return "", "", false
	}
	return key, lang, true
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
	// Hub-wide weekly-refresh (created by scheduler_mgmt.WeeklyRefreshSet)
	// is just "mcp-local-hub-weekly-refresh" — no per-server prefix. The
	// generic -weekly-refresh branch below would treat the whole string
	// as <server=weekly-refresh, daemon=""> which is wrong. Short-circuit
	// on exact match.
	if rest == "weekly-refresh" {
		return "", "weekly-refresh"
	}
	// Per-server weekly-refresh daemons are registered with the
	// two-word daemon name "weekly-refresh" (install.go —
	// "mcp-local-hub-<server>-weekly-refresh"). A plain LastIndex('-')
	// split would cut between "weekly" and "refresh" and mis-attribute
	// "serena-weekly"/"refresh" instead of the intended
	// "serena"/"weekly-refresh". Check the suffix first so the
	// hyphen-bearing daemon is restored before falling back to the
	// single-segment split used by every other daemon.
	const weeklySuffix = "-weekly-refresh"
	if strings.HasSuffix(rest, weeklySuffix) {
		return rest[:len(rest)-len(weeklySuffix)], "weekly-refresh"
	}
	// The last segment is the daemon; the rest is the server (which may contain '-').
	idx := strings.LastIndex(rest, "-")
	if idx < 0 {
		return rest, ""
	}
	return rest[:idx], rest[idx+1:]
}

// manifestPortMap walks every available manifest and returns a map
// server → daemon → port. Empty manifestDir uses the production
// embed-first resolution path; a non-empty dir reads that directory
// only (test hermetic use).
func manifestPortMap(manifestDir string) map[string]map[string]int {
	out := map[string]map[string]int{}
	if manifestDir == "" {
		names, _ := listManifestNamesEmbedFirst()
		for _, name := range names {
			data, err := loadManifestYAMLEmbedFirst(name)
			if err != nil {
				continue
			}
			m, err := config.ParseManifest(bytes.NewReader(data))
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
