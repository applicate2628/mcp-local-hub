package api

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestRegistrationDiagnosticRegistryExactAndUnique(t *testing.T) {
	want := []RegistrationDiagnosticCode{
		RegistrationCodeSchedulerUnavailable,
		RegistrationCodeRelayStdioSkipped,
		RegistrationCodeWeeklyRefreshFailed,
		RegistrationCodeTrustedRootRecordFailed,
		RegistrationCodePostCommitObserverFailed,
		RegistrationCodeCleanupUnsupported,
		RegistrationCodeCleanupScanFailed,
		RegistrationCodeCleanupSurvivorScanFailed,
		RegistrationCodeCleanupCASConflict,
		RegistrationCodeCleanupBackupFailed,
		RegistrationCodeCleanupRemoveFailed,
		RegistrationCodeRouterProofFailed,
		RegistrationCodeRouteProofFailed,
		RegistrationCodeUnregisterSchedulerUnavailable,
		RegistrationCodeUnregisterLanguageMissing,
		RegistrationCodeUnregisterIntentRemoveFailed,
		RegistrationCodeUnregisterReconcileFailed,
		RegistrationCodeUnregisterProxyForeignOwner,
		RegistrationCodeUnregisterProxyKillFailed,
		RegistrationCodeUnregisterTaskDeleteFailed,
		RegistrationCodeUnregisterClientRemoveFailed,
		RegistrationCodeLSPEnsureFailed,
		RegistrationCodeUnknown,
	}
	got := RegisteredRegistrationDiagnosticCodes()
	if !slices.Equal(got, want) {
		t.Fatalf("registered codes=%v want=%v", got, want)
	}
	seen := map[RegistrationDiagnosticCode]bool{}
	for _, code := range got {
		if seen[code] {
			t.Fatalf("duplicate registered code %q", code)
		}
		seen[code] = true
		if _, ok := registrationDiagnosticRegistry[code]; !ok {
			t.Fatalf("listed code %q absent from registry", code)
		}
	}
	if len(seen) != len(registrationDiagnosticRegistry) {
		t.Fatalf("list has %d codes, registry has %d", len(seen), len(registrationDiagnosticRegistry))
	}
}

func TestRegistrationDiagnosticUnknownSanitizesContextAndRetainsLocalCause(t *testing.T) {
	const raw = `D:\secret\project --password=hunter2`
	cause := errors.New(raw)
	diagnostic := NewRegistrationDiagnostic(RegistrationDiagnosticCode(raw), raw, raw, cause)
	if diagnostic.Code() != RegistrationCodeUnknown || diagnostic.Severity() != RegistrationSeverityError {
		t.Fatalf("unknown diagnostic=%q/%q", diagnostic.Code(), diagnostic.Severity())
	}
	if strings.Contains(diagnostic.PlanIdentity(), raw) || strings.Contains(diagnostic.Participant(), raw) ||
		strings.Contains(diagnostic.PlanIdentity(), "hunter2") || strings.Contains(diagnostic.Participant(), "hunter2") {
		t.Fatalf("diagnostic context was not sanitized: plan=%q participant=%q", diagnostic.PlanIdentity(), diagnostic.Participant())
	}
	if !errors.Is(diagnostic.Cause(), cause) {
		t.Fatalf("local cause=%v, want original sentinel", diagnostic.Cause())
	}
	if public := RegistrationCompatibilityText(diagnostic); strings.Contains(public, raw) || strings.Contains(public, "hunter2") {
		t.Fatalf("compatibility projection leaked raw cause: %q", public)
	}
}

func TestRegistrationReportCompatibilityProjectionPreservesWireShape(t *testing.T) {
	entry := WorkspaceEntry{Language: "go"}
	for _, tc := range []struct {
		name      string
		configure func(*RegisterReport)
		wantKeys  []string
		wantWarns int
	}{
		{
			name:      "clean omitted",
			configure: func(*RegisterReport) {},
			wantKeys:  []string{"entries", "workspace", "workspace_key"},
		},
		{
			name: "warning string array",
			configure: func(report *RegisterReport) {
				report.addDiagnostic(NewRegistrationDiagnostic(
					RegistrationCodeTrustedRootRecordFailed, `D:\secret\root`, "register", errors.New("password=hunter2"),
				))
			},
			wantKeys:  []string{"entries", "warnings", "workspace", "workspace_key"},
			wantWarns: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &RegisterReport{Workspace: "project", WorkspaceKey: "key", Entries: []WorkspaceEntry{entry}}
			tc.configure(report)
			report.projectCompatibilityWarnings()
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(decoded))
			for key := range decoded {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, tc.wantKeys) {
				t.Fatalf("keys=%v want=%v raw=%s", keys, tc.wantKeys, raw)
			}
			if tc.wantWarns > 0 {
				warnings, ok := decoded["warnings"].([]any)
				if !ok || len(warnings) != tc.wantWarns {
					t.Fatalf("warnings=%T/%v", decoded["warnings"], decoded["warnings"])
				}
			}
			if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), `D:\secret`) {
				t.Fatalf("wire report leaked private cause: %s", raw)
			}
		})
	}
}

func TestUnregisterReportCompatibilityProjectionPreservesWireShape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*UnregisterReport)
		wantKeys  []string
	}{
		{
			name:      "clean omitted",
			configure: func(*UnregisterReport) {},
			wantKeys:  []string{"removed", "workspace", "workspace_key"},
		},
		{
			name: "warning string array",
			configure: func(report *UnregisterReport) {
				report.addDiagnostic(NewRegistrationDiagnostic(
					RegistrationCodeUnregisterTaskDeleteFailed, "task", "go", errors.New(`D:\secret\task password=hunter2`),
				))
			},
			wantKeys: []string{"removed", "warnings", "workspace", "workspace_key"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &UnregisterReport{Workspace: "project", WorkspaceKey: "key", Removed: []string{"go"}}
			tc.configure(report)
			report.projectCompatibilityWarnings()
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(decoded))
			for key := range decoded {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, tc.wantKeys) {
				t.Fatalf("keys=%v want=%v raw=%s", keys, tc.wantKeys, raw)
			}
			if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), `D:\secret`) {
				t.Fatalf("wire report leaked private cause: %s", raw)
			}
		})
	}
}

func TestRegistrationReportDiagnosticsAccessorReturnsCopy(t *testing.T) {
	report := &RegisterReport{}
	report.addDiagnostic(NewRegistrationDiagnostic(RegistrationCodeRelayStdioSkipped, "register", "bindings", nil))
	first := report.Diagnostics()
	first[0] = RegistrationDiagnostic{}
	second := report.Diagnostics()
	if len(second) != 1 || second[0].Code() != RegistrationCodeRelayStdioSkipped {
		t.Fatalf("caller mutated report diagnostics: %+v", second)
	}
}

func TestRegistrationWarningWriteOwnerSourceGuard(t *testing.T) {
	source, err := os.ReadFile("register.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"report.Warnings = append",
		"report.Warnings=append",
	} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("production report warnings bypass compatibility projector: %q", forbidden)
		}
	}
}
