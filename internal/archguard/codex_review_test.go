package archguard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScanRejectsMissingConfiguredSourceRoot(t *testing.T) {
	root := newFixtureRepo(t, nil)
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"missing"}
	_, err := Scan(t.Context(), ScanOptions{Root: root, Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error=%v, want missing source-root error", err)
	}
}

func TestScanRejectsNonDirectoryConfiguredSourceRoot(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"not-a-directory": "x"})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"not-a-directory"}
	_, err := Scan(t.Context(), ScanOptions{Root: root, Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("error=%v, want non-directory source-root error", err)
	}
}

func TestConstructorRuleIgnoresShadowedImportAlias(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x
import api "mcp-local-hub/internal/api"
type Factory struct{}
func (Factory) NewAPI() any { return nil }
func Real() { _ = api.NewAPI() }
func Shadow(api Factory) { _ = api.NewAPI() }
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindAPIConstruction)
	if len(got) != 1 || got[0].Location.Symbol != "Real" {
		t.Fatalf("got=%#v", got)
	}
}

func TestConstructorRuleDetectsDotImportedConstructor(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x
import . "mcp-local-hub/internal/api"
func F() { _ = NewAPI() }
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindAPIConstruction)
	if len(got) != 1 || got[0].Location.Symbol != "F" {
		t.Fatalf("got=%#v", got)
	}
}

func TestLoadBaselineCanonicalizesFingerprint(t *testing.T) {
	v := sampleViolation("x", 0)
	body, err := json.Marshal(Baseline{
		SchemaVersion: 1,
		GeneratedFrom: "abc",
		Entries: []BaselineEntry{{
			Violation: v, Owner: "owner", WorkPackage: "WP-11", RemoveBy: "2027-01-01", Reason: "legacy",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), v.Fingerprint, "  "+v.Fingerprint+"  ", 1))
	got, err := LoadBaseline(writeTempFile(t, string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries[0].Fingerprint != v.Fingerprint {
		t.Fatalf("fingerprint=%q, want canonical %q", got.Entries[0].Fingerprint, v.Fingerprint)
	}
}

func TestLoadWorkersCanonicalizesFingerprint(t *testing.T) {
	fp := strings.Repeat("a", 64)
	body, err := json.Marshal(Workers{SchemaVersion: 1, Entries: []WorkerRecord{{
		Fingerprint: "  " + fp + "  ", Component: "x", Owner: "owner", Start: "start", Cancel: "cancel", Join: "join",
		BoundedBy: "bound", ContractTest: "x_test.go", WorkPackage: "WP-11",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadWorkers(writeTempFile(t, string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries[0].Fingerprint != fp {
		t.Fatalf("fingerprint=%q, want canonical %q", got.Entries[0].Fingerprint, fp)
	}
}

func TestGenericPackageExternalTestEmitsOneFinding(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/common/common.go":      "package common\n",
		"internal/common/common_test.go": "package common_test\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindGenericPackage)
	if len(got) != 1 {
		t.Fatalf("got=%#v, want one package-level finding", got)
	}
}

func TestTestOnlyBuildTagExcludesProductionRules(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/test_support.go": `//go:build test_state_path_env

package x

import api "mcp-local-hub/internal/api"

var mutable = 1
func SetClockForTest() {}
func F() { _ = api.NewAPI(); go func() {}() }
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.TestOnlyBuildTags = []string{"test_state_path_env"}
	report := mustScan(t, root, policy)
	for _, kind := range []ViolationKind{KindMutableGlobal, KindProductionTestHook, KindWorker, KindAPIConstruction} {
		if got := violationsOfKind(report, kind); len(got) != 0 {
			t.Fatalf("kind=%s got=%#v", kind, got)
		}
	}
}

func TestAlternativeProductionBuildDoesNotBecomeTestOnly(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/mixed.go": `//go:build test_state_path_env || windows

package x

var mutable = 1
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.TestOnlyBuildTags = []string{"test_state_path_env"}
	got := violationsOfKind(mustScan(t, root, policy), KindMutableGlobal)
	if len(got) != 1 {
		t.Fatalf("got=%#v, want production finding because windows also admits the file", got)
	}
}
