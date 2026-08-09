package clients

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

func assertAppliedReleaseUnconfirmed(t *testing.T, configPath string, err, releaseCause error) {
	t.Helper()
	if !errors.Is(err, ErrConfigLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
		t.Fatalf("mutation error = %v, want release sentinel and cause", err)
	}
	if got := ClassifyClientMutation(err); got != ClientMutationAppliedReleaseUnconfirmed {
		t.Fatalf("mutation settlement = %v, want applied release unconfirmed", got)
	}
	laterCalls := 0
	laterErr := withConfigMutationLock(configPath, func() error {
		laterCalls++
		return nil
	})
	if laterCalls != 0 || !errors.Is(laterErr, ErrConfigLockReleaseUnconfirmed) || !errors.Is(laterErr, releaseCause) {
		t.Fatalf("later retained-lock result = %v callback count=%d, want sentinel, cause, and zero callbacks", laterErr, laterCalls)
	}
}

func injectMutationOutcomeReleaseFailure(t *testing.T, configPath string) error {
	t.Helper()
	lockPath := configPath + ".lock"
	cause := errors.New("injected mutation outcome unlock failure")
	previous := configFlockUnlockFn
	var stranded []*flock.Flock
	configFlockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = append(stranded, fl)
			return cause
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		configFlockUnlockFn = previous
		for _, fl := range stranded {
			_ = fl.Unlock()
		}
		unconfirmedConfigLockReleasesMu.Lock()
		delete(unconfirmedConfigLockReleases, lockPath)
		unconfirmedConfigLockReleasesMu.Unlock()
	})
	return cause
}

func TestClientMutationSettlementLockOutcomeMatrix(t *testing.T) {
	t.Run("pre-body failure does not run callback and needs compensation", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "client.json")
		previous := SecureCreateParentDir
		SecureCreateParentDir = func(string) error { return errors.New("injected parent failure") }
		t.Cleanup(func() { SecureCreateParentDir = previous })
		called := 0
		execution, err := withConfigLockExecution(configPath, func() error {
			called++
			return nil
		})
		if err == nil {
			t.Fatal("withConfigLockExecution error = nil, want pre-body failure")
		}
		if execution.bodyEntered || called != 0 {
			t.Fatalf("pre-body failure entered callback: execution=%+v called=%d", execution, called)
		}
		if got := ClassifyClientMutation(err); got != ClientMutationNeedsCompensation {
			t.Fatalf("settlement = %v, want needs compensation", got)
		}
	})

	t.Run("body error needs compensation", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "client.json")
		bodyCause := errors.New("injected body failure")
		called := 0
		execution, err := withConfigLockExecution(configPath, func() error {
			called++
			if writeErr := os.WriteFile(configPath, []byte("body-wrote-before-error"), 0o600); writeErr != nil {
				return writeErr
			}
			return bodyCause
		})
		if !execution.bodyEntered || !errors.Is(err, bodyCause) || called != 1 {
			t.Fatalf("body result execution=%+v err=%v called=%d", execution, err, called)
		}
		if got := ClassifyClientMutation(err); got != ClientMutationNeedsCompensation {
			t.Fatalf("settlement = %v, want needs compensation", got)
		}
		if data, readErr := os.ReadFile(configPath); readErr != nil || string(data) != "body-wrote-before-error" {
			t.Fatalf("body-error bytes = %q err=%v, want callback write preserved for compensator", data, readErr)
		}
	})

	t.Run("successful body and release classify applied", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "client.json")
		called := 0
		err := withConfigMutationLock(configPath, func() error {
			called++
			return os.WriteFile(configPath, []byte("successfully-applied"), 0o600)
		})
		if err != nil || called != 1 {
			t.Fatalf("success err=%v called=%d", err, called)
		}
		if got := ClassifyClientMutation(err); got != ClientMutationApplied {
			t.Fatalf("settlement = %v, want applied", got)
		}
		if data, readErr := os.ReadFile(configPath); readErr != nil || string(data) != "successfully-applied" {
			t.Fatalf("success bytes = %q err=%v, want applied callback write", data, readErr)
		}
	})

	t.Run("successful body and release failure classify applied without later reacquire", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "client.json")
		releaseCause := injectMutationOutcomeReleaseFailure(t, configPath)
		called := 0
		err := withConfigMutationLock(configPath, func() error {
			called++
			return os.WriteFile(configPath, []byte("applied-before-release-failure"), 0o600)
		})
		if called != 1 || !errors.Is(err, ErrConfigLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
			t.Fatalf("release result err=%v called=%d", err, called)
		}
		if got := ClassifyClientMutation(err); got != ClientMutationAppliedReleaseUnconfirmed {
			t.Fatalf("settlement = %v, want applied release unconfirmed", got)
		}
		if data, readErr := os.ReadFile(configPath); readErr != nil || string(data) != "applied-before-release-failure" {
			t.Fatalf("release-failure bytes = %q err=%v, want applied callback write", data, readErr)
		}
		laterCalled := 0
		laterErr := withConfigMutationLock(configPath, func() error {
			laterCalled++
			return nil
		})
		if laterCalled != 0 || !errors.Is(laterErr, ErrConfigLockReleaseUnconfirmed) {
			t.Fatalf("later acquire err=%v callback count=%d, want retained-lock fail-fast", laterErr, laterCalled)
		}
	})
}

func TestLockingClientMutationsClassifyAppliedReleaseUnconfirmed(t *testing.T) {
	const oldEntry = `{"mcpServers":{"target":{"url":"http://old.example/mcp"}}}`
	const newEntry = `{"mcpServers":{"target":{"url":"http://new.example/mcp"}}}`
	const empty = `{"mcpServers":{}}`

	type operation struct {
		name        string
		initial     string
		wantPresent bool
		run         func(t *testing.T, client Client, configPath string) (error, string)
	}
	operations := []operation{
		{
			name:        "add",
			initial:     empty,
			wantPresent: true,
			run: func(t *testing.T, client Client, _ string) (error, string) {
				t.Helper()
				return client.AddEntry(MCPEntry{Name: "target", URL: "http://new.example/mcp"}), "target"
			},
		},
		{
			name:        "remove",
			initial:     oldEntry,
			wantPresent: false,
			run: func(t *testing.T, client Client, _ string) (error, string) {
				t.Helper()
				return client.RemoveEntry("target"), "target"
			},
		},
		{
			name:        "whole restore",
			initial:     empty,
			wantPresent: true,
			run: func(t *testing.T, client Client, configPath string) (error, string) {
				t.Helper()
				backupPath := filepath.Join(filepath.Dir(configPath), "whole-backup.json")
				if err := os.WriteFile(backupPath, []byte(oldEntry), 0o600); err != nil {
					t.Fatal(err)
				}
				return client.Restore(backupPath), "old.example"
			},
		},
		{
			name:        "entry restore",
			initial:     newEntry,
			wantPresent: true,
			run: func(t *testing.T, client Client, configPath string) (error, string) {
				t.Helper()
				backupPath := filepath.Join(filepath.Dir(configPath), "entry-backup.json")
				if err := os.WriteFile(backupPath, []byte(oldEntry), 0o600); err != nil {
					t.Fatal(err)
				}
				return client.RestoreEntryFromBackup(backupPath, "target"), "old.example"
			},
		},
		{
			name:        "rollback entry restore",
			initial:     newEntry,
			wantPresent: true,
			run: func(t *testing.T, client Client, configPath string) (error, string) {
				t.Helper()
				backupPath := filepath.Join(filepath.Dir(configPath), "rollback-backup.json")
				if err := os.WriteFile(backupPath, []byte(oldEntry), 0o600); err != nil {
					t.Fatal(err)
				}
				return client.RestoreEntryFromBackupForRollback(backupPath, "target"), "old.example"
			},
		},
		{
			name:        "scoped add",
			initial:     empty,
			wantPresent: true,
			run: func(t *testing.T, client Client, _ string) (error, string) {
				t.Helper()
				writerCalls := 0
				writer := func(path string, contents []byte) error {
					writerCalls++
					return os.WriteFile(path, contents, 0o600)
				}
				err := AddEntryWithConfigWriter(client, MCPEntry{Name: "target", URL: "http://new.example/mcp"}, writer)
				if writerCalls != 1 {
					t.Fatalf("scoped add writer calls = %d, want 1", writerCalls)
				}
				return err, "target"
			},
		},
		{
			name:        "scoped rollback entry restore",
			initial:     newEntry,
			wantPresent: true,
			run: func(t *testing.T, client Client, configPath string) (error, string) {
				t.Helper()
				backupPath := filepath.Join(filepath.Dir(configPath), "scoped-rollback-backup.json")
				if err := os.WriteFile(backupPath, []byte(oldEntry), 0o600); err != nil {
					t.Fatal(err)
				}
				writerCalls := 0
				writer := func(path string, contents []byte) error {
					writerCalls++
					return os.WriteFile(path, contents, 0o600)
				}
				err := RestoreEntryFromBackupForRollbackWithConfigWriter(client, backupPath, "target", writer)
				if writerCalls != 1 {
					t.Fatalf("scoped rollback writer calls = %d, want 1", writerCalls)
				}
				return err, "old.example"
			},
		},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			client, configPath := newLockingCursorForTest(t, operation.initial)
			releaseCause := injectMutationOutcomeReleaseFailure(t, configPath)
			operationErr, wantBytes := operation.run(t, client, configPath)
			assertAppliedReleaseUnconfirmed(t, configPath, operationErr, releaseCause)
			// Each operation returns through the same lockingClient classifier rather
			// than relying on its individual adapter body to describe the outcome.
			data, readErr := os.ReadFile(configPath)
			if readErr != nil || strings.Contains(string(data), wantBytes) != operation.wantPresent {
				t.Fatalf("config bytes = %q err=%v, marker %q present=%t want=%t", data, readErr, wantBytes, strings.Contains(string(data), wantBytes), operation.wantPresent)
			}
		})
	}
}

func TestCASMutationsClassifyAppliedReleaseUnconfirmed(t *testing.T) {
	const hubConfig = `{"mcpServers":{"serena":{"url":"` + casHubURL + `"}}}`
	nativeSnapshot := []byte(`{"mcpServers":{"serena":{"command":"native-mcp-cmd","args":["start"]}}}`)

	t.Run("restore", func(t *testing.T) {
		path := casWriteCfg(t, "cas-restore.json", hubConfig)
		client := newLockingClient(&claudeCode{path: path})
		mutator, ok := AsCASEntryMutator(client)
		if !ok {
			t.Fatal("locking claude client does not expose CAS mutator")
		}
		releaseCause := injectMutationOutcomeReleaseFailure(t, path)
		err := mutator.CASRestoreEntryFromBytes("serena", casURLMatch, nativeSnapshot)
		assertAppliedReleaseUnconfirmed(t, path, err, releaseCause)
		data, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Contains(data, []byte("native-mcp-cmd")) || bytes.Contains(data, []byte(casHubURL)) {
			t.Fatalf("CAS restore bytes = %q err=%v, want applied native snapshot", data, readErr)
		}
	})

	t.Run("remove", func(t *testing.T) {
		path := casWriteCfg(t, "cas-remove.json", hubConfig)
		client := newLockingClient(&claudeCode{path: path})
		mutator, ok := AsCASEntryMutator(client)
		if !ok {
			t.Fatal("locking claude client does not expose CAS mutator")
		}
		releaseCause := injectMutationOutcomeReleaseFailure(t, path)
		err := mutator.CASGuardedRemoveEntry("serena", casURLMatch)
		assertAppliedReleaseUnconfirmed(t, path, err, releaseCause)
		data, readErr := os.ReadFile(path)
		if readErr != nil || bytes.Contains(data, []byte("serena")) {
			t.Fatalf("CAS remove bytes = %q err=%v, want applied removal", data, readErr)
		}
	})
}

func TestDirectCleanupMutationsClassifyAppliedReleaseUnconfirmed(t *testing.T) {
	t.Run("cleanup remove", func(t *testing.T) {
		concrete, mutator, target, _ := newCleanupTestMutator(t, "")
		releaseCause := injectMutationOutcomeReleaseFailure(t, concrete.path)
		receipt, backupPath, err := runCleanupTestOperation(t, mutator, target, func(DirectCleanupCheckpoint) error { return nil })
		if receipt == nil || backupPath == "" {
			t.Fatalf("cleanup receipt=%v backup=%q, want armed receipt and backup", receipt, backupPath)
		}
		assertAppliedReleaseUnconfirmed(t, concrete.path, err, releaseCause)
		data, readErr := os.ReadFile(concrete.path)
		if readErr != nil || bytes.Contains(data, []byte("legacy-go")) || !bytes.Contains(data, []byte("sibling")) {
			t.Fatalf("direct cleanup bytes = %q err=%v, want target removed and sibling retained", data, readErr)
		}
	})

	t.Run("receipt restore", func(t *testing.T) {
		concrete, mutator, target, original := newCleanupTestMutator(t, "")
		receipt, _, cleanupErr := runCleanupTestOperation(t, mutator, target, func(DirectCleanupCheckpoint) error { return nil })
		if cleanupErr != nil || receipt == nil {
			t.Fatalf("cleanup before receipt restore err=%v receipt=%v", cleanupErr, receipt)
		}
		releaseCause := injectMutationOutcomeReleaseFailure(t, concrete.path)
		err := receipt.Restore()
		assertAppliedReleaseUnconfirmed(t, concrete.path, err, releaseCause)
		data, readErr := os.ReadFile(concrete.path)
		if readErr != nil || !bytes.Equal(data, original) {
			t.Fatalf("receipt restore bytes differ: got=%q err=%v want=%q", data, readErr, original)
		}
	})
}
