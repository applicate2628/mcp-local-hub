package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalMcphubPath(t *testing.T) {
	first := canonicalMcphubPath()
	if first == "" {
		t.Fatal("canonicalMcphubPath returned empty path")
	}
	if !filepath.IsAbs(first) {
		t.Fatalf("canonicalMcphubPath = %q, want absolute path", first)
	}
	if second := canonicalMcphubPath(); second != first {
		t.Fatalf("canonicalMcphubPath cache changed: first=%q second=%q", first, second)
	}
}

func TestPathsEqual(t *testing.T) {
	base := filepath.Join(string(os.PathSeparator), "tmp", "McphubPathTest")
	clean := filepath.Join(base, "bin", "mcphub")
	withDotDot := filepath.Join(base, "bin", "..", "bin", "mcphub")
	trailing := clean + string(os.PathSeparator)

	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "case-insensitive", a: strings.ToUpper(clean), b: strings.ToLower(clean), want: true},
		{name: "dot-dot-cleaned", a: withDotDot, b: clean, want: true},
		{name: "trailing-separator-cleaned", a: trailing, b: clean, want: true},
		{name: "different-path", a: filepath.Join(base, "bin", "mcphub"), b: filepath.Join(base, "other", "mcphub"), want: false},
		{name: "empty-left", a: "", b: clean, want: false},
		{name: "empty-right", a: clean, b: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathsEqual(tt.a, tt.b); got != tt.want {
				t.Fatalf("pathsEqual(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
