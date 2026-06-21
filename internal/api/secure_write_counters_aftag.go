//go:build af1_counters

// secure_write_counters_aftag.go — AF-1-tagged variant of the secure-write
// observation counters.
//
// Compiled ONLY when the build is invoked with -tags=af1_counters (the AF-1
// entry-count tests). Production binaries never see this file, so neither the
// counter variables nor the increment bodies exist in a shipped binary —
// `go tool nm <prod-bin> | grep secureWritePathBasedStringEntryCount` returns
// empty (compile-out proof).
//
// These counters prove the AF-1 closure (T5): the symlink-resolve relax lane
// must write THROUGH a pinned parent handle (secureWriteThroughResolvedParentHandle
// -> secureWriteClientConfigToResolvedParent) and must NOT re-walk the resolved
// string through the path-based secureWriteClientConfigImpl.
//
// The path-based string entry (secureWriteClientConfigImpl, which does
// filepath.Split + open-parent-by-path) increments the string counter via
// observeStringEntry(); the shared post-parent-open owner
// (secureWriteClientConfigToResolvedParent) increments the resolved-parent
// counter via observeResolvedParentEntry(). A symlink-lane write thus leaves the
// string counter at 0 and the resolved-parent counter > 0; a regression that
// reintroduced the string re-walk would bump the string counter.
//
// Plain ints (not atomic): the tests that read them run single-threaded and set
// this tag. The data race that motivated F3 only existed when these increments
// ran INLINE in the production code path; under this tag the AF-1 tests are
// single-threaded and never race them.

package api

var (
	secureWritePathBasedStringEntryCount int
	secureWriteResolvedParentEntryCount  int
)

// observeStringEntry records an entry into the path-based string lane
// (secureWriteClientConfigImpl).
func observeStringEntry() { secureWritePathBasedStringEntryCount++ }

// observeResolvedParentEntry records an entry into the shared resolved-parent
// owner (secureWriteClientConfigToResolvedParent).
func observeResolvedParentEntry() { secureWriteResolvedParentEntryCount++ }

// resetA3EntryCounters zeroes both counters so a test reads a clean slate. Lives
// here (not in a _test.go) so it shares the af1_counters gate with the variables
// it mutates; the AF-1 entry-count tests are the only callers.
func resetA3EntryCounters() {
	secureWritePathBasedStringEntryCount = 0
	secureWriteResolvedParentEntryCount = 0
}
