package lastfailure

import (
	"encoding/json"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestProjectStopClassRegistryIsTotalAndTerminal(t *testing.T) {
	state := newCallState(defaultResponseLimits)
	result := Result{Notes: []Note{
		NoteWrapperNotSupplied,
		NoteOverlayChainNotSupplied,
	}}
	seen := make(map[stopClass]bool, len(allStopClasses))
	for _, class := range allStopClasses {
		if seen[class] {
			t.Fatalf("duplicate stop class %d", class)
		}
		seen[class] = true
		projected, ok := projectStopClass(class, state, result)
		if !ok {
			t.Fatalf("registered stop class %d has no projection", class)
		}
		for name, domain := range map[string]evidenceDomainState{
			"arguments": projected.arguments, "metadata": projected.metadata,
			"directory_entries": projected.directoryEntries,
			"relevant_logs":     projected.relevantLogs,
			"log_bytes":         projected.logBytes,
			"diagnostics":       projected.diagnostics,
			"log_paths":         projected.logPaths,
			"overlay_chain":     projected.overlayChain,
			"evidence":          projected.evidence,
		} {
			if domain > domainInvalid {
				t.Fatalf("stop class %d domain %s = %d, want a terminal state", class, name, domain)
			}
		}
		switch projected.status {
		case evidence.StatusOK, evidence.StatusFailed:
			if projected.reason != "" {
				t.Fatalf("stop class %d status=%s has reason=%q", class, projected.status, projected.reason)
			}
		case evidence.StatusUnknown:
			if projected.reason == "" {
				t.Fatalf("stop class %d unknown projection has no stable reason", class)
			}
		default:
			t.Fatalf("stop class %d has zero/unknown public status %q", class, projected.status)
		}
	}
}

func TestProjectionRowsThatPreviouslyContradictedTheirDomains(t *testing.T) {
	state := newCallState(defaultResponseLimits)
	tests := []struct {
		name   string
		class  stopClass
		result Result
		want   evidenceProjection
		exact  bool
	}{
		{
			name:  "arguments invalid leaves evidence not applicable",
			class: stopArgumentsInvalid,
			want: projection(
				domainInvalid,
				domainNotApplicable, domainNotApplicable, domainNotApplicable,
				domainNotApplicable, domainNotApplicable, domainNotApplicable,
				domainNotApplicable, domainNotApplicable,
				evidence.StatusUnknown, ReasonArgsInvalid,
			),
			exact: true,
		},
		{
			name:  "wrapper proof settles metadata and makes later domains inapplicable",
			class: stopWrapperConfirmsNotFailed,
			result: Result{Notes: []Note{
				NoteWrapperUsedForContext,
				NoteWrapperConfirmsNoFailure,
			}},
			want: projection(
				domainSettledComplete, domainSettledComplete,
				domainNotApplicable, domainNotApplicable, domainNotApplicable,
				domainNotApplicable, domainNotApplicable, domainNotApplicable,
				domainNotApplicable, evidence.StatusOK, "",
			),
			exact: true,
		},
		{
			name:  "verified zero logs settles discovery and makes byte scan inapplicable",
			class: stopVerifiedNoPhaseLogs,
			result: Result{Notes: []Note{
				NoteWrapperNotSupplied,
			}},
			want: projection(
				domainSettledComplete, domainNotApplicable,
				domainSettledComplete, domainSettledComplete,
				domainNotApplicable, domainNotApplicable,
				domainSettledComplete, domainNotApplicable,
				domainNotApplicable, evidence.StatusUnknown, ReasonNoPhaseLogsFound,
			),
			exact: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := projectStopClass(tc.class, state, tc.result)
			if !ok {
				t.Fatal("projection missing")
			}
			if got != tc.want {
				t.Fatalf("projection = %#v, want %#v", got, tc.want)
			}
			if got.diagnosticsDroppedExact() != tc.exact {
				t.Fatalf("diagnostic exactness = %v, want %v", got.diagnosticsDroppedExact(), tc.exact)
			}
		})
	}
}

func TestDiagnosticsDroppedExactnessMatrix(t *testing.T) {
	states := []evidenceDomainState{
		domainNotStarted,
		domainNotApplicable,
		domainSettledComplete,
		domainLimited,
		domainUnreadable,
		domainCancelled,
		domainInvalid,
	}

	for _, directoryEntries := range states {
		for _, relevantLogs := range states {
			for _, logBytes := range states {
				for _, diagnostics := range states {
					got := projection(
						domainNotApplicable, domainNotApplicable,
						directoryEntries, relevantLogs, logBytes, diagnostics,
						domainNotApplicable, domainNotApplicable, domainNotApplicable,
						evidence.StatusUnknown, ReasonBuildInterrupted,
					).diagnosticsDroppedExact()
					want := (directoryEntries == domainNotApplicable &&
						relevantLogs == domainNotApplicable &&
						logBytes == domainNotApplicable &&
						diagnostics == domainNotApplicable) ||
						(directoryEntries == domainSettledComplete &&
							relevantLogs == domainSettledComplete &&
							(logBytes == domainSettledComplete || logBytes == domainNotApplicable) &&
							(diagnostics == domainSettledComplete || diagnostics == domainNotApplicable))
					if got != want {
						t.Fatalf("directory_entries=%d relevant_logs=%d log_bytes=%d diagnostics=%d: exact=%v, want %v",
							directoryEntries, relevantLogs, logBytes, diagnostics, got, want)
					}
				}
			}
		}
	}
}

func TestBuildInterruptedPreservesEarlierProducerIncompleteness(t *testing.T) {
	state := newCallState(defaultResponseLimits)
	state.report.Completeness.LogBytes = false
	projected, ok := projectStopClass(stopScanInterrupted, state, Result{})
	if !ok {
		t.Fatal("build-interrupted projection missing")
	}
	if projected.logBytes != domainLimited || projected.diagnostics != domainSettledComplete {
		t.Fatalf("projection=%#v, want limited log bytes and settled diagnostics", projected)
	}
	if projected.diagnosticsDroppedExact() {
		t.Fatal("limited log bytes with settled diagnostics reported an exact dropped count")
	}
}

func TestMissingProjectionFailsClosedAndSerializes(t *testing.T) {
	state := newCallState(defaultResponseLimits)
	state.report.Omitted.LogPaths = 3
	raw := Result{Status: evidence.StatusOK}

	if _, ok := projectStopClass(0, state, raw); ok {
		t.Fatal("unknown stop class unexpectedly projected")
	}
	got := finalizeProjectedResult(raw, state)
	if got.Status != evidence.StatusUnknown || got.Reason != ReasonCausalityInvariantViolation {
		t.Fatalf("missing projection = %s/%s, want unknown/%s",
			got.Status, got.Reason, ReasonCausalityInvariantViolation)
	}
	if got.DiagnosticsDroppedExact {
		t.Fatal("missing projection reported exact diagnostics")
	}
	if got.Resources.Omitted.LogPaths != 3 {
		t.Fatalf("observed omissions were lost: %+v", got.Resources.Omitted)
	}
	if got.Resources.Completeness != (Completeness{}) {
		t.Fatalf("missing projection completeness = %+v, want all false", got.Resources.Completeness)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("missing projection is not serializable: %v", err)
	}
}
