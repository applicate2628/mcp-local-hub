//go:build windows

package oneapirun

import (
	"strings"
	"testing"
)

func TestVcvarsCaptureBatchContent_GuardsSetWithCallSuccess(t *testing.T) {
	content := vcvarsCaptureBatchContent(`C:\Program Files\Microsoft Visual Studio\VC\Auxiliary\Build\vcvars64.bat`)

	call := `call "C:\Program Files\Microsoft Visual Studio\VC\Auxiliary\Build\vcvars64.bat" > NUL 2>&1`
	guard := "if errorlevel 1 exit /b %errorlevel%"
	set := "set"
	if !strings.Contains(content, call) {
		t.Fatalf("batch content missing quoted vcvars call %q: %q", call, content)
	}
	guardIndex := strings.Index(content, guard)
	setIndex := strings.LastIndex(content, set)
	if guardIndex < 0 {
		t.Fatalf("batch content missing failure guard %q: %q", guard, content)
	}
	if setIndex < 0 {
		t.Fatalf("batch content missing set dump: %q", content)
	}
	if guardIndex > setIndex {
		t.Fatalf("failure guard must run before set dump: %q", content)
	}
}

func TestParseSetDump_ParsesKeyValueLines(t *testing.T) {
	dump := "INCLUDE=C:\\VS\\include\r\n" +
		"LIB=C:\\VS\\lib;C:\\SDK\\lib\r\n" +
		"PATH=C:\\VS\\bin;C:\\Windows\\System32\r\n" +
		"VSCMD_ARG_TGT_ARCH=x64\r\n"
	env := parseSetDump([]byte(dump))

	if len(env) != 4 {
		t.Fatalf("parsed %d entries, want 4: %v", len(env), env)
	}
	want := map[string]string{
		"INCLUDE":            "C:\\VS\\include",
		"LIB":                "C:\\VS\\lib;C:\\SDK\\lib",
		"PATH":               "C:\\VS\\bin;C:\\Windows\\System32",
		"VSCMD_ARG_TGT_ARCH": "x64",
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if want[k] != v {
			t.Errorf("entry %q=%q, want %q", k, v, want[k])
		}
	}
}

func TestParseSetDump_SkipsBlankAndKeyless(t *testing.T) {
	// Blank lines, a line with no '=', and a line with an empty key are
	// all skipped; valid lines survive.
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
	// A value legitimately containing '=' must keep everything after the
	// FIRST '=' verbatim (only the first '=' splits key from value).
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
