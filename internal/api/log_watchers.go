package api

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

// LogWatcher describes one orphan tail/grep/bash watcher process.
// Mirrors the shape `scripts/cleanup-orphan-watchers.ps1` produces in
// dry-run output, so an operator can compare GUI dashboard rows
// against the standalone script's report.
type LogWatcher struct {
	PID         int    `json:"pid"`
	ParentPID   int    `json:"parent_pid"`
	ParentAlive bool   `json:"parent_alive"`
	Name        string `json:"name"`
	AgeSec      int64  `json:"age_sec"`
	Cmdline     string `json:"cmdline"`
	KillErr     string `json:"kill_err,omitempty"` // populated only on apply
}

// LogWatcherCleanupOpts controls the watcher scan + kill.
//
// PathRegex matches the watcher's command-line path; defaults to a broad
// `\.scratch[\\/].*\.(log|txt)` pattern that catches typical agent
// shell-snapshot launcher pipelines (Claude Code, codex CLI etc.).
//
// OrphanGrepRegex is consulted on `grep` processes whose parent is no
// longer in the snapshot — grep's command-line typically carries only
// the regex (no path), so a name-only match would over-fire on
// legitimate grep usage. Tokens here match the script defaults.
//
// IncludeLive — if true, processes whose parent IS still alive are also
// killed. Default false: a path-matched live-parent process almost
// always represents a CURRENT active agent session, not a zombie.
//
// DryRun — if true, return matches without calling kill.
type LogWatcherCleanupOpts struct {
	PathRegex       string
	OrphanGrepRegex string
	IncludeLive     bool
	DryRun          bool
}

const (
	defaultLogWatcherPathRegex   = `\.scratch[\\/].*\.(log|txt)`
	defaultOrphanGrepRegexTokens = `BOBYQA|Traceback|Done|early_kill|S11|Sonnet|AXIEM|ERROR|Error|stage_|tick`
)

// neverKillLogWatcher names processes that must NEVER be killed by the
// log-watcher sweep, even if their command-line happened to match the
// path pattern. Belt-and-suspenders against future broadening of the
// match rule. Mirrors scripts/cleanup-orphan-watchers.ps1's NeverKill.
var neverKillLogWatcher = map[string]bool{
	// Active simulation runners
	"em.exe":          true,
	"python.exe":      true,
	"pythonw.exe":     true,
	"python":          true,
	"python3":         true,
	// Agent runtimes
	"mcphub.exe":      true,
	"mcphub":          true,
	"codex.exe":       true,
	"codex":           true,
	"claude.exe":      true,
	"claude":          true,
	// Editors
	"cursor.exe":      true,
	"cursor":          true,
	"code.exe":        true,
	"code":            true,
	"antigravity.exe": true,
	"antigravity":     true,
	// Shells (legitimate active shells)
	"powershell.exe":  true,
	"pwsh.exe":        true,
	"cmd.exe":         true,
}

// watcherProcessNames are the only process names we INSPECT for the
// path/orphan-grep heuristics. Limiting to shell + tail + grep keeps
// the sweep narrow.
var watcherProcessNames = map[string]bool{
	"bash.exe": true,
	"bash":     true,
	"sh.exe":   true,
	"sh":       true,
	"tail.exe": true,
	"tail":     true,
	"grep.exe": true,
	"grep":     true,
}

// processRow is the platform-agnostic shape every snapshot path produces.
type processRow struct {
	PID       int
	ParentPID int
	Name      string
	Cmdline   string
	StartTime int64 // Unix seconds; 0 if unknown
}

// CleanupLogWatchers scans for orphan tail/grep/bash watcher processes
// and (unless DryRun) kills them. Returns the matched set with KillErr
// populated for apply failures.
//
// Per docs/superpowers/specs/2026-05-06-cleanup-buttons-design.md Q3:
// detection is implemented via shell-out (matching the existing
// internal/api/processes.go house style — `Get-CimInstance` on Windows,
// `ps -e -o ...` on POSIX) rather than Go-native x/sys, even though
// the codex consult initially recommended stdlib + x/sys. House-style
// consistency wins; the deviation is documented in the design memo.
func (a *API) CleanupLogWatchers(opts LogWatcherCleanupOpts) ([]LogWatcher, error) {
	pathRe, err := compileWatcherRegex(opts.PathRegex, defaultLogWatcherPathRegex)
	if err != nil {
		return nil, fmt.Errorf("path regex: %w", err)
	}
	orphanRe, err := compileWatcherRegex(opts.OrphanGrepRegex, defaultOrphanGrepRegexTokens)
	if err != nil {
		return nil, fmt.Errorf("orphan grep regex: %w", err)
	}

	rows, err := snapshotProcessesForWatcherScan()
	if err != nil {
		return nil, err
	}

	pidSet := make(map[int]bool, len(rows))
	rowByPID := make(map[int]processRow, len(rows))
	for _, r := range rows {
		pidSet[r.PID] = true
		rowByPID[r.PID] = r
	}

	matches := filterWatcherCandidates(rows, pidSet, pathRe, orphanRe)
	if opts.DryRun {
		return matches, nil
	}

	// Codex Cloud bot P2 advisory on PR #131 / kosyak
	// 2026-05-07-pid-reuse-race-in-watcher-kill-loop.md: between the
	// initial snapshot and the kill syscall, a watcher can exit and
	// the OS can recycle its PID for an unrelated process. Re-snapshot
	// once and require identity (Name + StartTime) match before killing.
	// One re-snapshot per call (~ms POSIX, ~500ms Windows wmic) bounds
	// the apply-loop cost.
	fresh, err := snapshotProcessesForWatcherScan()
	if err != nil {
		// Fail-safe: if revalidation snapshot fails, refuse all kills
		// rather than proceed with stale identity. Mark each candidate
		// as skipped with the reason so the operator sees the gap.
		for i := range matches {
			if matches[i].ParentAlive && !opts.IncludeLive {
				continue
			}
			matches[i].KillErr = "skipped: revalidation snapshot failed: " + err.Error()
		}
		return matches, nil
	}
	freshByPID := make(map[int]processRow, len(fresh))
	for _, r := range fresh {
		freshByPID[r.PID] = r
	}

	for i := range matches {
		// Honor IncludeLive default: skip kill if parent still alive.
		if matches[i].ParentAlive && !opts.IncludeLive {
			continue
		}
		cur, stillThere := freshByPID[matches[i].PID]
		original := rowByPID[matches[i].PID]
		if !stillThere {
			matches[i].KillErr = "skipped: process exited between snapshot and kill"
			continue
		}
		// Identity revalidation requires a non-zero StartTime anchor.
		// When wmic/PowerShell can't parse CIM_DATETIME (parseWmicProcessRows
		// fallback at log_watchers.go:497-500) or `ps -o etime=` returns
		// unparseable elapsed time, StartTime falls back to 0. If both
		// the original and fresh rows have StartTime==0, the check
		// `cur.StartTime != original.StartTime` becomes `0 != 0` = false
		// and the guard degenerates to pure name equality — letting a
		// recycled same-named PID through. xhigh+subagents reliability
		// finding on PR #131 / kosyak
		// 2026-05-07-startime-zero-fail-open-bypasses-pid-reuse-guard.md.
		// Fail closed: refuse to kill when we cannot prove identity.
		if original.StartTime == 0 {
			matches[i].KillErr = "skipped: snapshot start-time unknown (cannot revalidate identity)"
			continue
		}
		// Identity: comm name + start time. Same comm with different
		// start time means a fresh unrelated process took the slot.
		if cur.Name != original.Name || cur.StartTime != original.StartTime {
			matches[i].KillErr = "skipped: PID reused (identity mismatch)"
			continue
		}
		if err := killOnePID(matches[i].PID); err != nil {
			matches[i].KillErr = err.Error()
		}
	}
	return matches, nil
}

func compileWatcherRegex(user, fallback string) (*regexp.Regexp, error) {
	src := user
	if src == "" {
		// Mirror the PowerShell `-match` operator's default
		// case-insensitive behavior the source script uses.
		// Codex Cloud bot P2 on PR #131 / kosyak
		// `2026-05-06-go-regex-case-sensitive-while-ps1-source-was-insensitive.md`:
		// Go's regexp.Compile is case-sensitive by default, so
		// without (?i) here the default fallbacks would silently miss
		// mixed-case paths like `.SCRATCH/foo.LOG` or lowercase
		// tokens like `traceback`. User-provided overrides keep
		// whatever case sensitivity they encode (operator authority).
		src = "(?i)" + fallback
	}
	return regexp.Compile(src)
}

// effectiveParentAlive answers "is this process's recorded parent
// still its meaningful parent" — accounting for POSIX init/subreaper
// reparenting that makes the bare `pidSet[ppid]` check insufficient.
//
// Why the POSIX ppid==1 special case is correct here: this helper is
// consulted only from filterWatcherCandidates against rows that already
// passed the watcherProcessNames gate (`bash`, `sh`, `tail`, `grep`).
// Those user-space utility processes are NEVER spawned directly by
// init/systemd as services in normal operation. So when they appear
// with ppid==1 on POSIX, the only realistic way for that to happen
// is reparenting after their original parent (an agent shell, codex
// CLI, mcphub watchdog, etc.) exited — exactly the orphan case the
// log-watcher sweep targets. Treating ppid==1 as "parent alive" would
// keep `tail`/`grep` adopted by PID 1 untouched on the IncludeLive=false
// default path, defeating the cleanup feature.
//
// Codex Cloud bot P1 on PR #135 round 2: the prior fix that simply
// returned `pidSet[ppid]` regressed orphan detection for reparented
// POSIX watchers. Restoring the POSIX-init special case is the
// architectural fix because we have no separate record of the
// original parent PID — but limited to the watcher-process subset,
// the heuristic is sound. Windows does not auto-reparent on parent
// exit, so the bare check stays correct there.
func effectiveParentAlive(ppid int, pidSet map[int]bool) bool {
	if ppid == 0 {
		return false
	}
	if !pidSet[ppid] {
		return false
	}
	if runtime.GOOS != "windows" && ppid == 1 {
		// POSIX init / systemd as PID 1 has adopted us — original
		// parent already exited. Classify as orphan. Safe because
		// callers gate by watcherProcessNames (bash/sh/tail/grep),
		// which are not legitimate direct init children.
		return false
	}
	return true
}

// filterWatcherCandidates is the platform-agnostic filter step. Two
// passes are unioned by PID:
//   1. Path-matched: process name in watcherProcessNames + cmdline
//      matches PathRegex.
//   2. Orphan-grep: process name == "grep[.exe]" + parent absent +
//      cmdline matches OrphanGrepRegex.
// NeverKill names are excluded from both passes.
func filterWatcherCandidates(rows []processRow, pidSet map[int]bool, pathRe, orphanRe *regexp.Regexp) []LogWatcher {
	now := time.Now().Unix()
	dedup := make(map[int]LogWatcher)
	for _, r := range rows {
		if neverKillLogWatcher[strings.ToLower(r.Name)] {
			continue
		}
		isWatcher := watcherProcessNames[strings.ToLower(r.Name)]
		matched := false
		if isWatcher && pathRe.MatchString(r.Cmdline) {
			matched = true
		}
		if !matched && strings.HasPrefix(strings.ToLower(r.Name), "grep") {
			parentAlive := effectiveParentAlive(r.ParentPID, pidSet)
			if !parentAlive && orphanRe.MatchString(r.Cmdline) {
				matched = true
			}
		}
		if !matched {
			continue
		}
		parentAlive := effectiveParentAlive(r.ParentPID, pidSet)
		ageSec := int64(0)
		if r.StartTime > 0 {
			ageSec = now - r.StartTime
		}
		dedup[r.PID] = LogWatcher{
			PID:         r.PID,
			ParentPID:   r.ParentPID,
			ParentAlive: parentAlive,
			Name:        r.Name,
			AgeSec:      ageSec,
			Cmdline:     r.Cmdline,
		}
	}
	out := make([]LogWatcher, 0, len(dedup))
	for _, w := range dedup {
		out = append(out, w)
	}
	return out
}

// snapshotProcessesForWatcherScan delegates to the platform-specific
// snapshot. Windows reuses the existing runProcessSnapshot() (CSV from
// wmic / PowerShell). POSIX uses `ps -e -o pid,ppid,comm,etime,args`.
//
// Test seam: snapshotProcessesFn — tests inject a fixed roster.
var snapshotProcessesFn = func() ([]processRow, error) {
	if runtime.GOOS == "windows" {
		raw, err := runProcessSnapshot()
		if err != nil {
			return nil, err
		}
		return parseWmicProcessRows(strings.NewReader(raw))
	}
	return runPosixPS()
}

func snapshotProcessesForWatcherScan() ([]processRow, error) {
	return snapshotProcessesFn()
}

// runPosixPS shells out to `ps` with a fixed column order so we can
// parse without depending on the locale's BSD vs. SysV argv quirks.
// Output sample (Linux):
//
//	  PID  PPID COMMAND        ELAPSED ARGS
//	 1234   500 bash           00:01:23 /bin/bash -c "tail -F ...| grep ..."
//
// Codex Cloud bot P2 on PR #131 commit 1f59a65: without `-ww`, ps
// truncates the ARGS column at the controlling terminal width (or
// 80 cols if no tty). Watcher detection depends on finding `.scratch`
// path substrings deep in `bash -c "tail -F /full/path/here"` argv,
// which are exactly the strings ps would truncate. `-ww` widens
// the column to unlimited on both Linux procps and BSD ps (macOS).
func runPosixPS() ([]processRow, error) {
	cmd := exec.Command("ps", "-eww", "-o", "pid=,ppid=,comm=,etime=,args=")
	process.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePosixPSRows(strings.NewReader(string(out))), nil
}

func parsePosixPSRows(r io.Reader) []processRow {
	now := time.Now().Unix()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var rows []processRow
	for scanner.Scan() {
		line := scanner.Text()
		// Codex Cloud bot P1 on PR #131 / kosyak
		// 2026-05-06-shipped-frontend-without-checking-json-wire-shape.md:
		// `ps -e -o pid=,ppid=,comm=,etime=,args=` right-pads the
		// numeric / short columns with spaces, so SplitN on " " spends
		// budget on consecutive separators and skips legitimate rows.
		// strings.Fields collapses any-whitespace runs to single
		// tokens; we take 4 fixed columns then preserve ARGS verbatim
		// from the position where the 5th token begins so internal
		// spaces in cmdline survive.
		toks := strings.Fields(line)
		if len(toks) < 4 {
			continue
		}
		pid, err := strconv.Atoi(toks[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(toks[1])
		comm := toks[2]
		ageSec := parseEtime(toks[3])
		args := readArgsAfter4Tokens(line)
		startTime := int64(0)
		if ageSec > 0 {
			startTime = now - ageSec
		}
		rows = append(rows, processRow{
			PID:       pid,
			ParentPID: ppid,
			Name:      comm,
			Cmdline:   args,
			StartTime: startTime,
		})
	}
	return rows
}

// readArgsAfter4Tokens returns the substring of line starting at the
// 5th whitespace-delimited token. Walks the line by hand rather than
// using SplitN/Fields so inner whitespace (including double spaces in
// command arguments) is preserved verbatim from the 5th token onward.
// Returns "" if there are fewer than 5 tokens.
func readArgsAfter4Tokens(line string) string {
	i := 0
	tokensSeen := 0
	for tokensSeen < 4 && i < len(line) {
		// Skip leading whitespace before this token.
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			return ""
		}
		// Consume the token body.
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			i++
		}
		tokensSeen++
	}
	// Skip whitespace before the 5th token, then return everything
	// from there to end-of-line.
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return ""
	}
	return line[i:]
}

// parseEtime parses ps's ELAPSED format, e.g. "00:01:23", "1-02:03:04",
// "23:45". Returns the elapsed seconds, 0 if unparseable.
func parseEtime(s string) int64 {
	days := int64(0)
	rest := s
	if i := strings.Index(s, "-"); i >= 0 {
		d, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0
		}
		days = d
		rest = s[i+1:]
	}
	parts := strings.Split(rest, ":")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	var h, m, sec int64
	switch len(parts) {
	case 2:
		mm, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0
		}
		ss, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0
		}
		m = mm
		sec = ss
	case 3:
		hh, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0
		}
		mm, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0
		}
		ss, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return 0
		}
		h = hh
		m = mm
		sec = ss
	default:
		return 0
	}
	return days*86400 + h*3600 + m*60 + sec
}

// parseWmicProcessRows reads wmic-CSV (Windows snapshot) into the
// neutral processRow shape. Reuses CIM_DATETIME via parseWmicDate().
//
// Codex Cloud bot 3 P1 findings on PR #131 commit 1f59a65 / kosyak
// 2026-05-07-wmic-csv-parse-not-tolerant-shipped-parallel-mechanism.md:
// the prior implementation used encoding/csv.Reader, which is RFC 4180
// strict. wmic /format:csv violates RFC 4180 in three real-world ways:
//
//  1. Quotes inside quoted fields are NOT doubled (wmic predates the
//     RFC). csv.Reader rejects with ErrBareQuote on those rows.
//  2. wmic emits leading blank lines on some Windows versions; the
//     prior loop treated blank as the header and lost colIdx entirely.
//  3. csv.Reader.Read() returning err on a single malformed row caused
//     return nil, err, killing the whole snapshot.
//
// Fix: use encoding/csv.Reader with LazyQuotes to preserve multiline
// quoted records while tolerating wmic quote oddities. Keep per-row
// continue semantics for malformed rows and blank-line skip.
func parseWmicProcessRows(r io.Reader) ([]processRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true

	var rows []processRow
	var colIdx map[string]int
	for {
		fields, err := cr.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			// Keep snapshot resilient: malformed rows should not abort
			// the whole cleanup pass.
			continue
		}
		if len(fields) == 1 && strings.TrimSpace(fields[0]) == "" {
			continue
		}
		if colIdx == nil {
			colIdx = make(map[string]int, len(fields))
			for i, h := range fields {
				colIdx[strings.TrimSpace(h)] = i
			}
			continue
		}
		get := func(name string) string {
			if i, ok := colIdx[name]; ok && i < len(fields) {
				return fields[i]
			}
			return ""
		}
		pid, err := strconv.Atoi(strings.TrimSpace(get("ProcessId")))
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(strings.TrimSpace(get("ParentProcessId")))
		cmd := get("CommandLine")
		name := basenameFromCmdline(cmd)
		startTime := int64(0)
		if dt := parseWmicDate(get("CreationDate")); !dt.IsZero() {
			startTime = dt.Unix()
		}
		rows = append(rows, processRow{PID: pid, ParentPID: ppid, Name: name, Cmdline: cmd, StartTime: startTime})
	}
}

func basenameFromCmdline(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	// Quoted exe path?
	if cmd[0] == '"' {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			cmd = cmd[1 : 1+end]
		}
	} else if i := strings.Index(cmd, " "); i >= 0 {
		cmd = cmd[:i]
	}
	// Strip directory.
	for i := len(cmd) - 1; i >= 0; i-- {
		if cmd[i] == '/' || cmd[i] == '\\' {
			return cmd[i+1:]
		}
	}
	return cmd
}

// killOnePID best-effort kills a single PID. Uses os.Process.Kill which
// resolves to TerminateProcess on Windows and SIGKILL on POSIX — same
// semantic as `taskkill /F` and `kill -KILL` respectively. Operates on
// a single PID rather than a tree — log watchers do not have descendants
// we own (they are themselves descendants of agent shells).
func killOnePID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
