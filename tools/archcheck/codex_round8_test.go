package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsMalformedPathFilter(t *testing.T) {
	root, policy := fixture(t)
	var out, errOut bytes.Buffer
	code := run([]string{"scan", "--root", root, "--policy", policy, "--path", "internal/[ab"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--path") || !strings.Contains(errOut.String(), "character class") {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

func TestRunRejectsEmptyPathFilter(t *testing.T) {
	root, policy := fixture(t)
	var out, errOut bytes.Buffer
	code := run([]string{"scan", "--root", root, "--policy", policy, "--path", "   "}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--path") || !strings.Contains(errOut.String(), "empty") {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}
