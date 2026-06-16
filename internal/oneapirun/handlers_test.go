package oneapirun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pathListSeparator returns the OS path-list separator as a string (";"
// on Windows, ":" elsewhere) — matches what prependOneAPIToPath uses.
func pathListSeparator() string {
	return string(os.PathListSeparator)
}

// mockCallToolRequest wraps raw JSON bytes as a CallToolRequest so tests
// can invoke runInOneAPIEnvTool without constructing the full MCP request
// plumbing. Mirrors godbolt/handlers_test.go.
type mockCallToolRequest struct {
	Arguments json.RawMessage
}

func (m *mockCallToolRequest) toReal() *mcp.CallToolRequest {
	r := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}}
	r.Params.Arguments = m.Arguments
	return r
}

// newTestServer builds an OneAPIRunServer with the env-computation seams
// stubbed to fixed values, so tests never invoke the real ~1-2s vcvars64.
func newTestServer(vsEnv []string, vsOK bool, dllDirs []string) *OneAPIRunServer {
	return &OneAPIRunServer{
		captureVSEnv:  func() ([]string, bool) { return vsEnv, vsOK },
		oneAPIDLLDirs: func() []string { return dllDirs },
	}
}

// decodeRunResult extracts the structured runResult JSON from a tool
// result's TextContent. Fails the test on any shape mismatch.
func decodeRunResult(t *testing.T, result *mcp.CallToolResult) runResult {
	t.Helper()
	if result.IsError {
		text := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		t.Fatalf("tool returned IsError=true: %s", text)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty Content")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] is not TextContent: %T", result.Content[0])
	}
	var res runResult
	if err := json.Unmarshal([]byte(tc.Text), &res); err != nil {
		t.Fatalf("result is not valid runResult JSON: %v\nbody: %s", err, tc.Text)
	}
	return res
}

// ---------------------------------------------------------------------------
// Env-merge logic — exercised cross-platform with synthetic data.
// ---------------------------------------------------------------------------

func TestComputeRunEnv_VcvarsPlusOneAPIPrependsPath(t *testing.T) {
	vsEnv := []string{
		"INCLUDE=C:\\VS\\include",
		"LIB=C:\\VS\\lib",
		"PATH=C:\\VS\\bin;C:\\Windows\\System32",
		"VSCMD_VER=18.0",
	}
	dllDirs := []string{"C:\\oneAPI\\mkl\\latest\\bin", "C:\\oneAPI\\tbb\\latest\\bin"}

	env, source := computeRunEnv(
		func() ([]string, bool) { return vsEnv, true },
		func() []string { return dllDirs },
	)

	if source != envSourceSetvars {
		t.Errorf("source = %q, want %q", source, envSourceSetvars)
	}

	// VS vars must all survive.
	for _, want := range []string{"INCLUDE=C:\\VS\\include", "LIB=C:\\VS\\lib", "VSCMD_VER=18.0"} {
		if !containsEntry(env, want) {
			t.Errorf("VS var %q missing from merged env: %v", want, env)
		}
	}

	// PATH must have BOTH oneAPI dirs prepended, in order, BEFORE the
	// original VS bin / System32 entries.
	pathVal := pathValueOf(t, env)
	sep := pathListSeparator()
	wantPrefix := strings.Join(dllDirs, sep) + sep + "C:\\VS\\bin"
	if !strings.HasPrefix(pathVal, wantPrefix) {
		t.Errorf("PATH = %q, want prefix %q (oneAPI dirs prepended ahead of VS bin)", pathVal, wantPrefix)
	}
	if !strings.Contains(pathVal, "C:\\Windows\\System32") {
		t.Errorf("PATH lost original System32 entry: %q", pathVal)
	}
	// oneAPI mkl must come before tbb, and both before VS bin.
	mklIdx := strings.Index(pathVal, "mkl\\latest\\bin")
	tbbIdx := strings.Index(pathVal, "tbb\\latest\\bin")
	vsIdx := strings.Index(pathVal, "C:\\VS\\bin")
	if !(mklIdx >= 0 && tbbIdx > mklIdx && vsIdx > tbbIdx) {
		t.Errorf("PATH order wrong: mkl=%d tbb=%d vsbin=%d in %q", mklIdx, tbbIdx, vsIdx, pathVal)
	}
}

func TestComputeRunEnv_VcvarsOnlyNoOneAPIDirsStillVcvarsSource(t *testing.T) {
	// When vcvars is captured but no oneAPI dir exists, the source stays
	// "vcvars64+oneapi" (the VS toolchain — the load-bearing half — WAS
	// captured) and PATH is unchanged.
	vsEnv := []string{"PATH=C:\\VS\\bin", "INCLUDE=C:\\VS\\include"}

	env, source := computeRunEnv(
		func() ([]string, bool) { return vsEnv, true },
		func() []string { return nil },
	)
	if source != envSourceSetvars {
		t.Errorf("source = %q, want %q", source, envSourceSetvars)
	}
	if pathValueOf(t, env) != "C:\\VS\\bin" {
		t.Errorf("PATH should be unchanged when no oneAPI dirs: %q", pathValueOf(t, env))
	}
	if !containsEntry(env, "INCLUDE=C:\\VS\\include") {
		t.Errorf("INCLUDE missing: %v", env)
	}
}

func TestComputeRunEnv_OneAPIOnlyFallbackWhenVcvarsNotFound(t *testing.T) {
	// vcvars not found but oneAPI dirs present → env_source "oneapi-only",
	// dirs prepended onto the inherited os.Environ().
	dllDirs := []string{"C:\\oneAPI\\mkl\\latest\\bin"}

	env, source := computeRunEnv(
		func() ([]string, bool) { return nil, false },
		func() []string { return dllDirs },
	)
	if source != envSourceOneAPIOnly {
		t.Errorf("source = %q, want %q", source, envSourceOneAPIOnly)
	}
	pathVal := pathValueOf(t, env)
	if !strings.HasPrefix(pathVal, "C:\\oneAPI\\mkl\\latest\\bin") {
		t.Errorf("oneAPI dir not prepended onto inherited PATH: %q", pathVal)
	}
}

func TestComputeRunEnv_PlainWhenNeitherAvailable(t *testing.T) {
	env, source := computeRunEnv(
		func() ([]string, bool) { return nil, false },
		func() []string { return nil },
	)
	if source != envSourcePlain {
		t.Errorf("source = %q, want %q", source, envSourcePlain)
	}
	// Plain env is os.Environ() — non-empty on any real host, and PATH
	// must NOT have been rewritten with an oneAPI prefix (no dirs).
	if len(env) == 0 {
		t.Fatal("plain env should be the inherited os.Environ(), got empty")
	}
}

func TestPrependOneAPIToPath_SynthesizesPathWhenAbsent(t *testing.T) {
	// baseEnv has no PATH entry → a PATH is synthesized from dirs alone.
	base := []string{"FOO=bar"}
	dirs := []string{"D:\\a", "D:\\b"}
	out := prependOneAPIToPath(base, dirs)

	if !containsEntry(out, "FOO=bar") {
		t.Errorf("FOO lost: %v", out)
	}
	pathVal := pathValueOf(t, out)
	want := strings.Join(dirs, pathListSeparator())
	if pathVal != want {
		t.Errorf("synthesized PATH = %q, want %q", pathVal, want)
	}
}

func TestPrependOneAPIToPath_CaseInsensitivePathKey(t *testing.T) {
	// vcvars `set` can emit "Path=" rather than "PATH=". The merge must
	// still find and extend it, not synthesize a duplicate.
	base := []string{"Path=C:\\orig", "OTHER=x"}
	dirs := []string{"C:\\inject"}
	out := prependOneAPIToPath(base, dirs)

	// Exactly one path-like entry must exist (no duplicate PATH).
	pathCount := 0
	var pathEntry string
	for _, e := range out {
		k, _, _ := strings.Cut(e, "=")
		if strings.EqualFold(k, "PATH") {
			pathCount++
			pathEntry = e
		}
	}
	if pathCount != 1 {
		t.Fatalf("expected exactly 1 PATH-like entry, got %d: %v", pathCount, out)
	}
	// Original casing ("Path") preserved, value prepended.
	if !strings.HasPrefix(pathEntry, "Path=C:\\inject") {
		t.Errorf("PATH entry = %q, want it to keep 'Path' casing and prepend C:\\inject", pathEntry)
	}
	if !strings.HasSuffix(pathEntry, "C:\\orig") {
		t.Errorf("PATH entry lost original value: %q", pathEntry)
	}
}

func TestPrependOneAPIToPath_NoDirsReturnsCopy(t *testing.T) {
	base := []string{"PATH=C:\\orig", "A=1"}
	out := prependOneAPIToPath(base, nil)
	if len(out) != len(base) {
		t.Fatalf("len changed: got %d want %d", len(out), len(base))
	}
	for i := range base {
		if out[i] != base[i] {
			t.Errorf("entry %d changed: %q vs %q", i, out[i], base[i])
		}
	}
	// Must be a copy, not an alias (mutating out must not touch base).
	out[0] = "MUTATED=1"
	if base[0] == "MUTATED=1" {
		t.Error("prependOneAPIToPath returned an alias, not a copy")
	}
}

// ---------------------------------------------------------------------------
// Run handler — trivial cross-platform command + timeout + arg validation.
// ---------------------------------------------------------------------------

// trivialEcho returns a (command, args, expectedStdoutSubstring) for a
// cross-platform "print hi and exit 0" command.
func trivialEcho() (string, []string, string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "echo hi"}, "hi"
	}
	return "sh", []string{"-c", "echo hi"}, "hi"
}

func TestRunInOneAPIEnvTool_TrivialCommandExitZero(t *testing.T) {
	cmd, args, wantOut := trivialEcho()
	rs := newTestServer(nil, false, nil) // plain env path

	rawArgs, _ := json.Marshal(map[string]any{
		"command":     cmd,
		"args":        args,
		"timeout_sec": 30,
	})
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("runInOneAPIEnvTool returned error: %v", err)
	}
	res := decodeRunResult(t, result)

	if res.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (stderr: %q)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, wantOut) {
		t.Errorf("stdout = %q, want to contain %q", res.Stdout, wantOut)
	}
	if res.EnvSource != envSourcePlain {
		t.Errorf("env_source = %q, want %q", res.EnvSource, envSourcePlain)
	}
	if res.TimedOut {
		t.Error("timed_out should be false for a fast command")
	}
}

func TestRunInOneAPIEnvTool_ReportsEnvSourceFromSeams(t *testing.T) {
	cmd, args, _ := trivialEcho()
	// Stub vcvars-captured so the reported env_source is vcvars64+oneapi
	// even though we pass a minimal env (the command still runs because
	// runCommand inherits nothing — but echo via cmd/sh needs no PATH for
	// a builtin). To keep the run robust across hosts, give the stub the
	// real environment plus a marker.
	rs := newTestServer([]string{"ONEAPI_RUN_TEST_MARKER=1"}, true, []string{"Z:\\fake\\bin"})

	rawArgs, _ := json.Marshal(map[string]any{
		"command":     cmd,
		"args":        args,
		"timeout_sec": 30,
	})
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("runInOneAPIEnvTool: %v", err)
	}
	res := decodeRunResult(t, result)
	if res.EnvSource != envSourceSetvars {
		t.Errorf("env_source = %q, want %q", res.EnvSource, envSourceSetvars)
	}
}

func TestRunInOneAPIEnvTool_Timeout(t *testing.T) {
	// A command that sleeps longer than the timeout must be killed and
	// reported as timed_out. Use a cross-platform sleep.
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		// ping -n 5 127.0.0.1 sleeps ~4s without needing a sleep binary.
		cmd, args = "cmd", []string{"/c", "ping -n 6 127.0.0.1 > NUL"}
	} else {
		cmd, args = "sh", []string{"-c", "sleep 5"}
	}
	rs := newTestServer(nil, false, nil)

	rawArgs, _ := json.Marshal(map[string]any{
		"command":     cmd,
		"args":        args,
		"timeout_sec": 1,
	})
	start := time.Now()
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runInOneAPIEnvTool: %v", err)
	}
	res := decodeRunResult(t, result)
	if !res.TimedOut {
		t.Errorf("timed_out = false, want true (exit=%d stderr=%q)", res.ExitCode, res.Stderr)
	}
	// Should have been killed near the 1s timeout, well before the 5s sleep.
	if elapsed > 4*time.Second {
		t.Errorf("timeout did not kill the command promptly: %v elapsed", elapsed)
	}
}

func TestRunInOneAPIEnvTool_MissingCommandIsError(t *testing.T) {
	rs := newTestServer(nil, false, nil)
	rawArgs, _ := json.Marshal(map[string]any{"args": []string{"x"}})
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("runInOneAPIEnvTool: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for missing command")
	}
}

func TestRunInOneAPIEnvTool_SpawnFailureReportedNotErrored(t *testing.T) {
	// A non-existent binary must NOT surface as an MCP error; it is a
	// structured result with exit_code -1 and the failure on stderr, so
	// the agent can read the cause.
	rs := newTestServer(nil, false, nil)
	rawArgs, _ := json.Marshal(map[string]any{
		"command":     "this-binary-does-not-exist-oneapirun-test",
		"timeout_sec": 10,
	})
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("runInOneAPIEnvTool: %v", err)
	}
	res := decodeRunResult(t, result)
	if res.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 for spawn failure", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "failed to start") {
		t.Errorf("stderr = %q, want to mention spawn failure", res.Stderr)
	}
}

func TestRunInOneAPIEnvTool_DefaultTimeoutWhenOmitted(t *testing.T) {
	// timeout_sec omitted → defaultTimeoutSec applies; a fast command
	// still completes immediately (we only assert it didn't error/timeout).
	cmd, args, _ := trivialEcho()
	rs := newTestServer(nil, false, nil)
	rawArgs, _ := json.Marshal(map[string]any{
		"command": cmd,
		"args":    args,
	})
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("runInOneAPIEnvTool: %v", err)
	}
	res := decodeRunResult(t, result)
	if res.TimedOut {
		t.Error("fast command should not time out under default timeout")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", res.ExitCode)
	}
}

// ---------------------------------------------------------------------------
// Capped buffer — truncation marker.
// ---------------------------------------------------------------------------

func TestCappedBuffer_TruncatesWithMarker(t *testing.T) {
	c := cappedBuffer{limit: 10}
	n, err := c.Write([]byte("0123456789ABCDEF")) // 16 bytes, limit 10
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 16 {
		t.Errorf("Write reported n=%d, want 16 (must claim full consumption so child keeps running)", n)
	}
	got := c.String()
	if !strings.HasPrefix(got, "0123456789") {
		t.Errorf("buffer prefix = %q, want first 10 bytes", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("buffer missing truncation marker: %q", got)
	}
	// A second write after truncation is silently dropped (marker not
	// duplicated), still claiming full consumption.
	n2, _ := c.Write([]byte("more"))
	if n2 != 4 {
		t.Errorf("post-truncation Write n=%d, want 4", n2)
	}
	if strings.Count(c.String(), "truncated") != 1 {
		t.Errorf("truncation marker duplicated: %q", c.String())
	}
}

func TestCappedBuffer_UnderLimitNoMarker(t *testing.T) {
	c := cappedBuffer{limit: 100}
	_, _ = c.Write([]byte("short"))
	if c.String() != "short" {
		t.Errorf("buffer = %q, want %q (no truncation under limit)", c.String(), "short")
	}
	if c.truncated {
		t.Error("truncated flag set despite being under limit")
	}
}

// ---------------------------------------------------------------------------
// resolveCommandPath — BUG 1: resolve against the CHILD env's PATH.
// ---------------------------------------------------------------------------

// fakeExeName returns a platform-appropriate fake executable basename and the
// bare command name a caller would pass (no extension). On Windows the file
// needs an extension that PATHEXT recognizes (.exe); elsewhere a bare name +
// 0755 mode is enough.
func fakeExeName() (file string, bare string) {
	if runtime.GOOS == "windows" {
		return "icx-cl.exe", "icx-cl"
	}
	return "icx-cl", "icx-cl"
}

func TestResolveCommandPath_FindsExeOnChildPathNotServerPath(t *testing.T) {
	// Place a fake executable in a dir that is ONLY on the CHILD env's PATH,
	// never on the server (os.Getenv) PATH. resolveCommandPath must find it
	// there — proving it resolves against the passed-in env, not the process
	// environment (the BUG 1 fix).
	dir := t.TempDir()
	file, bare := fakeExeName()
	exePath := filepath.Join(dir, file)
	if err := os.WriteFile(exePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	childEnv := []string{"PATH=" + dir, "OTHER=x"}
	got := resolveCommandPath(bare, childEnv)
	if !samePath(got, exePath) {
		t.Errorf("resolveCommandPath(%q) = %q, want %q (resolved against child PATH)", bare, got, exePath)
	}
}

func TestResolveCommandPath_CaseInsensitivePathKey(t *testing.T) {
	// vcvars `set` can emit "Path=" — resolution must still read it.
	dir := t.TempDir()
	file, bare := fakeExeName()
	exePath := filepath.Join(dir, file)
	if err := os.WriteFile(exePath, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	childEnv := []string{"Path=" + dir}
	if got := resolveCommandPath(bare, childEnv); !samePath(got, exePath) {
		t.Errorf("resolveCommandPath with 'Path' key = %q, want %q", got, exePath)
	}
}

func TestResolveCommandPath_SeparatorBearingReturnedUnchanged(t *testing.T) {
	// A command that already carries a path separator is an explicit path —
	// returned verbatim, never re-resolved.
	for _, cmd := range []string{`C:\build\app.exe`, "/usr/bin/gdb", "sub/dir/tool"} {
		if got := resolveCommandPath(cmd, []string{"PATH=" + t.TempDir()}); got != cmd {
			t.Errorf("resolveCommandPath(%q) = %q, want unchanged", cmd, got)
		}
	}
}

func TestResolveCommandPath_UnresolvedReturnedUnchanged(t *testing.T) {
	// A bare name absent from every PATH dir is returned unchanged so exec
	// surfaces a clear "not found" error instead of this helper swallowing it.
	childEnv := []string{"PATH=" + t.TempDir()}
	if got := resolveCommandPath("definitely-not-a-real-binary", childEnv); got != "definitely-not-a-real-binary" {
		t.Errorf("unresolved command = %q, want unchanged", got)
	}
}

func TestResolveCommandPath_NoPathEntryReturnsUnchanged(t *testing.T) {
	if got := resolveCommandPath("icx-cl", []string{"OTHER=x"}); got != "icx-cl" {
		t.Errorf("no-PATH env: resolveCommandPath = %q, want unchanged", got)
	}
}

// ---------------------------------------------------------------------------
// withWritableTemp / setEnvOverride — BUG 2: writable TEMP/TMP.
// ---------------------------------------------------------------------------

func TestSetEnvOverride_ReplacesInheritedValueCaseInsensitive(t *testing.T) {
	// The inherited "Temp=r:\Temp" (RAM disk) must be REPLACED, not left in
	// place and not duplicated alongside a new TEMP entry.
	env := []string{"Temp=r:\\Temp", "TMP=r:\\Temp", "PATH=C:\\x"}
	out := setEnvOverride(env, tempKeys, "C:\\writable")

	tempVal, _ := envValue(out, "TEMP")
	tmpVal, _ := envValue(out, "TMP")
	if tempVal != "C:\\writable" {
		t.Errorf("TEMP = %q, want C:\\writable (inherited value must be replaced)", tempVal)
	}
	if tmpVal != "C:\\writable" {
		t.Errorf("TMP = %q, want C:\\writable", tmpVal)
	}
	// PATH untouched.
	if v, _ := envValue(out, "PATH"); v != "C:\\x" {
		t.Errorf("PATH changed: %q", v)
	}
	// No duplicate TEMP/TMP entries.
	if n := countKeys(out, "TEMP"); n != 1 {
		t.Errorf("got %d TEMP entries, want exactly 1: %v", n, out)
	}
	if n := countKeys(out, "TMP"); n != 1 {
		t.Errorf("got %d TMP entries, want exactly 1: %v", n, out)
	}
	// Original casing of the replaced key is preserved.
	if !containsEntry(out, "Temp=C:\\writable") {
		t.Errorf("expected the 'Temp' key casing preserved: %v", out)
	}
}

func TestSetEnvOverride_AppendsWhenAbsent(t *testing.T) {
	env := []string{"PATH=C:\\x"}
	out := setEnvOverride(env, tempKeys, "C:\\w")
	if v, _ := envValue(out, "TEMP"); v != "C:\\w" {
		t.Errorf("TEMP not appended: %v", out)
	}
	if v, _ := envValue(out, "TMP"); v != "C:\\w" {
		t.Errorf("TMP not appended: %v", out)
	}
}

func TestWithWritableTemp_CreatesDirAndOverridesTempTmp(t *testing.T) {
	// Point LOCALAPPDATA (and HOME for the non-Windows path) at a temp root so
	// the hub temp dir is created under the test sandbox, never the real
	// %LOCALAPPDATA%.
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)

	env := []string{"TEMP=r:\\Temp", "TMP=r:\\Temp"}
	out := withWritableTemp(env)

	tempVal, ok := envValue(out, "TEMP")
	if !ok {
		t.Fatalf("TEMP missing from env: %v", out)
	}
	if tempVal == "r:\\Temp" {
		t.Errorf("TEMP not overridden (still the inherited RAM-disk value): %q", tempVal)
	}
	// The override dir must actually EXIST (MkdirAll ran).
	info, err := os.Stat(tempVal)
	if err != nil || !info.IsDir() {
		t.Errorf("TEMP dir %q was not created: err=%v", tempVal, err)
	}
	// TMP must match TEMP.
	if tmpVal, _ := envValue(out, "TMP"); tmpVal != tempVal {
		t.Errorf("TMP = %q, want = TEMP %q", tmpVal, tempVal)
	}
}

// ---------------------------------------------------------------------------
// panic recovery — BUG 3: ALWAYS return structured JSON, never empty/crash.
// ---------------------------------------------------------------------------

func TestRunInOneAPIEnvTool_PanicRecoveredToStructuredJSON(t *testing.T) {
	// Force a panic from inside the handler via a seam that panics, then
	// assert the deferred recover produced a structured runResult JSON (never
	// empty, never a crash) with exit_code -1 and the panic on stderr.
	rs := &OneAPIRunServer{
		captureVSEnv:  func() ([]string, bool) { panic("simulated env-merge fault") },
		oneAPIDLLDirs: func() []string { return nil },
	}
	rawArgs, _ := json.Marshal(map[string]any{"command": "anything", "timeout_sec": 5})
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: rawArgs}).toReal())
	if err != nil {
		t.Fatalf("handler returned a Go error instead of recovering: %v", err)
	}
	if result == nil {
		t.Fatal("handler returned nil result on panic (must be structured, non-empty)")
	}
	res := decodeRunResult(t, result)
	if res.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 on panic", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "internal panic") {
		t.Errorf("stderr = %q, want to mention internal panic", res.Stderr)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// samePath compares two paths for equality, case-insensitively on Windows
// (NTFS is case-insensitive; filepath.Join preserves the casing the caller
// passed, so a candidate built as "icx-cl.EXE" legitimately resolves to the
// on-disk "icx-cl.exe"). On other platforms the comparison is exact.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func countKeys(env []string, key string) int {
	n := 0
	for _, e := range env {
		k, _, ok := strings.Cut(e, "=")
		if ok && strings.EqualFold(k, key) {
			n++
		}
	}
	return n
}

func containsEntry(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func pathValueOf(t *testing.T, env []string) string {
	t.Helper()
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok && strings.EqualFold(k, "PATH") {
			return v
		}
	}
	t.Fatalf("no PATH entry in env: %v", env)
	return ""
}
