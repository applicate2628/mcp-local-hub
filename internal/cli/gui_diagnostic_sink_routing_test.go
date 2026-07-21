package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/gui"

	"github.com/spf13/cobra"
)

// installSinksOnFreshCommand installs the GUI runtime sinks on a throwaway
// cobra command whose out and err writers are DISTINCT buffers, and returns
// them. Distinct buffers are the whole point: a test that wires one buffer to
// both streams cannot tell a stdout-bound write from a stderr-bound one, which
// is precisely the confusion that let the defect ship.
func installSinksOnFreshCommand(t *testing.T) (cmd *cobra.Command, stdout, stderr *bytes.Buffer) {
	t.Helper()

	// captureGUIDiagnosticEvents snapshots and restores the process-global
	// sinks, which are sticky by design.
	captureGUIDiagnosticEvents(t)

	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	cmd = &cobra.Command{Use: "gui"}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	installGUIRuntimeSinks(cmd)
	return cmd, stdout, stderr
}

// TestGUIRuntimeSinksRouteStderrIntentToStderr is the regression guard for the
// stream misrouting introduced with the diagnostic sink.
//
// cobra resolves BOTH OutOrStdout() and OutOrStderr() through getOut()
// (command.go:393-400), and getOut returns c.outWriter whenever it is non-nil
// (command.go:412-415). installGUIRuntimeSinks calls SetOut, so from that point
// on `cmd.OutOrStderr()` — the cobra idiom that reads as "stderr" — resolves to
// the STDOUT sink. Every intent-stderr diagnostic in startGuiServerWithStartup
// silently moved to stdout.
//
// Asserting that some writer is non-nil would pass in both the fixed and the
// broken tree. This test instead pins WHICH sink each accessor reaches, by
// identity and by where a written byte actually lands. It fails if someone
// reinstates a bare SetOut without moving the call sites, and it fails if a
// call site is moved back to OutOrStderr().
func TestGUIRuntimeSinksRouteStderrIntentToStderr(t *testing.T) {
	cmd, stdout, stderr := installSinksOnFreshCommand(t)

	// 1. Identity: the accessor a diagnostic uses must reach the matching sink.
	if got := cmd.ErrOrStderr(); got != io.Writer(guiRuntimeStderr) {
		t.Errorf("cmd.ErrOrStderr() = %T (%p), want the guiRuntimeStderr sink (%p); "+
			"intent-stderr diagnostics would not be durably captured as stderr",
			got, got, guiRuntimeStderr)
	}
	if got := cmd.OutOrStdout(); got != io.Writer(guiRuntimeStdout) {
		t.Errorf("cmd.OutOrStdout() = %T (%p), want the guiRuntimeStdout sink (%p)",
			got, got, guiRuntimeStdout)
	}

	// 2. The trap itself, pinned explicitly so the next reader cannot mistake
	//    it for a bug in this test. OutOrStderr resolves through getOut, so
	//    under an installed SetOut it is the STDOUT sink. This is cobra's
	//    documented-by-source behaviour, not something the sink can change.
	if got := cmd.OutOrStderr(); got != io.Writer(guiRuntimeStdout) {
		t.Errorf("cmd.OutOrStderr() = %p, want the guiRuntimeStdout sink (%p). "+
			"If cobra's getOut() semantics changed, the ErrOrStderr() call sites in "+
			"gui.go and the comments in gui_diagnostic_sink.go must be revisited.",
			got, guiRuntimeStdout)
	}

	// 3. Delivery: a stderr-intent write must land on the stderr stream and
	//    must NOT contaminate stdout. This is the operator-visible contract —
	//    `mcphub gui > out.txt` must not swallow a warning.
	const warning = "warning: MCPHUB_E2E_SUPERVISOR=none is set — supervisor spawn suppressed"
	fmt.Fprintln(cmd.ErrOrStderr(), warning)

	if !bytes.Contains(stderr.Bytes(), []byte(warning)) {
		t.Errorf("a stderr-intent diagnostic did not reach the stderr stream; stderr=%q stdout=%q",
			stderr.String(), stdout.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte(warning)) {
		t.Errorf("a stderr-intent diagnostic leaked onto STDOUT; a script consuming stdout would "+
			"see it as normal output. stdout=%q", stdout.String())
	}
}

// TestInstallGUIRuntimeSinksCapturesTheErrFallback pins that the stderr sink
// forwards to the command's ERR writer.
//
// installGUIRuntimeSinks originally captured the stderr fallback with
// cmd.OutOrStderr(), which resolves through getOut — so on a command with an
// out writer already set (any test, and any second GUI start in one process)
// the stderr sink forwarded to the OUT writer. The setFallback identity guard
// cannot catch that case: it only rejects a sink pointed at itself, and
// guiRuntimeStdout is not guiRuntimeStderr.
func TestInstallGUIRuntimeSinksCapturesTheErrFallback(t *testing.T) {
	cmd, stdout, stderr := installSinksOnFreshCommand(t)

	guiRuntimeStderr.mu.Lock()
	fallback := guiRuntimeStderr.fallback
	guiRuntimeStderr.mu.Unlock()

	if fallback != io.Writer(stderr) {
		t.Fatalf("guiRuntimeStderr.fallback = %p, want the command's err writer (%p); "+
			"capture it with cmd.ErrOrStderr(), not cmd.OutOrStderr()", fallback, stderr)
	}

	// Writing straight at the sink (as the supervisor exit monitor's goroutine
	// does) must reach stderr, not stdout.
	const crashLine = "warning: supervisor exited unexpectedly (PID 4242)"
	fmt.Fprintln(guiRuntimeStderr, crashLine)

	if !bytes.Contains(stderr.Bytes(), []byte(crashLine)) {
		t.Errorf("direct guiRuntimeStderr write missed the err stream; stderr=%q", stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte(crashLine)) {
		t.Errorf("direct guiRuntimeStderr write leaked onto stdout=%q", stdout.String())
	}
	_ = cmd
}

// readGUISource reads gui.go from the package directory (go test runs each
// package with its own source dir as the working directory).
func readGUISource(t *testing.T) ([]byte, error) {
	t.Helper()
	return os.ReadFile("gui.go")
}

// TestGUIStderrIntentSitesUseErrOrStderr is the source-level half of the guard.
//
// The runtime test above proves the accessors resolve correctly, but it cannot
// see a call site that still uses OutOrStderr() — startGuiServerWithStartup
// needs a bound listener, a supervisor and a tray to reach most of its
// diagnostics, so they are not exercisable in a unit test on this host.
//
// This scans the source instead, and it covers TWO disjoint regions for TWO
// DIFFERENT reasons, because source-order is not the same as invocation-order:
//
//  1. The post-install region — everything from installGUIRuntimeSinks(cmd) to
//     runSessionCleanupTicker. Every diagnostic there executes AFTER the sink's
//     SetOut, so an OutOrStderr() there resolves (through cobra's getOut) to the
//     STDOUT sink and misroutes. The `--force` / `--reset-port` helpers BELOW
//     that region legitimately keep OutOrStderr(): they run BEFORE the sinks are
//     installed, where outWriter is nil and OutOrStderr() genuinely is os.Stderr,
//     and their tests wire a single buffer via SetOut alone.
//
//  2. The buildRestartV3ParentDependencies region — which sits ABOVE the install
//     marker, so region (1) is STRUCTURALLY BLIND to it. That function builds the
//     targetPort closure, which is DEFINED pre-sink but INVOKED post-sink: it
//     runs only when RestartCoordinator.Start calls it
//     (internal/gui/gui_restart_protocol.go) on /api/gui/restart, after the sinks
//     are installed. So at invocation time its OutOrStderr() resolves to the
//     STDOUT sink exactly like a post-install site would — a defect region (1)
//     could never see because the call site's source position is pre-install.
//     RestartV3 is default-ON (internal/gui/gui_restart_gate.go), so this path is
//     live. The check is scoped to the function's own byte-region so the
//     genuinely pre-sink INLINE emitInvalidGUIPortWarning site in newGuiCmdReal's
//     RunE (which runs before any startGuiServer* call, where OutOrStderr() is
//     really os.Stderr) is not swept in.
func TestGUIStderrIntentSitesUseErrOrStderr(t *testing.T) {
	src, err := readGUISource(t)
	if err != nil {
		t.Fatalf("read gui.go: %v", err)
	}

	// Region 1: everything after the sink is installed.
	const (
		installMarker = "installGUIRuntimeSinks(cmd)"
		endMarker     = "func runSessionCleanupTicker("
	)
	start := bytes.Index(src, []byte(installMarker))
	if start < 0 {
		t.Fatalf("marker %q not found; if the install call moved, update this guard", installMarker)
	}
	end := bytes.Index(src, []byte(endMarker))
	if end < 0 || end <= start {
		t.Fatalf("marker %q not found after the install call; update this guard", endMarker)
	}

	region := src[start:end]
	if idx := bytes.Index(region, []byte("cmd.OutOrStderr()")); idx >= 0 {
		line := 1 + bytes.Count(src[:start+idx], []byte("\n"))
		t.Errorf("gui.go:%d uses cmd.OutOrStderr() after installGUIRuntimeSinks. "+
			"cobra resolves it through getOut(), so with the sink's SetOut installed it "+
			"returns the STDOUT sink and the diagnostic lands on stdout. "+
			"Use cmd.ErrOrStderr() for anything whose intent is stderr.", line)
	}

	// Region 2: the deferred targetPort closure inside
	// buildRestartV3ParentDependencies. It is DEFINED before the install marker
	// (so region 1 cannot see it) but INVOKED after it, so it has the same
	// misrouting exposure. Scope strictly to this function's byte-region so the
	// genuinely pre-sink inline site at newGuiCmdReal's RunE is excluded.
	const (
		deferredStartMarker = "func buildRestartV3ParentDependencies("
		deferredEndMarker   = "func runRestartV3ChildStartup("
	)
	dStart := bytes.Index(src, []byte(deferredStartMarker))
	if dStart < 0 {
		t.Fatalf("marker %q not found; if the function was renamed, update this guard", deferredStartMarker)
	}
	dEnd := bytes.Index(src, []byte(deferredEndMarker))
	if dEnd < 0 || dEnd <= dStart {
		t.Fatalf("marker %q not found after %q; update this guard", deferredEndMarker, deferredStartMarker)
	}

	deferredRegion := src[dStart:dEnd]
	if idx := bytes.Index(deferredRegion, []byte("cmd.OutOrStderr()")); idx >= 0 {
		line := 1 + bytes.Count(src[:dStart+idx], []byte("\n"))
		t.Errorf("gui.go:%d uses cmd.OutOrStderr() inside buildRestartV3ParentDependencies. "+
			"The targetPort closure is DEFINED pre-sink but INVOKED at restart time by "+
			"RestartCoordinator.Start (internal/gui/gui_restart_protocol.go) AFTER the sinks "+
			"are installed, so cobra's getOut() resolves it to the STDOUT sink and the "+
			"invalid-persisted-port warning lands on stdout. Use cmd.ErrOrStderr().", line)
	}
	// Positive assertion: the invalid-persisted-port warning must still route
	// through the stderr accessor. This also fails closed if the emit call is
	// removed outright, so the region-2 negative check above cannot pass vacuously.
	if !bytes.Contains(deferredRegion, []byte("cmd.ErrOrStderr()")) {
		t.Errorf("buildRestartV3ParentDependencies no longer routes any diagnostic through " +
			"cmd.ErrOrStderr(); the restart-time invalid-persisted-port warning must reach the " +
			"stderr sink. If emitInvalidGUIPortWarning was moved or removed, update this guard.")
	}
}

// TestRestartV3TargetPortWarningRoutesToStderrSink is the runtime companion to
// the region-2 source guard above. The source guard proves no cmd.OutOrStderr()
// survives inside buildRestartV3ParentDependencies; this drives the closure for
// real — with the diagnostic sink already installed — and proves the
// invalid-persisted-port warning lands on the STDERR sink, never on stdout.
//
// It reproduces the defect's invocation-time conditions exactly: the sink is
// installed on the command FIRST (so cmd.OutOrStderr() would resolve to the
// STDOUT sink through cobra's getOut), THEN the deferred targetPort closure is
// invoked, exactly as RestartCoordinator.Start invokes it at /api/gui/restart
// time. A revert of the emit site to cmd.OutOrStderr() flips both assertions:
// the warning would appear on stdout and be absent from stderr.
//
// Host-safety note: emitInvalidGUIPortWarning also fires an async, best-effort
// api.LogHubMcpEvent goroutine. The package TestMain redirects the api state
// root (api.SetDaemonStateRootForTest) plus LOCALAPPDATA / XDG_* to a throwaway
// temp dir for the whole cli test package, so that log write never touches the
// real host. Only the synchronous sink write reaches the stdout/stderr buffers
// asserted below.
func TestRestartV3TargetPortWarningRoutesToStderrSink(t *testing.T) {
	cmd, stdout, stderr := installSinksOnFreshCommand(t)

	runtime := restartV3ParentRuntime{
		// An unparseable persisted port forces guiPortIntentInvalid — the one
		// classification emitInvalidGUIPortWarning acts on.
		SettingsGet: func(string) (string, error) { return "not-a-port", nil },
		Spawn: func([]string, gui.SelfRestartHandoff) (gui.RestartParentChild, error) {
			t.Fatal("Spawn must not be reached: TargetPort resolves the port, it does not spawn")
			return nil, nil
		},
		Confirm: func(context.Context, int, []byte, gui.AuthenticatedReadinessIdentity) error { return nil },
		Exit:    func() {},
	}

	deps, err := buildRestartV3ParentDependencies(
		context.Background(), cmd, &phaseGCLILease{},
		filepath.Join(t.TempDir(), "pidport"),
		func() int { return 9125 },
		[]string{"gui"}, runtime,
	)
	if err != nil {
		t.Fatalf("buildRestartV3ParentDependencies: %v", err)
	}

	// A valid actual port passes validPersistedGUIPort; the INVALID persisted
	// port (from SettingsGet) is what triggers the warning. TargetPort falls back
	// to the actual port for invalid intent, so this returns 9125, nil.
	got, err := deps.TargetPort(9125)
	if err != nil {
		t.Fatalf("TargetPort: %v", err)
	}
	if got != 9125 {
		t.Fatalf("TargetPort = %d, want 9125 (fallback to actual port on invalid persisted intent)", got)
	}

	// gui-port-persisted-invalid is the load-bearing token of the warning line
	// (formatInvalidGUIPortWarning); the async log event carries the same token
	// but never touches these buffers.
	const marker = "gui-port-persisted-invalid"
	if !bytes.Contains(stderr.Bytes(), []byte(marker)) {
		t.Errorf("restart-time invalid-persisted-port warning did not reach the STDERR sink; "+
			"stderr=%q stdout=%q", stderr.String(), stdout.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte(marker)) {
		t.Errorf("restart-time invalid-persisted-port warning leaked onto STDOUT; a script "+
			"consuming `mcphub gui` stdout would see it as normal output. stdout=%q", stdout.String())
	}
}
