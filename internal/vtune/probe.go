package vtune

import (
	"os/exec"
	"strings"

	"mcp-local-hub/internal/process"
)

// versionProbeFunc resolves and probes vtune.exe for the vtune_status tool: it
// returns the resolved vtune path, the first line of `<vtune> --version`, a
// best-effort note about the SEP hardware-sampling-driver readiness, and
// whether the probe succeeded. The production implementation is
// defaultVersionProbe (a real exec.Command — which WORKS in the console-less
// daemon, unlike the python-subprocess probes other servers struggled with);
// tests inject a fake so they never run the real vtune.exe.
//
// sepNote is intentionally a free-form STRING, not a fabricated boolean:
// reliably determining SEP-driver readiness needs either a privileged driver
// query or a full sampling-collect probe, neither of which vtune_status should
// do. Instead the note reports what `vtune --version` advertises about the
// driver (VTune prints a "Sampling Driver" / "amplxe-driver" status line in
// some builds) and otherwise records the well-known fact that user-mode
// "hotspots"/"threading" need NO driver while the hardware-event analyses do.
type versionProbeFunc func() (path, version, sepNote string, available bool)

// listAnalysesFunc resolves vtune.exe and asks it for the host's ACTUAL
// supported collect (analysis) types via `vtune -collect-list`, returning the
// resolved path, the parsed list of analysis-type ids the host advertises, the
// raw `-collect-list` output (for diagnosis when parsing finds nothing), and
// whether the probe succeeded. The production implementation is
// defaultListAnalyses; tests inject a fake so they never run the real vtune.exe.
type listAnalysesFunc func() (path string, analyses []string, raw string, available bool)

// defaultVersionProbe resolves vtune.exe (via the findVTune discovery seam) and
// runs `<vtune> --version` through Go exec, returning the path, the first line
// of the version banner, a best-effort SEP-driver note, and whether the probe
// succeeded. The Go exec works in the console-less mcphub daemon.
func defaultVersionProbe() (string, string, string, bool) {
	path, err := findVTune()
	if err != nil {
		// Not installed: surface the install-guidance error text as the
		// "version" so the caller sees WHY it is unavailable, with available=false.
		return "", err.Error(), sepDriverNote(""), false
	}
	cmd := exec.Command(path, "--version")
	process.NoConsole(cmd)
	cmd.Env = vtuneEnv()
	out, err := cmd.Output()
	if err != nil {
		return path, "", sepDriverNote(""), false
	}
	version := firstNonEmptyLine(string(out))
	return path, version, sepDriverNote(string(out)), true
}

// defaultListAnalyses resolves vtune.exe and runs `<vtune> -collect-list`,
// parsing the host's advertised collect (analysis) type ids out of the output.
// Returns the resolved path, the parsed list, the raw output, and whether the
// probe succeeded.
func defaultListAnalyses() (string, []string, string, bool) {
	path, err := findVTune()
	if err != nil {
		return "", nil, err.Error(), false
	}
	cmd := exec.Command(path, "-collect-list")
	process.NoConsole(cmd)
	cmd.Env = vtuneEnv()
	out, err := cmd.Output()
	if err != nil {
		return path, nil, string(out), false
	}
	raw := string(out)
	return path, parseCollectList(raw), raw, true
}

// parseCollectList extracts the analysis-type ids from `vtune -collect-list`
// output. VTune prints one analysis per stanza; the id is the first token on a
// line, left-indented, followed by a description, e.g.:
//
//	hotspots
//	    Analyze application flow and identify sections of code that take a long
//	    time to execute (hotspots).
//	memory-access
//	    Analyze memory accesses ...
//	uarch-exploration
//	    ...
//
// So an analysis-id line is a NON-indented line whose first token matches the
// VTune analysis-id shape (lowercase letters / digits / hyphens). Description
// continuation lines are indented and are skipped. This is a tolerant parser:
// it never errors, it just yields whatever ids it can recognize so a format
// drift degrades to "fewer ids surfaced" rather than a hard failure.
func parseCollectList(raw string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		trimmedRight := strings.TrimRight(line, "\r\n")
		// Indented lines are descriptions / continuations — skip them.
		if trimmedRight == "" || trimmedRight[0] == ' ' || trimmedRight[0] == '\t' {
			continue
		}
		// The id is the first whitespace-delimited token on a non-indented line.
		fields := strings.Fields(trimmedRight)
		if len(fields) == 0 {
			continue
		}
		id := fields[0]
		if !looksLikeAnalysisID(id) {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// looksLikeAnalysisID reports whether token has the shape of a VTune analysis
// id: a non-empty run of lowercase letters, digits, and hyphens (e.g.
// "hotspots", "memory-access", "uarch-exploration"). This filters out header
// lines ("Available analysis types:") and stray punctuation that would
// otherwise be mistaken for an id.
func looksLikeAnalysisID(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// sepDriverNote derives a best-effort, human-readable note about the SEP
// hardware-sampling-driver readiness from the `vtune --version` banner. It does
// NOT fabricate a boolean: it reports any driver-status line VTune printed, and
// always appends the load-bearing operator guidance (user-mode hotspots /
// threading need no driver; the hardware-event analyses do). versionOut may be
// empty (the probe failed before capturing output), in which case only the
// generic guidance is returned.
func sepDriverNote(versionOut string) string {
	const guidance = "user-mode analyses (hotspots, threading) need no SEP driver; " +
		"hardware-event analyses (memory-access, uarch-exploration, memory-consumption) " +
		"require the SEP sampling driver / admin — run `vtune -collect hotspots` once to confirm driver state"
	for _, line := range strings.Split(versionOut, "\n") {
		l := strings.TrimSpace(line)
		ll := strings.ToLower(l)
		if strings.Contains(ll, "driver") || strings.Contains(ll, "sampling") {
			return l + " — " + guidance
		}
	}
	return guidance
}

// firstNonEmptyLine returns the first non-blank line of s (the vtune version
// line captured from `vtune --version`), trimmed of trailing whitespace.
// Mirrors the gdb session helper of the same name.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}
