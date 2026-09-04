package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunScanReturnsZeroWhenViolationsExist(t *testing.T) {
	root, policy := fixture(t)
	var out, errOut bytes.Buffer
	code := run([]string{"scan", "--root", root, "--policy", policy}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "mutable_global") {
		t.Fatalf("stdout=%s", out.String())
	}
}

func TestRunVerifyReturnsOneForArchitectureViolation(t *testing.T) {
	root, policy := fixture(t)
	baseline := write(t, "baseline.yaml", `{"schema_version":1,"generated_from":"x","entries":[]}`)
	workers := write(t, "workers.yaml", `{"schema_version":1,"entries":[]}`)
	owners := write(t, "owners.yaml", `{"schema_version":1,"rules":[]}`)
	var out, errOut bytes.Buffer
	code := run([]string{"verify", "--root", root, "--policy", policy, "--baseline", baseline, "--workers", workers, "--owners", owners}, &out, &errOut)
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

func TestRunVerifyRejectsPresentationFilters(t *testing.T) {
	root, policy := fixture(t)
	baseline := write(t, "baseline.yaml", `{"schema_version":1,"generated_from":"x","entries":[]}`)
	workers := write(t, "workers.yaml", `{"schema_version":1,"entries":[]}`)
	var out, errOut bytes.Buffer
	code := run([]string{"verify", "--root", root, "--policy", policy, "--baseline", baseline, "--workers", workers, "--kind", "worker"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--kind") {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

func TestRunBaselineRequiresOwnerForEveryViolation(t *testing.T) {
	root, policy := fixture(t)
	owners := write(t, "owners.yaml", `{"schema_version":1,"rules":[]}`)
	var out, errOut bytes.Buffer
	code := run([]string{"baseline", "--root", root, "--policy", policy, "--owners", owners, "--generated-from", "abc"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

func TestRunBaselineRequiresGeneratedFrom(t *testing.T) {
	root, policy := fixture(t)
	owners := write(t, "owners.yaml", `{"schema_version":1,"rules":[{"globs":["**"],"owner":"architecture","work_package":"WP-11","remove_by":"2027-01-01","reason":"legacy"}]}`)
	var out, errOut bytes.Buffer
	code := run([]string{"baseline", "--root", root, "--policy", policy, "--owners", owners}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--generated-from") {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

func TestRunScanPathFilterSupportsRecursiveGlob(t *testing.T) {
	root, policy := fixture(t)
	nested := filepath.Join(root, "internal", "x", "nested", "z.go")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("package nested\nvar nestedMutable = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"scan", "--root", root, "--policy", policy, "--path", "internal/**"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "nested/z.go") {
		t.Fatalf("stdout=%s", out.String())
	}
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module mcp-local-hub\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "internal/x/x.go")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("package x\nvar mutable = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := write(t, "policy.yaml", `{"schema_version":1,"module":"mcp-local-hub","source_roots":["internal"],"exclude_globs":[],"import_rules":[],"api_constructors":[],"production_constructors":[],"allowed_global_name_patterns":[],"test_hook_name_patterns":[],"history_comment_patterns":[],"history_allowed_globs":[],"embedded_document_min_bytes":4096,"file_budgets":{"production_advisory_lines":1000,"production_hard_lines":1500,"test_review_lines":2000},"generic_package_names":[]}`)
	return root, policy
}

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
