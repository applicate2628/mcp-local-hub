package lastfailure

import "testing"

func TestR19GNULDWarningPreservesWarningSeverity(t *testing.T) {
	diagnostic, ok := matchDiagnosticLine("/usr/bin/ld: warning: cannot find entry symbol missing_entry; defaulting to 0")
	if !ok {
		t.Fatal("GNU ld warning was not recognized")
	}
	if diagnostic.Severity != "warning" {
		t.Fatalf("severity=%q, want warning: %+v", diagnostic.Severity, diagnostic)
	}
}
