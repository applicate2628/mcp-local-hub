package gui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestClassifyGUITestHelperDispatchTruthTable(t *testing.T) {
	t.Parallel()
	const (
		r6Selector       = "^TestAuditLockTerminalWorker_RealHTTPEventPersistenceAndSecondRun$"
		blockingSelector = "^TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn$"
		pidfdSelector    = "^TestRetainedPIDFDAlive_LinuxChildHelper$"
	)
	abs := t.TempDir()
	tests := []struct {
		name       string
		args       []string
		env        []string
		goos       string
		wantRole   guiTestProcessRole
		wantReason guiTestDispatchFailure
	}{
		{name: "ordinary selector", args: []string{"gui.test", "-test.run=^TestOrdinary$"}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "r6 outer one dash attached", args: []string{"gui.test", "-test.run=" + r6Selector}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "r6 outer two dash attached", args: []string{"gui.test", "--test.run=" + r6Selector}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "r6 outer one dash split", args: []string{"gui.test", "-test.run", r6Selector}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "r6 outer two dash split", args: []string{"gui.test", "--test.run", r6Selector}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "empty attached", args: []string{"gui.test", "-test.run="}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "empty attached two dash", args: []string{"gui.test", "--test.run="}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "empty split one dash", args: []string{"gui.test", "-test.run", ""}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "empty split", args: []string{"gui.test", "--test.run", ""}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "parse stop", args: []string{"gui.test", "--", "-test.run=" + blockingSelector}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "first positional stops parsing", args: []string{"gui.test", "ordinary", "-test.run=" + blockingSelector}, goos: "linux", wantRole: guiTestRoleNormalParent},
		{name: "r6 receiver", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=1", auditLockR6StateRootEnv + "=" + abs}, goos: "linux", wantRole: guiTestRoleR6ReceiverChild},
		{name: "blocking child", args: []string{"gui.test", "-test.run=" + blockingSelector}, env: []string{auditLockBlockingHelperEnv + "=1", auditLockHelperLockEnv + "=" + abs, auditLockHelperEnteredEnv + "=" + abs}, goos: "linux", wantRole: guiTestRoleBlockingHelperChild},
		{name: "plain terminal worker", args: []string{"gui.test", auditLockTerminalWorkerArg}, goos: "linux", wantRole: guiTestRoleAuditTerminalWorkerChild},
		{name: "stderr terminal worker", args: []string{"gui.test", auditLockTerminalWorkerArg}, env: []string{auditLockTerminalWorkerStderrHelperEnv + "=1"}, goos: "linux", wantRole: guiTestRoleAuditTerminalWorkerChild},
		{name: "linux pidfd child", args: []string{"gui.test", "-test.run=" + pidfdSelector}, env: []string{pidfdTestChildEnv + "=1"}, goos: "linux", wantRole: guiTestRolePIDFDLinuxChild},
		{name: "linux stalled pidfd child", args: []string{"gui.test", "--test.run", pidfdSelector}, env: []string{pidfdTestChildEnv + "=1", pidfdTestChildStallEnv + "=1"}, goos: "linux", wantRole: guiTestRolePIDFDLinuxChild},
		{name: "missing split selector", args: []string{"gui.test", "-test.run"}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureInvalidSelectorGrammar},
		{name: "unsupported selector spelling", args: []string{"gui.test", "-test.run:" + r6Selector}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureInvalidSelectorGrammar},
		{name: "duplicate selector", args: []string{"gui.test", "-test.run=" + r6Selector, "--test.run", r6Selector}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureDuplicateSelector},
		{name: "blocking selector only", args: []string{"gui.test", "-test.run=" + blockingSelector}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureSelectorOnly},
		{name: "partial r6", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=1"}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailurePartialFrame},
		{name: "present empty r6 marker", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=", auditLockR6StateRootEnv + "=" + abs}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureUnknownValue},
		{name: "unknown marker value", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=2", auditLockR6StateRootEnv + "=" + abs}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureUnknownValue},
		{name: "conflicting frames", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=1", auditLockR6StateRootEnv + "=" + abs, auditLockBlockingHelperEnv + "=1", auditLockHelperLockEnv + "=" + abs, auditLockHelperEnteredEnv + "=" + abs}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureConflict},
		{name: "partial conflicting frame", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=1", auditLockR6StateRootEnv + "=" + abs, auditLockBlockingHelperEnv + "=1"}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureConflict},
		{name: "relative r6 path", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=1", auditLockR6StateRootEnv + "=relative"}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureInvalidPath},
		{name: "terminal worker with selector", args: []string{"gui.test", "-test.run=^TestOrdinary$", auditLockTerminalWorkerArg}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureWrongArgv},
		{name: "terminal fault on normal argv", args: []string{"gui.test", "-test.run=^TestOrdinary$"}, env: []string{auditLockTerminalWorkerStderrHelperEnv + "=1"}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureWrongArgv},
		{name: "pidfd on windows", args: []string{"gui.test", "-test.run=" + pidfdSelector}, env: []string{pidfdTestChildEnv + "=1"}, goos: "windows", wantRole: guiTestRoleInvalid, wantReason: guiTestFailurePIDFDFrameInvalid},
		{name: "pidfd stall without child", args: []string{"gui.test", "-test.run=" + pidfdSelector}, env: []string{pidfdTestChildStallEnv + "=1"}, goos: "linux", wantRole: guiTestRoleInvalid, wantReason: guiTestFailurePIDFDFrameInvalid},
		{name: "windows mixed case r6", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{strings.ToLower(auditLockR6ReceiverHelperEnv) + "=1", strings.ToLower(auditLockR6StateRootEnv) + "=" + abs}, goos: "windows", wantRole: guiTestRoleR6ReceiverChild},
		{name: "windows duplicate logical key", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{auditLockR6ReceiverHelperEnv + "=1", strings.ToLower(auditLockR6ReceiverHelperEnv) + "=1", auditLockR6StateRootEnv + "=" + abs}, goos: "windows", wantRole: guiTestRoleInvalid, wantReason: guiTestFailureDuplicateEnvKey},
		{name: "posix differently cased key unrelated", args: []string{"gui.test", "-test.run=" + r6Selector}, env: []string{strings.ToLower(auditLockR6ReceiverHelperEnv) + "=1"}, goos: "linux", wantRole: guiTestRoleNormalParent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dispatch := classifyGUITestHelperDispatch(tc.args, tc.env, tc.goos)
			if dispatch.role != tc.wantRole || dispatch.reason != tc.wantReason {
				t.Fatalf("dispatch=(role=%v reason=%q), want (role=%v reason=%q)", dispatch.role, dispatch.reason, tc.wantRole, tc.wantReason)
			}
		})
	}
}

func TestWithoutGUITestHelperEnvironmentUsesPlatformLogicalIdentity(t *testing.T) {
	t.Parallel()
	input := []string{
		"KEEP=1",
		auditLockR6ReceiverHelperEnv + "=1",
		strings.ToLower(auditLockR6StateRootEnv) + "=lower",
		pidfdTestChildEnv + "=1",
	}
	windows := withoutGUITestHelperEnvironment(input, "windows")
	if want := []string{"KEEP=1"}; !slices.Equal(windows, want) {
		t.Fatalf("Windows filtered env=%q, want %q", windows, want)
	}
	posix := withoutGUITestHelperEnvironment(input, "linux")
	if want := []string{"KEEP=1", strings.ToLower(auditLockR6StateRootEnv) + "=lower"}; !slices.Equal(posix, want) {
		t.Fatalf("POSIX filtered env=%q, want %q", posix, want)
	}
	if input[0] != "KEEP=1" || len(input) != 4 {
		t.Fatalf("filter mutated input=%q", input)
	}
}

func TestGUITestBinaryRejectsMalformedHelperDispatch(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name       string
		args       []string
		env        []string
		wantReason guiTestDispatchFailure
	}{
		{name: "blocking sentinel without selector", args: []string{"-test.run=^$"}, env: []string{auditLockBlockingHelperEnv + "=1"}, wantReason: guiTestFailurePartialFrame},
		{name: "blocking sentinel with wrong selector", args: []string{"-test.run=^TestDoesNotExist$"}, env: []string{auditLockBlockingHelperEnv + "=1", auditLockHelperLockEnv + "=" + filepath.Join(root, "lock"), auditLockHelperEnteredEnv + "=" + filepath.Join(root, "entered")}, wantReason: guiTestFailureConflict},
		{name: "blocking selector without sentinel", args: []string{"-test.run=^TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn$"}, wantReason: guiTestFailureSelectorOnly},
		{name: "terminal worker with extra positional", args: []string{auditLockTerminalWorkerArg, "extra"}, wantReason: guiTestFailureWrongArgv},
		{name: "conflicting helpers", args: []string{auditLockTerminalWorkerArg}, env: []string{auditLockBlockingHelperEnv + "=1"}, wantReason: guiTestFailurePartialFrame},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(withoutGUITestHelperEnvironment(os.Environ(), runtime.GOOS), tc.env...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if err == nil || !strings.Contains(stderr.String(), string(tc.wantReason)) || !asExitError(err, &exitErr) || exitErr.ExitCode() != 3 {
				t.Fatalf("malformed dispatch err=%v stderr=%q, want exit 3 reason %q", err, stderr.String(), tc.wantReason)
			}
		})
	}
}

func TestGUITestBinaryAllowsUnmarkedOuterR6Selector(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuditLockTerminalWorker_RealHTTPEventPersistenceAndSecondRun$")
	cmd.Env = withoutGUITestHelperEnvironment(os.Environ(), runtime.GOOS)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unmarked outer R6 selector: %v\n%s", err, output)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	value, ok := err.(*exec.ExitError)
	if ok {
		*target = value
	}
	return ok
}
