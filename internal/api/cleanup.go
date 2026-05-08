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
//
// Wire-format note (Cleanup-6 security fix): the raw Cmdline is kept on
// the struct for server-side use (manifest-pattern match in CleanupOrphans,
// CLI display via `mcphub cleanup`, NeverKill enforcement) but is hidden
// from JSON output (`json:"-"`) because full command lines often carry
// workspace paths, username segments, file paths, and process arguments
// that may include API keys or tokens. The GUI/HTTP wire instead exposes
// `cmdline_display` populated by redactCmdlineForDisplay — basename of
// the executable only — which is enough for an operator to recognize
// the process while keeping sensitive context off-wire and out of
// browser dev-tools / screenshots.
type OrphanProcess struct {
	PID             int    `json:"pid"`
	ParentID        int    `json:"parent_pid"`
	Server          string `json:"server"` // inferred from matching manifest
	RAMBytes        uint64 `json:"ram_bytes"`
	Cmdline         string `json:"-"`               // server-side use only; redacted from wire
	CmdlineDisplay  string `json:"cmdline_display"` // basename of executable for GUI display
	AgeSec          int64  `json:"age_sec"`
	KillErr         string `json:"kill_err,omitempty"`
}

// redactCmdlineForDisplay returns the executable's basename only, with no
// path and no arguments. Cleanup-6 security fix: full command lines often
// embed workspace paths (D:\dev\client-confidential-project\...), username
// segments (C:\Users\<name>\...), filesystem paths to private files, and
// process arguments that may contain API keys or tokens (--api-key=sk-...).
// The orphan-table UI in the GUI and any operator screenshots / browser
// dev-tools that observe the wire MUST see only the basename — the PID,
// inferred Server, RAM, and Age columns carry the operational info needed
// to confirm a kill.
//
// Behavior:
//   - Quoted form: `"C:\path with spaces\foo.exe" arg1 arg2` → `foo.exe`
//   - Unquoted form: `C:\path\foo.exe arg1 arg2` → `foo.exe`
//   - POSIX path: `/usr/local/bin/uvx mcp-server-time` → `uvx`
//   - Empty / whitespace-only: `<unknown>`
//
// The parsing matches the canonical pattern in isOurOwnProcess: an opening
// quote selects the quoted token; otherwise the first space ends the
// executable token. filepath.Base then strips the directory prefix.
func redactCmdlineForDisplay(cmdline string) string {
	c := strings.TrimSpace(cmdline)
	if c == "" {
		return "<unknown>"
	}
	first := c
	if c[0] == '"' {
		if end := strings.IndexByte(c[1:], '"'); end > 0 {
			first = c[1 : 1+end]
		} else {
			// Unterminated quote → treat the rest as the path token.
			first = c[1:]
		}
	} else if sp := strings.IndexAny(c, " \t"); sp > 0 {
		first = c[:sp]
	}
	first = strings.TrimSpace(first)
	if first == "" {
		return "<unknown>"
	}
	// filepath.Base handles both backslash and forward-slash separators on
	// Windows; on POSIX it splits on forward-slash only. The orphan
	// detector consumes Windows-shaped CommandLine values from CIM/wmic,
	// so we walk both separators ourselves to stay platform-independent.
	if i := strings.LastIndexAny(first, `\/`); i >= 0 {
		first = first[i+1:]
	}
	if first == "" {
		return "<unknown>"
	}
	return first
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
	// Drop broad launcher tokens (node, npx, uv, uvx, python, ...) so we
	// don't sweep the operator's unrelated dev-tooling processes that
	// happen to share an interpreter with one of our manifest commands.
	var allPatterns []string
	for _, ps := range patterns {
		for _, p := range ps {
			if isBroadLauncherToken(p) {
				continue
			}
			allPatterns = append(allPatterns, p)
		}
	}
	orphans := parseOrphans(strings.NewReader(string(out)), allPatterns)

	// Age filter + assign server. Same broad-token filter applies here:
	// matching o.Cmdline against `node` would label any unrelated node
	// process with the manifest's server name.
	filtered := orphans[:0]
	for _, o := range orphans {
		if o.AgeSec < opts.MinAgeSec {
			continue
		}
		for name, ps := range patterns {
			for _, p := range ps {
				if isBroadLauncherToken(p) {
					continue
				}
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

// isBroadLauncherToken returns true if the manifest pattern is a bare
// generic interpreter / launcher name (node, npx, python, ...). Such
// tokens are too noisy as orphan-match patterns: nearly every dev box
// has unrelated node/python/npx processes running, and a `Contains`
// match against `cmdline` would sweep them into our cleanup.
//
// The check normalizes the pattern before comparing:
//
//   - trim whitespace and surrounding single/double quotes
//   - lowercase
//   - split on BOTH `\\` and `/` separators so `C:\Users\...\python.exe`
//     and `/usr/bin/python3` both reduce to a bare interpreter name —
//     filepath.Base would NOT strip backslashes when running under
//     Linux/macOS CI, so we walk both separators ourselves.
//   - strip Windows executable suffixes (.exe, .cmd, .bat, .ps1) so
//     `node.exe`, `npx.cmd`, `uvx.exe` all reduce to the bare token
//
// Codex finding fix: PR #121 only matched bare tokens (e.g. `"node"`)
// and missed the .exe-suffixed and absolute-path forms that real wmic
// output produces. Cross-platform fix: use platform-independent base()
// since this code path runs in the orphan-cleanup logic that consumes
// wmic CSV output (Windows-style paths) on any platform's CI.
func isBroadLauncherToken(pattern string) bool {
	p := strings.TrimSpace(pattern)
	p = strings.Trim(p, `"'`)
	if p == "" {
		return true
	}
	p = strings.ToLower(p)
	// Platform-independent basename: take everything after the last
	// '/' or '\\', whichever comes later. Don't use filepath.Base on
	// Unix runners — it would treat `C:\Users\...\node.exe` as a
	// single token because '\\' is not a separator there.
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		p = p[i+1:]
	}
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if strings.HasSuffix(p, suffix) {
			p = strings.TrimSuffix(p, suffix)
			break
		}
	}
	switch p {
	case "node", "npx", "uv", "uvx", "python", "python3", "py":
		return true
	}
	return false
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
			PID:            r.pid,
			ParentID:       r.ppid,
			RAMBytes:       r.ram,
			Cmdline:        r.cmdline,
			CmdlineDisplay: redactCmdlineForDisplay(r.cmdline),
			AgeSec:         age,
		})
	}
	return out
}
