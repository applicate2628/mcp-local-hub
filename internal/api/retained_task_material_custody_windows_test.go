//go:build windows

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedTaskXMLMaterialEarlyErrorsAndFinalCloseKeepOwner(t *testing.T) {
	store, err := CreateRetainedTaskXMLStore(retainedTaskMaterialFixtureStateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	check := func(err error) *RetainedTaskXMLStoreAcquisitionError {
		t.Helper()
		var owner *RetainedTaskXMLStoreAcquisitionError
		if err == nil || !errors.As(err, &owner) || owner.RetainedStore() != store {
			t.Fatalf("operation error lost its original store: %v", err)
		}
		return owner
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ref, err := store.Retain(canceled, []byte("unpublished"))
	if ref != "" || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled retention lost its cause or published a ref")
	}
	check(err)
	_, err = store.Load(nil, "unavailable")
	check(err)
	_, err = store.Load(context.Background(), "unavailable")
	check(err)

	// A real selected-discard failure, distinct from the uncertain-close
	// receiving fixture. The TempDir owner retains these synthetic bytes.
	foreign := filepath.Join(store.RootPath(), "test-owned-unrecognized-leaf")
	if err := os.WriteFile(foreign, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := store.Close()
	owner := check(first)
	var duplicate *RetainedTaskXMLStoreAcquisitionError
	if errors.As(owner.Unwrap(), &duplicate) {
		t.Fatal("one Close operation received duplicate owner wrappers")
	}
	if again := store.Close(); again != first {
		t.Fatal("Close did not return its final cached owner-bearing outcome")
	}
	ref, err = store.Retain(context.Background(), []byte("after-close"))
	if ref != "" {
		t.Fatal("closed retention published a ref")
	}
	check(err)
	_, err = store.Load(context.Background(), "unavailable")
	check(err)
	if again := store.Close(); again != first {
		t.Fatal("later instance errors replaced the original Close result")
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "preserve" {
		t.Fatal("failed selected discard changed unadopted material")
	}
}
