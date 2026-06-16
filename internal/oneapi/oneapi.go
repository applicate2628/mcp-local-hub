// Package oneapi detects the Intel oneAPI environment shell
// (setvars.bat) and captures the authoritative, complete environment it
// produces, so the supervisor can inject the oneAPI runtime environment
// into the gdb / lldb debugger daemons (and the inferior exes they
// launch).
//
// WHY this exists (hot-prod operator feedback): an MKL-linked .exe fails
// to load its DLLs under the gdb / lldb MCP daemons because those daemons
// — and the inferior they spawn — do NOT inherit the Intel oneAPI
// environment. The operator's documented workaround was to manually wrap
// the debug session in an oneapi-shell. There was no oneAPI awareness in
// the codebase at all (grep-confirmed).
//
// KEY DESIGN CONSTRAINT (operator, verbatim "у oneapi есть свой env
// shell"): use oneAPI's OWN setvars.bat to obtain the env, NOT hardcoded
// component DLL dirs like `mkl\latest\bin`. setvars.bat sets the FULL
// environment (PATH prepends + MKLROOT / TBBROOT / CMPLR_ROOT + every
// component) and survives layout changes across oneAPI versions. We run
// it once, diff the resulting environment against our own process
// environment, and inject only the ADDITIONS / CHANGES (the delta).
//
// Platform scope: Windows-focused. On Linux / other platforms this is a
// no-op (DetectSetvars returns ("", false), so CaptureEnvDelta is never
// reached with a real path; the supervisor never injects). A Linux oneAPI
// env shell (`setvars.sh`) is a possible future extension but is out of
// scope here.
//
// Failure posture: every failure mode (setvars missing, non-zero exit,
// unparseable output, timeout) is a CLEAN no-op — CaptureEnvDelta returns
// an empty delta plus an error the caller logs as a warn event. It never
// blocks or fails a daemon spawn.
package oneapi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DisableEnvVar, when set to "1" in the supervisor's process
// environment, globally opts out of oneAPI env injection: DetectSetvars
// still locates the file for diagnostics, but the supervisor wiring skips
// both CaptureEnvDelta and the inject step. Documented in CLAUDE.md
// "Supervisor" notes.
const DisableEnvVar = "MCPHUB_DISABLE_ONEAPI_PATH"

// captureTimeout bounds the setvars subprocess so a hung shell never
// stalls the supervisor. setvars typically takes ~1-3s; 30s is generous
// headroom. A timeout is treated as a no-op-with-warn by the caller.
const captureTimeout = 30 * time.Second

// EnvKey is the canonical (uppercased) form used for cross-platform env
// key comparison. On Windows the environment block is case-insensitive
// (PATH / Path / path are the same logical variable), so all comparisons
// fold to uppercase. This keeps the diff stable regardless of how the
// child `set` dump or os.Environ() happens to case a key.
func EnvKey(k string) string { return strings.ToUpper(k) }

// PathKey is the normalized key for the PATH variable.
const PathKey = "PATH"

// ---------------------------------------------------------------------------
// Injectable seams (package vars) — let tests drive DetectSetvars and
// CaptureEnvDelta without touching the real filesystem or running the real
// (~1-3s) setvars.bat.
// ---------------------------------------------------------------------------

// fileExists reports whether path names an existing file. Overridable in
// tests so DetectSetvars can be exercised against a synthetic layout
// without a real oneAPI install. Production points at the real os.Stat
// probe (set in oneapi_<platform>.go via setvarsProbePaths + this).
var fileExists = func(path string) bool { return realFileExists(path) }

// setvarsRunner runs the oneAPI env shell at setvarsPath and returns the
// raw stdout of the trailing `set` dump (KEY=VALUE lines, one per line).
// It is the ONLY place a real subprocess is launched; tests inject a fake
// runner returning canned output so the env-delta logic is verified
// without the real setvars cost. Production assigns the platform runner
// in oneapi_windows.go / oneapi_other.go.
//
// The context carries the capture timeout; a runner MUST honor it and
// return ctx.Err() (or a wrapping error) on timeout.
var setvarsRunner = func(ctx context.Context, setvarsPath string) (string, error) {
	return realSetvarsRunner(ctx, setvarsPath)
}

// baselineEnv returns the process environment used as the diff baseline
// (the env the captured setvars output is compared against). Overridable
// in tests so the delta computation is deterministic regardless of the
// real os.Environ(). Production points at os.Environ via realBaselineEnv.
var baselineEnv = func() []string { return realBaselineEnv() }

// ---------------------------------------------------------------------------
// Detection.
// ---------------------------------------------------------------------------

// DetectSetvars locates the oneAPI env shell (setvars.bat on Windows).
//
// Probe order (Windows):
//  1. ONEAPI_ROOT env var, if set → "<ONEAPI_ROOT>\setvars.bat"
//  2. "%ProgramFiles(x86)%\Intel\oneAPI\setvars.bat"
//  3. "%ProgramFiles%\Intel\oneAPI\setvars.bat"
//
// Returns (path, true) for the FIRST candidate that exists on disk; else
// ("", false). On non-Windows it always returns ("", false) (the
// platform setvarsProbePaths returns no candidates).
//
// Detection deliberately does NOT run setvars — it only checks for the
// file's existence, so it is cheap and safe to call per spawn.
func DetectSetvars() (string, bool) {
	for _, cand := range setvarsProbePaths() {
		if cand == "" {
			continue
		}
		if fileExists(cand) {
			return cand, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Capture (cached, at-most-once per process).
// ---------------------------------------------------------------------------

// captureState holds the once-per-process cached result of running
// setvars + diffing. setvars is SLOW (~1-3s); we run it once for the
// supervisor's whole life and reuse the delta for every spawn. A re-run
// only happens in a fresh process.
type captureState struct {
	once  sync.Once
	delta map[string]string
	err   error
}

var capture = &captureState{}

// CaptureEnvDelta runs the oneAPI env shell ONCE per process, captures
// the environment it produces, diffs against the baseline process env,
// and returns only the oneAPI ADDITIONS / CHANGES (the delta map: keys
// that are new OR whose value changed).
//
// The setvarsPath argument is the path DetectSetvars resolved. The result
// is cached on first call; subsequent calls return the cached delta + err
// regardless of the path argument (the supervisor resolves the path once
// at startup, so this is correct — the cache key is "this process").
//
// On ANY failure (timeout, non-zero exit, empty / unparseable output) it
// returns (empty-but-non-nil map, error). The caller logs the error as a
// warn event and proceeds with NO injection — never fatal.
//
// Concretely (Windows): the platform runner executes
//
//	cmd /c ""<setvars>" > NUL 2>&1 && set"
//
// so setvars' banner goes to NUL and the trailing `set` dumps the
// resulting environment to stdout, which we parse into KEY=VALUE lines.
func CaptureEnvDelta(setvarsPath string) (map[string]string, error) {
	capture.once.Do(func() {
		capture.delta, capture.err = computeEnvDelta(setvarsPath)
	})
	return capture.delta, capture.err
}

// computeEnvDelta is the uncached core: run setvars, parse, diff. Split
// out so it is unit-testable directly (the test seam injects a fake
// runner + baseline) without going through the sync.Once cache.
func computeEnvDelta(setvarsPath string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	out, err := setvarsRunner(ctx, setvarsPath)
	if err != nil {
		// Distinguish timeout for a clearer warn body, but either way the
		// delta is empty and the caller no-ops.
		if ctx.Err() == context.DeadlineExceeded {
			return map[string]string{}, fmt.Errorf("oneapi: setvars capture timed out after %s: %w", captureTimeout, err)
		}
		return map[string]string{}, fmt.Errorf("oneapi: setvars capture failed: %w", err)
	}

	produced := parseSetOutput(out)
	if len(produced) == 0 {
		return map[string]string{}, fmt.Errorf("oneapi: setvars produced no parseable environment (empty or malformed `set` output)")
	}

	base := envSliceToMap(baselineEnv())
	delta := diffEnv(base, produced)
	if len(delta) == 0 {
		// setvars ran but added/changed nothing relative to our env — this
		// is a benign no-op (e.g. the supervisor already runs inside an
		// oneAPI shell). Return an empty delta and NO error so the caller
		// simply injects nothing.
		return map[string]string{}, nil
	}
	return delta, nil
}

// ResetCacheForTest clears the once-per-process capture cache. TEST-ONLY:
// it lets a test exercise the caching contract repeatedly and reset state
// between cases. Not called from production code.
func ResetCacheForTest() {
	capture = &captureState{}
}

// SetSeamsForTest installs fake detection / runner / baseline seams and
// returns a restore func. TEST-ONLY. Any nil argument leaves that seam
// untouched. It also resets the capture cache so a fresh fake runner is
// actually exercised.
func SetSeamsForTest(
	fakeFileExists func(string) bool,
	fakeRunner func(context.Context, string) (string, error),
	fakeBaseline func() []string,
) func() {
	prevFileExists := fileExists
	prevRunner := setvarsRunner
	prevBaseline := baselineEnv
	if fakeFileExists != nil {
		fileExists = fakeFileExists
	}
	if fakeRunner != nil {
		setvarsRunner = fakeRunner
	}
	if fakeBaseline != nil {
		baselineEnv = fakeBaseline
	}
	ResetCacheForTest()
	return func() {
		fileExists = prevFileExists
		setvarsRunner = prevRunner
		baselineEnv = prevBaseline
		ResetCacheForTest()
	}
}

// ---------------------------------------------------------------------------
// Parsing + diff helpers (pure).
// ---------------------------------------------------------------------------

// parseSetOutput parses the stdout of a Windows `set` dump (or any
// KEY=VALUE-per-line block) into a map keyed by the NORMALIZED (uppercase)
// key. The original-cased key is preserved in the OriginalKey of the value
// via a parallel structure? No — to keep this simple and because the
// supervisor only needs values, we keep the LAST-seen original key spelling
// alongside the value through the returned `producedVar` records.
//
// Lines without '=' are skipped (defensive: the `set` dump never emits
// them, but a banner line leaking through redirection must not corrupt the
// map). The first '=' splits key from value so values containing '=' (e.g.
// a PATH with `=` is impossible, but a connection-string-like value is) are
// not truncated.
func parseSetOutput(out string) map[string]producedVar {
	result := map[string]producedVar{}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 { // eq==0 → empty key; eq<0 → no '='. Both invalid.
			continue
		}
		k := line[:eq]
		v := line[eq+1:]
		result[EnvKey(k)] = producedVar{OriginalKey: k, Value: v}
	}
	return result
}

// producedVar is a captured environment entry: the value plus the
// original-cased key spelling (so the injected env keeps setvars' casing,
// e.g. "MKLROOT").
type producedVar struct {
	OriginalKey string
	Value       string
}

// envSliceToMap converts an os.Environ()-style []string ("KEY=VALUE") into
// a map keyed by NORMALIZED (uppercase) key → value. Used to build the
// diff baseline. Last write wins on a case-collision (matches Windows'
// undefined-but-single selection).
func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		m[EnvKey(kv[:eq])] = kv[eq+1:]
	}
	return m
}

// diffEnv returns the oneAPI delta: every produced key whose value is NEW
// (absent from baseline) or CHANGED (present but different value). The
// returned map is keyed by the ORIGINAL-cased key spelling (setvars'
// casing) → value, so the supervisor injects vars exactly as setvars named
// them. Comparison is by normalized key + exact value.
func diffEnv(baseline map[string]string, produced map[string]producedVar) map[string]string {
	delta := map[string]string{}
	for normKey, pv := range produced {
		baseVal, present := baseline[normKey]
		if !present || baseVal != pv.Value {
			delta[pv.OriginalKey] = pv.Value
		}
	}
	return delta
}

// SortedKeys returns the delta's keys in deterministic sorted order — used
// for the observability event body (vars_added) so the audit row is stable
// across runs.
func SortedKeys(delta map[string]string) []string {
	keys := make([]string, 0, len(delta))
	for k := range delta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
