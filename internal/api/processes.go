package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
)

// CountProcesses returns how many OS processes currently match the given
// complete command identity patterns. A root must contain every supplied
// token; descendants of that root are counted even when their command line
// does not repeat the root identity.
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
//
// lines is the raw CSV tokenized into one entry per line, populated
// ONCE by takeProcessSnapshot (deep-review r2 P4-5). CountProcessesFromSnapshot
// used to re-run a bufio.Scanner over the full `raw` string from byte
// zero for every scan entry (O(entries x processes) — a multi-hundred-
// line re-tokenization per entry); it now matches patterns directly
// against this pre-split slice (O(entries x patterns), zero
// re-tokenization of raw text after the snapshot is taken). Counting
// semantics are unchanged: a line matching any of an entry's patterns
// counts once for that entry.
type processSnapshot struct {
	raw   string
	lines []string
}

// procRow is one parsed runProcessSnapshot row. It is shared by process count
// parsing, orphan cleanup, and log-watcher cleanup so every consumer of the
// shared WMIC/PowerShell snapshot gets the same comma-safe row parse.
type procRow struct {
	pid, ppid int
	created   time.Time
	exePath   string
	cmdline   string
	ram       uint64
}

// takeProcessSnapshot captures the current process list ONCE so
// multiple entries can be scored against it without re-invoking wmic
// per call. Returns an empty snapshot on non-Windows or on snapshot
// failure — callers then see zero counts instead of an error, matching
// the contract of CountProcesses. Also splits the CSV into lines once
// here so CountProcessesFromSnapshot never re-tokenizes the raw text.
func takeProcessSnapshot() processSnapshot {
	if runtime.GOOS != "windows" {
		return processSnapshot{}
	}
	out, err := runProcessSnapshot()
	if err != nil {
		return processSnapshot{}
	}
	return processSnapshot{raw: out, lines: splitSnapshotLines(out)}
}

// splitSnapshotLines tokenizes a wmic/PowerShell CSV snapshot into logical
// records. A quoted CommandLine can contain embedded newlines, so this must use
// the shared WMIC record assembler rather than a plain line scanner.
func splitSnapshotLines(raw string) []string {
	records, _ := process.ReadWmicCSVRecords(strings.NewReader(raw))
	return records
}

// CountProcessesFromSnapshot is the batch variant of CountProcesses —
// reuses a single process-snapshot across many pattern sets. Intended
// for scan --processes which probes 20+ entries; doing one wmic per
// entry was measured at ~13 s vs ~1 s with this variant. Matches each
// entry's complete command identity against the snapshot's pre-split lines
// (deep-review r2 P4-5) instead of re-scanning the raw CSV text per entry.
func (a *API) CountProcessesFromSnapshot(snap processSnapshot, patterns []string) int {
	return countProcessesFromSnapshotAttribution(snap, processAttribution{rootVariants: []processRootIdentity{{argvSequence: patterns}}, legacyAnyTokenFallback: len(patterns) == 1})
}

// processAttribution carries complete root identities separately from the
// display/cleanup substring patterns. A managed mcphub daemon has a stronger
// root identity than its manifest command alone: `daemon --server S --daemon D`.
type processAttribution struct {
	rootVariants           []processRootIdentity
	legacyAnyTokenFallback bool
}

type processRootIdentity struct {
	argvSequence             []string
	requiresMcphubExecutable bool
}

func countProcessesFromSnapshotAttribution(snap processSnapshot, attribution processAttribution) int {
	if snap.raw == "" {
		return 0
	}
	return countAttributedProcessLines(snap.lines, attribution)
}

func processAttributionForManifest(serverName string, m *config.ServerManifest) processAttribution {
	if m == nil {
		return processAttribution{rootVariants: []processRootIdentity{{argvSequence: []string{serverName}}}, legacyAnyTokenFallback: true}
	}
	bare := strings.ToLower(stripExtension(basenameAcrossSeparators(m.Command)))
	if isMcphubBinaryBasename(bare) && len(m.Daemons) > 0 {
		variants := make([]processRootIdentity, 0, len(m.Daemons))
		for _, daemon := range m.Daemons {
			if daemon.Name != "" {
				variants = append(variants, processRootIdentity{argvSequence: []string{"daemon", "--server", serverName, "--daemon", daemon.Name}, requiresMcphubExecutable: true})
			}
		}
		if len(variants) > 0 {
			return processAttribution{rootVariants: variants}
		}
	}
	patterns := patternsFromManifest(serverName, m)
	return processAttribution{rootVariants: []processRootIdentity{{argvSequence: patterns}}}
}

// countAttributedProcessLines is the scan/process attribution owner. A root
// process must contain every manifest-derived command identity token; once a
// root is selected, all of its descendants are counted even when the child
// command line does not repeat those tokens. This prevents shared wrappers
// (notably multiple mcphub subcommands) from being attributed to every server
// while retaining the existing "server process tree" display contract.
//
// Older two-column process listings lack PID ancestry; they retain the legacy
// any-pattern count rather than inventing parentage from incomplete input.
func countAttributedProcessLines(records []string, attribution processAttribution) int {
	if len(attribution.rootVariants) == 0 {
		return 0
	}
	if attribution.legacyAnyTokenFallback {
		return countMatchingLines(records, attribution.rootVariants[0].argvSequence)
	}
	rows, err := parseProcessSnapshotRows(strings.NewReader(strings.Join(records, "\n")))
	if err != nil || len(rows) == 0 {
		// A multi-token request is a complete server identity. Falling back to
		// any-token matching when its PID ancestry is unavailable would make a
		// shared wrapper token (mcphub) attribute unrelated servers again.
		return 0
	}
	byPID := make(map[int]procRow, len(rows))
	roots := make(map[int]struct{})
	for _, row := range rows {
		byPID[row.pid] = row
		if processCommandMatchesAnyCompleteIdentity(row.cmdline, attribution.rootVariants) {
			roots[row.pid] = struct{}{}
		}
	}
	if len(roots) == 0 {
		return 0
	}
	count := 0
	for _, row := range rows {
		if processDescendsFromRoot(row, byPID, roots) {
			count++
		}
	}
	return count
}

func processCommandMatchesAnyCompleteIdentity(cmdline string, variants []processRootIdentity) bool {
	argv := process.TokenizeWindowsCommandLine(cmdline)
	for _, variant := range variants {
		if processArgvMatchesIdentity(argv, variant) {
			return true
		}
	}
	return false
}

func processArgvMatchesIdentity(argv []string, identity processRootIdentity) bool {
	if len(identity.argvSequence) == 0 || len(argv) == 0 {
		return false
	}
	if identity.requiresMcphubExecutable && !processArgvHasMcphubExecutable(argv) {
		return false
	}
	if !identity.requiresMcphubExecutable {
		return processArgvContainsAllIdentityTokens(argv, identity.argvSequence)
	}
	for start := 0; start+len(identity.argvSequence) <= len(argv); start++ {
		matched := true
		for i, want := range identity.argvSequence {
			if !processArgvTokenMatches(argv[start+i], want) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// processArgvContainsAllIdentityTokens preserves the public CountProcesses
// contract for generic caller-provided token sets while making every token
// boundary-exact. Managed daemons use the stronger ordered argv sequence above.
func processArgvContainsAllIdentityTokens(argv, wants []string) bool {
	for _, want := range wants {
		found := false
		for _, actual := range argv {
			if processArgvTokenMatches(actual, want) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func processArgvTokenMatches(actual, want string) bool {
	if strings.EqualFold(actual, want) {
		return true
	}
	if !isMcphubBinaryBasename(strings.ToLower(want)) {
		return false
	}
	bare := strings.ToLower(stripExtension(basenameAcrossSeparators(actual)))
	if isMcphubBinaryBasename(bare) {
		return true
	}
	return strings.HasSuffix(bare, ".js") && isMcphubBinaryBasename(strings.TrimSuffix(bare, ".js"))
}

func processArgvHasMcphubExecutable(argv []string) bool {
	if isMcphubExecutableToken(argv[0]) {
		return true
	}
	return len(argv) >= 2 && isNodeExecutableToken(argv[0]) && isMcphubScriptToken(argv[1])
}

func isMcphubExecutableToken(token string) bool {
	bare := strings.ToLower(stripExtension(basenameAcrossSeparators(token)))
	return isMcphubBinaryBasename(bare)
}

func isNodeExecutableToken(token string) bool {
	bare := strings.ToLower(stripExtension(basenameAcrossSeparators(token)))
	return bare == "node"
}

func isMcphubScriptToken(token string) bool {
	bare := strings.ToLower(basenameAcrossSeparators(token))
	if !strings.HasSuffix(bare, ".js") {
		return false
	}
	return isMcphubBinaryBasename(strings.TrimSuffix(bare, ".js"))
}

func processDescendsFromRoot(row procRow, byPID map[int]procRow, roots map[int]struct{}) bool {
	pid := row.pid
	seen := make(map[int]struct{})
	for pid != 0 {
		if _, ok := roots[pid]; ok {
			return true
		}
		if _, loop := seen[pid]; loop {
			return false
		}
		seen[pid] = struct{}{}
		current, ok := byPID[pid]
		if !ok {
			return false
		}
		pid = current.ppid
	}
	return false
}

// countMatchingLines returns how many of the given rows' CommandLine
// fields contain at least one of the patterns as a substring,
// deduplicating so a row matching multiple patterns still counts once —
// the same semantics parseWmicCount applies over an io.Reader.
func countMatchingLines(records []string, patterns []string) int {
	count := 0
	header, ok := parseWmicHeaderFromRecords(records, "Node", "CommandLine")
	if !ok {
		return 0
	}
	for _, record := range records {
		cmdline, ok := processRecordCommandLine(record, header)
		if !ok {
			continue
		}
		for _, p := range patterns {
			if strings.Contains(cmdline, p) {
				count++
				break
			}
		}
	}
	return count
}

func processRecordCommandLine(record string, header process.WmicCSVHeader) (string, bool) {
	row, ok := process.ParseWmicProcessCSVRecord(record, header)
	if !ok {
		return "", false
	}
	return row["CommandLine"], true
}

func parseProcessSnapshotRows(r io.Reader) ([]procRow, error) {
	records, err := process.ReadWmicCSVRecords(r)
	header, ok := parseWmicHeaderFromRecords(records,
		"Node", "CommandLine", "CreationDate", "ExecutablePath", "ParentProcessId", "ProcessId", "WorkingSetSize")
	if !ok {
		return nil, err
	}
	var rows []procRow
	for _, record := range records {
		row, ok := parseProcessSnapshotRow(record, header)
		if ok {
			rows = append(rows, row)
		}
	}
	return rows, err
}

func parseWmicHeaderFromRecords(records []string, required ...string) (process.WmicCSVHeader, bool) {
	for _, record := range records {
		header, ok := process.ParseWmicCSVHeader(record, required...)
		if ok {
			return header, true
		}
	}
	return process.WmicCSVHeader{}, false
}

func parseProcessSnapshotRow(record string, header process.WmicCSVHeader) (procRow, bool) {
	parsed, ok := process.ParseWmicProcessCSVRecord(record, header)
	if !ok {
		return procRow{}, false
	}
	ppid, err1 := strconv.Atoi(parsed["ParentProcessId"])
	pid, err2 := strconv.Atoi(parsed["ProcessId"])
	ram, err3 := strconv.ParseUint(parsed["WorkingSetSize"], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || pid == 0 {
		return procRow{}, false
	}
	created := parseWmicDate(parsed["CreationDate"])
	return procRow{
		pid:     pid,
		ppid:    ppid,
		created: created,
		exePath: parsed["ExecutablePath"],
		cmdline: parsed["CommandLine"],
		ram:     ram,
	}, true
}

// runProcessSnapshot returns a CSV-formatted process list compatible with
// the shape wmic historically produced. Tries wmic first (legacy Windows),
// falls back to PowerShell Get-CimInstance (Windows 11 24H2+ removed wmic).
//
// Output format:
//
//	Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
//	HOST,"cmdline text",20260417180000.000000+000,"C:\...\node.exe",555,1001,40000000
//	...
//
// CommandLine is quoted with "" escaping (wmic-compatible). CreationDate is
// formatted as CIM_DATETIME so parseWmicDate works unchanged. ExecutablePath
// (added so parseProcessRows can build the PID-recycle-safe identity proof the
// cleanup reapers kill through — see reapOrphans) sits between CreationDate and
// ParentProcessId, which is exactly where wmic's alphabetical `/format:csv`
// column order places it. Returned as a single string for convenience; callers
// wrap in strings.NewReader.
func runProcessSnapshot() (string, error) {
	// One deadline shared by the wmic attempt AND the PowerShell fallback, so a
	// slow wmic cannot buy a second full-price attempt (see probeChainBudget).
	ctx, cancel := newProbeChainContext()
	defer cancel()

	// Legacy path: wmic (present on Windows 10 and older Windows 11).
	wmicOut, wmicErr := runProbeCommandCtx(ctx, "wmic", "process", "get",
		"CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize",
		"/format:csv")
	if wmicErr == nil {
		return string(wmicOut), nil
	}

	// PowerShell fallback: works on every Windows with PowerShell installed,
	// which is every supported Windows version (5.1 built-in, 7 via MSI).
	// Emit rows in wmic CSV shape so the parsers don't need a second path.
	// Uses [string]::Format instead of backtick-escaping to keep the Go
	// raw-string literal clean (PowerShell's backtick would close the literal).
	// ExecutablePath is quoted (like CommandLine) so a path is always a single
	// CSV field for parseProcessRows's right-anchor; a Windows path can never
	// contain a `"` (illegal filename char), so no quote-escaping is needed.
	const psScript = `Get-CimInstance Win32_Process | ForEach-Object {
		$cmdline = if ($_.CommandLine) { ($_.CommandLine -replace '"', '""') } else { '' }
		$offset = [int]([TimeZoneInfo]::Local.GetUtcOffset($_.CreationDate).TotalMinutes)
		$sign = if ($offset -lt 0) { '-' } else { '+' }
		$created = $_.CreationDate.ToString('yyyyMMddHHmmss.ffffff') + $sign + [Math]::Abs($offset).ToString('000')
		$exe = if ($_.ExecutablePath) { $_.ExecutablePath } else { '' }
		[string]::Format('HOST,"{0}",{1},"{2}",{3},{4},{5}', $cmdline, $created, $exe, $_.ParentProcessId, $_.ProcessId, $_.WorkingSetSize)
	}`
	psOut, psErr := runProbeCommandCtx(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	if psErr != nil {
		return "", fmt.Errorf("both wmic and PowerShell process snapshot failed: wmic=%v; powershell=%w", wmicErr, psErr)
	}
	header := "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize\n"
	return header + string(psOut), nil
}

// parseWmicCount scans the CSV `wmic process get` output and returns the
// number of lines whose CommandLine field contains at least one of the given
// substring patterns. Deduplicates: a line matching multiple patterns counts once.
//
// Single-shot callers only (CountProcesses, direct unit tests). The batch
// path (CountProcessesFromSnapshot, deep-review r2 P4-5) tokenizes once via
// splitSnapshotLines and reuses countMatchingLines directly instead of
// going through this io.Reader-based entry point per call.
func parseWmicCount(r io.Reader, patterns []string) (int, error) {
	records, err := process.ReadWmicCSVRecords(r)
	return countAttributedProcessLines(records, processAttribution{rootVariants: []processRootIdentity{{argvSequence: patterns}}, legacyAnyTokenFallback: len(patterns) == 1}), err
}

// ProcessInfo describes one live process match.
type ProcessInfo struct {
	PID      int
	RAMBytes uint64
	Cmdline  string
}

// runMatchingProcessesSnapshot returns the wmic-shape CSV that
// ListMatchingProcesses parses (Node,CommandLine,ProcessId,
// WorkingSetSize). On Windows 11 24H2+ wmic.exe may be absent
// entirely (Microsoft is removing the legacy WMIC tool); fall
// back to a PowerShell Get-CimInstance probe that emits the same
// 4-column CSV shape so the downstream parser does not need a
// second path. Bot r1 P1 closure on PR #188: pre-fix, the
// install-upgrade GUI preflight relied on ListMatchingProcesses
// which only used wmic — on wmic-removed hosts, detection silently
// returned an error, the preflight degraded to "no GUI found",
// StopAll then succeeded, and Bootstrap finally failed with
// "target in use" leaving daemons down (exactly the regression A8
// was added to prevent).
func runMatchingProcessesSnapshot() ([]byte, error) {
	// Shared wmic→PowerShell chain deadline (see probeChainBudget).
	ctx, cancel := newProbeChainContext()
	defer cancel()

	if out, err := runProbeCommandCtx(ctx, "wmic", "process", "get",
		"CommandLine,ProcessId,WorkingSetSize", "/format:csv"); err == nil {
		return out, nil
	} else {
		// PS fallback below. Hold wmic error in case PS also fails.
		const psScript = `Get-CimInstance Win32_Process | ForEach-Object {
			$cmdline = if ($_.CommandLine) { ($_.CommandLine -replace '"', '""') } else { '' }
			[string]::Format('HOST,"{0}",{1},{2}', $cmdline, $_.ProcessId, $_.WorkingSetSize)
		}`
		psOut, psErr := runProbeCommandCtx(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
		if psErr != nil {
			return nil, fmt.Errorf("both wmic and PowerShell process listing failed: wmic=%v; powershell=%w", err, psErr)
		}
		header := "Node,CommandLine,ProcessId,WorkingSetSize\n"
		return append([]byte(header), psOut...), nil
	}
}

// ListMatchingProcesses returns full process info for every process whose
// CommandLine contains at least one of the given substring patterns.
// Pattern matching is CASE-INSENSITIVE (codex bot r3 P2 closure on
// PR #188: Windows preserves user-typed casing in cmdline strings,
// so an explorer launch like `MCPHUB.EXE gui` would be missed by a
// case-sensitive `"mcphub.exe"` prefilter — that miss would let
// install --upgrade proceed to StopAll, then Bootstrap fails with
// "target in use" leaving daemons down).
// Windows-only; returns nil on other platforms.
func (a *API) ListMatchingProcesses(patterns []string) ([]ProcessInfo, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	out, err := runMatchingProcessesSnapshot()
	if err != nil {
		return nil, err
	}
	// Pre-lowercase patterns once so the per-line match is cheap.
	lowerPatterns := make([]string, 0, len(patterns))
	for _, p := range patterns {
		lowerPatterns = append(lowerPatterns, strings.ToLower(p))
	}
	records, err := process.ReadWmicCSVRecords(strings.NewReader(string(out)))
	if err != nil {
		return nil, err
	}
	header, ok := parseWmicHeaderFromRecords(records, "Node", "CommandLine", "ProcessId", "WorkingSetSize")
	if !ok {
		return nil, nil
	}
	var results []ProcessInfo
	for _, record := range records {
		row, ok := process.ParseWmicProcessCSVRecord(record, header)
		if !ok {
			continue
		}
		lineLower := strings.ToLower(row["CommandLine"])
		matched := false
		for _, p := range lowerPatterns {
			if strings.Contains(lineLower, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		pid, _ := strconv.Atoi(row["ProcessId"])
		ram, _ := strconv.ParseUint(row["WorkingSetSize"], 10, 64)
		cmdline := row["CommandLine"]
		results = append(results, ProcessInfo{PID: pid, RAMBytes: ram, Cmdline: cmdline})
	}
	return results, nil
}

// splitCSVLine preserves the existing api-local helper name while delegating
// the WMIC splitting rules to the process package's shared implementation.
func splitCSVLine(line string) []string {
	return process.SplitWmicCSVLine(line)
}

func isUnsignedDecimalField(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// netstatLineLoopbackPortPID parses a single `netstat -ano` line and, when it
// represents a LISTENING socket on the IPv4 loopback 127.0.0.1:<port>, returns
// that (port, pid). It is the SHARED low-level parser behind both the
// single-port matcher (netstatLinePIDForLoopbackPort) and the all-ports
// snapshot (LoopbackPortOwnersSnapshot) so the LISTENING-state + exact
// v4-loopback-address + non-zero-PID gate is defined exactly ONCE.
//
// Strictness preserved verbatim from the old single-port matcher:
//   - line must contain "LISTENING" (state gate);
//   - fields[1] must be exactly the v4 loopback form 127.0.0.1:<port> — the
//     "127.0.0.1:" prefix is required, so 0.0.0.0:<port> and the IPv6
//     [::1]:<port> form never match;
//   - the trailing field must parse to a non-zero PID.
//
// Returns (port, pid, true) only when all three hold.
func netstatLineLoopbackPortPID(line string) (int, int, bool) {
	if !strings.Contains(line, "LISTENING") {
		return 0, 0, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, 0, false
	}
	const loopbackPrefix = "127.0.0.1:"
	portStr, ok := strings.CutPrefix(fields[1], loopbackPrefix)
	if !ok {
		return 0, 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || pid == 0 {
		return 0, 0, false
	}
	return port, pid, true
}

// netstatLinePIDForLoopbackPort extracts the PID from a single `netstat -ano`
// line only when the line represents a LISTENING socket on exactly
// 127.0.0.1:<port>. Returns (pid, true) on a strict match. Thin filter over
// the shared netstatLineLoopbackPortPID parser.
func netstatLinePIDForLoopbackPort(line string, port int) (int, bool) {
	if port <= 0 {
		return 0, false
	}
	gotPort, pid, ok := netstatLineLoopbackPortPID(line)
	if !ok || gotPort != port {
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
		out, err := runProbeCommand(probeCommandTimeout, "netstat", "-ano")
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
		out2, err := runProbeCommand(probeCommandTimeout, "wmic", "process", "where",
			fmt.Sprintf("ProcessId=%d", pid),
			"get", "WorkingSetSize,CreationDate", "/format:csv")
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

	// processIdentityByPID returns (image, parentImage) for a PID,
	// used by portHeldByOurDaemon (install.go) for the ownership
	// gate that distinguishes "our running daemon owns this port"
	// from "a foreign PID stole the port while a same-named scheduler
	// task happens to be Running" (bot r1 P1 + r2 P1 closure on
	// PR #180 / bug-bash A6 #6).
	//
	// Both layers are needed because:
	//   - stdio-bridge: image == "mcphub.exe" (our binary holds the listener)
	//   - native-http external port: image == "mcphub.exe" (in-proc http.Server)
	//   - native-http internal port: image is the upstream child (python.exe,
	//     node.exe, ...); parentImage == "mcphub.exe" because mcphub spawned it
	//
	// portHeldByOurDaemon treats EITHER image OR parentImage matching
	// mcphub.exe as ownership signal (combined with scheduler.Running
	// check for full three-part gate).
	//
	// Internally uses wmic first, falls back to PowerShell Get-CimInstance
	// on hosts where wmic is removed (Windows 11 24H2+) — same fallback
	// pattern as runProcessSnapshot above (r2 P2 closure).
	processIdentityByPID = func(pid int) (image, parentImage string, ok bool) {
		image, parentPID, ok := procNameAndParent(pid)
		if !ok {
			return "", "", false
		}
		if parentPID <= 0 {
			// No parent (System / PID-reuse with parent gone). Return
			// image only; caller still gets a valid ownership signal
			// when image itself matches mcphub.exe.
			return image, "", true
		}
		// Best-effort parent lookup: if parent has exited or wmic/PS
		// fails for it, return the empty parentImage (caller can still
		// match by image alone).
		parentImage, _, _ = procNameAndParent(parentPID)
		return image, parentImage, true
	}
	processNameAndParentByPID = procNameAndParentErr

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
		out, err := runProbeCommand(probeCommandTimeout, "netstat", "-ano")
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
			// Match the single-port lookup semantics: only 127.0.0.1 listeners
			// are valid daemon candidates for status enrichment.
			addr := fields[1]
			if !strings.HasPrefix(addr, "127.0.0.1:") {
				continue
			}
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
		out2, err := runProbeCommand(probeCommandTimeout, "wmic", "process", "where",
			wmicWhere.String(),
			"get", "ProcessId,WorkingSetSize,CreationDate", "/format:csv")
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
// The fractional seconds are preserved so cleanup's PID-reuse proof can use a
// tighter-than-seconds tolerance.
func parseWmicDate(s string) time.Time {
	if len(s) < 14 {
		return time.Time{}
	}
	base, err := time.Parse("20060102150405", s[:14])
	if err != nil {
		return time.Time{}
	}
	nsec := 0
	pos := 14
	if pos < len(s) && s[pos] == '.' {
		pos++
		start := pos
		for pos < len(s) && s[pos] >= '0' && s[pos] <= '9' {
			pos++
		}
		frac := s[start:pos]
		if len(frac) > 9 {
			frac = frac[:9]
		}
		for len(frac) < 9 {
			frac += "0"
		}
		if frac != "" {
			if parsed, err := strconv.Atoi(frac); err == nil {
				nsec = parsed
			}
		}
	}
	loc := time.Local
	if pos+4 <= len(s) && (s[pos] == '+' || s[pos] == '-') {
		if minutes, err := strconv.Atoi(s[pos+1 : pos+4]); err == nil {
			if s[pos] == '-' {
				minutes = -minutes
			}
			loc = time.FixedZone("", minutes*60)
		}
	}
	return time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), base.Minute(), base.Second(), nsec, loc)
}

// procNameAndParent returns the image basename and parent-PID for one
// process. Used by processIdentityByPID (the install.go ownership gate
// seam). Tries wmic first; falls back to PowerShell Get-CimInstance on
// hosts where wmic is removed (Windows 11 24H2+). Both paths return
// equivalent CSV-shaped data so the parser is shared.
//
// Returns ("", 0, false) on any failure (wmic missing AND PowerShell
// missing, both queries failing, parse error, PID not found). The
// caller (portHeldByOurDaemon) treats this as fail-closed.
func procNameAndParent(pid int) (image string, parentPID int, ok bool) {
	image, parentPID, err := procNameAndParentErr(pid)
	return image, parentPID, err == nil
}

// procNameAndParentErr is the real implementation, reporting WHY it failed.
//
// The distinction is load-bearing, not cosmetic: a caller that treats "the WMI
// probe did not answer in time" the same as "the process is genuinely gone" will
// take an ownership downgrade it has not earned. procNameAndParent keeps the
// boolean shape for callers that already fail closed on any negative.
func procNameAndParentErr(pid int) (image string, parentPID int, err error) {
	if pid <= 0 {
		return "", 0, errProcessIdentityUnresolved
	}
	// ONE deadline for the whole wmic→PowerShell chain. This was the frame the
	// readiness hang was captured in: a bare cmd.Output() here is an infinite
	// WaitForSingleObject, and the caller (portHeldByOurDaemon → fixedPortStatus
	// → the GUI readiness handler) had no deadline of its own to fall back on.
	ctx, cancel := newProbeChainContext()
	defer cancel()

	timedOut := false

	// Try wmic first.
	if out, werr := runWmicNameParent(ctx, pid); werr == nil {
		if name, pp, parsed := parseNameParent(out); parsed {
			return name, pp, nil
		}
	} else if errors.Is(werr, ErrProbeTimeout) {
		timedOut = true
	}
	// PowerShell fallback (Get-CimInstance). Emits a single CSV-shaped
	// line: `Node,Name,ParentProcessId` (matching wmic's column order
	// after the leading Node column). Shares ctx with the wmic attempt above:
	// the fallback covers "wmic.exe removed on 24H2+" (which fails FAST), not
	// "wmic is slow", so it must not restart the clock.
	if out, perr := runPSNameParent(ctx, pid); perr == nil {
		if name, pp, parsed := parseNameParent(out); parsed {
			return name, pp, nil
		}
	} else if errors.Is(perr, ErrProbeTimeout) {
		timedOut = true
	}
	if timedOut {
		// At least one arm was cut by its deadline, so we did NOT establish that
		// the process is absent — we only established that we do not know.
		return "", 0, fmt.Errorf("identity probe for pid %d: %w", pid, ErrProbeTimeout)
	}
	return "", 0, errProcessIdentityUnresolved
}

// runWmicNameParent runs `wmic process where ProcessId=N get Name,ParentProcessId /format:csv`
// under the caller's chain deadline. Returns raw CSV output and the wmic
// process error (if any); a deadline cut is reported as ErrProbeTimeout so the
// caller can tell "did not answer in time" from "answered with a failure".
func runWmicNameParent(ctx context.Context, pid int) (string, error) {
	out, err := runProbeCommandCtx(ctx, "wmic", "process", "where",
		fmt.Sprintf("ProcessId=%d", pid),
		"get", "Name,ParentProcessId", "/format:csv")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// runPSNameParent runs the PowerShell equivalent of runWmicNameParent.
// Emits one row in wmic CSV shape so parseNameParent can be shared:
//
//	Node,Name,ParentProcessId
//	HOST,mcphub.exe,4200
//
// The leading Node column is hard-coded to a placeholder ("HOST") to
// match wmic's output structure exactly — the parser ignores it.
func runPSNameParent(ctx context.Context, pid int) (string, error) {
	psScript := fmt.Sprintf(
		`$p = Get-CimInstance Win32_Process -Filter 'ProcessId=%d'; if ($p) { '{0},{1},{2}' -f 'HOST', $p.Name, $p.ParentProcessId }`,
		pid,
	)
	out, err := runProbeCommandCtx(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	if err != nil {
		return "", err
	}
	// Prepend header line so the shared parser sees the expected shape.
	return "Node,Name,ParentProcessId\n" + string(out), nil
}

// parseNameParent parses wmic-shaped CSV output:
//
//	Node,Name,ParentProcessId
//	(blank)
//	HOST,mcphub.exe,4200
//
// Returns the first non-header, non-blank row's Name + ParentProcessId.
// Empty Name → parsed=false. Non-numeric parent PID → name parsed, PID=0.
func parseNameParent(out string) (name string, parentPID int, parsed bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Node,") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		nm := strings.TrimSpace(fields[len(fields)-2])
		ppStr := strings.TrimSpace(fields[len(fields)-1])
		if nm == "" {
			continue
		}
		pp, _ := strconv.Atoi(ppStr)
		return nm, pp, true
	}
	return "", 0, false
}
