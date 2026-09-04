// Package testkit contains small shared helpers for repository tests.
package testkit

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// AssertJSONGolden compares value's indented JSON with a read-only fixture.
// Fixtures use encoding/json's two-space indentation and a final newline;
// CRLF file endings are accepted for Windows checkouts. Array order, number
// precision and null/empty distinctions are preserved. Callers normalize
// unstable fields (times, process IDs, paths) before calling this helper.
// It never creates or updates fixtures, even when the assertion fails.
func AssertJSONGolden(t testing.TB, path string, value any) {
	t.Helper()
	actual, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal actual JSON for %s: %v", path, err)
	}
	actual = append(actual, '\n')
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read JSON golden %s: %v", path, err)
	}
	expected = bytes.ReplaceAll(expected, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(expected, actual) {
		t.Fatalf("JSON golden mismatch for %s:\n--- expected\n%s\n+++ actual\n%s", path, expected, actual)
	}
}
