package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparationFlagGrammarIsExclusiveIncludingSuppliedEmpty(t *testing.T) {
	valid := []string{"--prepare-source", "/export", "--source-checkout", "/checkout", "--source-snapshot-output", "/snapshot.json", "--expected-commit", strings.Repeat("a", 40)}
	var stderr bytes.Buffer
	options, ok := parseOptions(append(append([]string{}, valid...), "--generated-source", "build.ps1", "--generated-source", "cmd/mcphub/versioninfo.json"), &stderr)
	if !ok || len(options.generatedSource) != 2 {
		t.Fatalf("complete grammar rejected: %s", stderr.String())
	}
	for _, flag := range []string{"--candidate", "--expected-version", "--expected-build-date", "--expected-source-epoch", "--source-root", "--source-snapshot"} {
		var output, diagnostic bytes.Buffer
		args := append(append([]string{}, valid...), flag, "")
		if exit := run(args, &output, &diagnostic); exit != 2 || output.Len() != 0 {
			t.Fatalf("mixed %s: exit=%d stdout=%q stderr=%q", flag, exit, output.String(), diagnostic.String())
		}
	}
	for _, args := range [][]string{
		{"--generated-source", "build.ps1"},
		{"--prepare-source", ""},
		{"--source-checkout", ""},
		{"--source-snapshot-output", ""},
		append(append([]string{}, valid...), "positional"),
		append(append([]string{}, valid...), "--expected-commit", ""),
	} {
		var output, diagnostic bytes.Buffer
		if exit := run(args, &output, &diagnostic); exit != 2 || output.Len() != 0 {
			t.Fatalf("incomplete grammar exit=%d stdout=%q stderr=%q", exit, output.String(), diagnostic.String())
		}
	}
}

func TestExistingSourceVerificationStillEmitsItsTaggedRow(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("module example.invalid/legacy\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	snapshot := filepath.Join(parent, "snapshot.json")
	table := `{"schema":"reused-graphify-source-snapshot-v1","files":{"go.mod":{"sha256":"` + hex.EncodeToString(hash[:]) + `"}}}`
	if err := os.WriteFile(snapshot, []byte(table), 0600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostic bytes.Buffer
	if exit := run([]string{"--source-root", root, "--source-snapshot", snapshot}, &output, &diagnostic); exit != 0 {
		t.Fatalf("legacy verifier exit=%d stderr=%q", exit, diagnostic.String())
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if len(row) != 4 || string(row["schema"]) != `"windows-publisher-source-input-v1"` || string(row["file_count"]) != "1" || row["source_epoch"] == nil || row["source_snapshot_sha256"] == nil {
		t.Fatalf("legacy verifier wire shape: %s", output.Bytes())
	}
}
