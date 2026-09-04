package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/archguard"
)

func TestRunWorkersUnclassifiedTreatsBaselineWorkerAsClassified(t *testing.T) {
	root := t.TempDir()
	writeRepoModule(t, root)
	source := filepath.Join(root, "internal", "x", "x.go")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package x\nfunc F(){ go run() }\nfunc run(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policyPath := write(t, "policy.yaml", `{"schema_version":1,"module":"mcp-local-hub","source_roots":["internal"],"exclude_globs":[],"import_rules":[],"api_constructors":[],"production_constructors":[],"allowed_global_name_patterns":[],"test_hook_name_patterns":[],"history_comment_patterns":[],"history_allowed_globs":[],"embedded_document_min_bytes":4096,"file_budgets":{"production_advisory_lines":1000,"production_hard_lines":1500,"test_review_lines":2000},"generic_package_names":[]}`)
	policy, err := archguard.LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := archguard.Scan(context.Background(), archguard.ScanOptions{Root: root, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	var worker archguard.Violation
	for _, violation := range report.Violations {
		if violation.Kind == archguard.KindWorker {
			worker = violation
			break
		}
	}
	if worker.Fingerprint == "" {
		t.Fatal("fixture did not produce a worker")
	}
	baselineBody, err := yaml.Marshal(archguard.Baseline{
		SchemaVersion: 1,
		GeneratedFrom: "abc",
		Entries: []archguard.BaselineEntry{{
			Violation: worker, Owner: "owner", WorkPackage: "WP-11", RemoveBy: "2027-01-01", Reason: "legacy",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline := write(t, "baseline.yaml", string(baselineBody))
	workers := write(t, "workers.yaml", "schema_version: 1\nentries: []\n")

	var out, errOut bytes.Buffer
	code := run([]string{
		"workers", "--unclassified", "--root", root, "--policy", policyPath,
		"--baseline", baseline, "--workers", workers,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if strings.Contains(out.String(), worker.Fingerprint) {
		t.Fatalf("legacy baseline worker was reported as unclassified: %s", out.String())
	}
}
