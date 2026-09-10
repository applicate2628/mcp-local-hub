package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsCollidingReportOutputPathsBeforeWriting(t *testing.T) {
	root, policy := fixture(t)
	report := filepath.Join(t.TempDir(), "report.out")
	var out, errOut bytes.Buffer
	code := run([]string{
		"scan",
		"--root", root,
		"--policy", policy,
		"--report-json", report,
		"--report-md", filepath.Join(filepath.Dir(report), ".", filepath.Base(report)),
	}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "distinct files") {
		t.Fatalf("code=%d stderr=%q, want colliding-output rejection", code, errOut.String())
	}
	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Fatalf("report path was written before collision rejection: err=%v", err)
	}
}
