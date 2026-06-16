//go:build windows

package oneapirun

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/process"
)

// vcvarsRelPath is the path from a Visual-Studio installationPath to its
// vcvars64.bat (the amd64 host/target environment batch).
var vcvarsRelPath = filepath.Join("VC", "Auxiliary", "Build", "vcvars64.bat")

// vsYearProbe are the VS major-version / "year" directory names probed
// under "%ProgramFiles%\Microsoft Visual Studio\<year>\<edition>" when
// vswhere is unavailable. The live host runs VS "18" Community; older
// hosts use the 2022/2019/2017 marketing years. Newest first so a host
// with several VS installs picks the newest toolchain.
var vsYearProbe = []string{"18", "2022", "2019", "2017"}

// vsEditionProbe are the edition directory names probed under each year.
var vsEditionProbe = []string{"Community", "Professional", "Enterprise", "BuildTools", "Preview"}

// captureVSEnvCached is the production captureVSEnv seam. vcvars64.bat is
// slow (~1-2s) and its environment is stable for the process lifetime, so
// the capture runs at most once per process (sync.Once). Concurrent first
// callers all block on the same capture and then share the cached result.
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

// captureVSEnvUncached locates vcvars64.bat and captures the environment it
// sets, returning the env as "KEY=VALUE" strings and ok=true on success.
// ok=false (with nil env) on any failure — vcvars not found, the batch
// failing, or an unparseable dump — so the caller falls back cleanly.
func captureVSEnvUncached() ([]string, bool) {
	vcvars, ok := findVcvars64()
	if !ok {
		return nil, false
	}
	env, ok := captureVcvarsEnv(vcvars)
	if !ok {
		return nil, false
	}
	return env, true
}

// findVcvars64 locates vcvars64.bat. Preference order:
//
//  1. vswhere ("%ProgramFiles(x86)%\Microsoft Visual Studio\Installer\
//     vswhere.exe" -latest -property installationPath) → <path>\<vcvarsRel>
//  2. probe "%ProgramFiles%\Microsoft Visual Studio\<year>\<edition>\
//     <vcvarsRel>" across vsYearProbe × vsEditionProbe.
//
// Returns the first existing vcvars64.bat path, or ("", false).
func findVcvars64() (string, bool) {
	if path, ok := findVcvarsViaVswhere(); ok {
		return path, true
	}
	return findVcvarsViaProbe()
}

// findVcvarsViaVswhere asks vswhere for the latest VS installationPath and
// joins vcvarsRelPath onto it. Returns ("", false) when vswhere is absent,
// fails, prints nothing, or the resulting vcvars64.bat does not exist.
func findVcvarsViaVswhere() (string, bool) {
	pf86 := os.Getenv("ProgramFiles(x86)")
	if pf86 == "" {
		return "", false
	}
	vswhere := filepath.Join(pf86, "Microsoft Visual Studio", "Installer", "vswhere.exe")
	if !fileExists(vswhere) {
		return "", false
	}
	cmd := exec.Command(vswhere, "-latest", "-property", "installationPath")
	process.NoConsole(cmd) // suppress console flash on windowsgui parent
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	installPath := strings.TrimSpace(string(out))
	if installPath == "" {
		return "", false
	}
	// vswhere can print multiple lines for multiple installs; take the
	// first non-empty line (the -latest install).
	if nl := strings.IndexAny(installPath, "\r\n"); nl >= 0 {
		installPath = strings.TrimSpace(installPath[:nl])
	}
	vcvars := filepath.Join(installPath, vcvarsRelPath)
	if fileExists(vcvars) {
		return vcvars, true
	}
	return "", false
}

// findVcvarsViaProbe walks the known VS install layout under ProgramFiles
// for a vcvars64.bat, newest year / first-matching edition first.
func findVcvarsViaProbe() (string, bool) {
	roots := []string{}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		roots = append(roots, filepath.Join(pf, "Microsoft Visual Studio"))
	}
	// Some hosts (and the BuildTools-only installs) live under
	// ProgramFiles(x86) for the older 2017/2019 layout.
	if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
		roots = append(roots, filepath.Join(pf86, "Microsoft Visual Studio"))
	}
	for _, root := range roots {
		for _, year := range vsYearProbe {
			for _, edition := range vsEditionProbe {
				vcvars := filepath.Join(root, year, edition, vcvarsRelPath)
				if fileExists(vcvars) {
					return vcvars, true
				}
			}
		}
	}
	return "", false
}

// captureVcvarsEnv runs vcvars64.bat and dumps the resulting environment.
//
// It writes a tiny temp .bat (`call "<vcvars>" >NUL 2>&1` then `set`) and runs
// `cmd /c <tempbat>`, rather than an inline `cmd /c ""<vcvars>" ... && set"`.
// The inline form is fragile: Go's exec.Command escapes the cmdLine argument
// for the MS C-runtime, and cmd /c's own first/last-quote-stripping rule then
// mangles the quoted, space-bearing vcvars path (e.g. "C:\Program Files\..."),
// so vcvars never runs and the capture fails (observed live: env_source
// fell back to oneapi-only, cl.exe absent). Quoting the path INSIDE the .bat
// sidesteps both layers — cmd parses it natively, no Go escaping involved.
//
// Returns (env, true) on success; (nil, false) when the temp file cannot be
// written, the command fails to run, or its output has no parseable
// KEY=VALUE lines.
func captureVcvarsEnv(vcvars string) ([]string, bool) {
	bat, err := os.CreateTemp("", "mcphub-vcvars-*.bat")
	if err != nil {
		return nil, false
	}
	batPath := bat.Name()
	defer os.Remove(batPath)
	// CRLF so cmd parses the batch reliably; `call` so control returns to
	// dump `set` after vcvars finishes; banner discarded to NUL.
	content := "@echo off\r\ncall \"" + vcvars + "\" > NUL 2>&1\r\nset\r\n"
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
// directory). Used to confirm vcvars64.bat / vswhere.exe before invoking.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
