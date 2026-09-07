package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsUnknownKindFilter(t *testing.T) {
	root, policy := fixture(t)
	var out, errOut bytes.Buffer
	code := run([]string{
		"scan",
		"--root", root,
		"--policy", policy,
		"--kind", "mutable_globals",
	}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "unknown --kind") || !strings.Contains(errOut.String(), "mutable_global") {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
}

func TestRunAcceptsCanonicalKindFilter(t *testing.T) {
	root, policy := fixture(t)
	var out, errOut bytes.Buffer
	code := run([]string{
		"scan",
		"--root", root,
		"--policy", policy,
		"--kind", " mutable_global ",
	}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "mutable_global") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
}
