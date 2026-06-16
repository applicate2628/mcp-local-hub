//go:build windows

package oneapi

import (
	"strings"
	"testing"
)

func TestSetvarsCaptureBatchContent_GuardsSetWithCallFailure(t *testing.T) {
	content := setvarsCaptureBatchContent(`C:\Program Files (x86)\Intel\oneAPI\setvars.bat`)

	call := `call "C:\Program Files (x86)\Intel\oneAPI\setvars.bat" --force > NUL 2>&1`
	noDefClear := `set "NoDefaultCurrentDirectoryInExePath="`
	guard := "if errorlevel 1 exit /b %errorlevel%"

	if !strings.Contains(content, call) {
		t.Fatalf("batch missing quoted --force setvars call %q: %q", call, content)
	}
	if !strings.Contains(content, noDefClear) {
		t.Fatalf("batch missing NoDefaultCurrentDirectoryInExePath clear: %q", content)
	}
	guardIndex := strings.Index(content, guard)
	dumpIndex := strings.LastIndex(content, "set")
	if guardIndex < 0 {
		t.Fatalf("batch missing failure guard %q: %q", guard, content)
	}
	// The guard must run AFTER the call and BEFORE the final `set` dump, so a
	// failing setvars exits non-zero instead of dumping the unchanged env.
	if guardIndex < strings.Index(content, call) {
		t.Fatalf("guard must follow the setvars call: %q", content)
	}
	if guardIndex > dumpIndex {
		t.Fatalf("failure guard must precede the set dump: %q", content)
	}
}

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
