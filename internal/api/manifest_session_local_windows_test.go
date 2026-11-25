package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateManifestRestoreRefusesBeforeDirectoryAndPublication(t *testing.T) {
	root := t.TempDir()
	linked := false
	err := restoreExactManifestNoReplaceWithLinkFn(root, "private-fixture", []byte(privateManifestFixture), nil, func(string, string) error { linked = true; return nil })
	if !errors.Is(err, ErrGuardedManifestLifecycle) || linked {
		t.Fatalf("ordinary restore published private identity: linked=%t err=%v", linked, err)
	}
	if _, err := os.Stat(filepath.Join(root, "private-fixture")); !os.IsNotExist(err) {
		t.Fatalf("refused restore materialized private directory: %v", err)
	}
}
