package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestEntryCommentsEncoderPreservesPowerShellJSONWire(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("testdata", "entry-comments-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	request, err := decodeEntryCommentsRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := "first\r\nsecond\nmcphub-source-v1;sha256=" + strings.Repeat("c", 64)
	if request.existing != want {
		t.Fatal("PowerShell producer changed original Comments bytes")
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--encode-entry-comments", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var result struct {
		Comments string `json:"comments"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	existing, binding, err := binaryadmission.ParseWindowsCanonicalEntryCommentsV1(result.Comments)
	if err != nil || existing != want || binding.BuildID() != request.buildID || binding.ExpectedCanonicalEntrySHA256() != request.launcherSHA256 || binding.ReverseProjection() != request.reverseProjection {
		t.Fatalf("build-tool/codec round trip changed original metadata: %v", err)
	}
}

func TestEntryCommentsRequestRejectsAmbiguousWire(t *testing.T) {
	valid := `{"existing_comments":"","build_id":"00112233445566778899aabbccddeeff","launcher_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reverse_projection":"../mcphub.exe"}`
	for _, raw := range []string{
		strings.Replace(valid, `"existing_comments":""`, `"existing_comments":"","existing_comments":"other"`, 1),
		strings.Replace(valid, `"build_id"`, `"Build_ID"`, 1),
		strings.Replace(valid, `"existing_comments":""`, `"extra":"","existing_comments":""`, 1),
		strings.Replace(valid, `"existing_comments":""`, `"existing_comments":null`, 1),
		strings.Replace(valid, `"existing_comments":"",`, "", 1),
		valid + `{}`,
	} {
		if _, err := decodeEntryCommentsRequest([]byte(raw)); err == nil {
			t.Fatalf("ambiguous request accepted: %s", raw)
		}
	}
}

func TestCanonicalEntryToolModesCannotMix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	for _, args := range [][]string{
		{"--encode-entry-comments", path, "--candidate", path},
		{"--encode-entry-comments", path, "--source-root", path},
		{"--validate-entry-set", path, "--expected-source-epoch", strings.Repeat("a", 64)},
		{"--validate-entry-set", path, "--candidate", path},
	} {
		var stderr bytes.Buffer
		if _, ok := parseOptions(args, &stderr); ok {
			t.Fatalf("mixed/incomplete modes accepted: %v", args)
		}
	}
}
