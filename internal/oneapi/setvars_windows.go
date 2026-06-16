//go:build windows

package oneapi

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/process"
)

// SetvarsEnv captures and returns the COMPLETE Visual-Studio + Intel-oneAPI
// build/run environment that oneAPI's setvars.bat configures: the VS toolchain
// (setvars initializes VS internally) PLUS every installed oneAPI component
// (compiler, mkl, tbb, ipp, mpi, …). The captured env carries the full PATH
// (runtime DLL dirs for ALL components — broader than the hand-enumerated
// DLLDirs, which only covers each "<component>\latest\bin"), LIB (link libs),
// INCLUDE (headers), and the component root vars (MKLROOT/CMPLR_ROOT/CPATH/…).
//
// Returns (env, true) on success; (nil, false) on any failure (setvars not
// found, the batch failing, or an unparseable dump) so the caller falls back
// cleanly (e.g. to os.Environ() + DLLDirs). The capture runs a ~1-3 s
// subprocess at most ONCE per process (sync.Once) since the environment is
// stable for the process lifetime; concurrent first callers share the result.
//
// This is the single owner of the full-environment capture, shared by the
// oneapi-run command runner (which needs LIB/INCLUDE to compile + link) and
// drmemory's instrumented-target spawn (which needs the complete runtime PATH
// so an icx-built target loads EVERY oneAPI DLL it links, not just those under
// "<component>\latest\bin"). Non-Windows hosts get the (nil, false) stub in
// setvars_other.go.
func SetvarsEnv() ([]string, bool) {
	setvarsOnce.Do(func() {
		setvarsCached, setvarsOK = captureSetvarsUncached()
	})
	return setvarsCached, setvarsOK
}

var (
	setvarsOnce   sync.Once
	setvarsCached []string
	setvarsOK     bool
)

func captureSetvarsUncached() ([]string, bool) {
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
// (DetectRoot() + "\setvars.bat"). Returns ("", false) when no oneAPI install
// is found or the script is missing.
func findSetvars() (string, bool) {
	root, ok := DetectRoot()
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
// It writes a tiny temp .bat (`set NoDefaultCurrentDirectoryInExePath=` then
// `call "<setvars>" --force >NUL` then `set`) and runs `cmd /c <tempbat>`,
// rather than an inline `cmd /c ""<setvars>" ... && set"`. The inline form is
// fragile: Go's exec.Command escapes the cmdLine argument for the MS C-runtime,
// and cmd /c's own first/last-quote-stripping rule then mangles the quoted,
// space-bearing setvars path, so setvars never runs. Quoting the path INSIDE
// the .bat sidesteps both layers.
//
// `set "NoDefaultCurrentDirectoryInExePath="` is LOAD-BEARING: the supervisor
// spawns these daemons with NoDefaultCurrentDirectoryInExePath=1 (a hardening
// default), which tells cmd NOT to search the current directory for
// executables. setvars.bat initializes each oneAPI component by `pushd`-ing
// into the component's env dir and invoking its vars.bat as a BARE command
// (`call vars.bat`), resolved only via current-directory search — so with the
// flag set, EVERY component fails with "'vars.bat' is not recognized" and only
// the Visual-Studio half of the environment is configured (MKLROOT empty,
// mkl.h / libircmt.lib / the component DLLs unreachable). Diagnosed live:
// clearing the var makes setvars exit 0 and configure compiler + mkl + tbb + …
// completely. `--force` defeats the SETVARS_COMPLETED re-entry guard. Clearing
// the var inside this capture sub-cmd does not affect the daemon's own env.
//
// Returns (env, true) on success; (nil, false) when the temp file cannot be
// written, the command fails to run, or its output has no parseable
// KEY=VALUE lines.
// batchUnsafeChars are the characters that could break out of the quoted
// `call "<setvars>"` line the capture .bat embeds (setvars_windows: the path is
// the env-controlled ONEAPI_ROOT + "\setvars.bat"). Inside a cmd double-quoted
// string `&|<>^()` are LITERAL, so they are NOT listed — critically, `(` and
// `)` must stay allowed because the DEFAULT 64-bit install path is
// `C:\Program Files (x86)\Intel\oneAPI`. The listed chars inject even inside
// quotes: `"` closes the quote (OS-illegal in a Windows name, but cheap to
// reject), `%`/`!` are variable / delayed-expansion, and CR/LF would split the
// .bat into extra lines. A genuine oneAPI path contains none of these, so
// refusing one (→ the caller falls back to DLLDirs) is strictly safe
// defense-in-depth in case a future code path feeds a setvars path that skipped
// the fileExists gate.
const batchUnsafeChars = "\"%!\r\n"

func captureSetvarsEnv(setvars string) ([]string, bool) {
	if strings.ContainsAny(setvars, batchUnsafeChars) {
		return nil, false
	}
	bat, err := os.CreateTemp("", "mcphub-setvars-*.bat")
	if err != nil {
		return nil, false
	}
	batPath := bat.Name()
	defer os.Remove(batPath)
	content := setvarsCaptureBatchContent(setvars)
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
	// The capture sub-cmd CLEARED NoDefaultCurrentDirectoryInExePath so setvars
	// could initialize its components, so the dumped env lacks it. Re-assert it
	// before the captured env is handed to a CHILD (run_in_oneapi_env command /
	// drmemory + vtune target): otherwise the child would run with
	// current-directory executable search RE-ENABLED — a CWD exe/DLL-planting
	// hardening regression vs the supervisor's =1 default. (Security review F2.)
	return withNoDefaultCurrentDir(env), true
}

// withNoDefaultCurrentDir returns env with NoDefaultCurrentDirectoryInExePath
// forced to "1" (any existing entry dropped, case-insensitively, then re-added
// — Windows env keys are case-insensitive). Restores the hardened "do not
// search the current directory for executables" posture in the captured env.
func withNoDefaultCurrentDir(env []string) []string {
	const key = "NoDefaultCurrentDirectoryInExePath"
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if k, _, hasEq := strings.Cut(e, "="); hasEq && strings.EqualFold(k, key) {
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"=1")
}

// setvarsCaptureBatchContent builds the temp .bat body for the capture. The
// `if errorlevel 1 exit /b %errorlevel%` guard is LOAD-BEARING (incorporated
// from PR #346's vcvars fix): without it, a FAILING `call setvars` would still
// fall through to the unconditional `set`, so cmd.Output() would succeed and
// dump the UNCHANGED (NoDef-cleared) parent environment — captureSetvarsEnv
// would then return (parentEnv, true), wrongly reporting a successful oneAPI
// capture and DEFEATING the caller's fallback chain (which only triggers on a
// (nil,false) capture result). Exiting non-zero before `set` surfaces the
// failure to cmd.Output() so the capture correctly returns (nil, false). See
// the NoDefaultCurrentDirectoryInExePath note in captureSetvarsEnv for why the
// var is cleared first.
func setvarsCaptureBatchContent(setvars string) string {
	return "@echo off\r\n" +
		"set \"NoDefaultCurrentDirectoryInExePath=\"\r\n" +
		"call \"" + setvars + "\" --force > NUL 2>&1\r\n" +
		"if errorlevel 1 exit /b %errorlevel%\r\n" +
		"set\r\n"
}

// parseSetDump parses the output of cmd's `set` command — one KEY=VALUE per
// line — into a "KEY=VALUE" slice. Lines without '=' (or with an empty KEY)
// are skipped. Only the FIRST '=' splits the key, so a value containing '='
// is kept verbatim. CRLF line endings are trimmed.
func parseSetDump(out []byte) []string {
	var env []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	// `set` output lines stay within the default 64 KiB scanner token limit; a
	// pathological super-long PATH could exceed it, so raise the buffer to 1 MiB.
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
