//go:build !af1_counters

package api

import (
	"fmt"
	"sync"
	"testing"
)

// TestSecureWriteClientConfig_ConcurrentWriters_NoRace is the F3 regression
// guard: concurrent production secure writes to DISTINCT targets must not race
// on any shared process-global state. Before F3 the secure-write lanes
// incremented two plain (unsynchronized) ints —
// secureWritePathBasedStringEntryCount / secureWriteResolvedParentEntryCount —
// on EVERY production write, so concurrent /api/install +
// /api/init-client-config + /api/resolve-symlink-and-write writes from
// different goroutines were a real data race. F3 moved those counters (and
// their increments) behind the af1_counters build tag; in the production build
// (this file is compiled only with !af1_counters) the observe* recorders are
// empty, so there is no shared mutation to race.
//
// This test is gated !af1_counters DELIBERATELY: under -tags=af1_counters the
// counters ARE present and shared, so concurrent writers would (correctly)
// race them — that is the test-only accounting state the AF-1 entry-count tests
// read single-threaded. The F3 fix is precisely that PRODUCTION carries no such
// state, so the no-race proof must run in the production-counter configuration.
//
// Run with: go test -race -run Concurrent ./internal/api/
func TestSecureWriteClientConfig_ConcurrentWriters_NoRace(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")

	const writers = 8
	// Pre-create each writer's DISTINCT hardened parent + target path on the
	// TEST goroutine (hardenedTempDir uses t.Fatal, which must not run in a
	// spawned goroutine). The goroutines then only call SecureWriteClientConfig,
	// so the only thing they could contend on is shared PACKAGE state — exactly
	// what F3 removed from the production build.
	targets := make([]string, writers)
	for i := range targets {
		targets[i] = fmt.Sprintf("%s/config-%d.json", hardenedTempDir(t), i)
	}

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(n int) {
			defer wg.Done()
			if err := SecureWriteClientConfig(targets[n], []byte(fmt.Sprintf(`{"w":%d}`, n))); err != nil {
				t.Errorf("concurrent writer %d: SecureWriteClientConfig: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
}
