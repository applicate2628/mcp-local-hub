package api

import (
	"bufio"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

// isOurOwnProcess returns true when the cmdline's executable token is one
// of our own binaries: mcphub.exe (running as daemon or any subcommand),
// the standalone godbolt / lldb-bridge / perftools exes, or the legacy
// mcp.exe name from early installations. Basename match, case-insensitive.
// Used by the orphan detector to skip processes that are running by
// design — their parent is typically the Task Scheduler service, so the
// parent-is-our-daemon heuristic alone cannot protect them.
func isOurOwnProcess(cmdline string) bool {
	if cmdline == "" {
		return false
	}
	// Extract the first token (the exe path). Handles both quoted and
	// unquoted cmdlines: `"C:\path with spaces\mcphub.exe" daemon ...`
	// and `C:\path\mcphub.exe daemon ...`.
	first := cmdline
	if cmdline[0] == '"' {
		if end := strings.IndexByte(cmdline[1:], '"'); end > 0 {
			first = cmdline[1 : 1+end]
		}
	} else if sp := strings.IndexByte(cmdline, ' '); sp > 0 {
		first = cmdline[:sp]
	}
	base := strings.ToLower(filepath.Base(first))
	switch base {
	case "mcphub.exe", "mcphub",
		"mcp.exe", "mcp",
		"godbolt.exe", "godbolt",
		"lldb-bridge.exe", "lldb-bridge",
		"perftools.exe", "perftools":
		return true
	}
	return false
}

// OrphanProcess describes one orphan MCP subprocess discovered by CleanupOrphans.
// KillErr is populated only when DryRun=false and taskkill failed for this PID
// (access denied, process already gone, etc.); empty on success or dry-run.
type OrphanProcess struct {
	PID      int
	ParentID int
	Server   string // inferred from matching manifest
	RAMBytes uint64
	Cmdline  string
	AgeSec   int64
	KillErr  string
}

// CleanupOpts controls CleanupOrphans.
type CleanupOpts struct {
	ManifestDir string
	MinAgeSec   int64  // don't kill processes younger than this (default 60)
	DryRun      bool   // if true, just report
	Server      string // empty = all servers; otherwise only that one
}

// CleanupOrphans finds MCP server processes that match a manifest's command
// pattern but whose parent is NOT our `mcp.exe daemon` wrapper. Reports them
// (dry-run) or kills them (non-dry-run).
func (a *API) CleanupOrphans(opts CleanupOpts) ([]OrphanProcess, error) {
	if runtime.GOOS != "windows" {
		// Process introspection below uses Windows-specific tooling.
		// Return an empty result on other platforms so the CLI stays usable
		// (`mcp cleanup` just prints "No orphan processes found.").
		return nil, nil
	}
	if opts.MinAgeSec == 0 {
		opts.MinAgeSec = 60
	}
	// Collect patterns per manifest.
	patterns := map[string][]string{}
	if opts.Server != "" {
		patterns[opts.Server] = patternsForServer(opts.Server, opts.ManifestDir)
	} else {
		names, err := readManifestNames(opts.ManifestDir)
		if err != nil {
			return nil, err
		}
		for name := range names {
			patterns[name] = patternsForServer(name, opts.ManifestDir)
		}
	}

	// Snapshot processes. wmic was the historical tool but Windows 11 24H2+
	// ships without it; PowerShell's Get-CimInstance works on every modern
	// Windows and produces equivalent data.
	out, err := runProcessSnapshot()
	if err != nil {
		return nil, err
	}

	// Flat list of patterns — any match counts this PID as a candidate orphan.
	var allPatterns []string
	for _, ps := range patterns {
		allPatterns = append(allPatterns, ps...)
	}
	orphans := parseOrphans(strings.NewReader(string(out)), allPatterns)

	// Age filter + assign server.
	filtered := orphans[:0]
	for _, o := range orphans {
		if o.AgeSec < opts.MinAgeSec {
			continue
		}
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

	// Kill if not dry-run. Preserve taskkill's stderr on each failure so the
	// caller can distinguish "access denied" from "PID already gone" in the
	// per-orphan report instead of silently swallowing the error.
	if !opts.DryRun {
		for i := range filtered {
			cmd := exec.Command("taskkill", "/PID", strconv.Itoa(filtered[i].PID), "/F")
			process.NoConsole(cmd)
			out, err := cmd.CombinedOutput()
			if err != nil {
				msg := strings.TrimSpace(string(out))
				if msg == "" {
					msg = err.Error()
				}
				filtered[i].KillErr = msg
			}
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

	// Index by PID so we can inspect parent's cmdline.
	byPID := map[int]row{}
	for _, r := range rows {
		byPID[r.pid] = r
	}

	var out []OrphanProcess
	for _, r := range rows {
		// Never flag our own binaries — mcphub.exe (running as daemon or
		// as any subcommand), the standalone godbolt/lldb-bridge/perftools
		// exes, or the legacy mcp.exe name. These are running by design;
		// their parent is the Task Scheduler service, so the parent-is-our-
		// daemon check below won't save them. Without this explicit
		// allowlist the cleanup would flag e.g. `mcphub.exe daemon --server
		// gdb` as an orphan gdb-server just because "gdb" appears in its
		// cmdline.
		if isOurOwnProcess(r.cmdline) {
			continue
		}
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
		// Is any ANCESTOR one of our own daemons? Walk the parent chain
		// up to a bounded depth (16 levels is well beyond anything real).
		// Single-level check misses uvx/npx/node sub-processes that wrap
		// the actual server — e.g. mcphub.exe daemon → uv → python →
		// server.py forms a 4-deep chain where python's direct parent is
		// uv, not our daemon. Walking the chain means every descendant
		// of a live `mcphub.exe daemon` is correctly excluded.
		ourDescendant := false
		for cur, depth := r.ppid, 0; depth < 16; depth++ {
			parent, ok := byPID[cur]
			if !ok {
				break
			}
			pcmd := parent.cmdline
			if strings.Contains(pcmd, "daemon") &&
				(strings.Contains(pcmd, "mcphub.exe") || strings.Contains(pcmd, "mcp.exe")) {
				ourDescendant = true
				break
			}
			if parent.ppid == 0 || parent.ppid == cur {
				break // reached the root or a self-loop
			}
			cur = parent.ppid
		}
		if ourDescendant {
			continue // NOT orphan — descendant of our daemon
		}
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
