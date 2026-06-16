//go:build !windows

package oneapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// SetvarsEnv captures and returns the COMPLETE Intel oneAPI build/run
// environment that oneAPI's setvars.sh configures on POSIX hosts (Linux;
// macOS where a oneAPI install exists). The captured env carries the runtime
// library search path (LD_LIBRARY_PATH — the POSIX analogue of Windows PATH for
// DLLs — plus LIBRARY_PATH), the header search path (CPATH), and the component
// root vars (MKLROOT/CMPLR_ROOT/TBBROOT/…), so an icx-built / MKL-linked target
// loads its shared objects and a build finds mkl.h + the link libs.
//
// Returns (env, true) on success; (nil, false) on any failure (setvars not
// found, bash unavailable, the script failing, or an unparseable dump) so the
// caller falls back cleanly to os.Environ(). Runs a subprocess at most ONCE per
// process (sync.Once) since the environment is stable for the process lifetime.
//
// This is the POSIX counterpart of the Windows capture in setvars_windows.go.
// Unlike Windows there is no NoDefaultCurrentDirectoryInExePath hazard —
// setvars.sh sources each component env script by absolute path, so all
// components initialize. NB: this path COMPILES and is logically complete, but
// has NOT been live-verified on a real Linux+oneAPI host (the project's primary
// host is Windows); treat it as the documented Linux contract, not a tested
// one, until exercised on Linux.
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
	return captureSetvarsEnv(setvars)
}

// findSetvars locates the Intel oneAPI setvars.sh at the install root
// (DetectRoot() + "/setvars.sh"). Returns ("", false) when no oneAPI install
// is found or the script is missing.
func findSetvars() (string, bool) {
	root, ok := DetectRoot()
	if !ok {
		return "", false
	}
	setvars := filepath.Join(root, "setvars.sh")
	if fileExists(setvars) {
		return setvars, true
	}
	return "", false
}

// captureSetvarsEnv sources setvars.sh in a bash subshell and dumps the
// resulting environment via `env`. `source` (a bash builtin, hence `bash -c`
// not `sh -c`) is required so the variables land in the shell that then runs
// `env` — executing setvars.sh in a child would lose them. `--force` defeats
// setvars' SETVARS_COMPLETED re-entry guard. The setvars path is passed as a
// positional argument and referenced as "$1" so unusual POSIX path characters
// are not interpolated into shell source code. env output is the standard
// newline-delimited KEY=VALUE form, parsed the same way the Windows `set` dump
// is, for portability across GNU and BSD `env`.
//
// Returns (env, true) on success; (nil, false) when bash is unavailable, the
// command fails, or its output has no parseable KEY=VALUE lines.
func captureSetvarsEnv(setvars string) ([]string, bool) {
	cmd := exec.Command("bash", "-c", `source "$1" --force >/dev/null 2>&1 && env`, "bash", setvars)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	env := parseEnvLines(out)
	if len(env) == 0 {
		return nil, false
	}
	return env, true
}

// parseEnvLines parses newline-delimited `env` output into a "KEY=VALUE"
// slice. Lines without '=' (or with an empty KEY) are skipped. Only the FIRST
// '=' splits the key, so a value containing '=' is kept verbatim. (A value
// containing an embedded newline — vanishingly rare in an oneAPI env — would be
// split; the Windows side has the same property with its `set` dump.)
func parseEnvLines(out []byte) []string {
	var env []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
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
// directory). Used to confirm setvars.sh before invoking.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
