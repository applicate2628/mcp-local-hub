//go:build !windows

package oneapi

import (
	"strings"
	"testing"
)

func TestParseEnvLines_ParsesKeyValue(t *testing.T) {
	dump := "PATH=/usr/bin:/bin\n" +
		"LD_LIBRARY_PATH=/opt/intel/oneapi/mkl/latest/lib:/opt/intel/oneapi/compiler/latest/lib\n" +
		"CPATH=/opt/intel/oneapi/mkl/latest/include\n" +
		"MKLROOT=/opt/intel/oneapi/mkl/latest\n"
	env := parseEnvLines([]byte(dump))
	if len(env) != 4 {
		t.Fatalf("parsed %d entries, want 4: %v", len(env), env)
	}
	want := map[string]string{
		"PATH":            "/usr/bin:/bin",
		"LD_LIBRARY_PATH": "/opt/intel/oneapi/mkl/latest/lib:/opt/intel/oneapi/compiler/latest/lib",
		"CPATH":           "/opt/intel/oneapi/mkl/latest/include",
		"MKLROOT":         "/opt/intel/oneapi/mkl/latest",
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if want[k] != v {
			t.Errorf("entry %q=%q, want %q", k, v, want[k])
		}
	}
}

func TestParseEnvLines_SkipsBlankAndKeyless(t *testing.T) {
	dump := "A=1\n\nno-equals\n=valueless\nB=2\n"
	env := parseEnvLines([]byte(dump))
	if len(env) != 2 || env[0] != "A=1" || env[1] != "B=2" {
		t.Fatalf("want [A=1 B=2], got %v", env)
	}
}

func TestParseEnvLines_ValueWithEquals(t *testing.T) {
	env := parseEnvLines([]byte("WEIRD=a=b=c\n"))
	if len(env) != 1 {
		t.Fatalf("want 1 entry, got %v", env)
	}
	if k, v, _ := strings.Cut(env[0], "="); k != "WEIRD" || v != "a=b=c" {
		t.Errorf("got %q=%q, want WEIRD=a=b=c", k, v)
	}
}

func TestRootProbePaths_IncludesStandardPosixRoots(t *testing.T) {
	t.Setenv("ONEAPI_ROOT", "/custom/oneapi")
	paths := rootProbePaths()
	if len(paths) == 0 || paths[0] != "/custom/oneapi" {
		t.Fatalf("ONEAPI_ROOT should be probed first, got %v", paths)
	}
	joined := strings.Join(paths, "|")
	if !strings.Contains(joined, "/opt/intel/oneapi") {
		t.Errorf("expected /opt/intel/oneapi among probe paths, got %v", paths)
	}
}
