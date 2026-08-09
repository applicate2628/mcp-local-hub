package lastfailure

import "testing"

func TestR20NinjaOwnErrorIsSpecificFailureCause(t *testing.T) {
	line := "ninja: error: loading 'build.ninja': No such file or directory"
	diag, ok := matchDiagnosticLine(line)
	if !ok {
		t.Fatalf("ninja cause was not recognized: %q", line)
	}
	if diag.File != "ninja" || diag.Severity != SeverityError || diag.Tier != TierSpecific {
		t.Fatalf("ninja cause = %+v, want ninja/error/specific", diag)
	}
}
