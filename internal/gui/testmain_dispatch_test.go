package gui

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestGUITestBinaryRejectsMalformedHelperDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  []string
	}{
		{name: "blocking sentinel without selector", args: []string{"-test.run=^$"}, env: []string{"MCPHUB_AUDIT_LOCK_BLOCKING_HELPER=1"}},
		{name: "blocking sentinel with wrong selector", args: []string{"-test.run=^TestDoesNotExist$"}, env: []string{"MCPHUB_AUDIT_LOCK_BLOCKING_HELPER=1"}},
		{name: "blocking selector without sentinel", args: []string{"-test.run=^TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn$"}},
		{name: "bare terminal worker", args: []string{"audit-lock-terminal-worker"}},
		{name: "conflicting helpers", args: []string{"audit-lock-terminal-worker"}, env: []string{"MCPHUB_AUDIT_LOCK_BLOCKING_HELPER=1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(withoutGUITestHelperEnvironment(os.Environ()), tc.env...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				t.Fatalf("malformed helper dispatch exited 0; stderr=%q", stderr.String())
			}
		})
	}
}

func withoutGUITestHelperEnvironment(env []string) []string {
	keys := []string{
		"MCPHUB_AUDIT_LOCK_BLOCKING_HELPER",
		"MCPHUB_AUDIT_LOCK_HELPER_LOCK",
		"MCPHUB_AUDIT_LOCK_HELPER_ENTERED",
		auditLockTerminalWorkerStderrHelperEnv,
		auditLockR6ReceiverHelperEnv,
		auditLockR6StateRootEnv,
	}
	result := make([]string, 0, len(env))
	for _, entry := range env {
		keep := true
		for _, key := range keys {
			if strings.HasPrefix(entry, key+"=") {
				keep = false
				break
			}
		}
		if keep {
			result = append(result, entry)
		}
	}
	return result
}
