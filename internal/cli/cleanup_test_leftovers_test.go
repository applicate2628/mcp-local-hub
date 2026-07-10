package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestCleanupTestLeftoversRejectsDestructiveFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "apply", args: []string{"--apply"}},
		{name: "confirm token", args: []string{"--confirm-token", "token"}},
		{name: "confirm", args: []string{"--confirm"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runnerCalls := 0
			cmd := newCleanupTestLeftoversCmd(func(api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error) {
				runnerCalls++
				return api.TestLeftoverPreview{}, nil
			})
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs(test.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("%v was accepted", test.args)
			}
			wantErr := "unknown flag: " + test.args[0]
			if !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("%v rejection = %q, want pflag error containing %q", test.args, err, wantErr)
			}
			if runnerCalls != 0 {
				t.Fatalf("preview runner called %d times for rejected args %v", runnerCalls, test.args)
			}
		})
	}
}

func TestCleanupTestLeftoversRejectsNegativeMinAge(t *testing.T) {
	runnerCalls := 0
	cmd := newCleanupTestLeftoversCmd(func(api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error) {
		runnerCalls++
		return api.TestLeftoverPreview{}, nil
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--min-age-sec=-1"})

	err := cmd.Execute()
	const wantErr = "cleanup test-leftovers: --min-age-sec must be non-negative"
	if err == nil {
		t.Fatalf("negative --min-age-sec was accepted")
	}
	if err.Error() != wantErr {
		t.Fatalf("negative --min-age-sec rejection = %q, want %q", err, wantErr)
	}
	if runnerCalls != 0 {
		t.Fatalf("preview runner called %d times for negative --min-age-sec", runnerCalls)
	}
}

func TestCleanupTestLeftoversRejectsDestructiveParentFlags(t *testing.T) {
	tests := [][]string{
		{"--confirm", "test-leftovers"},
		{"--dry-run=false", "test-leftovers"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cmd := newCleanupCmdReal()
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs(args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("cleanup parent flags %v were accepted by test-leftovers", args)
			}
			if !strings.Contains(err.Error(), strings.TrimLeft(strings.Split(args[0], "=")[0], "-")) {
				t.Fatalf("refusal for %v did not identify the unsupported flag: %v", args, err)
			}
		})
	}
}

func TestCleanupTestLeftoversPassesDiagnosticOptions(t *testing.T) {
	wantRoot := t.TempDir()
	var got api.TestLeftoverPreviewOpts
	cmd := newCleanupTestLeftoversCmd(func(opts api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error) {
		got = opts
		return emptyTestLeftoverPreview(), nil
	})
	cmd.SetArgs([]string{"--min-age-sec", "123", "--temp-root", wantRoot})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.MinAgeSec != 123 || got.TempRoot != wantRoot {
		t.Fatalf("runner opts = %+v, want min=123 temp=%q", got, wantRoot)
	}
	if !strings.Contains(out.String(), "0 candidate(s)") || !strings.Contains(out.String(), "apply-deferred-v1") {
		t.Fatalf("empty preview output lacks lifecycle summary: %q", out.String())
	}
}

func TestCleanupTestLeftoversPreviewHumanOutput(t *testing.T) {
	rawPath := filepath.Join(t.TempDir(), "private", "mcphub-reliability-field.exe")
	result := api.TestLeftoverPreview{
		SnapshotVerdict:    "snapshot-degraded",
		SnapshotDiagnostic: "process snapshot ended early; candidates are not exhaustive",
		Exhaustive:         false,
		RequestedMinAgeSec: 60,
		TempRootVerdict:    "not-supplied",
		ProtectedScopeVerdicts: map[string]string{
			"production-state": "path-canonical",
			"install-path":     "path-canonical",
			"repo-path":        "path-canonical",
		},
		Candidates: []api.TestLeftoverCandidate{{
			PID:                  7401,
			ParentPID:            7000,
			StartedAt:            "2026-07-09T20:00:00Z",
			IdentityVerdict:      "identity-available",
			ExecutablePath:       rawPath,
			ExecutableDisplay:    filepath.Base(rawPath),
			ExecutablePathPolicy: "basename-only",
			ArgvShape:            "supervise",
			ImageFamily:          "reliability-temp",
			PatternClass:         "standalone-supervise",
			PathVerdict:          "path-canonical",
			PathRelations:        []string{"os-temp-root"},
			AgeSec:               1200,
			AgeVsRequestedMin:    "at-or-above-requested-min-age",
			AgeVsApplyFloor:      "at-or-above-apply-floor",
			ParentLiveness:       "parent-proven-dead",
			BuildInfoTag:         "test-tag-present",
			EnvironmentOverride:  "not-collected-v1",
			ApplyLifecycle:       "apply-deferred-v1",
			WouldRefuse:          "supervise-not-tree-reachable",
			OperatorNote:         "manual-reap-only: verify identity out-of-band before killing",
		}},
	}
	cmd := newCleanupTestLeftoversCmd(func(api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error) {
		return result, nil
	})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	for _, want := range []string{
		"snapshot-degraded",
		"not exhaustive",
		"protected-scopes=install-path:path-canonical,production-state:path-canonical,repo-path:path-canonical",
		"7401",
		rawPath,
		"supervise",
		"standalone-supervise",
		"supervise-not-tree-reachable",
		"manual-reap-only: verify identity out-of-band before killing",
		"apply-deferred-v1",
		"preview only",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("human output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "safe-to-kill") || strings.Contains(text, "apply-eligible") {
		t.Fatalf("human output emitted an authorization phrase:\n%s", text)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestCleanupTestLeftoversPreviewJSONOutput(t *testing.T) {
	rawPath := filepath.Join(t.TempDir(), "private", "mcphub-reliability-json.exe")
	result := api.TestLeftoverPreview{
		SnapshotVerdict:    "snapshot-complete",
		Exhaustive:         true,
		RequestedMinAgeSec: 60,
		TempRootVerdict:    "not-supplied",
		Candidates: []api.TestLeftoverCandidate{{
			PID:                  9001,
			ExecutablePath:       rawPath,
			ExecutableDisplay:    filepath.Base(rawPath),
			ExecutablePathPolicy: "basename-only",
			CommandLine:          rawPath + " supervise --api-key=secret-json",
			ArgvShape:            "supervise",
			ImageFamily:          "reliability-temp",
			PatternClass:         "standalone-supervise",
			PathRelations:        []string{},
			ApplyLifecycle:       "apply-deferred-v1",
			WouldRefuse:          "supervise-not-tree-reachable",
		}},
	}
	cmd := newCleanupTestLeftoversCmd(func(api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error) {
		return result, nil
	})
	cmd.SetArgs([]string{"--json"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var decoded api.TestLeftoverPreview
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON %q: %v", stdout.String(), err)
	}
	if len(decoded.Candidates) != 1 || decoded.Candidates[0].PID != 9001 {
		t.Fatalf("decoded result = %+v", decoded)
	}
	if strings.Contains(stdout.String(), rawPath) || strings.Contains(stdout.String(), "secret-json") {
		t.Fatalf("JSON leaked raw local evidence: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), filepath.Base(rawPath)) {
		t.Fatalf("JSON omitted basename evidence: %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestCleanupTestLeftoversPreviewHumanOutputLabelsUnavailableAge(t *testing.T) {
	result := emptyTestLeftoverPreview()
	result.Candidates = []api.TestLeftoverCandidate{{
		PID:                  9100,
		ExecutableDisplay:    "mcphub-reliability-unknown.exe",
		ExecutablePathPolicy: "basename-only",
		AgeSec:               -1,
		AgeVsRequestedMin:    "age-unavailable",
		AgeVsApplyFloor:      "age-unavailable",
		ApplyLifecycle:       "apply-deferred-v1",
		WouldRefuse:          "identity-unavailable",
	}}
	cmd := newCleanupTestLeftoversCmd(func(api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error) {
		return result, nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "age=-1s") || !strings.Contains(out.String(), "age=unavailable") {
		t.Fatalf("unavailable age was not labeled for humans:\n%s", out.String())
	}
}

func TestCleanupTestLeftoversIsWiredUnderCleanup(t *testing.T) {
	cleanup := newCleanupCmdReal()
	child, _, err := cleanup.Find([]string{"test-leftovers"})
	if err != nil {
		t.Fatalf("find test-leftovers: %v", err)
	}
	if child == nil || child.Name() != "test-leftovers" {
		t.Fatalf("cleanup child = %#v", child)
	}
	if child.Flags().Lookup("apply") != nil || child.Flags().Lookup("confirm-token") != nil || child.Flags().Lookup("confirm") != nil {
		t.Fatal("test-leftovers exposes a destructive flag")
	}
	for _, name := range []string{"min-age-sec", "temp-root", "json"} {
		if child.Flags().Lookup(name) == nil {
			t.Errorf("test-leftovers missing --%s", name)
		}
	}
}

func TestCleanupTestLeftoversPreviewErrorIsVisible(t *testing.T) {
	cmd := newCleanupTestLeftoversCmd(func(api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error) {
		return api.TestLeftoverPreview{}, errors.New("synthetic snapshot unavailable")
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "synthetic snapshot unavailable") {
		t.Fatalf("preview error = %v", err)
	}
}

func emptyTestLeftoverPreview() api.TestLeftoverPreview {
	return api.TestLeftoverPreview{
		SnapshotVerdict:    "snapshot-complete",
		Exhaustive:         true,
		RequestedMinAgeSec: api.DefaultTestLeftoverMinAgeSec,
		TempRootVerdict:    "not-supplied",
		Candidates:         []api.TestLeftoverCandidate{},
	}
}
