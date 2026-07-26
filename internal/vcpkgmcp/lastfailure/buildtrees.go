package lastfailure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
type FS interface {
	Stat(path string) (os.FileInfo, error)
	ReadDir(path string) ([]os.DirEntry, error)
	ReadFile(path string) ([]byte, error)
}

type osFS struct{}

func (osFS) Stat(p string) (os.FileInfo, error)      { return os.Stat(p) }
func (osFS) ReadDir(p string) ([]os.DirEntry, error) { return os.ReadDir(p) }
func (osFS) ReadFile(p string) ([]byte, error)       { return os.ReadFile(p) }

// DefaultFS wires FS to the real OS.
func DefaultFS() FS { return osFS{} }

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

// portNameRE is the documented vcpkg port-name rule: "The name must be
// lowercase ASCII letters, digits, or hyphens (-). It must not start nor end
// with a hyphen." (Microsoft Learn, vcpkg.json Reference, "name" field:
// https://learn.microsoft.com/en-us/vcpkg/reference/vcpkg-json).
//
// A port name is used as ONE path segment under the buildtrees root, so it
// must be validated as one before being joined: `filepath.Join(root, port)`
// happily normalises `..\outside` into a sibling of the root, which would
// make this tool scan, read and report logs from OUTSIDE the explicit root
// the caller granted it.
var portNameRE = regexp.MustCompile(`^[a-z0-9]+(?:-+[a-z0-9]+)*$`)

// errPortEscapesRoot marks a port whose joined path leaves buildtreesRoot.
var errPortEscapesRoot = errors.New("port path escapes the buildtrees root")

// portDirWithin validates port as a single legal vcpkg port-name segment and
// returns its directory under buildtreesRoot, guaranteeing the result stays
// beneath that root.
//
// Both checks are kept, deliberately: the name rule rejects the input shape,
// and the containment check is the actual security boundary — it holds even
// if the name rule is ever loosened, and it catches platform-specific path
// normalisation (Windows alternate separators, trailing dots/spaces, 8.3
// aliases) that a charset regex alone cannot reason about.
func portDirWithin(buildtreesRoot, port string) (string, error) {
	if !portNameRE.MatchString(port) {
		return "", fmt.Errorf("%q is not a legal vcpkg port name "+
			"(lowercase ASCII letters, digits and hyphens; must not start or end with a hyphen)", port)
	}
	root := filepath.Clean(buildtreesRoot)
	joined := filepath.Clean(filepath.Join(root, port))
	rel, err := filepath.Rel(root, joined)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPortEscapesRoot, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q resolves to %q, outside %q", errPortEscapesRoot, port, joined, root)
	}
	return joined, nil
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
func detectTripletFromPortDir(fsys FS, portDir string) (triplet string, candidates []string, ok bool) {
	entries, err := fsys.ReadDir(portDir)
	if err != nil {
		return "", nil, false
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
				found[t] = true
			}
		case strings.HasSuffix(name, ".vcpkg_abi_info.txt"):
			t := strings.TrimSuffix(name, ".vcpkg_abi_info.txt")
			if t != "" {
				found[t] = true
			}
		}
	}
	switch len(found) {
	case 0:
		return "", nil, false
	case 1:
		for t := range found {
			return t, nil, true
		}
	}
	for t := range found {
		candidates = append(candidates, t)
	}
	return "", candidates, false
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
func classifyPortDir(fsys FS, portDir, triplet string) (phases []phaseLogFile, otherLogPaths []string, configureLogYAMLPaths []string, stdoutNarration string, err error) {
	entries, err := fsys.ReadDir(portDir)
	if err != nil {
		return nil, nil, nil, "", err
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

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		full := filepath.Join(portDir, name)

		switch {
		case name == "extract-out.log":
			phases = append(phases, phaseLogFile{Phase: PhaseExtract, Stream: "out", Path: full})
		case name == "extract-err.log":
			phases = append(phases, phaseLogFile{Phase: PhaseExtract, Stream: "err", Path: full})
		case name == configOutName:
			phases = append(phases, phaseLogFile{Phase: PhaseConfig, Stream: "out", Path: full})
		case name == configErrName:
			phases = append(phases, phaseLogFile{Phase: PhaseConfig, Stream: "err", Path: full})
		case name == stdoutName:
			stdoutNarration = full
		case strings.HasPrefix(name, installPrefix) && strings.HasSuffix(name, "-out.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, installPrefix), "-out.log")
			phases = append(phases, phaseLogFile{Phase: PhaseInstall, Stream: "out", Config: cfg, Path: full})
		case strings.HasPrefix(name, installPrefix) && strings.HasSuffix(name, "-err.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, installPrefix), "-err.log")
			phases = append(phases, phaseLogFile{Phase: PhaseInstall, Stream: "err", Config: cfg, Path: full})
		case strings.HasPrefix(name, buildPrefix) && strings.HasSuffix(name, "-out.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, buildPrefix), "-out.log")
			phases = append(phases, phaseLogFile{Phase: PhaseBuild, Stream: "out", Config: cfg, Path: full})
		case strings.HasPrefix(name, buildPrefix) && strings.HasSuffix(name, "-err.log"):
			cfg := strings.TrimSuffix(strings.TrimPrefix(name, buildPrefix), "-err.log")
			phases = append(phases, phaseLogFile{Phase: PhaseBuild, Stream: "err", Config: cfg, Path: full})
		case strings.HasPrefix(name, patchPrefix) && strings.HasSuffix(name, "-out.log"):
			// Config field repurposed to hold the 0-based patch ordinal N
			// (patch-<triplet>-<N>-out.log) — same "extra descriptor" slot
			// build-config normally occupies, never both at once.
			ord := strings.TrimSuffix(strings.TrimPrefix(name, patchPrefix), "-out.log")
			phases = append(phases, phaseLogFile{Phase: PhasePatch, Stream: "out", Config: ord, Path: full})
		case strings.HasPrefix(name, patchPrefix) && strings.HasSuffix(name, "-err.log"):
			ord := strings.TrimSuffix(strings.TrimPrefix(name, patchPrefix), "-err.log")
			phases = append(phases, phaseLogFile{Phase: PhasePatch, Stream: "err", Config: ord, Path: full})
		case strings.HasSuffix(name, "CMakeConfigureLog.yaml.log"):
			configureLogYAMLPaths = append(configureLogYAMLPaths, full)
			otherLogPaths = append(otherLogPaths, full)
		case strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".txt"):
			// Every other artifact (config-<triplet>-<cfg>-ninja.log,
			// -CMakeCache.txt.log, -CMakeConfigureLog.yaml.log,
			// <triplet>.vcpkg_abi_info.txt, a different triplet's
			// leftovers, ...): not diagnostic-scanned, but always
			// surfaced in log_paths per the design doc invariant.
			otherLogPaths = append(otherLogPaths, full)
		}
	}
	return phases, otherLogPaths, configureLogYAMLPaths, stdoutNarration, nil
}
