//go:build windows

package oneapi

import (
	"strings"
	"testing"
)

func TestParseSetDump_ParsesKeyValueLines(t *testing.T) {
	dump := "INCLUDE=C:\\VS\\include\r\n" +
		"LIB=C:\\VS\\lib;C:\\SDK\\lib\r\n" +
		"PATH=C:\\VS\\bin;C:\\Windows\\System32\r\n" +
		"MKLROOT=C:\\oneAPI\\mkl\\latest\r\n"
	env := parseSetDump([]byte(dump))

	if len(env) != 4 {
		t.Fatalf("parsed %d entries, want 4: %v", len(env), env)
	}
	want := map[string]string{
		"INCLUDE": "C:\\VS\\include",
		"LIB":     "C:\\VS\\lib;C:\\SDK\\lib",
		"PATH":    "C:\\VS\\bin;C:\\Windows\\System32",
		"MKLROOT": "C:\\oneAPI\\mkl\\latest",
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if want[k] != v {
			t.Errorf("entry %q=%q, want %q", k, v, want[k])
		}
	}
}

func TestParseSetDump_SkipsBlankAndKeyless(t *testing.T) {
	dump := "A=1\r\n" +
		"\r\n" +
		"no-equals-here\r\n" +
		"=valueless-key\r\n" +
		"B=2\r\n"
	env := parseSetDump([]byte(dump))
	if len(env) != 2 {
		t.Fatalf("parsed %d entries, want 2 (A,B): %v", len(env), env)
	}
	if env[0] != "A=1" || env[1] != "B=2" {
		t.Errorf("unexpected entries: %v", env)
	}
}

func TestParseSetDump_ValueWithEquals(t *testing.T) {
	// A value legitimately containing '=' keeps everything after the FIRST '='.
	dump := "WEIRD=a=b=c\r\n"
	env := parseSetDump([]byte(dump))
	if len(env) != 1 {
		t.Fatalf("parsed %d entries, want 1: %v", len(env), env)
	}
	k, v, _ := strings.Cut(env[0], "=")
	if k != "WEIRD" || v != "a=b=c" {
		t.Errorf("entry = %q=%q, want WEIRD=a=b=c", k, v)
	}
}

func TestFileExists_DirIsNotFile(t *testing.T) {
	dir := t.TempDir()
	if fileExists(dir) {
		t.Errorf("fileExists(%q) = true for a directory, want false", dir)
	}
	if fileExists(dir + "\\does-not-exist.bat") {
		t.Error("fileExists returned true for a missing file")
	}
}
