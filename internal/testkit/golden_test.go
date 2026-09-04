package testkit

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssertJSONGoldenStableAndReadOnly(t *testing.T) {
	const fixture = "{\n  \"a\": [\n    \"first\",\n    9007199254740993\n  ],\n  \"b\": null\n}\n"
	got := map[string]any{
		"b": nil,
		"a": []any{"first", json.Number("9007199254740993")},
	}
	for _, endings := range []string{"LF", "CRLF"} {
		t.Run(endings, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "expected.json")
			want := []byte(fixture)
			if endings == "CRLF" {
				want = bytes.ReplaceAll(want, []byte("\n"), []byte("\r\n"))
			}
			if err := os.WriteFile(path, want, 0600); err != nil {
				t.Fatal(err)
			}
			AssertJSONGolden(t, path, got)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, want) {
				t.Fatalf("fixture changed: %v", err)
			}
		})
	}
}

// Run failures in this test binary so Fatalf's actual testing.TB behavior is
// exercised, rather than replacing it with a mock interface.
func TestAssertJSONGoldenRejectsWithoutUpdating(t *testing.T) {
	const modeKey = "_MCPHUB_TESTKIT_GOLDEN_CHILD"
	const pathKey = "_MCPHUB_TESTKIT_GOLDEN_PATH"
	if mode := os.Getenv(modeKey); mode != "" {
		var got any
		switch mode {
		case "missing", "changed":
			got = map[string]any{"value": 2}
		case "array-order":
			got = []string{"second", "first"}
		case "precision":
			got = json.Number("9007199254740992")
		case "null-vs-empty":
			got = []string{}
		case "marshal":
			got = make(chan int)
		default:
			t.Fatalf("unknown child mode %q", mode)
		}
		AssertJSONGolden(t, os.Getenv(pathKey), got)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		fixture string
		message string
	}{
		{"missing", "", "read JSON golden"},
		{"changed", "{\n  \"value\": 1\n}\n", "JSON golden mismatch"},
		{"array-order", "[\n  \"first\",\n  \"second\"\n]\n", "JSON golden mismatch"},
		{"precision", "9007199254740993\n", "JSON golden mismatch"},
		{"null-vs-empty", "null\n", "JSON golden mismatch"},
		{"marshal", "{}\n", "marshal actual JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "expected.json")
			if tc.name != "missing" {
				if err := os.WriteFile(path, []byte(tc.fixture), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(executable, "-test.run=^TestAssertJSONGoldenRejectsWithoutUpdating$", "-test.timeout=10s")
			cmd.Env = append(os.Environ(), modeKey+"="+tc.name, pathKey+"="+path)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.message) {
				t.Fatalf("expected failure containing %q: err=%v output=%s", tc.message, err, output)
			}
			after, readErr := os.ReadFile(path)
			if tc.name == "missing" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("missing fixture was created: %v", readErr)
				}
			} else if readErr != nil || !bytes.Equal(after, []byte(tc.fixture)) {
				t.Fatalf("failed assertion changed fixture: %v", readErr)
			}
		})
	}
}
