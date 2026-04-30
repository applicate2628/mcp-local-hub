package api

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

// CountProcesses returns how many OS processes currently match the given
// command-line substring patterns. Typical usage: feed it the server name
// and the primary command/arg tokens from its manifest.
//
// Windows-only for Phase 3A.2. On Linux/macOS it returns (0, nil) — the
// caller gets zero results without error, which keeps scan/cleanup flows
// usable without crashing.
func (a *API) CountProcesses(patterns []string) (int, error) {
	if runtime.GOOS != "windows" {
		return 0, nil
	}
	out, err := runProcessSnapshot()
	if err != nil {
		return 0, err
	}
	return parseWmicCount(strings.NewReader(out), patterns)
}

// processSnapshot is a pre-fetched wmic/PowerShell output buffer used
// by scan --processes to amortize the ~500 ms-per-invocation cost of
// snapshotting the process table across all entries. Empty string is
// the zero value — CountProcessesFromSnapshot treats it as "no
// processes match" (same behavior as CountProcesses on non-Windows).
type processSnapshot struct {
	raw string
}

// takeProcessSnapshot captures the current process list ONCE so
// multiple entries can be scored against it without re-invoking wmic
// per call. Returns an empty snapshot on non-Windows or on snapshot
// failure — callers then see zero counts instead of an error, matching
// the contract of CountProcesses.
func takeProcessSnapshot() processSnapshot {
	if runtime.GOOS != "windows" {
		return processSnapshot{}
	}
	out, err := runProcessSnapshot()
	if err != nil {
		return processSnapshot{}
	}
	return processSnapshot{raw: out}
}

// CountProcessesFromSnapshot is the batch variant of CountProcesses —
// reuses a single process-snapshot across many pattern sets. Intended
// for scan --processes which probes 20+ entries; doing one wmic per
// entry was measured at ~13 s vs ~1 s with this variant.
func (a *API) CountProcessesFromSnapshot(snap processSnapshot, patterns []string) int {
	if snap.raw == "" {
		return 0
	}
	n, _ := parseWmicCount(strings.NewReader(snap.raw), patterns)
	return n
}

// runProcessSnapshot returns a CSV-formatted process list compatible with
// the shape wmic historically produced. Tries wmic first (legacy Windows),
// falls back to PowerShell Get-CimInstance (Windows 11 24H2+ removed wmic).
//
// Output format:
//
//	Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize
//	HOST,"cmdline text",20260417180000.000000+000,555,1001,40000000
//	...
//
// CommandLine is quoted with "" escaping (wmic-compatible). CreationDate is
// formatted as CIM_DATETIME so parseWmicDate works unchanged. Returned as a
// single string for convenience; callers wrap in strings.NewReader.
func runProcessSnapshot() (string, error) {
	// Legacy path: wmic (present on Windows 10 and older Windows 11).
	wmicCmd := exec.Command("wmic", "process", "get",
		"CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize",
		"/format:csv")
	process.NoConsole(wmicCmd)
	wmicOut, wmicErr := wmicCmd.Output()
	if wmicErr == nil {
		return string(wmicOut), nil
	}

	// PowerShell fallback: works on every Windows with PowerShell installed,
	// which is every supported Windows version (5.1 built-in, 7 via MSI).
	// Emit rows in wmic CSV shape so the parsers don't need a second path.
	// Uses [string]::Format instead of backtick-escaping to keep the Go
	// raw-string literal clean (PowerShell's backtick would close the literal).
	const psScript = `Get-CimInstance Win32_Process | ForEach-Object {
		$cmdline = if ($_.CommandLine) { ($_.CommandLine -replace '"', '""') } else { '' }
		$created = $_.CreationDate.ToString('yyyyMMddHHmmss') + '.000000+000'
		[string]::Format('HOST,"{0}",{1},{2},{3},{4}', $cmdline, $created, $_.ParentProcessId, $_.ProcessId, $_.WorkingSetSize)
	}`
	psCmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	process.NoConsole(psCmd)
	psOut, psErr := psCmd.Output()
	if psErr != nil {
		return "", fmt.Errorf("both wmic and PowerShell process snapshot failed: wmic=%v; powershell=%v", wmicErr, psErr)
	}
	header := "Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize\n"
	return header + string(psOut), nil
}

// parseWmicCount scans the CSV `wmic process get` output and returns the
// number of lines whose CommandLine field contains at least one of the given
// substring patterns. Deduplicates: a line matching multiple patterns counts once.
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

// ProcessInfo describes one live process match.
type ProcessInfo struct {
	PID      int
	RAMBytes uint64
	Cmdline  string
}

// ListMatchingProcesses returns full process info for every process whose
// CommandLine contains at least one of the given substring patterns.
// Windows-only (wmic); returns nil on other platforms.
func (a *API) ListMatchingProcesses(patterns []string) ([]ProcessInfo, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	cmd := exec.Command("wmic", "process", "get", "CommandLine,ProcessId,WorkingSetSize", "/format:csv")
	process.NoConsole(cmd)
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
// embedded commas are preserved. wmic output doesn't escape quotes inside
// quoted fields, so a minimal state machine suffices.
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

// netstatLinePIDForLoopbackPort extracts the PID from a single `netstat -ano`
// line only when the line represents a LISTENING socket on exactly
// 127.0.0.1:<port>. Returns (pid, true) on a strict match.
func netstatLinePIDForLoopbackPort(line string, port int) (int, bool) {
	if !strings.Contains(line, "LISTENING") || port <= 0 {
		return 0, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, false
	}
	if fields[1] != fmt.Sprintf("127.0.0.1:%d", port) {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || pid == 0 {
		return 0, false
	}
	return pid, true
}

// init populates status_enrich.go's lookupProcess function pointer with a
// real Windows implementation that combines netstat (to find the PID owning
// the port) and wmic (to fetch RAM + start time for that PID).
//
// On Linux/macOS the function pointer stays nil; callers in status_enrich.go
// already check for nil before invoking it, so PID/RAM/Uptime columns just
// stay blank on non-Windows hosts.
func init() {
	if runtime.GOOS != "windows" {
		return
	}
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		if port <= 0 {
			return 0, 0, 0, false
		}
		// Step 1: PID via netstat
		netstatCmd := exec.Command("netstat", "-ano")
		process.NoConsole(netstatCmd)
		out, err := netstatCmd.Output()
		if err != nil {
			return 0, 0, 0, false
		}
		var pid int
		for line := range strings.SplitSeq(string(out), "\n") {
			if parsedPID, ok := netstatLinePIDForLoopbackPort(line, port); ok {
				pid = parsedPID
				break
			}
		}
		if pid == 0 {
			return 0, 0, 0, false
		}
		// Step 2: RAM + CreationDate via wmic
		wmicLookupCmd := exec.Command("wmic", "process", "where",
			fmt.Sprintf("ProcessId=%d", pid),
			"get", "WorkingSetSize,CreationDate", "/format:csv")
		process.NoConsole(wmicLookupCmd)
		out2, err := wmicLookupCmd.Output()
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

	// Batch variant: one netstat + one wmic for N ports.
	lookupProcessBatch = func(ports []int) map[int]struct {
		PID       int
		RAMBytes  uint64
		UptimeSec int64
	} {
		result := make(map[int]struct {
			PID       int
			RAMBytes  uint64
			UptimeSec int64
		}, len(ports))
		if len(ports) == 0 {
			return result
		}

		// Step 1: one netstat -ano → build port→pid map.
		netCmd := exec.Command("netstat", "-ano")
		process.NoConsole(netCmd)
		out, err := netCmd.Output()
		if err != nil {
			return result
		}
		wantPort := make(map[int]bool, len(ports))
		for _, p := range ports {
			wantPort[p] = true
		}
		portToPID := make(map[int]int, len(ports))
		pidSet := make(map[int]bool, len(ports))
		for line := range strings.SplitSeq(string(out), "\n") {
			if !strings.Contains(line, "LISTENING") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			// Local-addr field format: "127.0.0.1:<port>" or "[::]:<port>".
			addr := fields[1]
			idx := strings.LastIndex(addr, ":")
			if idx < 0 {
				continue
			}
			port, err := strconv.Atoi(addr[idx+1:])
			if err != nil || !wantPort[port] {
				continue
			}
			pid, _ := strconv.Atoi(fields[len(fields)-1])
			if pid == 0 {
				continue
			}
			if _, already := portToPID[port]; !already {
				portToPID[port] = pid
				pidSet[pid] = true
			}
		}
		if len(pidSet) == 0 {
			return result
		}

		// Step 2: one wmic call filtered to exactly the PIDs we care
		// about. `WHERE (ProcessId=A or ProcessId=B …)` — avoids the
		// per-pid loop the old code did.
		var wmicWhere strings.Builder
		first := true
		for pid := range pidSet {
			if !first {
				wmicWhere.WriteString(" or ")
			}
			first = false
			fmt.Fprintf(&wmicWhere, "ProcessId=%d", pid)
		}
		wmicMultiCmd := exec.Command("wmic", "process", "where",
			wmicWhere.String(),
			"get", "ProcessId,WorkingSetSize,CreationDate", "/format:csv")
		process.NoConsole(wmicMultiCmd)
		out2, err := wmicMultiCmd.Output()
		if err != nil {
			// Fall back to PIDs without RAM/uptime — still useful.
			for port, pid := range portToPID {
				result[port] = struct {
					PID       int
					RAMBytes  uint64
					UptimeSec int64
				}{PID: pid}
			}
			return result
		}
		type procInfo struct {
			ram     uint64
			created time.Time
		}
		pidInfo := make(map[int]procInfo, len(pidSet))
		for _, line := range strings.Split(string(out2), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "Node,") {
				continue
			}
			fields := splitCSVLine(line)
			// Node,CreationDate,ProcessId,WorkingSetSize
			if len(fields) < 4 {
				continue
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(fields[2]))
			if pid == 0 {
				continue
			}
			ram, _ := strconv.ParseUint(strings.TrimSpace(fields[3]), 10, 64)
			created := parseWmicDate(strings.TrimSpace(fields[1]))
			pidInfo[pid] = procInfo{ram: ram, created: created}
		}
		for port, pid := range portToPID {
			info := pidInfo[pid]
			var uptime int64
			if !info.created.IsZero() {
				uptime = int64(time.Since(info.created).Seconds())
			}
			result[port] = struct {
				PID       int
				RAMBytes  uint64
				UptimeSec int64
			}{PID: pid, RAMBytes: info.ram, UptimeSec: uptime}
		}
		return result
	}
}

// parseWmicDate parses wmic's CIM_DATETIME format: YYYYMMDDHHMMSS.mmmmmm+ZZZ.
// The timestamp is in local time (the +ZZZ offset is discarded); using
// time.Local gives correct time.Since() results. Parsing as UTC would
// produce negative uptime on non-UTC hosts.
func parseWmicDate(s string) time.Time {
	if len(s) < 14 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102150405", s[:14], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}
