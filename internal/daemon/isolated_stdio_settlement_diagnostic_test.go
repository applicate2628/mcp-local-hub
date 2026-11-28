package daemon

import (
	"testing"

	"mcp-local-hub/internal/process"
)

func TestIsolatedStdioSettlementDiagnosticExcludesUnobservedGeneration(t *testing.T) {
	original := process.ContainedRootIdentity{Known: true, PID: 47, NativeBirthFILETIME: 12345678}
	d := isolatedStdioSettlementDiagnostic(3, StdioHostSettlement{OriginalIdentity: original})
	if d.Sequence != 3 || d.OriginalPID != 47 || d.OriginalBirthFILETIME != 12345678 || d.Complete {
		t.Fatalf("unexpected original generation: %+v", d)
	}
	d = isolatedStdioSettlementDiagnostic(4, StdioHostSettlement{OriginalIdentity: original, NoNativeAttempt: true})
	if d.Sequence != 4 || d.OriginalPID != 0 || d.OriginalBirthFILETIME != 0 {
		t.Fatalf("no-attempt receipt exposed original identity: %+v", d)
	}
}
