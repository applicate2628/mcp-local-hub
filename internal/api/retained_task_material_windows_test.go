//go:build windows

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func retainedTaskMaterialFixtureStateDir(t *testing.T) string {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(root))
	return root
}

// These physical-owner checks move from the V3 CLI to its shared API owner.
// Their original failure/retention oracles are retained, not duplicated.
func TestRetainedTaskXMLMaterialExactReadbackAndCleanup(t *testing.T) {
	stateDir := retainedTaskMaterialFixtureStateDir(t)
	store, err := CreateRetainedTaskXMLStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	root := store.root
	want := []byte("<Task><Actions><Exec><Command>exact</Command></Exec></Actions></Task>\r\n")
	ref, err := store.Retain(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background(), ref)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("load bytes=%q err=%v", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback store survived Close: %v", err)
	}
}

func TestRetainedTaskXMLMaterialFailureKeepsMaterializationCustody(t *testing.T) {
	for _, mode := range []string{"write-after-materialization", "read-refusal", "read-mismatch", "cancel-after-write"} {
		t.Run(mode, func(t *testing.T) {
			stateDir := retainedTaskMaterialFixtureStateDir(t)
			io := productionRetainedTaskMaterialIO()
			realWrite, realRead := io.write, io.read
			xml := []byte("<Task><Actions><Exec><Command>synthetic</Command></Exec></Actions></Task>")
			digest := fmt.Sprintf("%x", sha256.Sum256(xml))
			expectedRef := "task-" + digest + ".xml"
			cause := errors.New("synthetic " + mode + " refusal")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writes := 0
			io.write = func(path string, body []byte) error {
				if err := realWrite(path, body); err != nil {
					return err
				}
				writes++
				if mode == "write-after-materialization" {
					return cause
				}
				if mode == "cancel-after-write" {
					cancel()
				}
				return nil
			}
			switch mode {
			case "read-refusal":
				io.read = func(context.Context, string, []string, string) ([]byte, error) { return nil, cause }
			case "read-mismatch":
				io.read = func(ctx context.Context, root string, parts []string, hash string) ([]byte, error) {
					if _, err := realRead(ctx, root, parts, hash); err != nil {
						return nil, err
					}
					return []byte("different retained bytes"), nil
				}
			}
			store, err := createRetainedTaskXMLStoreWithIO(stateDir, io)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ref, err := store.Retain(ctx, xml)
			if ref != "" || err == nil || writes != 1 {
				t.Fatalf("failed retention publication/write: ref=%q err=%v writes=%d", ref, err, writes)
			}
			switch mode {
			case "write-after-materialization", "read-refusal":
				if !errors.Is(err, cause) {
					t.Fatalf("retention lost actual injected cause: %v", err)
				}
			case "cancel-after-write":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("post-publication cancellation lost: %v", err)
				}
			case "read-mismatch":
				if !strings.Contains(err.Error(), "retained task XML readback differs") {
					t.Fatalf("equality refusal was not distinct: %v", err)
				}
			}
			realBody, readErr := ReadStateFileBeneathRootNoFollow(context.Background(), store.root, []string{expectedRef}, digest)
			if readErr != nil || !bytes.Equal(realBody, xml) {
				t.Fatalf("precondition: actual canonical writer did not publish private XML: %v", readErr)
			}
			if info, err := os.Lstat(filepath.Join(store.root, expectedRef+".lock")); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("precondition: actual canonical writer lock leaf missing/invalid: %v", err)
			}
			if _, ok := store.hashes[expectedRef]; ok {
				t.Fatal("failed retention entered the verified-ref index")
			}
			if _, err := store.Load(context.Background(), expectedRef); err == nil {
				t.Fatal("unverified ref became Load-readable")
			}
			root := store.root
			if err := store.Close(); err != nil {
				t.Fatalf("actual XML/lock materialization lost cleanup custody: %v", err)
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned materialized root remains: %v", err)
			}
		})
	}
}

func TestRetainedTaskXMLMaterialFailedCloseRemainsUnconfirmed(t *testing.T) {
	stateDir := retainedTaskMaterialFixtureStateDir(t)
	io := productionRetainedTaskMaterialIO()
	realRemove := io.remove
	xml := []byte("<Task>synthetic cleanup</Task>")
	ref := "task-" + fmt.Sprintf("%x", sha256.Sum256(xml)) + ".xml"
	cause := errors.New("synthetic owned XML removal refusal")
	io.remove = func(path string) error {
		if filepath.Base(path) == ref {
			return cause
		}
		return realRemove(path)
	}
	store, err := createRetainedTaskXMLStoreWithIO(stateDir, io)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Retain(context.Background(), xml); err != nil || got != ref {
		t.Fatalf("precondition: verified retention failed: %v", err)
	}
	if first := store.Close(); !errors.Is(first, cause) {
		t.Fatalf("initial close lost owned removal refusal: %v", first)
	}
	if second := store.Close(); !errors.Is(second, cause) {
		t.Fatalf("second Close fabricated cleanup or lost refusal: %v", second)
	}
	if _, err := os.Lstat(filepath.Join(store.root, ref)); err != nil {
		t.Fatalf("failed close erased the retained XML: %v", err)
	}
	if _, err := store.Retain(context.Background(), xml); err == nil {
		t.Fatal("failed Close reopened writes")
	}
}

func TestRetainedTaskXMLMaterialBorrowedRootIsPreserved(t *testing.T) {
	root := t.TempDir()
	store := &RetainedTaskXMLMaterialStore{root: root, hashes: make(map[string]string), io: productionRetainedTaskMaterialIO()}
	if err := store.Close(); err == nil {
		t.Fatal("borrowed root was accepted as cleanup-owned")
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("borrowed root was removed: %v", err)
	}
}

func TestRetainedTaskXMLMaterialConflictingRefLeafIsPreserved(t *testing.T) {
	store, err := CreateRetainedTaskXMLStore(retainedTaskMaterialFixtureStateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	xml := []byte("<Task>new attempted bytes</Task>")
	ref := "task-" + fmt.Sprintf("%x", sha256.Sum256(xml)) + ".xml"
	foreign := []byte("unadopted conflicting leaf")
	path := filepath.Join(store.root, ref)
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	gotRef, retainErr := store.Retain(context.Background(), xml)
	if gotRef != "" || retainErr == nil {
		t.Errorf("conflicting pre-existing object was overwritten/admitted: ref=%q err=%v", gotRef, retainErr)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, foreign) {
		t.Errorf("unadopted ref-shaped object changed: %v", err)
	}
	if err := store.Close(); err == nil {
		t.Error("unknown leaf was erased or cleanup falsely confirmed")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, foreign) {
		t.Errorf("close erased unadopted object: %v", err)
	}
}

func TestRetainedTaskXMLMaterialRejectsIncompleteIOBeforeCreation(t *testing.T) {
	for _, missing := range []string{"write", "read", "remove"} {
		t.Run(missing, func(t *testing.T) {
			stateDir := t.TempDir()
			io := productionRetainedTaskMaterialIO()
			switch missing {
			case "write":
				io.write = nil
			case "read":
				io.read = nil
			case "remove":
				io.remove = nil
			}
			if store, err := createRetainedTaskXMLStoreWithIO(stateDir, io); store != nil || err == nil {
				t.Fatalf("incomplete dependencies were admitted: store=%v err=%v", store, err)
			}
			entries, err := os.ReadDir(stateDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected constructor created a root: entries=%d err=%v", len(entries), err)
			}
		})
	}
}

func TestRetainedTaskXMLMaterialOriginalChildIdentityRejectsReplacement(t *testing.T) {
	store, err := CreateRetainedTaskXMLStore(retainedTaskMaterialFixtureStateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if !store.identityReceived || sameRetainedTaskDirectory(store.parentIdentity, store.rootIdentity) {
		t.Fatal("Task child identity was replaced with namespace/parent identity")
	}
	want := []byte("<Task>original retained bytes</Task>")
	ref, err := store.Retain(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	// This succeeds only after the constructor and Retain released their native
	// no-delete handles. It is an owned test replacement, never a production retry.
	original := store.root + "-original"
	if err := os.Rename(store.root, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.root, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(store.root, ref)
	if err := os.WriteFile(foreign, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), ref); err == nil {
		t.Fatal("equal bytes at a substituted root became original material")
	}
	if _, err := store.Retain(context.Background(), want); err == nil {
		t.Fatal("retention entered a substituted root")
	}
	if err := store.Close(); err == nil {
		t.Fatal("discard entered a substituted root")
	}
	for _, path := range []string{foreign, filepath.Join(original, ref)} {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("actual original or foreign bytes changed: %v", err)
		}
	}
}

func TestRetainedTaskXMLMaterialReopenCopiesRefsAndPreservesPossibleAbsence(t *testing.T) {
	stateDir := retainedTaskMaterialFixtureStateDir(t)
	store, err := CreateRetainedTaskXMLStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("<Task>recovered original</Task>")
	ref, err := store.Retain(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{ref: fmt.Sprintf("%x", sha256.Sum256(want))}
	for _, unsafe := range []string{"", ".", "..", "nested/ref", `nested\ref`, "ref:stream"} {
		if partial, err := ReopenRetainedTaskXMLStore(stateDir, store.root, map[string]string{unsafe: refs[ref]}, true); err == nil || partial != nil {
			t.Fatal("non-leaf recovery ref entered physical acquisition")
		}
	}
	recovered, err := ReopenRetainedTaskXMLStore(stateDir, store.root, refs, true)
	if err != nil {
		t.Fatal(err)
	}
	refs[ref] = "changed caller map"
	got, err := recovered.Load(context.Background(), ref)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("recovered exact material/copy: %v", err)
	}
	if _, err := recovered.Retain(context.Background(), []byte("unadopted bytes")); err == nil {
		t.Fatal("recovered store admitted a new opaque ref")
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	partial, missingErr := ReopenRetainedTaskXMLStore(stateDir, store.root, nil, true)
	var acquisition *RetainedTaskXMLStoreAcquisitionError
	if missingErr == nil || partial == nil || !errors.As(missingErr, &acquisition) || acquisition.RetainedStore() != partial {
		t.Fatal("required missing root lost its error/actual partial owner")
	}
	absent, err := ReopenRetainedTaskXMLStore(stateDir, store.root, nil, false)
	if err != nil || absent == nil || !absent.absent || absent.identityReceived {
		t.Fatalf("possibly absent recovery fabricated an identity: %v", err)
	}
	if err := os.Mkdir(store.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := absent.Close(); err == nil {
		t.Fatal("possibly absent recovery adopted a new directory for deletion")
	}
	if info, err := os.Lstat(store.root); err != nil || !info.IsDir() {
		t.Fatal("new foreign root was removed")
	}
}

func TestRetainedTaskXMLMaterialUnconfirmedNativeClosePreservesMaterial(t *testing.T) {
	store, err := CreateRetainedTaskXMLStore(retainedTaskMaterialFixtureStateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("<Task>must remain after close refusal</Task>")
	ref, err := store.Retain(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := store.openOriginalRoot(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.close(); err != nil {
		t.Fatal(err)
	}
	// Synthetic receiving degradation after actual successful native closes.
	// This is a refusal test, not evidence of an injected native Close failure.
	cause := errors.New("synthetic unconfirmed original child close")
	guard.root.close.Closed, guard.root.close.Error = false, cause
	if err := store.receiveNativeClose(guard); !errors.Is(err, cause) || len(store.unsettled) != 1 {
		t.Fatal("original native close outcome/owner was erased")
	}
	_, loadErr := store.Load(context.Background(), ref)
	if !errors.Is(loadErr, cause) {
		t.Fatal("later reopen hid an unconfirmed close")
	}
	var loadOwner *RetainedTaskXMLStoreAcquisitionError
	if !errors.As(loadErr, &loadOwner) || loadOwner.RetainedStore() != store {
		t.Error("Load returned error lost the original store owner")
	}
	closeErr := store.Close()
	if !errors.Is(closeErr, cause) {
		t.Fatal("selected discard hid the original native failure")
	}
	var closeOwner *RetainedTaskXMLStoreAcquisitionError
	if !errors.As(closeErr, &closeOwner) || closeOwner.RetainedStore() != store {
		t.Error("Close returned error lost the original store owner")
	}
	if len(store.unsettled) != 1 || store.unsettled[0] != guard || !errors.Is(store.nativeCloseErr, cause) {
		t.Fatal("returned owner lost the original guard or close cause")
	}
	if got, err := os.ReadFile(filepath.Join(store.root, ref)); err != nil || !bytes.Equal(got, want) {
		t.Fatal("native close refusal erased retained material")
	}
}
