package api

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadStateFileInodeAnchoredWithMaxBytesOverridesOnlyCallerCeiling(t *testing.T) {
	path := filepath.Join(hardenedTempDir(t), "oneapi-runs-v1.json")
	payload := bytes.Repeat([]byte("x"), maxStateFileBytes+1)
	if err := WriteStateFileBytesAtomic(path, payload); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadStateFileInodeAnchored(path); err == nil {
		t.Fatal("default ordinary-state reader accepted payload above its cap")
	}
	got, err := ReadStateFileInodeAnchoredWithMaxBytes(path, int64(len(payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("explicit-cap read = (%d bytes, %v), want exact payload", len(got), err)
	}
	if _, err := ReadStateFileInodeAnchoredWithMaxBytes(path, int64(len(payload)-1)); err == nil {
		t.Fatal("explicit cap below payload accepted")
	}
	for _, maxBytes := range []int64{0, -1} {
		if _, err := ReadStateFileInodeAnchoredWithMaxBytes(filepath.Join(t.TempDir(), "not-opened.json"), maxBytes); err == nil {
			t.Fatalf("maxBytes=%d accepted before input validation", maxBytes)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reader calls must not alter target: %v", err)
	}
}
