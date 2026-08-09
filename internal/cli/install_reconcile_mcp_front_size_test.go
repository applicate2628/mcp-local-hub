package cli

import (
	"strings"
	"testing"
)

func TestWriteMCPFrontReconcileReport_RejectsUnreadableOversize(t *testing.T) {
	path := t.TempDir() + "/report.json"
	report := &mcpFrontReconcileReport{
		Version:          mcpFrontReconcileReportVersion,
		SnapshotComplete: true,
		Rows: map[string]mcpFrontReconcileRow{
			"oversized": {
				Surface:     mcpFrontSurfaceSerena,
				Client:      strings.Repeat("x", mcpFrontReconcileReportMaxBytes),
				EntryName:   "serena",
				BaselineSet: true,
			},
		},
	}
	if err := writeMCPFrontReconcileReport(path, report); err == nil || !strings.Contains(err.Error(), "exceeds readable cap") {
		t.Fatalf("oversized report error=%v, want pre-write readable-cap refusal", err)
	}
	if prior, err := readMCPFrontReconcileReport(path); err != nil || prior != nil {
		t.Fatalf("oversized report reached disk: prior=%+v err=%v", prior, err)
	}
}
