package lastfailure

import (
	"os"
	"path/filepath"
	"strings"
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

// dirExists reports whether path exists and is a directory.
func dirExists(fsys FS, path string) bool {
	fi, err := fsys.Stat(path)
	return err == nil && fi.IsDir()
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
// LogPaths always includes them even though they are not diagnostic-scanned.
func classifyPortDir(fsys FS, portDir, triplet string) (phases []phaseLogFile, otherLogPaths []string, stdoutNarration string, err error) {
	entries, err := fsys.ReadDir(portDir)
	if err != nil {
		return nil, nil, "", err
	}

	configOutName := "config-" + triplet + "-out.log"
	configErrName := "config-" + triplet + "-err.log"
	installPrefix := "install-" + triplet + "-"
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
		case strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".txt"):
			// Every other artifact (config-<triplet>-<cfg>-ninja.log,
			// -CMakeCache.txt.log, -CMakeConfigureLog.yaml.log,
			// <triplet>.vcpkg_abi_info.txt, a different triplet's
			// leftovers, ...): not diagnostic-scanned, but always
			// surfaced in log_paths per the design doc invariant.
			otherLogPaths = append(otherLogPaths, full)
		}
	}
	return phases, otherLogPaths, stdoutNarration, nil
}
