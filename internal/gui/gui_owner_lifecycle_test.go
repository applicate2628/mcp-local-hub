package gui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestGUIProvisionalLeaseExcludesAbsentQuit(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", dir)
	path := filepath.Join(dir, "gui.pidport")
	provisional, err := AcquireSingleInstanceLockOnlyAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := provisional.ReleaseErr(); err != nil {
			t.Error(err)
		}
	}()
	called := false
	absent, err := WithAbsentGUIOwner(context.Background(), path, func() error { called = true; return nil })
	if err != nil || absent || called {
		t.Fatalf("absent Quit crossed original lease: absent=%t called=%t err=%v", absent, called, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("provisional lease published active metadata: %v", err)
	}
}

func TestGUIStoppedBootstrapDoesNotPublishAndExplicitResumeIsConditional(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", dir)
	path := filepath.Join(dir, "gui.pidport")
	lease, err := acquireSingleInstanceAt(path, 9125)
	if err != nil {
		t.Fatal(err)
	}
	original, err := ReadGUIOwnerRecord(path)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	stopped, stopErr := api.NewAPI().RecordGUIStop(context.Background(), original.Generation, "tray-quit")
	if stopErr != nil || !stopped.Applied {
		lease.Release()
		t.Fatalf("stop=%#v err=%v", stopped, stopErr)
	}
	if err := lease.OwnerLifecycle().BeginHandoff("h", "g", 12345, 9126); !errors.Is(err, api.ErrGUIStoppedByUser) {
		lease.Release()
		t.Fatalf("restart defeated Quit: %v", err)
	}
	if err := lease.OwnerLifecycle().TerminalSettle(); err != nil {
		lease.Release()
		t.Fatal(err)
	}
	if err := lease.ReleaseErr(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	late, err := acquireSingleInstanceAt(path, 9125)
	if late != nil {
		late.Release()
		t.Fatal("delayed automatic launch acquired active owner")
	}
	if !errors.Is(err, api.ErrGUIStoppedByUser) {
		t.Fatalf("delayed launch = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("delayed launch published: %v", err)
	}
	resumed, err := AcquireSingleInstanceAt(path, 9125, SingleInstanceAcquireOptions{GUIResumeControl: &stopped.Control})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Release()
	defer func() {
		if err := resumed.OwnerLifecycle().TerminalSettle(); err != nil {
			t.Error(err)
		}
	}()
	if err := api.NewAPI().WithGUIOwnerControl(func(c api.GUIOwnerControl) error {
		if c.RunDesired != api.GUIRunRunning || c.StopCause != "" {
			t.Fatalf("resume policy=%#v", c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

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
	if port, terminal, err := legacy.ReservationEndpoint(); err != nil || terminal || port != legacy.Port {
		t.Fatal("legacy positive endpoint classification changed")
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
	if port, terminal, err := record.ReservationEndpoint(); err != nil || terminal || port != record.Port {
		t.Fatal("active positive endpoint was lost")
	}
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
	if _, terminal, err := handoff.ReservationEndpoint(); err == nil || terminal {
		t.Fatal("handoff was treated as an absence variant")
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
	if port, terminal, err := tombstone.ReservationEndpoint(); err != nil || !terminal || port != 0 {
		t.Fatal("original terminal tombstone was not recognized")
	}
}

func TestGUIOwnerRecordReservationEndpointRejectsUnsettledVariants(t *testing.T) {
	for _, record := range []GUIOwnerRecord{
		{},
		{Version: 2, State: guiOwnerStateActive, Port: 0},
		{Version: 1, State: guiOwnerStateActive, Legacy: true, Port: 0},
		{Version: 2, State: guiOwnerStateTombstone, Port: 9125},
		{Version: 2, State: "unknown", Port: 9125},
	} {
		if port, terminal, err := record.ReservationEndpoint(); err == nil || terminal || port != 0 {
			t.Fatal("unsettled record yielded a reservable endpoint classification")
		}
	}
}
