package lastfailure

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR22WrapperNegativeProofRequiresRequestedCommandTarget(t *testing.T) {
	wrapper := filepath.Join(t.TempDir(), "build_failed.log")
	content := "[2026-08-09 00:00:00] triplet=cl\n" +
		"command: vcpkg install otherlib --triplet=cl\n" +
		"exit_code: 1\n" +
		"build_failed_count: 1\n" +
		"failed_ports:\n" +
		"- failedlib:cl\n"
	if err := os.WriteFile(wrapper, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result := LastFailure(Args{Port: "somelib", Triplet: "cl", BuildFailedLog: wrapper}, testDeps())
	if result.Status == evidence.StatusOK && containsNote(result.Notes, NoteWrapperConfirmsNoFailure) {
		t.Fatalf("wrapper for another requested target proved somelib did not fail: %+v", result)
	}
}

func TestR22WireRedactionFailsClosedOnUnclassifiedValueCarriers(t *testing.T) {
	for _, command := range []string{
		"git fetch https://host/repo?code=opaque-secret",
		"git fetch https://host/repo?code=%zz-opaque-secret",
		"tool MY_PAT=opaque-secret",
		"tool --vendor-auth-option=opaque-secret",
	} {
		if got := redactCommandForWire(command); got != redactedCommand {
			t.Errorf("redactCommandForWire(%q)=%q, want %q", command, got, redactedCommand)
		}
	}
}

func TestR22WrapperCommandContextMustBeTheMatchingVcpkgInvocation(t *testing.T) {
	for name, command := range map[string]string{
		"wrong executable": "other-tool install somelib --triplet=cl",
		"wrong triplet":    "vcpkg install somelib --triplet=other",
	} {
		t.Run(name, func(t *testing.T) {
			data := []byte("[2026-08-09 00:00:00] triplet=cl\ncommand: " + command + "\nbuild_failed_count: 1\nfailed_ports:\n- failedlib:cl\n")
			info, ok, err := parseWrapperContentWithLimitsForGOOS(data, defaultResponseLimits, "linux")
			if err != nil || !ok {
				t.Fatalf("parse wrapper: ok=%v err=%v", ok, err)
			}
			if info.RequestedTargetWasAttempted("somelib", "cl") {
				t.Fatalf("command %q proved the requested vcpkg context: %+v", command, info)
			}
		})
	}
}
