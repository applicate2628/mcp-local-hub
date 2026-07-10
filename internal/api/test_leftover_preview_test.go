package api

import (
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	processpkg "mcp-local-hub/internal/process"
)

const testLeftoverPreviewCSVHeader = "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize\n"

var testLeftoverPreviewNow = time.Date(2026, 7, 10, 0, 20, 0, 0, time.UTC)

func TestLeftoverPreviewClassifiesDiagnosticFamilies(t *testing.T) {
	root := t.TempDir()
	operatorRoot := filepath.Join(root, "operator-scope")

	reliabilityExe := writePreviewFixture(t, filepath.Join(root, "reliability", "mcphub-reliability-123.exe"))
	guiExe := writePreviewFixture(t, filepath.Join(root, "repo", "internal", "gui", "e2e", "bin", "mcphub.exe"))
	goBuildExe := writePreviewFixture(t, filepath.Join(root, "go-build123", "b001", "exe", "mcphub.exe"))
	operatorExe := writePreviewFixture(t, filepath.Join(operatorRoot, "run", "mcphub-reliability-operator.exe"))
	superviseExe := writePreviewFixture(t, filepath.Join(root, "supervise", "mcphub-reliability-field.exe"))
	unrelatedExe := writePreviewFixture(t, filepath.Join(root, "other", "node.exe"))

	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(101, 9001, reliabilityExe, quotePreviewArg(reliabilityExe)+" daemon reliability", validPreviewCreated) +
		previewCSVRow(102, 9002, guiExe, quotePreviewArg(guiExe)+" gui --no-browser", validPreviewCreated) +
		previewCSVRow(103, 9003, goBuildExe, quotePreviewArg(goBuildExe)+" gui --port 0", validPreviewCreated) +
		previewCSVRow(104, 9004, operatorExe, quotePreviewArg(operatorExe)+" daemon operator", validPreviewCreated) +
		previewCSVRow(105, 9005, superviseExe, quotePreviewArg(superviseExe)+" supervise", validPreviewCreated) +
		previewCSVRow(999, 9006, unrelatedExe, quotePreviewArg(unrelatedExe)+" server.js", validPreviewCreated)

	a := previewAPIWithSnapshot(csv)
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{
		MinAgeSec: 3600,
		TempRoot:  operatorRoot,
	})
	if err != nil {
		t.Fatalf("PreviewTestLeftovers: %v", err)
	}
	if result.SnapshotVerdict != "snapshot-complete" || !result.Exhaustive {
		t.Fatalf("snapshot verdict = %q exhaustive=%v, want complete/true", result.SnapshotVerdict, result.Exhaustive)
	}
	if result.RequestedMinAgeSec != 3600 {
		t.Fatalf("requested min age = %d, want 3600", result.RequestedMinAgeSec)
	}
	if len(result.Candidates) != 5 {
		t.Fatalf("got %d candidates, want 5: %+v", len(result.Candidates), result.Candidates)
	}

	wantClasses := map[int]string{
		101: "reliability-temp",
		102: "gui-e2e",
		103: "go-build-cache",
		104: "operator-temp-root",
		105: "standalone-supervise",
	}
	seen := make(map[int]bool)
	for _, candidate := range result.Candidates {
		seen[candidate.PID] = true
		if candidate.PatternClass != wantClasses[candidate.PID] {
			t.Errorf("PID %d class = %q, want %q", candidate.PID, candidate.PatternClass, wantClasses[candidate.PID])
		}
		if candidate.StartedAt != "2026-07-10T00:00:00Z" {
			t.Errorf("PID %d started_at = %q", candidate.PID, candidate.StartedAt)
		}
		if candidate.ExecutableDisplay != filepath.Base(candidate.ExecutablePath) {
			t.Errorf("PID %d executable display = %q, raw=%q", candidate.PID, candidate.ExecutableDisplay, candidate.ExecutablePath)
		}
		if candidate.ExecutablePathPolicy != "basename-only" {
			t.Errorf("PID %d path policy = %q", candidate.PID, candidate.ExecutablePathPolicy)
		}
		if candidate.ArgvShape == "" || candidate.ArgvShape == "unrecognized" {
			t.Errorf("PID %d argv shape = %q", candidate.PID, candidate.ArgvShape)
		}
		if candidate.AgeSec != 1200 {
			t.Errorf("PID %d age = %d, want 1200", candidate.PID, candidate.AgeSec)
		}
		if candidate.AgeVsRequestedMin != "younger-than-requested-min-age" {
			t.Errorf("PID %d requested-age verdict = %q", candidate.PID, candidate.AgeVsRequestedMin)
		}
		if candidate.AgeVsApplyFloor != "at-or-above-apply-floor" {
			t.Errorf("PID %d apply-floor verdict = %q", candidate.PID, candidate.AgeVsApplyFloor)
		}
		if candidate.ParentLiveness != "parent-proven-dead" {
			t.Errorf("PID %d parent verdict = %q", candidate.PID, candidate.ParentLiveness)
		}
		if candidate.BuildInfoTag != "test-tag-present" {
			t.Errorf("PID %d buildinfo = %q", candidate.PID, candidate.BuildInfoTag)
		}
		if candidate.EnvironmentOverride != "not-collected-v1" {
			t.Errorf("PID %d environment = %q", candidate.PID, candidate.EnvironmentOverride)
		}
		if candidate.ApplyLifecycle != "apply-deferred-v1" {
			t.Errorf("PID %d lifecycle = %q", candidate.PID, candidate.ApplyLifecycle)
		}
	}
	for _, pid := range []int{101, 102, 103, 104, 105} {
		if !seen[pid] {
			t.Errorf("missing expected PID %d", pid)
		}
	}
	if seen[999] {
		t.Fatal("unrelated node row was listed")
	}
}

func TestLeftoverPreviewStandaloneSuperviseIsManualOnly(t *testing.T) {
	exe := writePreviewFixture(t, filepath.Join(t.TempDir(), "mcphub-reliability-field.exe"))
	a := previewAPIWithSnapshot(testLeftoverPreviewCSVHeader +
		previewCSVRow(7401, 7333, exe, quotePreviewArg(exe)+" supervise", validPreviewCreated))
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(result.Candidates))
	}
	candidate := result.Candidates[0]
	if candidate.PatternClass != "standalone-supervise" {
		t.Errorf("class = %q", candidate.PatternClass)
	}
	if candidate.WouldRefuse != "supervise-not-tree-reachable" {
		t.Errorf("would_refuse = %q", candidate.WouldRefuse)
	}
	if candidate.OperatorNote != "manual-reap-only: verify identity out-of-band before killing" {
		t.Errorf("operator note = %q", candidate.OperatorNote)
	}
}

func TestLeftoverPreviewSuperviseClassUsesParentLiveness(t *testing.T) {
	root := t.TempDir()
	liveExe := writePreviewFixture(t, filepath.Join(root, "live", "mcphub-reliability-live.exe"))
	deadExe := writePreviewFixture(t, filepath.Join(root, "dead", "mcphub-reliability-dead.exe"))
	parentExe := writePreviewFixture(t, filepath.Join(root, "parent", "gui-owner.exe"))
	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(7402, 42, liveExe, quotePreviewArg(liveExe)+" supervise", validPreviewCreated) +
		previewCSVRow(7403, 7333, deadExe, quotePreviewArg(deadExe)+" supervise", validPreviewCreated) +
		previewCSVRow(42, 1, parentExe, quotePreviewArg(parentExe)+" gui", validPreviewCreated)
	a := previewAPIWithSnapshot(csv)
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = func(pid int) (processpkg.PIDState, error) {
		if pid != 7333 {
			t.Fatalf("unexpected parent probe for PID %d", pid)
		}
		return processpkg.PIDStateDead, nil
	}

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: 600})
	if err != nil {
		t.Fatal(err)
	}
	byPID := candidateMap(result.Candidates)
	live := byPID[7402]
	if live.ParentLiveness != "parent-alive" || live.PatternClass != "live-supervise" {
		t.Errorf("live-parent supervise classification = %+v", live)
	}
	if live.WouldRefuse != "parent-alive-or-unproven" || live.OperatorNote != "" {
		t.Errorf("live-parent supervise evidence = %+v", live)
	}
	dead := byPID[7403]
	if dead.ParentLiveness != "parent-proven-dead" || dead.PatternClass != "standalone-supervise" {
		t.Errorf("dead-parent supervise classification = %+v", dead)
	}
	if dead.WouldRefuse != "supervise-not-tree-reachable" || dead.OperatorNote != testLeftoverManualOnlyNote {
		t.Errorf("dead-parent supervise evidence = %+v", dead)
	}
}

func TestLeftoverPreviewOmitsUnrelatedRows(t *testing.T) {
	root := t.TempDir()
	operatorRoot := filepath.Join(root, "operator")
	unrelatedUnderRoot := writePreviewFixture(t, filepath.Join(operatorRoot, "mcphub.exe"))
	prefixCollision := writePreviewFixture(t, filepath.Join(root, "operator-other", "mcphub.exe"))
	nonExecutableFamily := writePreviewFixture(t, filepath.Join(root, "mcphub-reliability-not-an-image.txt"))
	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(201, 1, unrelatedUnderRoot, quotePreviewArg(unrelatedUnderRoot)+" gui", validPreviewCreated) +
		previewCSVRow(202, 1, prefixCollision, quotePreviewArg(prefixCollision)+" supervise", validPreviewCreated) +
		previewCSVRow(203, 1, nonExecutableFamily, quotePreviewArg(nonExecutableFamily)+" daemon x", validPreviewCreated)

	a := previewAPIWithSnapshot(csv)
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent
	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{TempRoot: operatorRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("operator root broadened the admitted basename family: %+v", result.Candidates)
	}
}

func TestLeftoverPreviewLabelsDegradedSnapshot(t *testing.T) {
	exe := writePreviewFixture(t, filepath.Join(t.TempDir(), "mcphub-reliability-degraded.exe"))
	valid := previewCSVRow(301, 7777, exe, quotePreviewArg(exe)+" daemon x", validPreviewCreated)
	csv := testLeftoverPreviewCSVHeader + valid + strings.Repeat("x", 1024*1024+1) + "\n"
	a := previewAPIWithSnapshot(csv)
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err != nil {
		t.Fatalf("degraded preview returned fatal error: %v", err)
	}
	if result.SnapshotVerdict != "snapshot-degraded" || result.Exhaustive {
		t.Fatalf("snapshot verdict = %q exhaustive=%v, want degraded/false", result.SnapshotVerdict, result.Exhaustive)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].PID != 301 {
		t.Fatalf("best-effort row not retained: %+v", result.Candidates)
	}
	if result.Candidates[0].WouldRefuse != "snapshot-degraded" {
		t.Fatalf("degraded row refusal = %q, want snapshot-degraded", result.Candidates[0].WouldRefuse)
	}
}

func TestLeftoverPreviewSnapshotAcquisitionFailureIsVisible(t *testing.T) {
	a := NewAPI()
	a.testLeftoverSnapshotFn = func() (string, error) {
		return "", errors.New("synthetic census failure")
	}
	_, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err == nil {
		t.Fatal("expected acquisition failure")
	}
	if !strings.Contains(err.Error(), "process snapshot") || !strings.Contains(err.Error(), "synthetic census failure") {
		t.Fatalf("error did not preserve visible acquisition context: %v", err)
	}
}

func TestLeftoverPreviewRejectsUnusableSnapshot(t *testing.T) {
	for _, raw := range []string{"", "not a process snapshot\n"} {
		t.Run(fmt.Sprintf("bytes_%d", len(raw)), func(t *testing.T) {
			a := previewAPIWithSnapshot(raw)
			_, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
			if err == nil {
				t.Fatalf("unusable snapshot %q produced empty success", raw)
			}
			if !strings.Contains(err.Error(), "no usable process rows") {
				t.Fatalf("unexpected unusable-snapshot error: %v", err)
			}
		})
	}
}

func TestLeftoverPreviewPreservesZeroDiagnosticAgeAndDoesNotFilter(t *testing.T) {
	exe := writePreviewFixture(t, filepath.Join(t.TempDir(), "mcphub-reliability-zero-age.exe"))
	a := previewAPIWithSnapshot(testLeftoverPreviewCSVHeader +
		previewCSVRow(350, 1, exe, quotePreviewArg(exe)+" daemon x", validPreviewCreated))
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: 0})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequestedMinAgeSec != 0 {
		t.Fatalf("requested min age = %d, want explicit zero", result.RequestedMinAgeSec)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("diagnostic age setting filtered candidates: %+v", result.Candidates)
	}
}

func TestLeftoverPreviewApplyFloorRefusalUsesCandidateAge(t *testing.T) {
	root := t.TempDir()
	olderExe := writePreviewFixture(t, filepath.Join(root, "mcphub-reliability-older.exe"))
	youngerExe := writePreviewFixture(t, filepath.Join(root, "mcphub-reliability-younger.exe"))
	installExe := writePreviewFixture(t, filepath.Join(root, "installed", "mcphub.exe"))
	previousInstall := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = installExe
	t.Cleanup(func() { testCanonicalMcphubPathOverride = previousInstall })

	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(360, 1, olderExe, quotePreviewArg(olderExe)+" daemon x", validPreviewCreated) +
		previewCSVRow(361, 1, youngerExe, quotePreviewArg(youngerExe)+" daemon x", "20260710001500.000000+000")
	a := previewAPIWithSnapshot(csv)
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: DefaultTestLeftoverMinAgeSec})
	if err != nil {
		t.Fatal(err)
	}
	byPID := candidateMap(result.Candidates)
	if got := byPID[360].WouldRefuse; got != TestLeftoverNotEvaluated {
		t.Errorf("older candidate would_refuse = %q, want %q", got, TestLeftoverNotEvaluated)
	}
	if got := byPID[361].WouldRefuse; got != "min-age-below-apply-floor" {
		t.Errorf("younger candidate would_refuse = %q, want min-age-below-apply-floor", got)
	}
}

func TestLeftoverPreviewKeepsCandidateWhenEvidenceIsMissing(t *testing.T) {
	missingExe := filepath.Join(t.TempDir(), "mcphub-reliability-missing.exe")
	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(401, 8888, missingExe, quotePreviewArg(missingExe)+" daemon x", "20269999000000.000000+000")
	a := previewAPIWithSnapshot(csv)
	a.testLeftoverParentStateFn = func(int) (processpkg.PIDState, error) {
		return processpkg.PIDStateUnknown, errors.New("probe denied")
	}

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("missing evidence suppressed candidate: %+v", result.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.StartedAt != "" || candidate.IdentityVerdict != "identity-unavailable" {
		t.Errorf("identity evidence = started %q verdict %q", candidate.StartedAt, candidate.IdentityVerdict)
	}
	if candidate.PathVerdict != "path-canonicalization-error" {
		t.Errorf("path verdict = %q", candidate.PathVerdict)
	}
	if candidate.BuildInfoTag != "unreadable" {
		t.Errorf("buildinfo = %q", candidate.BuildInfoTag)
	}
	if candidate.ParentLiveness != "parent-unproven" {
		t.Errorf("parent verdict = %q", candidate.ParentLiveness)
	}
	if candidate.EnvironmentOverride != "not-collected-v1" || candidate.ApplyLifecycle != "apply-deferred-v1" {
		t.Errorf("missing evidence produced wrong lifecycle: %+v", candidate)
	}
	if candidate.WouldRefuse == "not-evaluated-v1" {
		t.Error("conclusive missing identity/path evidence did not produce a refusal hint")
	}
}

func TestLeftoverPreviewBuildInfoFindings(t *testing.T) {
	root := t.TempDir()
	want := map[string]string{
		"tagged":     "test-tag-present",
		"untagged":   "test-tag-absent",
		"unreadable": "unreadable",
		"unparsable": "unparsable",
	}
	var csv strings.Builder
	csv.WriteString(testLeftoverPreviewCSVHeader)
	pid := 500
	for name := range want {
		exe := writePreviewFixture(t, filepath.Join(root, "mcphub-reliability-"+name+".exe"))
		csv.WriteString(previewCSVRow(pid, 9, exe, quotePreviewArg(exe)+" daemon x", validPreviewCreated))
		pid++
	}

	a := previewAPIWithSnapshot(csv.String())
	a.testLeftoverParentStateFn = deadParent
	a.testLeftoverBuildInfoReadFn = func(path string) (*debug.BuildInfo, error) {
		switch {
		case strings.Contains(path, "-tagged"):
			return taggedPreviewBuildInfo(path)
		case strings.Contains(path, "-untagged"):
			return &debug.BuildInfo{}, nil
		case strings.Contains(path, "-unreadable"):
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
		default:
			return nil, errors.New("unrecognized executable format")
		}
	}

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(result.Candidates), len(want))
	}
	for _, candidate := range result.Candidates {
		name := strings.TrimSuffix(strings.TrimPrefix(candidate.ExecutableDisplay, "mcphub-reliability-"), ".exe")
		if candidate.BuildInfoTag != want[name] {
			t.Errorf("%s buildinfo = %q, want %q", name, candidate.BuildInfoTag, want[name])
		}
	}
}

func TestLeftoverPreviewDefaultBuildInfoReaderClassifiesPlainFileAsUnparsable(t *testing.T) {
	path := writePreviewFixture(t, filepath.Join(t.TempDir(), "mcphub-reliability-plain.exe"))
	if got := testLeftoverBuildInfoFinding(path, nil); got != "not-collected" {
		t.Fatalf("nil reader = %q, want not-collected", got)
	}
	if got := testLeftoverBuildInfoFinding(path, buildinfo.ReadFile); got != "unparsable" {
		t.Fatalf("plain on-disk image = %q, want unparsable", got)
	}
}

func TestLeftoverPreviewProtectedPathRelationsAreDiagnosticOnly(t *testing.T) {
	productionExe := writePreviewFixture(t, filepath.Join(daemonStateRootOverride, "nested", "mcphub-reliability-production.exe"))
	installExe := writePreviewFixture(t, filepath.Join(t.TempDir(), "mcphub-reliability-install.exe"))
	previousInstall := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = installExe
	t.Cleanup(func() { testCanonicalMcphubPathOverride = previousInstall })

	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(551, 1, productionExe, quotePreviewArg(productionExe)+" daemon x", validPreviewCreated) +
		previewCSVRow(552, 1, installExe, quotePreviewArg(installExe)+" daemon x", validPreviewCreated)
	a := previewAPIWithSnapshot(csv)
	a.testLeftoverParentStateFn = deadParent
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: 600})
	if err != nil {
		t.Fatal(err)
	}
	byPID := candidateMap(result.Candidates)
	if byPID[551].WouldRefuse != "production-state" {
		t.Errorf("production relation = %+v", byPID[551])
	}
	if byPID[552].WouldRefuse != "install-path" {
		t.Errorf("install relation = %+v", byPID[552])
	}
	for _, pid := range []int{551, 552} {
		if byPID[pid].ApplyLifecycle != "apply-deferred-v1" {
			t.Errorf("PID %d protected relation changed lifecycle: %+v", pid, byPID[pid])
		}
	}
}

func TestLeftoverPreviewProtectedScopeCanonicalizationFailureIsVisible(t *testing.T) {
	for _, unavailableScope := range []string{"production-state", "install-path"} {
		t.Run(unavailableScope, func(t *testing.T) {
			root := t.TempDir()
			candidateExe := writePreviewFixture(t, filepath.Join(root, "candidate", "mcphub-reliability-scope.exe"))
			validInstall := writePreviewFixture(t, filepath.Join(root, "installed", "mcphub.exe"))
			validProduction := filepath.Join(root, "production")
			if err := os.MkdirAll(validProduction, 0o755); err != nil {
				t.Fatal(err)
			}

			previousState := daemonStateRootOverride
			previousInstall := testCanonicalMcphubPathOverride
			daemonStateRootOverride = validProduction
			testCanonicalMcphubPathOverride = validInstall
			t.Cleanup(func() {
				daemonStateRootOverride = previousState
				testCanonicalMcphubPathOverride = previousInstall
			})
			switch unavailableScope {
			case "production-state":
				daemonStateRootOverride = filepath.Join(root, "missing-production")
			case "install-path":
				testCanonicalMcphubPathOverride = filepath.Join(root, "missing-install", "mcphub.exe")
			}

			a := previewAPIWithSnapshot(testLeftoverPreviewCSVHeader +
				previewCSVRow(555, 1, candidateExe, quotePreviewArg(candidateExe)+" daemon x", validPreviewCreated))
			a.testLeftoverParentStateFn = deadParent
			a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
			result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: 600})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Candidates) != 1 {
				t.Fatalf("candidates = %+v", result.Candidates)
			}
			if got := result.Candidates[0].WouldRefuse; got != "protected-scope-unverified" {
				t.Errorf("would_refuse = %q, want protected-scope-unverified", got)
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			jsonText := string(raw)
			if !strings.Contains(jsonText, `"protected_scope_verdicts"`) ||
				!strings.Contains(jsonText, fmt.Sprintf(`%q:%q`, unavailableScope, "protected-scope-unverified")) {
				t.Errorf("protected-scope verdict missing from preview JSON: %s", jsonText)
			}
		})
	}
}

func TestLeftoverPreviewParentPresentInCensusIsAliveEvidence(t *testing.T) {
	root := t.TempDir()
	exe := writePreviewFixture(t, filepath.Join(root, "mcphub-reliability-child.exe"))
	parentExe := writePreviewFixture(t, filepath.Join(root, "explorer.exe"))
	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(560, 42, exe, quotePreviewArg(exe)+" daemon x", validPreviewCreated) +
		previewCSVRow(42, 1, parentExe, quotePreviewArg(parentExe), validPreviewCreated)
	a := previewAPIWithSnapshot(csv)
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = func(int) (processpkg.PIDState, error) {
		t.Fatal("parent probe should not run when the parent is present in the census")
		return processpkg.PIDStateUnknown, nil
	}
	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: 600})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	if result.Candidates[0].ParentLiveness != "parent-alive" || result.Candidates[0].WouldRefuse != "parent-alive-or-unproven" {
		t.Fatalf("live-parent evidence = %+v", result.Candidates[0])
	}
}

func TestLeftoverPreviewGoBuildClassificationRequiresExeComponent(t *testing.T) {
	exe := writePreviewFixture(t, filepath.Join(t.TempDir(), "go-build-not-cache", "b001", "exe", "mcphub.exe"))
	a := previewAPIWithSnapshot(testLeftoverPreviewCSVHeader +
		previewCSVRow(570, 1, exe, quotePreviewArg(exe)+" gui", validPreviewCreated))
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent
	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("non-cache go-build prefix was admitted: %+v", result.Candidates)
	}
}

func TestLeftoverPreviewExactGUIFixtureIsNotRefusedAsRepoPath(t *testing.T) {
	candidate := TestLeftoverCandidate{
		IdentityVerdict:     "identity-available",
		ArgvShape:           "gui-e2e",
		PatternClass:        "gui-e2e",
		PathVerdict:         "path-canonical",
		PathRelations:       []string{"repo-path"},
		BuildInfoTag:        "test-tag-present",
		ParentLiveness:      "parent-proven-dead",
		AgeVsApplyFloor:     "at-or-above-apply-floor",
		EnvironmentOverride: "not-collected-v1",
	}
	if got := testLeftoverWouldRefuse(candidate, false, false); got != "not-evaluated-v1" {
		t.Fatalf("exact GUI fixture refusal = %q, want not-evaluated-v1", got)
	}
}

func TestLeftoverPreviewAmbiguousFamilyFailsClosed(t *testing.T) {
	// Every non-classification guard passes, so without an explicit ambiguous
	// case this candidate would fall through to not-evaluated-v1 and falsely
	// tell the operator no specific guard failed.
	candidate := TestLeftoverCandidate{
		IdentityVerdict:     "identity-available",
		ArgvShape:           "gui",
		PatternClass:        "ambiguous-multi-match",
		PathVerdict:         "path-canonical",
		PathRelations:       []string{},
		BuildInfoTag:        "test-tag-present",
		ParentLiveness:      "parent-proven-dead",
		AgeVsApplyFloor:     "at-or-above-apply-floor",
		EnvironmentOverride: "not-collected-v1",
	}
	if got := testLeftoverWouldRefuse(candidate, false, false); got != TestLeftoverAmbiguousFamily {
		t.Fatalf("ambiguous candidate refusal = %q, want %q", got, TestLeftoverAmbiguousFamily)
	}
	if got := testLeftoverWouldRefuse(candidate, false, false); got == TestLeftoverNotEvaluated {
		t.Fatalf("ambiguous candidate fell through to %q", TestLeftoverNotEvaluated)
	}

	// Same passing evidence with a single resolved family confirms every
	// downstream guard clears, so the ambiguous refusal above is caused solely
	// by the ambiguous PatternClass, not by any other failing check.
	resolved := candidate
	resolved.PatternClass = "gui-e2e"
	if got := testLeftoverWouldRefuse(resolved, false, false); got != TestLeftoverNotEvaluated {
		t.Fatalf("resolved-family control refusal = %q, want %q", got, TestLeftoverNotEvaluated)
	}
}

func TestLeftoverPreviewPathClassificationIsStrictAndDisplayOnly(t *testing.T) {
	root := t.TempDir()
	operatorRoot := filepath.Join(root, "scope")
	realDir := filepath.Join(operatorRoot, "real")
	exe := writePreviewFixture(t, filepath.Join(realDir, "mcphub-reliability-alias.exe"))
	aliasSpelling := realDir + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "real" + string(os.PathSeparator) + filepath.Base(exe)
	missingExe := filepath.Join(operatorRoot, "missing", "mcphub-reliability-broken.exe")

	csv := testLeftoverPreviewCSVHeader +
		previewCSVRow(601, 6, aliasSpelling, quotePreviewArg(aliasSpelling)+" daemon x", validPreviewCreated) +
		previewCSVRow(602, 6, missingExe, quotePreviewArg(missingExe)+" daemon x", validPreviewCreated)
	a := previewAPIWithSnapshot(csv)
	a.testLeftoverParentStateFn = deadParent
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{TempRoot: operatorRoot + string(os.PathSeparator)})
	if err != nil {
		t.Fatal(err)
	}
	if result.TempRootVerdict != "path-canonical" {
		t.Fatalf("temp root verdict = %q", result.TempRootVerdict)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(result.Candidates))
	}
	byPID := candidateMap(result.Candidates)
	if byPID[601].PatternClass != "operator-temp-root" || byPID[601].PathVerdict != "path-canonical" {
		t.Errorf("canonical alias classification = %+v", byPID[601])
	}
	if byPID[602].PathVerdict != "path-canonicalization-error" {
		t.Errorf("broken path classification = %+v", byPID[602])
	}
	if byPID[602].ApplyLifecycle != "apply-deferred-v1" {
		t.Error("path error changed preview-only lifecycle")
	}

	canonicalRoot, err := processpkg.CanonicalizePathStrict(operatorRoot + string(os.PathSeparator))
	if err != nil {
		t.Fatalf("strict canonical root: %v", err)
	}
	canonicalExe, err := processpkg.CanonicalizePathStrict(aliasSpelling)
	if err != nil {
		t.Fatalf("strict canonical exe: %v", err)
	}
	if canonicalRoot == "" || canonicalExe == "" {
		t.Fatal("strict canonicalizer returned an empty successful path")
	}
	if _, err := processpkg.CanonicalizePathStrict(missingExe); err == nil {
		t.Fatal("strict canonicalizer accepted a missing path")
	}
}

func TestLeftoverPreviewJSONRedactsRawPathAndArgv(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-user-workspace")
	exe := writePreviewFixture(t, filepath.Join(root, "mcphub-reliability-redact.exe"))
	cmdline := quotePreviewArg(exe) + " supervise --api-key=secret-preview-value"
	a := previewAPIWithSnapshot(testLeftoverPreviewCSVHeader +
		previewCSVRow(701, 7, exe, cmdline, validPreviewCreated))
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(raw)
	if strings.Contains(jsonText, root) || strings.Contains(jsonText, "secret-preview-value") || strings.Contains(jsonText, cmdline) {
		t.Fatalf("JSON leaked raw path or argv: %s", jsonText)
	}
	if !strings.Contains(jsonText, filepath.Base(exe)) || !strings.Contains(jsonText, `"argv_shape":"supervise"`) {
		t.Fatalf("JSON omitted redacted evidence: %s", jsonText)
	}
}

func TestLeftoverPreviewNeverCallsTerminateSeam(t *testing.T) {
	t.Setenv("MCPHUB_TEST_LEFTOVER_APPLY", "1")
	t.Setenv("MCPHUB_TEST_LEFTOVER_CONFIRM_TOKEN", "must-be-ignored")
	exe := writePreviewFixture(t, filepath.Join(t.TempDir(), "mcphub-reliability-no-action.exe"))
	a := previewAPIWithSnapshot(testLeftoverPreviewCSVHeader +
		previewCSVRow(801, 8, exe, quotePreviewArg(exe)+" supervise", validPreviewCreated))
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	previous := orphanTerminateFn
	terminateCalls := 0
	orphanTerminateFn = func(processpkg.PIDIdentityProof) error {
		terminateCalls++
		return errors.New("fail-on-call termination seam reached")
	}
	t.Cleanup(func() { orphanTerminateFn = previous })

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{})
	if err != nil {
		t.Fatalf("PreviewTestLeftovers: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected diagnostic candidate, got %+v", result.Candidates)
	}
	if terminateCalls != 0 {
		t.Fatalf("termination seam called %d times, want 0", terminateCalls)
	}
}

func TestLeftoverPreviewNonWindowsDefaultSnapshotIsNoOp(t *testing.T) {
	// Force the default-snapshotter branch onto a non-Windows platform. The
	// unsupported-platform verdict is set ONLY on the guard path, so observing
	// it proves the guard fired and runProcessSnapshot (wmic/powershell) was
	// never consulted — on the real host that call would set snapshot-complete
	// / snapshot-degraded or return an error instead.
	prev := testLeftoverGOOS
	testLeftoverGOOS = "linux"
	t.Cleanup(func() { testLeftoverGOOS = prev })

	a := NewAPI() // no injected testLeftoverSnapshotFn: the default snapshotter is in play
	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: 42, TempRoot: "/some/root"})
	if err != nil {
		t.Fatalf("non-windows default-snapshot preview returned error: %v", err)
	}
	if result.SnapshotVerdict != TestLeftoverSnapshotUnsupportedPlatform {
		t.Fatalf("snapshot verdict = %q, want %q", result.SnapshotVerdict, TestLeftoverSnapshotUnsupportedPlatform)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("no-op preview produced candidates: %+v", result.Candidates)
	}
	if !result.Exhaustive {
		t.Fatalf("no-op preview should read as exhaustive; nothing remains to enumerate")
	}
	if result.RequestedMinAgeSec != 42 {
		t.Fatalf("requested min age = %d, want 42 echoed through the no-op", result.RequestedMinAgeSec)
	}
}

func TestLeftoverPreviewInjectedCensusClassifiesRegardlessOfGOOS(t *testing.T) {
	// The non-Windows guard must gate on the DEFAULT snapshotter only. With an
	// injected census present, the hermetic seam must still classify even when
	// GOOS is non-Windows, or every injected-census unit test would silently
	// no-op on Linux/macOS CI.
	prev := testLeftoverGOOS
	testLeftoverGOOS = "linux"
	t.Cleanup(func() { testLeftoverGOOS = prev })

	exe := writePreviewFixture(t, filepath.Join(t.TempDir(), "mcphub-reliability-seam.exe"))
	a := previewAPIWithSnapshot(testLeftoverPreviewCSVHeader +
		previewCSVRow(910, 1, exe, quotePreviewArg(exe)+" daemon x", validPreviewCreated))
	a.testLeftoverBuildInfoReadFn = taggedPreviewBuildInfo
	a.testLeftoverParentStateFn = deadParent

	result, err := a.PreviewTestLeftovers(TestLeftoverPreviewOpts{MinAgeSec: 600})
	if err != nil {
		t.Fatalf("injected census on non-windows GOOS returned error: %v", err)
	}
	if result.SnapshotVerdict != TestLeftoverSnapshotComplete {
		t.Fatalf("injected census verdict = %q, want %q (guard must not short-circuit the seam)",
			result.SnapshotVerdict, TestLeftoverSnapshotComplete)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].PID != 910 {
		t.Fatalf("injected census did not classify under non-windows GOOS: %+v", result.Candidates)
	}
}

const validPreviewCreated = "20260710000000.000000+000"

func previewAPIWithSnapshot(csv string) *API {
	a := NewAPI()
	a.testLeftoverSnapshotFn = func() (string, error) { return csv, nil }
	a.testLeftoverNowFn = func() time.Time { return testLeftoverPreviewNow }
	return a
}

func taggedPreviewBuildInfo(string) (*debug.BuildInfo, error) {
	return &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "-tags", Value: "netgo,test_state_path_env,osusergo"}}}, nil
}

func writePreviewFixture(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("diagnostic fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func previewCSVRow(pid, ppid int, exe, cmdline, created string) string {
	return fmt.Sprintf("HOST,%s,%s,%s,%d,%d,1\n",
		previewCSVQuote(cmdline), created, previewCSVQuote(exe), ppid, pid)
}

func previewCSVQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quotePreviewArg(value string) string {
	return `"` + value + `"`
}

func candidateMap(candidates []TestLeftoverCandidate) map[int]TestLeftoverCandidate {
	out := make(map[int]TestLeftoverCandidate, len(candidates))
	for _, candidate := range candidates {
		out[candidate.PID] = candidate
	}
	return out
}
