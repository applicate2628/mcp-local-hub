//go:build windows

package oneapirun

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/oneapi"
	"mcp-local-hub/internal/process"
)

// captureVSEnvCached is the production captureVSEnv seam. setvars.bat is slow
// (~1-3 s) and its environment is stable for the process lifetime, so the
// capture runs at most once per process (sync.Once). Concurrent first callers
// all block on the same capture and then share the cached result.
var (
	vsEnvOnce   sync.Once
	vsEnvCached []string
	vsEnvOK     bool
)

func captureVSEnvCached() ([]string, bool) {
	vsEnvOnce.Do(func() {
		vsEnvCached, vsEnvOK = captureVSEnvUncached()
	})
	return vsEnvCached, vsEnvOK
}

// captureVSEnvUncached locates the Intel oneAPI setvars.bat and captures the
// COMPLETE build+run environment it sets: the Visual-Studio toolchain (setvars
// initializes VS internally) PLUS every installed oneAPI component (compiler,
// mkl, tbb, ipp, mpi, …). The captured env therefore carries the full PATH
// (runtime DLL dirs), LIB (link libs incl. libircmt.lib + the MKL import
// libs), INCLUDE (headers incl. mkl.h), and the component root vars
// (MKLROOT/TBBROOT/CMPLR_ROOT/CPATH/…) — everything a native compile, link, or
// run needs. Returns (env, true) on success; (nil, false) on any failure
// (setvars not found, the batch failing, an unparseable dump) so the caller
// falls back cleanly.
//
// WHY setvars.bat instead of hand-enumerated dirs: capturing the environment
// the vendor's own script sets is complete, version-correct, and future-proof
// — it picks up every component and every variable (MKLROOT, CPATH, …) a real
// oneAPI dev shell has, which a hand-picked PATH/LIB/INCLUDE enumeration would
// miss (e.g. an earlier design prepended only the runtime DLL dirs to PATH, so
// `icx-cl` could RUN a prebuilt MKL exe but failed to LINK — "LNK1104: cannot
// open file 'libircmt.lib'" — and failed to COMPILE an MKL TU — "mkl.h: No
// such file or directory"). An earlier note claimed setvars was "broken on the
// host"; re-verified false — `setvars.bat --force` exits 0 and configures VS +
// all components correctly. The fast hand-enumerated oneapi.DLLDirs path is
// retained only for the runtime-only consumers (the supervisor debugger
// injector + drmemory's target spawn) and as this capture's degraded fallback.
func captureVSEnvUncached() ([]string, bool) {
	setvars, ok := findSetvars()
	if !ok {
		return nil, false
	}
	env, ok := captureSetvarsEnv(setvars)
	if !ok {
		return nil, false
	}
	return env, true
}

// findSetvars locates the Intel oneAPI setvars.bat at the install root
// (oneapi.DetectRoot() + "\setvars.bat"). Returns ("", false) when no oneAPI
// install is found or the script is missing.
func findSetvars() (string, bool) {
	root, ok := oneapi.DetectRoot()
	if !ok {
		return "", false
	}
	setvars := filepath.Join(root, "setvars.bat")
	if fileExists(setvars) {
		return setvars, true
	}
	return "", false
}

// captureSetvarsEnv runs setvars.bat and dumps the resulting environment.
//
// It writes a tiny temp .bat (`call "<setvars>" --force >NUL` then `set`) and
// runs `cmd /c <tempbat>`, rather than an inline `cmd /c ""<setvars>" ... &&
// set"`. The inline form is fragile: Go's exec.Command escapes the cmdLine
// argument for the MS C-runtime, and cmd /c's own first/last-quote-stripping
// rule then mangles the quoted, space-bearing setvars path (e.g.
// "C:\Program Files (x86)\Intel\oneAPI\setvars.bat"), so setvars never runs
// and the capture fails. Quoting the path INSIDE the .bat sidesteps both
// layers — cmd parses it natively, no Go escaping involved. `--force` makes
// setvars configure the environment even when the inherited env already
// carries SETVARS_COMPLETED (otherwise oneAPI's own re-entry guard would make
// it a no-op and the dump would lack the component dirs).
//
// Returns (env, true) on success; (nil, false) when the temp file cannot be
// written, the command fails to run, or its output has no parseable
// KEY=VALUE lines.
func captureSetvarsEnv(setvars string) ([]string, bool) {
	bat, err := os.CreateTemp("", "mcphub-setvars-*.bat")
	if err != nil {
		return nil, false
	}
	batPath := bat.Name()
	defer os.Remove(batPath)
	// CRLF so cmd parses the batch reliably; `call` so control returns to
	// dump `set` after setvars finishes; banner discarded to NUL.
	//
	// `set "NoDefaultCurrentDirectoryInExePath="` is LOAD-BEARING: the
	// supervisor spawns this daemon with NoDefaultCurrentDirectoryInExePath=1
	// (a hardening default), which tells cmd NOT to search the current
	// directory for executables. setvars.bat initializes each oneAPI component
	// by `pushd`-ing into the component's env dir and invoking its vars.bat as
	// a BARE command (`call vars.bat`), which resolves only via the
	// current-directory search — so with the flag set, EVERY component fails
	// with "'vars.bat' is not recognized" and only the Visual-Studio half of
	// the environment is configured (MKLROOT empty, mkl.h / libircmt.lib
	// unreachable). Diagnosed live: clearing the var makes setvars exit 0 and
	// configure compiler + mkl + tbb + … completely. Clearing it only inside
	// this capture sub-cmd does not affect the daemon's own process env.
	content := "@echo off\r\nset \"NoDefaultCurrentDirectoryInExePath=\"\r\ncall \"" + setvars + "\" --force > NUL 2>&1\r\nset\r\n"
	if _, err := bat.WriteString(content); err != nil {
		_ = bat.Close()
		return nil, false
	}
	if err := bat.Close(); err != nil {
		return nil, false
	}
	cmd := exec.Command("cmd", "/c", batPath)
	process.NoConsole(cmd) // suppress console flash on windowsgui parent
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	env := parseSetDump(out)
	if len(env) == 0 {
		return nil, false
	}
	return env, true
}

// parseSetDump parses the output of cmd's `set` command — one KEY=VALUE
// per line — into a "KEY=VALUE" slice. Lines without '=' (or with an empty
// KEY) are skipped. Values may legitimately contain '=' (e.g. some VS vars),
// so only the FIRST '=' splits the key; the remainder is the value verbatim.
// CRLF line endings are trimmed.
func parseSetDump(out []byte) []string {
	var env []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	// `set` output lines stay well within the default 64 KiB scanner token
	// limit; a pathological super-long PATH could exceed it, so raise the
	// buffer to 1 MiB to be safe.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if line == "" {
			continue
		}
		key, _, hasEq := strings.Cut(line, "=")
		if !hasEq || key == "" {
			continue
		}
		env = append(env, line)
	}
	return env
}

// fileExists reports whether path names an existing regular file (not a
// directory). Used to confirm setvars.bat before invoking.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
