package api

import (
	"strings"
	"testing"
)

func TestManifestHashContent(t *testing.T) {
	// Same bytes → same hash.
	h1 := ManifestHashContent([]byte("name: demo\n"))
	h2 := ManifestHashContent([]byte("name: demo\n"))
	if h1 != h2 {
		t.Errorf("same bytes different hash: %q vs %q", h1, h2)
	}
	// Different bytes → different hash.
	h3 := ManifestHashContent([]byte("name: other\n"))
	if h1 == h3 {
		t.Errorf("different bytes same hash: %q", h1)
	}
	// Format: 64 hex chars.
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64", len(h1))
	}
	for _, c := range h1 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("non-hex char %q in hash %q", c, h1)
		}
	}
}

func TestManifestHashContent_EmptyInput(t *testing.T) {
	h := ManifestHashContent([]byte{})
	// SHA-256("") is a well-known value.
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if h != want {
		t.Errorf("empty hash = %q, want %q", h, want)
	}
}
