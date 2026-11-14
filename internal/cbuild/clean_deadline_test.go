package cbuild

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPurgeRefusesExpiredBudgetAfterBindingWait(t *testing.T) {
	source := t.TempDir()
	buildDir := writeCleanCache(t, source, "dev")
	b := &builder{configured: make(map[configuredCacheKey]string)}
	if _, err := b.settleConfiguredCache(source, "dev", "-- Build files have been written to: "+buildDir); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"working_dir": source, "preset": "dev", "purge_build_dir": true, "timeout_sec": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep validation pending after the project-operation lock is acquired.
	b.configuredMu.Lock()
	locked := true
	defer func() {
		if locked {
			b.configuredMu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := b.cmakeClean().Call(context.Background(), raw)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.operationsMu.Lock()
		acquired := false
		for _, op := range b.operations {
			acquired = acquired || len(op.token) == 1
		}
		b.operationsMu.Unlock()
		if acquired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("clean never acquired the project-operation lock")
		}
		time.Sleep(time.Millisecond)
	}
	// The per-call deadline starts before lock acquisition. The caller has
	// no deadline, so consulting only the caller would allow the deletion.
	time.Sleep(1100 * time.Millisecond)
	b.configuredMu.Unlock()
	locked = false
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired purge returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clean did not settle after releasing validation")
	}
	if _, err := os.Stat(filepath.Join(buildDir, "keep.txt")); err != nil {
		t.Fatalf("expired purge touched the build tree: %v", err)
	}
	b.operationsMu.Lock()
	defer b.operationsMu.Unlock()
	if len(b.operations) != 0 {
		t.Fatal("expired clean leaked its project-operation lock")
	}
}
