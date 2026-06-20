//go:build !windows

package api

import (
	"os"
	"testing"
)

func broadenVaultReadForTest(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod read-broadened vault file: %v", err)
	}
}

func vaultReadBroadeningErrorForTest() error {
	return ErrTooLoose
}
