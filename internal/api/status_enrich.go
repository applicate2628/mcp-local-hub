package api

import (
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/config"
)

// lookupProcess queries netstat + wmic for the process bound to 127.0.0.1:port.
// Populated by internal/api/processes.go's init() on Windows (Task 2); stays
// nil elsewhere so cross-platform callers fall through the (ok==false) branch.
var lookupProcess func(port int) (pid int, ramBytes uint64, uptimeSec int64, ok bool)

// enrichStatus walks the scheduler.Status rows and adds Port (from manifest),
// Server, Daemon (parsed from TaskName), and — when the task is Running —
// PID/RAMBytes/UptimeSec from a live wmic query. manifestDir points at the
// repo's servers/ directory; passed explicitly so tests can use t.TempDir().
func enrichStatus(rows []DaemonStatus, manifestDir string) {
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
		// Windows Task Scheduler flips the task back to "Ready" the moment its
		// action (launch the daemon process) completes — even though the
		// spawned daemon keeps running. Gating on State=="Running" here made
		// PID/RAM/UPTIME columns blank for every live daemon. Always probe by
		// port and let the lookup itself decide whether a daemon is alive.
		if lookupProcess != nil && rows[i].Port != 0 {
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
