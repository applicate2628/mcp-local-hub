package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"

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
// This scans the source instead: within startGuiServerWithStartup's region —
// everything after installGUIRuntimeSinks — no cmd.OutOrStderr() may appear.
// The `--force` / `--reset-port` helpers below that region legitimately keep
// OutOrStderr(): they run BEFORE the sinks are installed, where outWriter is
// nil and OutOrStderr() genuinely is os.Stderr, and their tests wire a single
// buffer via SetOut alone.
func TestGUIStderrIntentSitesUseErrOrStderr(t *testing.T) {
	src, err := readGUISource(t)
	if err != nil {
		t.Fatalf("read gui.go: %v", err)
	}

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
}
