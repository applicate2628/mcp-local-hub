// internal/clients/config_path_sandbox_audit.go
//
// THE SANDBOX AUDIT: a fixture-level assertion that fails when any admitted
// client adapter's ConfigPath() resolves OUTSIDE the test process's sandbox.
//
// WHY THIS EXISTS — the class, not the instance.
//
// Every adapter in clientRegistry() resolves its config path from the ENVIRONMENT.
// Only four of the 47 resolve purely from the home dir; the rest read %APPDATA%,
// $XDG_CONFIG_HOME, %ProgramData%, $COPILOT_HOME, $KIMI_CODE_HOME, or the
// $MIMOCODE_* set. A test fixture that redirects HOME/USERPROFILE/LOCALAPPDATA
// and stops there admits REAL adapters pointed at the developer's REAL configs.
//
// That has happened twice, and both times a human noticed the damage after the
// fact:
//
//  1. internal/cli `mcpFrontPR588Env` rewrote all ten server URLs in the
//     operator's live %APPDATA%\Code\User\mcp.json to throwaway test ports.
//  2. internal/cli `withHermeticHome` (language_server_test.go) was WRITE-capable:
//     `mcphub language-server cleanup` takes an UNFILTERED AllClients()
//     (internal/cli/language_server.go:141) and per matching entry calls
//     adapter.BackupKeep (:292 — which PRUNES the operator's existing backups)
//     then adapter.RemoveEntry (:302).
//
// Instance (1) was closed by enumerating the env vars in one place. That is a
// DENYLIST: it goes stale the next time a client adapter reads a new env var, and
// it does nothing at all for a fixture that forgets to call it — which is exactly
// how instance (2) survived instance (1)'s fix.
//
// This audit is the allowlist form of the same invariant, and it is enforced at
// the ONE place every adapter is constructed rather than at each fixture:
//
//   - It enumerates NOTHING. It checks the RESOLVED path against the sandbox
//     root, so a brand-new adapter reading a brand-new env var is covered the day
//     it is added, with no list to update.
//   - It hooks the registry's construction seam, so it fires for adapters the
//     PRODUCTION CODE UNDER TEST constructs, not only for adapters the fixture
//     constructed. An assertion over what the test built would have missed
//     instance (2) entirely, since that fixture never built a client map — the
//     command under test did.
//   - It needs no per-fixture opt-in, so a NEW fixture is covered for free. This
//     is the property instance (2) proved is load-bearing.
//   - It names the offending adapter, the resolved path, the environment
//     variable responsible, and the _test.go frame that reached it.
//
// SANDBOX ROOT = the process's test-temp root (os.TempDir(), captured once at
// install) plus any extra roots the installer passes. It is deliberately NOT
// "this subtest's own t.TempDir()". Two reasons, both structural:
//
//   - The hook fires deep inside production code and has no *testing.T, so it
//     cannot know which subtest is running. Threading one in would require every
//     fixture to register its root — reintroducing the per-fixture opt-in whose
//     absence is the whole point.
//   - The class being prevented is "escaped to the operator's real machine". One
//     test writing into another test's temp dir is a different and far less
//     severe defect (no operator data is touched, and the dirs are reaped by the
//     test framework). This audit deliberately does not police that; scoping it
//     that tightly would trade a guard that always runs for a guard that must be
//     remembered.
//
// SCOPE — which packages install it, and why not the fourth. The three
// orchestration packages install it from TestMain, each next to the state-dir
// fences already there:
//
//	internal/cli   settings_registry_test.go TestMain
//	internal/api   main_test.go TestMain
//	internal/gui   main_test.go TestMain
//
// internal/clients does NOT install it, deliberately. Its adapter unit tests
// construct a real adapter precisely to ASSERT its real path derivation, and
// resolving a path STRING is not a leak — file I/O against a real path is, and
// those tests confine every write to t.TempDir() (audited: the package is clean).
// Installing here would fail ~30 tests whose subject is the derivation itself.
// The same one-off case inside an installing package uses SuspendSandboxAudit.
//
// This is a runtime hook, not a build tag, so it takes effect in both the default
// untagged build and the -tags=test_state_path_env build, and it is absent from
// release binaries (which never call EnforceSandboxedConfigPaths). Same shape and
// rationale as api.EnableSupervisorIPCTestPipeIsolation
// (internal/api/supervisor_ipc_address_windows.go:40).
package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// SandboxAuditModeEnv selects the audit's failure channel. BOTH values stop the
// escaping goroutine before the offending read or write reaches the operator's
// file — they differ only in how much of the run survives.
//
// UNSET (the default) is FAIL-FAST: the first escape panics, aborting the whole
// test binary. Loudest, and the right default for a package already known clean.
//
// "report" aborts only the OFFENDING TEST (runtime.Goexit, the same mechanism
// t.FailNow uses) and lets the rest of the package run, collecting every escape
// and printing the whole set at Restore(). Use it to onboard a package that has
// never run under the audit, where fail-fast shows you one violation per run.
//
// report is NOT a bypass. It does not let the escaping code proceed, and it still
// fails the package: Restore() returns a non-zero count that TestMain turns into
// a non-zero exit code. There is deliberately no value that disables the audit —
// the only sanctioned exemption is SuspendSandboxAudit at a single call site.
const SandboxAuditModeEnv = "MCPHUB_CLIENT_CONFIG_SANDBOX_AUDIT"

// sandboxAuditor holds one installed audit. Adapters are constructed from many
// goroutines (parallel tests, handler goroutines), so roots is read-only after
// construction and the violation set is mutex-guarded.
type sandboxAuditor struct {
	roots  []string // cleaned; lowercased on case-insensitive filesystems
	report bool

	mu         sync.Mutex
	seen       map[string]bool
	violations []string
}

// activeSandboxAuditor is nil in production. The construction seam does one
// atomic load per adapter and returns immediately when it is nil, so the
// production cost is a single uncontended pointer load — it never even calls
// ConfigPath().
var activeSandboxAuditor atomic.Pointer[sandboxAuditor]

// auditConstructedClient is the construction seam. Both registry factory call
// sites (ConfigPathForName and AllClientsWithErrors, the only two places this
// package builds an adapter) call it immediately after a successful
// construction, so every adapter this build can produce passes through here.
func auditConstructedClient(name string, c Client) {
	a := activeSandboxAuditor.Load()
	if a == nil {
		return
	}
	a.check(name, c.ConfigPath())
}

// EnforceSandboxedConfigPaths installs the audit for the calling test binary and
// returns a restore func. Call it from TestMain, and turn a non-zero return into
// a non-zero exit code:
//
//	auditRestore := clients.EnforceSandboxedConfigPaths(tmp)
//	code := m.Run()
//	if n := auditRestore(); n > 0 {
//	    code = 1
//	}
//	os.Exit(code)
//
// extraRoots widens the sandbox beyond the process temp root — pass any
// throwaway directory the package's TestMain created outside os.TempDir(). The
// restore func un-installs the audit and returns the number of DISTINCT escapes
// recorded (always 0 in fail-fast mode, which panics instead of recording).
func EnforceSandboxedConfigPaths(extraRoots ...string) func() int {
	roots := make([]string, 0, len(extraRoots)+2)
	seenRoot := map[string]bool{}
	appendRoot := func(p string) {
		n := normalizeSandboxPath(p)
		if seenRoot[n] {
			return
		}
		seenRoot[n] = true
		roots = append(roots, n)
	}
	add := func(p string) {
		if p == "" {
			return
		}
		appendRoot(p)
		// A temp root is a symlink on macOS (/var -> /private/var) and can be one
		// on Linux. Accept the resolved form too, since os.MkdirTemp hands back
		// whichever spelling its parent carried. Deduped: on Windows the resolved
		// form usually differs only in case, which normalizes to the same root.
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			appendRoot(resolved)
		}
	}
	// Captured ONCE, here, before any test runs: a fixture is free to t.Setenv
	// TMP/TMPDIR later, and the sandbox definition must not move under it.
	add(os.TempDir())
	for _, r := range extraRoots {
		add(r)
	}

	a := &sandboxAuditor{
		roots:  roots,
		report: strings.EqualFold(os.Getenv(SandboxAuditModeEnv), "report"),
		seen:   map[string]bool{},
	}
	prev := activeSandboxAuditor.Swap(a)
	return func() int {
		activeSandboxAuditor.Store(prev)
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(a.violations) > 0 {
			fmt.Fprintf(os.Stderr, "\nCLIENT-CONFIG SANDBOX AUDIT: %d escape(s) recorded in report mode:\n\n%s\n",
				len(a.violations), strings.Join(a.violations, "\n\n"))
		}
		return len(a.violations)
	}
}

// SuspendSandboxAudit un-installs the audit until the returned func is called.
//
// THE ONLY legitimate caller is a test whose SUBJECT is real path derivation —
// e.g. api's TestDefaultScanConfigPaths_CoversEverySupportedClient, which
// deliberately resolves every registered client's real path and only
// string-compares two resolvers against each other. Such a test reads no file, so
// it cannot leak; it just cannot satisfy a sandbox predicate by construction.
//
// It is NOT an escape hatch for a fixture that is inconvenient to isolate. If the
// test touches the filesystem at the resolved path — Exists, GetEntry, AddEntry,
// RemoveEntry, Backup, BackupKeep, InitEmpty — suspending the audit re-opens
// exactly the hole this file exists to close. Isolate the fixture instead.
//
// Suspension is process-wide, so a suspending test must not run in parallel with
// tests that rely on the audit. Call it with defer, never across a t.Parallel().
func SuspendSandboxAudit() (restore func()) {
	prev := activeSandboxAuditor.Swap(nil)
	return func() { activeSandboxAuditor.Store(prev) }
}

func (a *sandboxAuditor) check(name, path string) {
	if path == "" {
		// A factory that could not resolve a path cannot leak through one.
		return
	}
	abs := path
	if !filepath.IsAbs(abs) {
		// A relative path resolves against the test process's cwd — the repo
		// checkout — which is outside the sandbox and IS an escape. Make it
		// absolute so the message shows where it actually lands.
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	normalized := normalizeSandboxPath(abs)
	for _, root := range a.roots {
		if withinSandboxRoot(normalized, root) {
			return
		}
	}
	a.record(name, abs)
}

func (a *sandboxAuditor) record(name, abs string) {
	frames := sandboxTestFrames()
	msg := formatSandboxEscapeWithFrames(name, abs, a.roots, frames)

	// Dedupe per (adapter, path, TEST) — not per (adapter, path). Report mode's
	// whole job is to enumerate the offending FIXTURES, and 40 tests sharing one
	// leaky helper all escape via the same adapter and the same path; keying
	// without the frame would report the first and silently swallow the other 39.
	key := name + "\x00" + abs
	if len(frames) > 0 {
		key += "\x00" + frames[len(frames)-1]
	}

	a.mu.Lock()
	first := !a.seen[key]
	if first {
		a.seen[key] = true
		if a.report {
			a.violations = append(a.violations, msg)
		}
	}
	a.mu.Unlock()

	if !a.report {
		panic(msg)
	}
	if first {
		fmt.Fprintf(os.Stderr, "\n%s\n", msg)
	}
	// Abort THIS test, not the binary. runtime.Goexit is what t.FailNow uses: it
	// runs the goroutine's defers (so t.Cleanup and t.Setenv restoration still
	// happen) and terminates it, and the testing package reports the test as
	// failed. Crucially the escaping call never returns, so the read or write it
	// was about to perform against the operator's real config does not happen —
	// report mode surveys the package without ever letting an escape land.
	runtime.Goexit()
}

// formatSandboxEscape builds the operator-actionable failure text. It must name
// the adapter, the resolved path, the environment variable responsible, and the
// _test.go frame that got here — anything less and the next developer cannot act
// on it.
func formatSandboxEscape(name, abs string, roots []string) string {
	return formatSandboxEscapeWithFrames(name, abs, roots, sandboxTestFrames())
}

func formatSandboxEscapeWithFrames(name, abs string, roots, frames []string) string {
	var b strings.Builder
	b.WriteString("CLIENT-CONFIG SANDBOX ESCAPE\n\n")
	fmt.Fprintf(&b, "  adapter:  %s\n", name)
	fmt.Fprintf(&b, "  resolved: %s\n", abs)
	fmt.Fprintf(&b, "  sandbox:  %s\n", strings.Join(roots, "\n            "))
	if culprits := sandboxEnvCulprits(abs); len(culprits) > 0 {
		b.WriteString("  env var responsible (longest matching prefix first):\n")
		for _, c := range culprits {
			fmt.Fprintf(&b, "            %s=%s\n", c.key, c.value)
		}
	} else {
		b.WriteString("  env var responsible: none — the adapter derived this path\n" +
			"            without an environment prefix (home-dir or hard-coded root).\n" +
			"            Redirect HOME/USERPROFILE for this fixture.\n")
	}
	if len(frames) > 0 {
		b.WriteString("  reached from:\n")
		for _, f := range frames {
			fmt.Fprintf(&b, "            %s\n", f)
		}
	} else {
		b.WriteString("  reached from: no _test.go frame on this goroutine (the adapter was\n" +
			"            constructed on a goroutine spawned by production code).\n")
	}
	b.WriteString("\n" +
		"  This adapter resolved to a path OUTSIDE the test sandbox, i.e. to a REAL\n" +
		"  config file on this machine. Reading it makes the test host-dependent;\n" +
		"  writing it destroys the operator's data. Two fixtures have already done\n" +
		"  the latter — see internal/clients/config_path_sandbox_audit.go.\n\n" +
		"  FIX: in the fixture named above, redirect HOME/USERPROFILE/LOCALAPPDATA to\n" +
		"  a t.TempDir() and call cli.neutralizeClientConfigPathEnv(t, thatDir)\n" +
		"  (internal/cli/client_config_env_isolation_test.go) — or, outside package\n" +
		"  cli, set the env var named above to a path under that same dir. Do NOT\n" +
		"  narrow the client set to dodge this: the next adapter added to the\n" +
		"  registry would leak again.\n" +
		"  Set " + SandboxAuditModeEnv + "=report to collect every escape in one run\n" +
		"  instead of aborting on the first.")
	return b.String()
}

type sandboxEnvCulprit struct{ key, value string }

// sandboxEnvCulprits names the environment variables whose CURRENT value is a
// directory-boundary prefix of the escaped path, longest first.
//
// This is derived from os.Environ(), never from a maintained list of "path env
// vars" — that list is precisely the thing that went stale twice. An adapter
// added tomorrow that reads $SOME_NEW_CLIENT_HOME is attributed correctly with
// no edit here.
func sandboxEnvCulprits(abs string) []sandboxEnvCulprit {
	normalized := normalizeSandboxPath(abs)
	var out []sandboxEnvCulprit
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key, value := kv[:eq], kv[eq+1:]
		// Below 4 chars a "prefix" is noise: a drive letter, "/", "." and the
		// like match everything and name nothing.
		if len(value) < 4 || !filepath.IsAbs(value) {
			continue
		}
		if withinSandboxRoot(normalized, normalizeSandboxPath(value)) {
			out = append(out, sandboxEnvCulprit{key: key, value: value})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].value) > len(out[j].value) })
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// sandboxTestFrames returns the _test.go frames on the current goroutine,
// innermost first. That is the fixture and the test that reached the adapter —
// the only two facts that tell the next developer WHICH file to fix.
func sandboxTestFrames() []string {
	pcs := make([]uintptr, 40)
	n := runtime.Callers(3, pcs)
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.File, "_test.go") {
			out = append(out, fmt.Sprintf("%s:%d (%s)", filepath.Base(f.File), f.Line, f.Function))
		}
		if !more || len(out) >= 4 {
			break
		}
	}
	return out
}

// normalizeSandboxPath cleans a path and, on a case-insensitive filesystem,
// folds its case so the prefix compare is not defeated by C:\Users vs c:\users.
func normalizeSandboxPath(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

// withinSandboxRoot reports whether normalized path p is root or lives under it.
// The separator check is what keeps "C:\Temp2\x" from matching root "C:\Temp".
func withinSandboxRoot(p, root string) bool {
	if root == "" {
		return false
	}
	if p == root {
		return true
	}
	root = strings.TrimSuffix(root, string(filepath.Separator))
	return strings.HasPrefix(p, root+string(filepath.Separator))
}
