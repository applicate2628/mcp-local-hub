package gui

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func TestGUIOwnerRecord_ReadsLegacyButNewAcquireWritesV2(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(path, []byte("42 9125\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := ReadGUIOwnerRecord(path)
	if err != nil {
		t.Fatalf("read legacy record: %v", err)
	}
	if !legacy.Legacy || legacy.Version != 1 || legacy.PID != 42 || legacy.Port != 9125 {
		t.Fatalf("legacy record = %#v", legacy)
	}

	lock, err := acquireSingleInstanceAt(path, 9126)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()
	record, err := ReadGUIOwnerRecord(path)
	if err != nil {
		t.Fatalf("read v2 record: %v", err)
	}
	if record.Legacy || record.Version != guiOwnerRecordVersion || record.State != guiOwnerStateActive || record.PID != os.Getpid() || record.Port != 9126 || record.StartTime.IsZero() || record.Generation == "" {
		t.Fatalf("v2 record = %#v", record)
	}
	if _, err := NewGUIOwnerLifecycle(path); err != nil {
		t.Fatalf("open current owner lifecycle: %v", err)
	}
}

func TestGUIOwnerLifecycle_CASNeverOverwritesSuccessor(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "gui.pidport")
	first, err := writeCurrentGUIOwnerRecord(path, os.Getpid(), 9125)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newGUIOwnerLifecycle(path, first)
	successor := first
	successor.Generation = "successor-generation"
	if err := writeGUIOwnerRecord(path, successor); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.TerminalSettle(); err != nil {
		t.Fatalf("terminal settle: %v", err)
	}
	got, err := ReadGUIOwnerRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != successor.Generation || got.State != guiOwnerStateActive || got.Port != successor.Port {
		t.Fatalf("CAS overwrote successor: %#v", got)
	}
}

func TestAcquireSingleInstance_BoundLifecycleAvoidsPostPublishOpenFailure(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "gui.pidport")
	lock, err := acquireSingleInstanceAt(path, 9125)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a copy solely to restore the intentionally-corrupted fixture before
	// terminal settlement. The production startup path now carries the bound
	// lifecycle and performs no post-publication constructor read.
	published, err := ReadGUIOwnerRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if lock.OwnerLifecycle() == nil {
		t.Fatal("acquire returned no bound lifecycle")
	}
	if err := os.WriteFile(path, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGUIOwnerLifecycle(path); err == nil {
		t.Fatal("injected post-publication read failure unexpectedly opened lifecycle")
	}
	// The acquisition-bound object remains available; after restoring the
	// exact generation it terminally settles before the flock is released.
	if err := writeGUIOwnerRecord(path, published); err != nil {
		t.Fatal(err)
	}
	if err := lock.OwnerLifecycle().TerminalSettle(); err != nil {
		t.Fatalf("terminal settle: %v", err)
	}
	lock.Release()
	settled, err := ReadGUIOwnerRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != guiOwnerStateTombstone {
		t.Fatalf("record after release = %#v, want tombstone (never active)", settled)
	}
	assertSingleInstanceFlockAvailable(t, path)
}

func TestGUIOwnerLifecycle_HandoffRollbackAndTerminalSettlement(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	path := filepath.Join(dir, "gui.pidport")
	record, err := writeCurrentGUIOwnerRecord(path, os.Getpid(), 9125)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newGUIOwnerLifecycle(path, record)
	if err := lifecycle.BeginHandoff("handoff-1", "restart-1", 4321, 9126); err != nil {
		t.Fatalf("begin handoff: %v", err)
	}
	handoff, err := ReadGUIOwnerRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.State != guiOwnerStateHandoff || handoff.HandoffID != "handoff-1" || handoff.HandoffGeneration != "restart-1" || handoff.HandoffTargetPID != 4321 || handoff.HandoffTargetPort != 9126 {
		t.Fatalf("handoff record = %#v", handoff)
	}
	if err := lifecycle.RestoreActive("handoff-1", "restart-1"); err != nil {
		t.Fatalf("restore active: %v", err)
	}
	if err := lifecycle.TerminalSettle(); err != nil {
		t.Fatalf("terminal settle: %v", err)
	}
	tombstone, err := ReadGUIOwnerRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.State != guiOwnerStateTombstone || tombstone.Port != 0 || tombstone.Generation != record.Generation {
		t.Fatalf("tombstone = %#v", tombstone)
	}
}
