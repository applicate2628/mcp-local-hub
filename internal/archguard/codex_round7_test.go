package archguard

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestEmbeddedDocumentResolvesSiblingPackageConstants(t *testing.T) {
	heading := "# Heading\n" + strings.Repeat("a", 60)
	body := "\n```\n" + strings.Repeat("b", 60)
	root := newFixtureRepo(t, map[string]string{
		"internal/x/fragments.go": "package x\nconst heading = " + strconv.Quote(heading) + "\nconst body = " + strconv.Quote(body) + "\n",
		"internal/x/document.go":  "package x\nconst document = heading + body\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.EmbeddedDocumentMinBytes = 100
	got := violationsOfKind(mustScan(t, root, policy), KindEmbeddedDocument)
	if len(got) != 1 || got[0].Location.Symbol != "document" || got[0].Metric != len(heading)+len(body) {
		t.Fatalf("got=%#v, want sibling-file document expansion", got)
	}
}

func TestPolicyRejectsMalformedGlobClass(t *testing.T) {
	body := strings.Replace(validPolicyJSON, `"from": ["internal/cli"]`, `"from": ["internal/[ab"]`, 1)
	_, err := LoadPolicy(writeTempFile(t, body))
	if err == nil || !strings.Contains(err.Error(), "character class") {
		t.Fatalf("error=%v, want malformed glob rejection", err)
	}
}

func TestWorkerDeadlineWhitespaceIsCanonicalizedAndAccepted(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	body := fmt.Sprintf(`{
  "schema_version": 1,
  "entries": [{
    "fingerprint": %q,
    "component": "archguard",
    "owner": "architecture",
    "start": "F",
    "cancel": "context cancellation",
    "join": "wait group",
    "bounded_by": "request deadline",
    "contract_test": "worker_test.go",
    "work_package": "WP-11A",
    "remove_by": " 2027-01-01 "
  }]
}`, fingerprint)
	loaded, err := LoadWorkers(writeTempFile(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Entries[0].RemoveBy; got != "2027-01-01" {
		t.Fatalf("remove_by=%q, want canonical date", got)
	}

	violation := sampleViolation("worker", 0)
	violation.Kind = KindWorker
	violation.Fingerprint = Fingerprint(violation)
	record := loaded.Entries[0]
	record.Fingerprint = violation.Fingerprint
	record.RemoveBy = " 2027-01-01 "
	result := Verify(
		reportOf(violation),
		Baseline{SchemaVersion: 1, GeneratedFrom: "base"},
		Workers{SchemaVersion: 1, Entries: []WorkerRecord{record}},
		VerifyOptions{Now: mustDate(t, "2026-08-25")},
	)
	if !result.OK() {
		t.Fatalf("verification=%#v, whitespace-padded valid deadline must pass", result)
	}
}

func TestPolicyRejectsReservedTestOnlyBuildTags(t *testing.T) {
	for _, tag := range []string{"linux", "amd64", "gc", "cgo", "go1.23", "goexperiment.regabi", "amd64.v3"} {
		t.Run(tag, func(t *testing.T) {
			body := strings.Replace(
				validPolicyJSON,
				`"embedded_document_min_bytes": 4096`,
				`"test_only_build_tags": [`+strconv.Quote(tag)+`], "embedded_document_min_bytes": 4096`,
				1,
			)
			_, err := LoadPolicy(writeTempFile(t, body))
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("tag=%q error=%v, want reserved-tag rejection", tag, err)
			}
		})
	}
}

func TestZeroLengthHistoryRegexProducesFinding(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": "package x\n// ordinary comment\nfunc F() {}\n",
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.HistoryCommentPatterns = []string{"^"}
	got := violationsOfKind(mustScan(t, root, policy), KindHistoryComment)
	if len(got) != 1 || got[0].Evidence != "regex:^" {
		t.Fatalf("got=%#v, want a zero-length history-regex finding", got)
	}
}

func TestProductionTypeFieldsAndInterfaceMethodsAreCheckedForTestHooks(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{
		"internal/x/x.go": `package x

import testsupport "example.com/testsupport"

type Hooks struct {
	SetClockForTest func()
}

type API interface {
	RestoreClockForTest()
}

type Safe interface {
	Do(SetClockForTest func())
}

type Embedded struct { testsupport.SetClockForTest }
`,
	})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	got := violationsOfKind(mustScan(t, root, policy), KindProductionTestHook)
	if len(got) != 3 {
		t.Fatalf("got=%#v, want struct field, interface method, and embedded field only", got)
	}
	symbols := map[string]bool{}
	for _, violation := range got {
		symbols[violation.Location.Symbol] = true
	}
	if !symbols["Hooks.SetClockForTest"] || !symbols["API.RestoreClockForTest"] || !symbols["Embedded.SetClockForTest"] {
		t.Fatalf("symbols=%#v", symbols)
	}
}

func TestOwnersRejectMalformedGlobClass(t *testing.T) {
	_, err := LoadOwners(writeTempFile(t, `{"schema_version":1,"rules":[{"globs":["internal/[ab"],"owner":"architecture","work_package":"WP-11","remove_by":"2027-01-01","reason":"legacy"}]}`))
	if err == nil || !strings.Contains(err.Error(), "character class") {
		t.Fatalf("error=%v, want malformed owner glob rejection", err)
	}
}

func TestScanRejectsProgrammaticMalformedGlobClass(t *testing.T) {
	root := newFixtureRepo(t, map[string]string{"internal/x/x.go": "package x\n"})
	policy := mustLoadPolicyForTest(t)
	policy.SourceRoots = []string{"internal"}
	policy.ImportRules = []ImportRule{{From: []string{"internal/[ab"}, Deny: []string{"internal/gui/**"}}}
	_, err := Scan(t.Context(), ScanOptions{Root: root, Policy: policy})
	if err == nil || !strings.Contains(err.Error(), "character class") {
		t.Fatalf("error=%v, want programmatic malformed glob rejection", err)
	}
}
