package gui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestWindowsProductBoundRestartMarker(t *testing.T) {
	root := apitest.HardenedTempDir(t)
	scope, err := api.OpenWindowsProductStateNamespace(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	})
	store := NewHandoffMarkerStore(root, DefaultRestartDeadlines())
	if current, err := store.ReadBound(t.Context(), scope); err != nil || current != nil {
		t.Fatalf("genuine absent bound marker: %v %v", current, err)
	}
	if _, err := os.Stat(store.path); !os.IsNotExist(err) {
		t.Fatal("read materialized the marker")
	}
	old := flock.New(store.path + ".lock")
	if err := old.Lock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = store.ReadBound(ctx, scope)
	cancel()
	if releaseErr := old.Unlock(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bound marker failed to observe legacy range: %v", err)
	}
	if err := store.withRecordLockIn(t.Context(), scope, "read", HandoffMarkerFailureRead, func() error {
		locked, err := old.TryLock()
		if locked {
			_ = old.Unlock()
			return errors.New("legacy marker bypassed bound owner")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	initial, err := store.Begin(HandoffBegin{Generation: "bound-marker-fixture", Route: HandoffRouteSamePort})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.ReadBound(t.Context(), scope)
	if err != nil || record.Phase != HandoffPhaseInProgress || record.Generation != initial.Generation {
		t.Fatalf("nonterminal original marker: %v %v", record, err)
	}
	settled, err := store.Interrupt(initial.Generation, "fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.ReadBound(t.Context(), scope)
	if err != nil || record.Generation != settled.Generation || record.Sequence != settled.Sequence || record.Phase != HandoffPhaseInterrupted {
		t.Fatalf("terminal original marker: %v %v", record, err)
	}
	before, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(filepath.Clean(store.path))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.DELETE, windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ReadBound(t.Context(), scope)
	if closeErr := windows.CloseHandle(h); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil {
		t.Fatal("sharing-obstructed bound marker read was accepted")
	}
	after, err := os.ReadFile(store.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read failure did not preserve original marker", err)
	}
}
