//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

func requireProductStoreOwner(t *testing.T, err error, store *api.RetainedTaskXMLMaterialStore) {
	t.Helper()
	var owner *api.RetainedTaskXMLStoreAcquisitionError
	if err == nil || !errors.As(err, &owner) || owner.RetainedStore() != store {
		t.Fatalf("real CLI receiver lost the original public store error: %v", err)
	}
}

func productStoreLoadFailureFixture(t *testing.T, stateDir string) (*api.RetainedTaskXMLMaterialStore, WindowsProductPairTaskRecovery) {
	t.Helper()
	store, err := api.CreateRetainedTaskXMLStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	body := []byte("<Task>test-owned retained original</Task>")
	ref, err := store.Retain(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	// Remove only this actually retained synthetic leaf after its writer closed.
	// The later real Load must report its original read error with this owner.
	if err := os.Remove(filepath.Join(store.RootPath(), ref)); err != nil {
		t.Fatal(err)
	}
	return store, WindowsProductPairTaskRecovery{StoreRoot: store.RootPath(), Backups: []WindowsProductPairTaskBackup{{
		TaskName: `\mcp-local-hub-custody`, Ref: ref, SHA256: fmt.Sprintf("%x", sha256.Sum256(body)),
	}}}
}

// API's uncertain-close fixture proves private guard custody. These real CLI
// compositions prove forwarding of actual public store-operation errors, without
// a fake RetainedStore implementation or a cross-package native fault hook.
func TestWindowsProductStoreErrorReceiversKeepOriginalOwner(t *testing.T) {
	t.Run("constructor begin and selected cleanup", func(t *testing.T) {
		stateDir := withTempStateDir(t)
		store, err := api.CreateRetainedTaskXMLStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(store.RootPath(), "test-owned-unrecognized-leaf")
		if err := os.WriteFile(foreign, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		beginErr := errors.New("test-owned original begin failure")
		txn, err := newWindowsProductTxnWith(windowsProductTxnRequest{JournalAllowance: productJournalFixtureAllowance(),
			Context: context.Background(), StateDir: stateDir,
			ProductPath:          filepath.Join(stateDir, "mcphub.exe"),
			LegacyWindowlessPath: filepath.Join(stateDir, "mcphub-windowless.exe"),
			StagedProduct:        filepath.Join(stateDir, "candidate.exe"), Mode: WindowsProductPairModeUpgrade,
		}, func(string) (*windowsProductPairTaskStore, error) {
			return store, nil
		}, func(_ context.Context, _, _, _ string, actual scheduler.RetainedTaskXMLStore) (*scheduler.OwnedEntrypointTaskTxn, error) {
			if actual != store {
				t.Fatal("constructor substituted the original store before begin")
			}
			return nil, beginErr
		})
		if txn != nil || !errors.Is(err, beginErr) {
			t.Fatal("constructor lost its original begin failure")
		}
		requireProductStoreOwner(t, err, store)
		if got, err := os.ReadFile(foreign); err != nil || string(got) != "preserve" {
			t.Fatal("constructor cleanup changed unadopted material")
		}
	})
	t.Run("product recovery load", func(t *testing.T) {
		f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
		store, recovery := productStoreLoadFailureFixture(t, f.stateDir)
		f.journal.Tasks = recovery
		if err := persistWindowsProductRecoveryJournalV3(productJournalFixtureContext(), productJournalFixtureAllowance(), filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3), f.journal, api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
			t.Fatal(err)
		}
		req := f.request(&recoveryTaskTxnFake{})
		var original error
		req.Deps.ReopenTasks = func(ctx context.Context, actual WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
			if actual.StoreRoot != recovery.StoreRoot || len(actual.Backups) != 1 || actual.Backups[0] != recovery.Backups[0] {
				t.Fatal("product recovery substituted the actual retained material")
			}
			_, original = store.Load(ctx, actual.Backups[0].Ref)
			return nil, original
		}
		settled, err := reconcileWindowsProductRecovery(req)
		if settled != nil || original == nil || !errors.Is(err, original) || !errors.Is(err, ErrWindowsProductRecoveryIdentity) {
			t.Fatal("product recovery erased the actual retained-load error")
		}
		requireProductStoreOwner(t, err, store)
	})
	t.Run("pair recovery load", func(t *testing.T) {
		f := newRecoveryFixture(t)
		store, recovery := productStoreLoadFailureFixture(t, f.stateDir)
		f.journal.Tasks = recovery
		f.persistJournal(t)
		var order []string
		req := f.request(&recoveryTaskTxnFake{}, &order, nil)
		var original error
		req.Deps.ReopenTasks = func(ctx context.Context, actual WindowsProductPairTaskRecovery) (WindowsProductPairTaskTxn, error) {
			if actual.StoreRoot != recovery.StoreRoot || len(actual.Backups) != 1 || actual.Backups[0] != recovery.Backups[0] {
				t.Fatal("pair recovery substituted the actual retained material")
			}
			_, original = store.Load(ctx, actual.Backups[0].Ref)
			return nil, original
		}
		settled, err := reconcileWindowsProductPairRecovery(req)
		if settled != nil || original == nil || !errors.Is(err, original) || !errors.Is(err, ErrWindowsProductPairRecoveryIdentity) {
			t.Fatal("pair recovery erased the actual retained-load error")
		}
		requireProductStoreOwner(t, err, store)
	})
	t.Run("product selected discard", func(t *testing.T) {
		f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySettledRollback)
		store, err := api.CreateRetainedTaskXMLStore(f.stateDir)
		if err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(store.RootPath(), "test-owned-unrecognized-leaf")
		if err := os.WriteFile(foreign, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.journal.Tasks = WindowsProductPairTaskRecovery{StoreRoot: store.RootPath(), Backups: []WindowsProductPairTaskBackup{}}
		if err := persistWindowsProductRecoveryJournalV3(productJournalFixtureContext(), productJournalFixtureAllowance(), filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3), f.journal, api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
			t.Fatal(err)
		}
		req := f.request(&recoveryTaskTxnFake{})
		var original error
		req.Deps.DiscardTaskStore = func(actual WindowsProductPairTaskRecovery) error {
			if actual.StoreRoot != store.RootPath() {
				t.Fatal("product discard substituted its retained material")
			}
			original = store.Close()
			return original
		}
		settled, err := reconcileWindowsProductRecovery(req)
		if settled != nil || original == nil || !errors.Is(err, original) || !errors.Is(err, ErrWindowsProductPairRecoveryCleanup) {
			t.Fatal("product selected discard erased the actual store error")
		}
		requireProductStoreOwner(t, err, store)
		if got, err := os.ReadFile(foreign); err != nil || string(got) != "preserve" {
			t.Fatal("product discard changed unadopted material")
		}
	})
	t.Run("pair selected discard", func(t *testing.T) {
		f := newRecoveryFixture(t)
		f.journal.Phase = WindowsProductPairRecoverySettledRollback
		f.persistJournal(t)
		store, err := api.CreateRetainedTaskXMLStore(f.stateDir)
		if err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(store.RootPath(), "test-owned-unrecognized-leaf")
		if err := os.WriteFile(foreign, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.journal.Tasks = WindowsProductPairTaskRecovery{StoreRoot: store.RootPath(), Backups: []WindowsProductPairTaskBackup{}}
		f.persistJournal(t)
		var order []string
		req := f.request(&recoveryTaskTxnFake{}, &order, nil)
		var original error
		req.Deps.DiscardTaskStore = func(actual WindowsProductPairTaskRecovery) error {
			if actual.StoreRoot != store.RootPath() {
				t.Fatal("pair discard substituted its retained material")
			}
			original = store.Close()
			return original
		}
		settled, err := reconcileWindowsProductPairRecovery(req)
		if settled != nil || original == nil || !errors.Is(err, original) || !errors.Is(err, ErrWindowsProductPairRecoveryCleanup) {
			t.Fatal("pair selected discard erased the actual store error")
		}
		requireProductStoreOwner(t, err, store)
		if got, err := os.ReadFile(foreign); err != nil || string(got) != "preserve" {
			t.Fatal("pair discard changed unadopted material")
		}
	})
}

func TestCanonicalEntryTaskReopenPreservesRetainedXMLOnInvalidLegacyProjection(t *testing.T) {
	ctx := t.Context()
	stateDir := apitest.HardenedTempDir(t)
	store, err := api.CreateRetainedTaskXMLStore(stateDir)
	if store != nil {
		t.Cleanup(func() {
			if closeErr := store.Close(); closeErr != nil {
				t.Errorf("discard test-owned retained XML fixture: %v", closeErr)
			}
		})
	}
	if err != nil || store == nil {
		t.Fatalf("create test-owned retained XML fixture: %v", err)
	}
	xml := []byte("<Task><Description>synthetic original rollback XML</Description></Task>")
	ref, err := store.Retain(ctx, xml)
	if err != nil {
		t.Fatalf("retain test-owned task XML: %v", err)
	}
	root := store.RootPath()
	leaf := filepath.Join(root, ref)
	recovery := WindowsProductPairTaskRecovery{
		StoreRoot: root,
		Backups: []WindowsProductPairTaskBackup{{
			TaskName: "\\mcp-local-hub-task-reopen-preservation-fixture",
			Ref:      ref,
			SHA256:   fmt.Sprintf("%x", sha256.Sum256(xml)),
		}},
	}

	// Close each observation handle before reopen so the test cannot prevent deletion.
	snapshot := func(path, label string, directory bool) os.FileInfo {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
			t.Fatalf("%s is not the expected ordinary fixture object: %v", label, err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s fixture object: %v", label, err)
		}
		captured, statErr := file.Stat()
		if err := errors.Join(statErr, file.Close()); err != nil {
			t.Fatalf("receive and close %s fixture observation: %v", label, err)
		}
		return captured
	}
	rootBefore := snapshot(root, "retained rollback root before reopen", true)
	leafBefore := snapshot(leaf, "retained rollback XML before reopen", false)
	if got, err := os.ReadFile(leaf); err != nil || !bytes.Equal(got, xml) {
		t.Fatalf("retained rollback XML preflight failed: %v", err)
	}

	canonical := filepath.Join(stateDir, "mcphub.exe")
	product := filepath.Join(stateDir, "mcphub-product", "mcphub.exe")
	invalidLegacy := filepath.Join(stateDir, "unqualified", "mcphub-windowless.exe")
	lockPath := filepath.Join(stateDir, "windows-product-entrypoint.lock")
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scheduler lock fixture was not initially vacant: %v", err)
	}
	if err := os.Mkdir(filepath.Dir(product), 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalBytes := []byte("synthetic canonical entry bytes")
	productBytes := []byte("synthetic Product bytes")
	if err := os.WriteFile(canonical, canonicalBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(product, productBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalBefore := snapshot(canonical, "canonical entry before reopen", false)
	productBefore := snapshot(product, "Product before reopen", false)

	txn, err := reopenCanonicalEntryTasks(ctx, stateDir, invalidLegacy, recovery, canonical, product)
	if txn != nil {
		t.Fatalf("invalid legacy projection returned a task transaction: error=%v close=%v", err, txn.Close())
	}
	const wantError = "entrypoint task transaction: invalid explicit legacy helper projection"
	if err == nil || err.Error() != wantError {
		t.Fatalf("task reopen error=%v, want %q", err, wantError)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid projection reached scheduler lock acquisition: %v", err)
	}
	for _, fixture := range []struct {
		path, label string
		before      os.FileInfo
		want        []byte
	}{
		{canonical, "canonical entry", canonicalBefore, canonicalBytes},
		{product, "Product", productBefore, productBytes},
	} {
		after := snapshot(fixture.path, fixture.label, false)
		got, err := os.ReadFile(fixture.path)
		if err != nil || !os.SameFile(fixture.before, after) || !bytes.Equal(got, fixture.want) {
			t.Fatalf("failed task reopen changed %s: %v", fixture.label, err)
		}
	}

	rootAfter := snapshot(root, "retained rollback root after reopen refusal", true)
	leafAfter := snapshot(leaf, "retained rollback XML after reopen refusal", false)
	if !os.SameFile(rootBefore, rootAfter) || !os.SameFile(leafBefore, leafAfter) {
		t.Fatal("failed task reopen replaced the original rollback root or XML")
	}
	if got, err := os.ReadFile(leaf); err != nil || !bytes.Equal(got, xml) {
		t.Fatalf("failed task reopen changed the original rollback XML bytes: %v", err)
	}
}
