package oneapi

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// envToMap splits "KEY=VALUE" entries into a map (last write wins). Malformed
// entries (no '=') are skipped.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}

// envKeySet returns the normalized-key set of an env slice (case-folded on
// Windows, matching mergeEnv's own key comparison).
func envKeySet(env []string) map[string]bool {
	s := make(map[string]bool, len(env))
	for _, e := range env {
		if k, _, ok := strings.Cut(e, "="); ok {
			s[normalizeEnvKey(k)] = true
		}
	}
	return s
}

func TestMergeEnv_OverlayWinsKeepsBaseAppendsNew(t *testing.T) {
	base := []string{"PATH=/base/bin", "HOME=/home/u", "KEEPME=sentinel"}
	overlay := []string{"PATH=/oneapi/bin:/base/bin", "MKLROOT=/opt/mkl"}

	got := mergeEnv(base, overlay)
	m := envToMap(got)

	// Overlay PATH replaces base PATH — setvars builds the complete PATH, so the
	// overlay value (which already contains the prior PATH) supersedes base.
	if m["PATH"] != "/oneapi/bin:/base/bin" {
		t.Errorf("PATH = %q, want the overlay value", m["PATH"])
	}
	// A new oneAPI-only key is appended.
	if m["MKLROOT"] != "/opt/mkl" {
		t.Errorf("MKLROOT = %q, want the appended overlay value", m["MKLROOT"])
	}
	// An untouched base key survives — this is the t.Setenv-sentinel analogue
	// the raw-snapshot approach dropped.
	if m["KEEPME"] != "sentinel" {
		t.Errorf("KEEPME dropped: %q (want \"sentinel\")", m["KEEPME"])
	}
	if m["HOME"] != "/home/u" {
		t.Errorf("HOME dropped: %q", m["HOME"])
	}

	// No key is duplicated in the merged slice.
	seen := map[string]bool{}
	for _, e := range got {
		k, _, _ := strings.Cut(e, "=")
		nk := normalizeEnvKey(k)
		if seen[nk] {
			t.Errorf("merged env duplicates key %q: %v", k, got)
		}
		seen[nk] = true
	}

	// base must not be mutated.
	if base[0] != "PATH=/base/bin" {
		t.Errorf("mergeEnv mutated base: %v", base)
	}
}

func TestMergeEnv_WindowsCaseInsensitiveKeyMatch(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive env keys are Windows-only")
	}
	base := []string{`Path=C:\base`}
	overlay := []string{`PATH=C:\oneapi;C:\base`}

	got := mergeEnv(base, overlay)
	if len(got) != 1 {
		t.Fatalf("case-insensitive Path/PATH not merged; got %d entries: %v", len(got), got)
	}
	if _, v, _ := strings.Cut(got[0], "="); v != `C:\oneapi;C:\base` {
		t.Errorf("merged Path value = %q, want the overlay value", v)
	}
}

func TestMergeEnv_SkipsMalformedOverlayKeepsMalformedBase(t *testing.T) {
	base := []string{"A=1", "noequalsign"}
	overlay := []string{"alsonoequals", "B=2"}

	got := mergeEnv(base, overlay)
	m := envToMap(got)
	if m["A"] != "1" || m["B"] != "2" {
		t.Errorf("well-formed entries lost: %v", got)
	}
	foundBaseMalformed, leakedOverlayMalformed := false, false
	for _, e := range got {
		switch e {
		case "noequalsign":
			foundBaseMalformed = true
		case "alsonoequals":
			leakedOverlayMalformed = true
		}
	}
	if !foundBaseMalformed {
		t.Errorf("malformed base entry was dropped: %v", got)
	}
	if leakedOverlayMalformed {
		t.Errorf("malformed overlay entry leaked into the merge: %v", got)
	}
}

// TestEnvOverlay_HostIndependent asserts the two valid outcomes without
// assuming whether THIS host has Intel oneAPI installed:
//   - no oneAPI → EnvOverlay returns nil (the documented inherit-os.Environ
//     fallback);
//   - oneAPI present → the result is a superset of os.Environ() (every live key
//     preserved, oneAPI vars layered on top).
func TestEnvOverlay_HostIndependent(t *testing.T) {
	got := EnvOverlay()
	if got == nil {
		t.Log("no oneAPI install on this host — EnvOverlay returned nil (documented fallback)")
		return
	}
	overlayKeys := envKeySet(got)
	for k := range envKeySet(os.Environ()) {
		if !overlayKeys[k] {
			t.Errorf("EnvOverlay dropped a live os.Environ() key: %q", k)
		}
	}
	if len(got) < len(os.Environ()) {
		t.Errorf("EnvOverlay has %d entries, fewer than os.Environ()'s %d — keys were lost", len(got), len(os.Environ()))
	}
}
