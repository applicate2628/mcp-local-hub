package lastfailure

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// This file scans a vcpkg buildtrees layout directly — the PRIMARY,
// always-available source (see package doc). Naming pattern grounded on a
// real observed directory this session (r:\b\wingpl\boost-algorithm\, a
// surviving buildtree from an actual vcpkg run; the shorter-lived
// r:\b\wingpl\sqlite3\ for the "port dir exists but the build never
// reached extract" degenerate case):
//
//	<buildtrees-root>/<port>/
//	  extract-out.log                                  (phase=extract, no triplet token)
//	  extract-err.log
//	  config-<triplet>-out.log                         (phase=config, whole-config-step out/err)
//	  config-<triplet>-err.log
//	  config-<triplet>-<cfg>-ninja.log                 (phase=config, per-artifact raw dump —
//	  config-<triplet>-<cfg>-CMakeCache.txt.log         listed in log_paths, NOT diagnostic-scanned:
//	  config-<triplet>-<cfg>-CMakeConfigureLog.yaml.log these are build-plan/cache dumps, not
//	                                                     compiler/linker output)
//	  install-<triplet>-<cfg>-out.log                  (phase=install; vcpkg's ninja invocation
//	  install-<triplet>-<cfg>-err.log                   both COMPILES and installs in one step, so
//	                                                     a compile error lands here too — see Phase
//	                                                     doc comment in types.go)
//	  stdout-<triplet>.log                              (top-level per-port narration; scanned last,
//	                                                     as a bonus/fallback source)
//	  <triplet>.vcpkg_abi_info.txt                      (not a phase log; used only to detect triplet)
//
// <cfg> is a build configuration token (rel/dbg observed; "expect dbg too"
// per the increment brief — this scanner does not hardcode an enum of
// config tokens, it discovers whatever is actually on disk for the known
// triplet, see classifyPortDir).
//
// Triplet is NEVER reverse-parsed out of a dash-joined filename (a vcpkg
// triplet itself commonly contains dashes, e.g. "x64-windows", which would
// make naive dash-splitting ambiguous). Instead, when the triplet is not
// already known (explicit param / wrapper), it is recovered from the two
// filename shapes that have exactly one unambiguous free segment:
// "stdout-<triplet>.log" and "<triplet>.vcpkg_abi_info.txt" — see
// detectTripletFromPortDir.

// FS abstracts the filesystem calls this package needs, so tests exercise
// fixtures under testdata/ without ever touching the real machine.
//
// Open and OpenDir make both byte streams and directory enumeration bounded
// before materialization.
type FS interface {
	Stat(path string) (os.FileInfo, error)
	Open(path string) (io.ReadCloser, error)
	OpenDir(path string) (DirReader, error)
}

// DirReader remains a package alias for compatibility with the existing
// deterministic test seams. Generic paging and close ownership live in
// boundedio.
type DirReader = boundedio.DirReader

type osFS struct{}

func (osFS) Stat(p string) (os.FileInfo, error)   { return os.Stat(p) }
func (osFS) Open(p string) (io.ReadCloser, error) { return os.Open(p) }
func (osFS) OpenDir(p string) (DirReader, error)  { return os.Open(p) }

// DefaultFS wires FS to the real OS.
func DefaultFS() FS { return osFS{} }

// maxLogBytes bounds how much of ONE phase log this package streams.
//
// A buildtrees log is attacker-shaped only in the loosest sense, but it IS
// unbounded in size: a verbose nested build can emit hundreds of megabytes,
// and the previous code read every phase log with a single whole-file
// ReadFile before any cap applied — the 50-diagnostic output cap bounded the
// ANSWER, never the allocation. 32 MiB is far above every log observed in the
// scout pass over 618 real files (largest: a few MiB) while keeping the worst
// case per call bounded.
//
// Exceeding the limit is NOT silently truncated into a confident verdict: the
// unread tail could hold a later error, or an interrupt marker that turns a
// failure into a stopped build, so the caller reports
// unknown(phase_log_size_limit_exceeded) — the same fail-closed rule already
// applied to an unreadable log.
const maxLogBytes int64 = 32 << 20

// readMetadataLimited applies a limit+sentinel bound before materializing the
// wrapper/configuration documents that require a complete byte slice to parse.
// Phase logs use phaseLogStreamScanner instead and are never materialized.
func readMetadataLimited(fsys FS, path string, limit int64) ([]byte, bool, error) {
	return readMetadataLimitedContext(context.Background(), fsys, path, limit)
}

func readMetadataLimitedContext(ctx context.Context, fsys FS, path string, limit int64) ([]byte, bool, error) {
	result, err := boundedio.ReadFile(ctx, fsys, path, limit, 64<<10)
	if err != nil {
		return nil, false, err
	}
	return result.Data, result.Limited, nil
}

const directoryReadPageEntries = 128

// readDirBounded delegates generic paging, terminal-request arithmetic, exact
// close ownership, and whole-directory overflow omission to boundedio.
func readDirBounded(ctx context.Context, fsys FS, path string, limit int) ([]os.DirEntry, bool, error) {
	result, err := boundedio.ReadDirComplete(ctx, fsys, path, limit, directoryReadPageEntries)
	if err != nil {
		return nil, false, err
	}
	return result.Entries, result.Limited, nil
}

// probeDir reports whether path is an existing directory, TRI-STATE.
//
// The boolean predecessor of this function returned `err == nil &&
// fi.IsDir()`, which mapped a permission-denied / sharing-violation /
// transient-I/O Stat onto the same `false` as a genuinely absent directory.
// Callers then reported that false as unknown(buildtrees_cleaned) or
// unknown(port_dir_not_found) — two VERIFIED-absence claims manufactured out
// of a failure to look. Those reasons are now reserved for
// evidence.PresenceAbsent, and PresenceUnreadable gets its own reason.
func probeDir(fsys FS, path string) (evidence.Presence, error) {
	return evidence.ProbeDir(fsys.Stat, path)
}

// phaseLogFile is one classified phase-log file inside a port directory.
type phaseLogFile struct {
	Phase  Phase
	Stream string // "out" | "err"
	Config string // "" | observed config token (rel, dbg, ...)
	Path   string
}

// detectTripletFromPortDir recovers the triplet from the two unambiguous
// marker-file shapes in portDir. Returns ok=false (not an error) when
// nothing matched; returns candidates when MORE THAN ONE distinct triplet
// value was found (never silently picks one).
func detectTripletFromPortDir(ctx context.Context, fsys FS, portDir string, limit int) (triplet string, candidates []string, ok, limitExceeded bool, err error) {
	entries, limitExceeded, err := readDirBounded(ctx, fsys, portDir, limit)
	if err != nil {
		return "", nil, false, false, err
	}
	if limitExceeded {
		return "", nil, false, true, nil
	}
	found := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "stdout-") && strings.HasSuffix(name, ".log"):
			t := strings.TrimSuffix(strings.TrimPrefix(name, "stdout-"), ".log")
			if t != "" {
				if found[t] || len(found) < 2 {
					found[t] = true
				}
			}
		case strings.HasSuffix(name, ".vcpkg_abi_info.txt"):
			t := strings.TrimSuffix(name, ".vcpkg_abi_info.txt")
			if t != "" {
				if found[t] || len(found) < 2 {
					found[t] = true
				}
			}
		}
	}
	switch len(found) {
	case 0:
		return "", nil, false, false, nil
	case 1:
		for t := range found {
			return t, nil, true, false, nil
		}
	}
	for t := range found {
		candidates = append(candidates, t)
	}
	sort.Strings(candidates)
	return "", candidates, false, false, nil
}

type portDirClassification struct {
	phases                     []phaseLogFile
	otherLogPaths              []string
	configureLogYAMLPaths      []string
	stdoutNarration            string
	directoryLimitExceeded     bool
	relevantLogLimitExceeded   bool
	relevantLogsDroppedAtLeast int
	otherLogPathsDropped       int
	entriesExamined            int
	relevantLogsRetained       int
}

// classifyPortDir lists portDir and classifies every recognizable log file
// for the given (already-known) triplet. otherLogPaths collects every
// other *.log/*.txt file in the directory (artifact dumps, abi info) so
// LogPaths always includes them even though they are not diagnostic-scanned
// as a PRIMARY source; configureLogYAMLPaths is the subset of those that
// are CMakeConfigureLog.yaml.log artifacts — callers may scan these as a
// last resort, but per the scout-pass finding, a diagnostic recovered there
// can belong to a try_compile CAPABILITY PROBE rather than the port's real
// build, so it is kept separate rather than silently folded into the
// primary phase scan.
func classifyPortDir(ctx context.Context, fsys FS, portDir, triplet string, limits responseLimits) (out portDirClassification, err error) {
	entries, exceeded, err := readDirBounded(ctx, fsys, portDir, limits.directoryEntries)
	if err != nil {
		return out, err
	}
	out.directoryLimitExceeded = exceeded
	out.entriesExamined = len(entries)
	if exceeded {
		// boundedio intentionally omits the overflowing directory prefix.
		// High-water reporting still records the admitted semantic ceiling,
		// not the zero public entries returned after whole-directory omission.
		out.entriesExamined = limits.directoryEntries
		return out, nil
	}

	configOutName := "config-" + triplet + "-out.log"
	configErrName := "config-" + triplet + "-err.log"
	installPrefix := "install-" + triplet + "-"
	patchPrefix := "patch-" + triplet + "-"
	// buildPrefix classifies build-<triplet>-<cfg>-{out,err}.log, the log a
	// NON-ninja build step writes (autotools/make/NMAKE ports, and any port
	// whose vcpkg_build_* helper runs a separate build command). This was
	// previously unclassified and therefore NEVER diagnostic-scanned: the
	// file still appeared in log_paths (it fell through to otherLogPaths), so
	// the answer LOOKED complete while the real error was never read.
	// Verified field failure: three real libmesh:cl failures whose
	// build-cl-dbg-err.log held an unambiguous first error each time, all
	// reported as unknown(no_diagnostic_found).
	buildPrefix := "build-" + triplet + "-"
	stdoutName := "stdout-" + triplet + ".log"
	relevantSeen := 0
	addPhase := func(file phaseLogFile) {
		relevantSeen++
		if relevantSeen <= limits.relevantLogs {
			out.phases = append(out.phases, file)
		}
	}
	otherPaths := newBoundedStringCollector(limits.listEntries, limits.pathBytes)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		full := filepath.Join(portDir, name)

		switch {
		case name == "extract-out.log":
			addPhase(phaseLogFile{Phase: PhaseExtract, Stream: "out", Path: full})
		case name == "extract-err.log":
			addPhase(phaseLogFile{Phase: PhaseExtract, Stream: "err", Path: full})
		case name == configOutName:
			addPhase(phaseLogFile{Phase: PhaseConfig, Stream: "out", Path: full})
		case name == configErrName:
			addPhase(phaseLogFile{Phase: PhaseConfig, Stream: "err", Path: full})
		case name == stdoutName:
			relevantSeen++
			if relevantSeen <= limits.relevantLogs {
				out.stdoutNarration = full
			}
		case strings.HasPrefix(name, installPrefix) && strings.HasSuffix(name, "-out.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, installPrefix), "-out.log")
			addPhase(phaseLogFile{Phase: PhaseInstall, Stream: "out", Config: cfg, Path: full})
		case strings.HasPrefix(name, installPrefix) && strings.HasSuffix(name, "-err.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, installPrefix), "-err.log")
			addPhase(phaseLogFile{Phase: PhaseInstall, Stream: "err", Config: cfg, Path: full})
		case strings.HasPrefix(name, buildPrefix) && strings.HasSuffix(name, "-out.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, buildPrefix), "-out.log")
			addPhase(phaseLogFile{Phase: PhaseBuild, Stream: "out", Config: cfg, Path: full})
		case strings.HasPrefix(name, buildPrefix) && strings.HasSuffix(name, "-err.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, buildPrefix), "-err.log")
			addPhase(phaseLogFile{Phase: PhaseBuild, Stream: "err", Config: cfg, Path: full})
		case strings.HasPrefix(name, patchPrefix) && strings.HasSuffix(name, "-out.log"):
			// Config field repurposed to hold the 0-based patch ordinal N
			// (patch-<triplet>-<N>-out.log) — same "extra descriptor" slot
			// build-config normally occupies, never both at once.
			ord := strings.TrimSuffix(strings.TrimPrefix(name, patchPrefix), "-out.log")
			addPhase(phaseLogFile{Phase: PhasePatch, Stream: "out", Config: ord, Path: full})
		case strings.HasPrefix(name, patchPrefix) && strings.HasSuffix(name, "-err.log"):
			ord := strings.TrimSuffix(strings.TrimPrefix(name, patchPrefix), "-err.log")
			addPhase(phaseLogFile{Phase: PhasePatch, Stream: "err", Config: ord, Path: full})
		case strings.HasSuffix(name, "CMakeConfigureLog.yaml.log"):
			relevantSeen++
			if relevantSeen <= limits.relevantLogs {
				out.configureLogYAMLPaths = append(out.configureLogYAMLPaths, full)
			}
			otherPaths.add(full)
		case strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".txt"):
			// Every other artifact (config-<triplet>-<cfg>-ninja.log,
			// -CMakeCache.txt.log, -CMakeConfigureLog.yaml.log,
			// <triplet>.vcpkg_abi_info.txt, a different triplet's
			// leftovers, ...): not diagnostic-scanned, but always
			// surfaced in log_paths per the design doc invariant.
			otherPaths.add(full)
		}
	}
	phaseIndex := func(ph Phase) int {
		switch ph {
		case PhaseExtract:
			return 0
		case PhasePatch:
			return 1
		case PhaseConfig:
			return 2
		case PhaseBuild:
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(out.phases, func(i, j int) bool {
		pi, pj := phaseIndex(out.phases[i].Phase), phaseIndex(out.phases[j].Phase)
		if pi != pj {
			return pi < pj
		}
		return filepath.Base(out.phases[i].Path) < filepath.Base(out.phases[j].Path)
	})
	out.otherLogPaths = otherPaths.values
	out.otherLogPathsDropped = otherPaths.dropped
	out.relevantLogsRetained = min(relevantSeen, limits.relevantLogs)
	if relevantSeen > limits.relevantLogs {
		out.relevantLogLimitExceeded = true
		out.relevantLogsDroppedAtLeast = relevantSeen - limits.relevantLogs
		return out, nil
	}
	return out, nil
}
