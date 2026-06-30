package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
)

func TestReapStaleTransientsReadsUnderSupervisorStateFlock(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	if err := api.WriteStateFileBytesAtomic(statePath, []byte(`{"version":`)); err != nil {
		t.Fatal(err)
	}

	lock := flock.New(statePath + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock supervisor state file: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ReapStaleTransients(context.Background(), ReaperDeps{
			StateDir: stateDir,
			PIDAlive: func(int) bool {
				return false
			},
			SettleDuration: 10 * time.Millisecond,
		})
		done <- err
	}()

	select {
	case err := <-done:
		_ = lock.Unlock()
		t.Fatalf("ReapStaleTransients returned before the flock holder published a valid file; err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	valid := []byte(`{"version":1,"transient_pids":[{"pid":1234,"kind":"workspace-weekly-refresh","started_at":"2026-06-30T00:00:00Z"}]}`)
	if err := api.WriteStateFileBytesLockHeld(statePath, valid); err != nil {
		_ = lock.Unlock()
		t.Fatalf("publish valid supervisor state under held flock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock supervisor state file: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("ReapStaleTransients after flock release: %v", err)
	}
	got, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read supervisor state: %v", err)
	}
	if len(got.TransientPIDs) != 0 {
		t.Fatalf("dead transient PID was not cleared: %+v", got.TransientPIDs)
	}
}
