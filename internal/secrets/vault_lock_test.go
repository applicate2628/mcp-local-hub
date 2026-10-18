package secrets

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWithVaultLock_ConcurrentSetNoLostOrCorruptEntries simulates many
// concurrent SecretsSet-style read-modify-write operations against one vault
// file. Each goroutine, under WithVaultLock, does the same OpenVault → Set →
// save sequence the api/cli wrappers do, writing its OWN unique key. With the
// flock serializing the RMW, every key must survive: no lost updates (a
// later writer must observe earlier writers' keys, because it re-opens the
// vault under the lock) and no corrupt/torn encrypted file. Run with -race.
//
// Without WithVaultLock, two goroutines could each OpenVault (decrypt the
// same older map), each Set a different key, and each save the whole map —
// the second save silently drops the first's key (the LWW loss documented in
// work-items/bugs/a3a-vault-concurrent-edit-lww.md).
func TestWithVaultLock_ConcurrentSetNoLostOrCorruptEntries(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	if err := InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("KEY_%03d", i)
			val := fmt.Sprintf("val-%03d", i)
			err := WithVaultLock(vaultPath, func() error {
				v, err := OpenVault(keyPath, vaultPath)
				if err != nil {
					return err
				}
				return v.Set(key, val)
			})
			if err != nil {
				t.Errorf("set %s: %v", key, err)
			}
		}(i)
	}
	wg.Wait()

	// The vault must decrypt cleanly (no corruption) and contain ALL n keys
	// with their correct values (no lost updates).
	v, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("final OpenVault (corrupt vault?): %v", err)
	}
	keys := v.List()
	if len(keys) != n {
		t.Fatalf("final key count = %d, want %d (lost updates) — keys: %v", len(keys), n, keys)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("KEY_%03d", i)
		want := fmt.Sprintf("val-%03d", i)
		got, err := v.Get(key)
		if err != nil {
			t.Errorf("missing key %s: %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("key %s = %q, want %q", key, got, want)
		}
	}
}

func TestTryWithVaultLock_ContentionErrorAndRelease(t *testing.T) {
	vaultPath := filepath.Join(t.TempDir(), "secrets.age")
	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	var releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
	holderJoined := false
	t.Cleanup(func() {
		releaseHolder()
		if !holderJoined {
			select {
			case <-holderDone:
				holderJoined = true
			case <-time.After(2 * time.Second):
				t.Errorf("blocking holder did not settle during cleanup")
			}
		}
	})
	go func() {
		holderDone <- WithVaultLock(vaultPath, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-holderDone:
		holderJoined = true
		t.Fatalf("blocking holder exited before contention probe: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("blocking holder did not acquire vault lock")
	}
	called := false
	locked, err := TryWithVaultLock(vaultPath, func() error {
		called = true
		return nil
	})
	if locked || err != nil || called {
		t.Fatalf("contended try = locked:%t err:%v called:%t, want false/nil/false", locked, err, called)
	}
	releaseHolder()
	select {
	case err := <-holderDone:
		holderJoined = true
		if err != nil {
			t.Fatalf("release blocking holder: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocking holder did not release")
	}

	sentinel := errors.New("callback failure")
	locked, err = TryWithVaultLock(vaultPath, func() error { return sentinel })
	if !locked || !errors.Is(err, sentinel) {
		t.Fatalf("callback failure = locked:%t err:%v, want true/sentinel", locked, err)
	}
	locked, err = TryWithVaultLock(vaultPath, func() error { return nil })
	if !locked || err != nil {
		t.Fatalf("post-error reacquire = locked:%t err:%v, want true/nil", locked, err)
	}
}
