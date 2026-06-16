package oneapi

import (
	"os"
	"strings"
	"testing"
)

func sep() string { return string(os.PathListSeparator) }

func TestPrependEnvList_PrependsToExistingKey(t *testing.T) {
	env := []string{"PATH=C:\\orig", "OTHER=x"}
	out := PrependEnvList(env, "PATH", []string{"C:\\a", "C:\\b"})

	var pathEntry string
	n := 0
	for _, e := range out {
		if k, _, _ := strings.Cut(e, "="); strings.EqualFold(k, "PATH") {
			n++
			pathEntry = e
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 PATH entry, got %d: %v", n, out)
	}
	want := "PATH=C:\\a" + sep() + "C:\\b" + sep() + "C:\\orig"
	if pathEntry != want {
		t.Errorf("PATH = %q, want %q", pathEntry, want)
	}
	if !containsEntryOA(out, "OTHER=x") {
		t.Errorf("OTHER lost: %v", out)
	}
}

func TestPrependEnvList_CaseInsensitiveKeyPreservesCasing(t *testing.T) {
	// A captured `set` dump can emit "Lib=" not "LIB="; the prepend must find
	// and extend it (not synthesize a duplicate) and keep the original casing.
	env := []string{"Lib=C:\\orig"}
	out := PrependEnvList(env, "LIB", []string{"C:\\intel\\lib"})
	if len(out) != 1 {
		t.Fatalf("want 1 entry (no duplicate LIB), got %d: %v", len(out), out)
	}
	want := "Lib=C:\\intel\\lib" + sep() + "C:\\orig"
	if out[0] != want {
		t.Errorf("entry = %q, want %q (keep 'Lib' casing)", out[0], want)
	}
}

func TestPrependEnvList_SynthesizesWhenAbsent(t *testing.T) {
	env := []string{"FOO=bar"}
	out := PrependEnvList(env, "INCLUDE", []string{"C:\\mkl\\include"})
	if !containsEntryOA(out, "FOO=bar") {
		t.Errorf("FOO lost: %v", out)
	}
	if !containsEntryOA(out, "INCLUDE=C:\\mkl\\include") {
		t.Errorf("INCLUDE not synthesized: %v", out)
	}
}

func TestPrependEnvList_EmptyDirsReturnsCopy(t *testing.T) {
	env := []string{"PATH=C:\\orig", "A=1"}
	out := PrependEnvList(env, "PATH", nil)
	if len(out) != len(env) {
		t.Fatalf("len changed: got %d want %d", len(out), len(env))
	}
	out[0] = "MUTATED=1"
	if env[0] == "MUTATED=1" {
		t.Error("PrependEnvList returned an alias, not a copy")
	}
}

func containsEntryOA(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
