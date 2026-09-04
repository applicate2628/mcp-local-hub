package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunScanDotPathMatchesRepositoryRoot(t *testing.T) {
	root, policy := fixture(t)
	var out, errOut bytes.Buffer
	code := run([]string{"scan", "--root", root, "--policy", policy, "--path", "."}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "mutable_global") {
		t.Fatalf("stdout=%s, dot path must include repository descendants", out.String())
	}
}
