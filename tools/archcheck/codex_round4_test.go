package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathMatchesAnySupportsRecursiveDoubleStar(t *testing.T) {
	pattern := []string{"internal/**/*_test.go"}
	for _, path := range []string{
		"internal/x_test.go",
		"internal/a/b_test.go",
		"internal/a/b/c_test.go",
	} {
		if !pathMatchesAny(pattern, path) {
			t.Errorf("pattern did not match %q", path)
		}
	}
	if pathMatchesAny(pattern, "internal/a/b.go") {
		t.Fatal("test-file pattern matched a non-test Go file")
	}
}

func TestRunBaselineWritesRequestedReports(t *testing.T) {
	root, policy := fixture(t)
	owners := write(t, "owners.yaml", `{"schema_version":1,"rules":[{"globs":["**"],"owner":"architecture","work_package":"WP-11","remove_by":"2027-01-01","reason":"legacy"}]}`)
	reportDir := t.TempDir()
	jsonPath := filepath.Join(reportDir, "report.json")
	markdownPath := filepath.Join(reportDir, "report.md")

	var out, errOut bytes.Buffer
	code := run([]string{
		"baseline",
		"--root", root,
		"--policy", policy,
		"--owners", owners,
		"--generated-from", "abc",
		"--report-json", jsonPath,
		"--report-md", markdownPath,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	jsonBody, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	markdownBody, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(jsonBody, []byte("mutable_global")) {
		t.Fatalf("json report=%s", jsonBody)
	}
	if !bytes.Contains(markdownBody, []byte("# Architecture Report")) {
		t.Fatalf("markdown report=%s", markdownBody)
	}
}

func TestRunRequiresExplicitPolicyPath(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"scan"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--policy") {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}
