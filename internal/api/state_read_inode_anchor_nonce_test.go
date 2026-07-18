package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRestartNonceIsClassifiedSecretBearing(t *testing.T) {
	if !isSecretBearingStateFilePath("gui-restart-nonce") {
		t.Fatal("gui-restart-nonce is an authorization credential and must be secret-bearing")
	}
}

func TestConsumeStateSecretFileInodeAnchoredReadsAndUnlinks(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "gui-restart-nonce")
	want := bytes.Repeat([]byte{0x6a}, 32)
	if err := WriteStateFileBytesAtomic(path, want); err != nil {
		t.Fatalf("write nonce: %v", err)
	}

	got, err := ConsumeStateSecretFileInodeAnchored(path, int64(len(want)))
	if err != nil {
		t.Fatalf("ConsumeStateSecretFileInodeAnchored: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("consumed nonce = %x, want %x", got, want)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed nonce entry still exists: %v", err)
	}
}

func TestConsumeStateSecretFileInodeAnchoredRefusesReadBroadenedNonce(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "gui-restart-nonce")
	if err := WriteStateFileBytesAtomic(path, bytes.Repeat([]byte{0x7b}, 32)); err != nil {
		t.Fatalf("write nonce: %v", err)
	}
	broadenVaultReadForTest(t, path)

	if _, err := ConsumeStateSecretFileInodeAnchored(path, 32); err == nil {
		t.Fatal("strict nonce consume accepted a read-broadened authorization credential")
	}
}

func TestConsumeStateSecretFileInodeAnchoredRejectsAndUnlinksTruncatedSecret(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "gui-restart-nonce")
	if err := WriteStateFileBytesAtomic(path, []byte("short")); err != nil {
		t.Fatalf("write truncated nonce: %v", err)
	}

	if _, err := ConsumeStateSecretFileInodeAnchored(path, 32); err == nil {
		t.Fatal("strict nonce consume accepted a truncated authorization credential")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("truncated nonce entry survived failed consumption: %v", err)
	}
}
