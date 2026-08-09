package clients

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var (
	errCleanupRemoveInjected     = errors.New("injected cleanup remove failure")
	errCleanupValidationInjected = errors.New("injected cleanup validation failure")
)

type cleanupTestClaude struct {
	*claudeCode
	writeMode  string
	writeCalls int
}

func newCleanupTestMutator(t *testing.T, removeMode string) (*cleanupTestClaude, DirectCleanupMutator, *DirectCleanupTarget, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude.json")
	original := []byte(`{
  "mcpServers": {
    "legacy-go": {
      "command": "mcp-language-server",
      "args": ["--lsp", "go", "--workspace", "C:\\workspace"]
    },
    "sibling": {
      "command": "operator-tool",
      "args": ["--keep"]
    }
  },
  "unrelated": true
}
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	concrete := &cleanupTestClaude{claudeCode: &claudeCode{path: path}, writeMode: removeMode}
	wrapped := newLockingClient(concrete)
	mutator, ok := AsDirectCleanupMutator(wrapped)
	if !ok {
		t.Fatal("cleanupTestClaude was not admitted through the existing CAS capability gate")
	}
	mutator.(*directCleanupMutator).writer = func(writePath string, contents []byte) error {
		concrete.writeCalls++
		switch {
		case concrete.writeMode == "before" && concrete.writeCalls == 1:
			return errCleanupRemoveInjected
		case concrete.writeMode == "after" && concrete.writeCalls == 1:
			if err := fallbackWriteConfigFile(writePath, contents); err != nil {
				return err
			}
			return errCleanupRemoveInjected
		default:
			return fallbackWriteConfigFile(writePath, contents)
		}
	}
	target, err := mutator.CaptureDirectCleanupTarget(DirectCleanupIdentity{
		Name:    "legacy-go",
		Command: "mcp-language-server",
		Args:    []string{"--lsp", "go", "--workspace", `C:\workspace`},
	})
	if err != nil {
		t.Fatalf("CaptureDirectCleanupTarget: %v", err)
	}
	return concrete, mutator, target, original
}

func runCleanupTestOperation(
	t *testing.T,
	mutator DirectCleanupMutator,
	target *DirectCleanupTarget,
	revalidate func(DirectCleanupCheckpoint) error,
) (DirectCleanupReceipt, string, error) {
	t.Helper()
	var receipt DirectCleanupReceipt
	backupPath, err := mutator.CleanupDirectEntryAtomically(
		target,
		3,
		func(got DirectCleanupReceipt) error {
			receipt = got
			return nil
		},
		revalidate,
	)
	if receipt == nil && backupPath != "" {
		t.Fatal("backup completed without pre-arming a cleanup receipt")
	}
	return receipt, backupPath, err
}

func TestDirectCleanupAtomic_PreRemoveRevalidationRefusalDoesNotRemove(t *testing.T) {
	concrete, mutator, target, original := newCleanupTestMutator(t, "")
	receipt, _, err := runCleanupTestOperation(t, mutator, target, func(checkpoint DirectCleanupCheckpoint) error {
		if checkpoint == DirectCleanupPostBackupPreRemove {
			return errCleanupValidationInjected
		}
		return nil
	})
	if !errors.Is(err, errCleanupValidationInjected) {
		t.Fatalf("cleanup error = %v, want injected validation cause", err)
	}
	if concrete.writeCalls != 0 {
		t.Fatalf("cleanup write calls = %d, want 0", concrete.writeCalls)
	}
	after, readErr := os.ReadFile(concrete.path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(original) {
		t.Fatalf("pre-remove refusal changed config bytes:\n%s", after)
	}
	if restoreErr := receipt.Restore(); restoreErr != nil {
		t.Fatalf("idempotent receipt restore: %v", restoreErr)
	}
}

func TestDirectCleanupReceipt_TotalClassifier(t *testing.T) {
	t.Run("absent restores original and preserves sibling", func(t *testing.T) {
		concrete, mutator, target, _ := newCleanupTestMutator(t, "")
		receipt, _, err := runCleanupTestOperation(t, mutator, target, func(DirectCleanupCheckpoint) error { return nil })
		if err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if err := concrete.setMember("new-sibling", map[string]any{"command": "operator-new"}); err != nil {
			t.Fatal(err)
		}
		if err := receipt.Restore(); err != nil {
			t.Fatalf("restore absent target: %v", err)
		}
		if entry, err := concrete.GetEntry("legacy-go"); err != nil || entry == nil {
			t.Fatalf("restored target: entry=%v err=%v", entry, err)
		}
		if entry, err := concrete.GetEntry("new-sibling"); err != nil || entry == nil {
			t.Fatalf("new sibling was not preserved: entry=%v err=%v", entry, err)
		}
	})

	t.Run("exact original is an idempotent no-write", func(t *testing.T) {
		concrete, mutator, target, original := newCleanupTestMutator(t, "before")
		receipt, _, err := runCleanupTestOperation(t, mutator, target, func(DirectCleanupCheckpoint) error { return nil })
		if !errors.Is(err, errCleanupRemoveInjected) {
			t.Fatalf("cleanup error = %v, want remove cause", err)
		}
		if err := receipt.Restore(); err != nil {
			t.Fatalf("restore exact original: %v", err)
		}
		after, readErr := os.ReadFile(concrete.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(after) != string(original) {
			t.Fatalf("exact-original restore rewrote bytes:\n%s", after)
		}
	})

	t.Run("foreign current target is preserved", func(t *testing.T) {
		concrete, mutator, target, _ := newCleanupTestMutator(t, "")
		receipt, _, err := runCleanupTestOperation(t, mutator, target, func(DirectCleanupCheckpoint) error { return nil })
		if err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if err := concrete.setMember("legacy-go", map[string]any{
			"command": "foreign-operator-tool",
			"args":    []any{"--owned"},
		}); err != nil {
			t.Fatal(err)
		}
		before, readErr := os.ReadFile(concrete.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		restoreErr := receipt.Restore()
		if !errors.Is(restoreErr, ErrCleanupRestoreConflict) {
			t.Fatalf("restore error = %v, want ErrCleanupRestoreConflict", restoreErr)
		}
		after, readErr := os.ReadFile(concrete.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(after) != string(before) {
			t.Fatalf("foreign target was changed:\nbefore=%s\nafter=%s", before, after)
		}
	})
}

func TestDirectCleanupReceipt_PartialRemoveErrorRestores(t *testing.T) {
	concrete, mutator, target, _ := newCleanupTestMutator(t, "after")
	receipt, _, err := runCleanupTestOperation(t, mutator, target, func(DirectCleanupCheckpoint) error { return nil })
	if !errors.Is(err, errCleanupRemoveInjected) {
		t.Fatalf("cleanup error = %v, want remove cause", err)
	}
	if err := receipt.Restore(); err != nil {
		t.Fatalf("restore after partial remove: %v", err)
	}
	if entry, getErr := concrete.GetEntry("legacy-go"); getErr != nil || entry == nil {
		t.Fatalf("target not restored after partial remove: entry=%v err=%v", entry, getErr)
	}
}

func TestDirectCleanupAtomic_PostRemoveRefusalRestoresBeforeUnlock(t *testing.T) {
	concrete, mutator, target, _ := newCleanupTestMutator(t, "")
	receipt, _, err := runCleanupTestOperation(t, mutator, target, func(checkpoint DirectCleanupCheckpoint) error {
		if checkpoint == DirectCleanupPostRemove {
			return errCleanupValidationInjected
		}
		return nil
	})
	if !errors.Is(err, errCleanupValidationInjected) {
		t.Fatalf("cleanup error = %v, want validation cause", err)
	}
	if entry, getErr := concrete.GetEntry("legacy-go"); getErr != nil || entry == nil {
		t.Fatalf("post-remove refusal did not restore under lock: entry=%v err=%v", entry, getErr)
	}
	if err := receipt.Restore(); err != nil {
		t.Fatalf("repeated receipt restore: %v", err)
	}
}

func TestDirectCleanupAtomic_TargetChangeBeforeOperationFailsCAS(t *testing.T) {
	concrete, mutator, target, _ := newCleanupTestMutator(t, "")
	if err := concrete.setMember("legacy-go", map[string]any{
		"command": "foreign-operator-tool",
		"args":    []any{"--changed"},
	}); err != nil {
		t.Fatal(err)
	}
	receipt, backupPath, err := runCleanupTestOperation(t, mutator, target, func(DirectCleanupCheckpoint) error { return nil })
	if !errors.Is(err, ErrCASConflict) {
		t.Fatalf("cleanup error = %v, want ErrCASConflict", err)
	}
	if receipt != nil || backupPath != "" {
		t.Fatalf("CAS refusal armed receipt=%v backup=%q; want neither", receipt != nil, backupPath)
	}
	if concrete.writeCalls != 0 {
		t.Fatalf("cleanup write calls = %d, want 0", concrete.writeCalls)
	}
}
