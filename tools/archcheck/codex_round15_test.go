package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunBaselineRejectsOutputCollisionsBeforeWriting(t *testing.T) {
	root, policy := fixture(t)
	owners := write(t, "owners.yaml", `{"schema_version":1,"rules":[{"globs":["**"],"owner":"architecture","work_package":"WP-11","remove_by":"2027-01-01","reason":"legacy"}]}`)
	for _, reportFlag := range []string{"--report-json", "--report-md"} {
		t.Run(reportFlag, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "out")
			var stdout, stderr bytes.Buffer
			code := run([]string{"baseline", "--root", root, "--policy", policy, "--owners", owners, "--generated-from", "abc", "--baseline", outPath, reportFlag, outPath}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), "distinct") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if _, err := os.Stat(outPath); !os.IsNotExist(err) {
				t.Fatalf("colliding output was written before validation: %v", err)
			}
		})
	}
}

func TestRunRejectsOutputCollisionsWithInputFiles(t *testing.T) {
	root, policy := fixture(t)
	original, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"scan", "--root", root, "--policy", policy, "--report-json", policy}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "distinct") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	after, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("policy input was overwritten by report output")
	}
}

func TestValidateDistinctNamedPathsDetectsHardLinks(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := validateDistinctNamedPaths(
		namedPath{name: "first", path: first},
		namedPath{name: "second", path: second},
	); err == nil {
		t.Fatal("hard-linked outputs were not rejected")
	}
}
