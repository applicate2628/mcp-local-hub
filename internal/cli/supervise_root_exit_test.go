package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

// Consumer regression adapted from Wave c3b8c802. Reuse the existing admitted
// exit-code helper, so no new helper role or package-root lifetime is introduced.
func waitDaemonRootExitChild(t *testing.T, code int) error {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestFormatChildExit_RealProcessShowsExitCode$")
	cmd.Env = append(withoutCLITestHelperEnvironment(os.Environ()), "MCPHUB_TEST_CHILD_EXIT_CODE="+strconv.Itoa(code))
	process.NoConsole(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start controlled legacy child: %v", err)
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("controlled legacy child exceeded its deadline: %v", ctx.Err())
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || cmd.ProcessState.ExitCode() != code {
		t.Fatalf("controlled child did not report requested exit %d", code)
	}
	if code == 0 {
		if waitErr != nil {
			t.Fatalf("zero-exit child Wait: %v", waitErr)
		}
	} else {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != code {
			t.Fatalf("nonzero child did not return its real ExitError: %v", waitErr)
		}
	}
	return waitErr
}

// These are consumer-side snapshots, including full Windows DWORD values.
// No ProcessState or ExitError is fabricated for the creator path.
func TestDaemonWaitExitCodeUsesKnownCreatorResult(t *testing.T) {
	for _, code := range []uint32{0, 7, 259, 0x80000001, 0xffffffff} {
		t.Run(strconv.FormatUint(uint64(code), 10), func(t *testing.T) {
			root := process.CreatorRootExitSnapshot{Known: true, ExitCode: int(code)}
			if got := daemonWaitExitCode(&root, nil); got != root.ExitCode {
				t.Fatalf("known creator exit = %d, want %d without RawExitError", got, root.ExitCode)
			}
		})
	}
	t.Run("observer-error-keeps-known-code", func(t *testing.T) {
		root := process.CreatorRootExitSnapshot{Known: true, ExitCode: 7}
		observerErr := errors.New("synthetic observer error after a known root result")
		if got := daemonWaitExitCode(&root, observerErr); got != 7 {
			t.Fatalf("known creator exit with observer error = %d, want 7", got)
		}
	})
}

func TestDaemonWaitExitCodePreservesLegacyWait(t *testing.T) {
	cleanWait := waitDaemonRootExitChild(t, 0)
	failedWait := waitDaemonRootExitChild(t, 7)
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "clean", err: cleanWait, want: 0},
		{name: "nonzero", err: failedWait, want: 7},
		{name: "wrapped-nonzero", err: fmt.Errorf("legacy wait: %w", failedWait), want: 7},
		{name: "non-exit-error", err: errors.New("synthetic legacy wait failure"), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonWaitExitCode(nil, tc.err); got != tc.want {
				t.Fatalf("legacy exit = %d, want %d", got, tc.want)
			}
		})
	}
	// An unrelated real ExitError must not override an authoritative snapshot,
	// including a known zero. Unknown payload must not acquire its authority.
	for _, tc := range []struct {
		name string
		root process.CreatorRootExitSnapshot
		want int
	}{
		{name: "known-zero-precedes-error", root: process.CreatorRootExitSnapshot{Known: true}, want: 0},
		{name: "known-259-precedes-error", root: process.CreatorRootExitSnapshot{Known: true, ExitCode: 259}, want: 259},
		{name: "unknown-ignores-payload-and-error-code", root: process.CreatorRootExitSnapshot{ExitCode: 259}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonWaitExitCode(&tc.root, failedWait); got != tc.want {
				t.Fatalf("creator exit selection = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDaemonWaitExitCodeUnknownRetainsTerminalRefusal(t *testing.T) {
	// The real public API rejects a missing original owner with Unknown and
	// an error. This is not evidence of a native wait or CloseHandle failure.
	owner := &process.ProvisionalProcess{}
	root, waitErr := owner.WaitCreatorRoot()
	if root.Known || waitErr == nil {
		t.Fatal("missing original owner did not preserve Unknown and its error")
	}
	if got := daemonWaitExitCode(&root, waitErr); got != 0 {
		t.Fatalf("unknown diagnostic default = %d, want the existing default 0", got)
	}
	records := &daemonCreatorRecords{}
	operation, err := records.begin("root-exit-unknown-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := records.retain(operation, owner); err != nil {
		t.Fatal(err)
	}
	records.endStart(operation, nil)
	operation.root, operation.rootErr = root, waitErr
	// This is the caller's existing Unknown policy, not a terminal receipt.
	terminalErr := errors.New("synthetic caller refusal: original root exit unavailable")
	operation.terminalErr = terminalErr
	published := false
	err = records.publishAfterCreatorClosure(operation, func() { published = true })
	if published || !errors.Is(err, terminalErr) {
		t.Fatal("Unknown entered exit publication or lost the terminal refusal")
	}
	records.workerFinished(operation, false, err)
	if operation.root.Known || operation.rootErr != waitErr || operation.complete || operation.waiterErr != terminalErr {
		t.Fatal("Unknown diagnostic default became known/complete or lost its errors")
	}
	if _, err := records.begin(operation.task); err == nil {
		t.Fatal("Unknown terminal refusal admitted a replacement owner")
	}
}
