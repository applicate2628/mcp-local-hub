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
// Windows-only for Phase 3A.2. On Linux/macOS it returns (0, nil) until a
// cross-platform implementation lands later.
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

// init populates status_enrich.go's lookupProcess function pointer with a
// real Windows implementation that combines netstat (to find the PID owning
// the port) and wmic (to fetch RAM + start time for that PID).
func init() {
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		if port <= 0 {
			return 0, 0, 0, false
		}
		// Step 1: PID via netstat
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
		// Step 2: RAM + CreationDate via wmic
		out2, err := exec.Command("wmic", "process", "where",
			fmt.Sprintf("ProcessId=%d", pid),
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
