//go:build !af1_counters

// secure_write_counters_prod.go — production variant of the AF-1 secure-write
// observation counters.
//
// The two observe* functions are the ONLY way the secure-write code paths
// (secureWriteClientConfigImpl on each platform, and the shared
// secureWriteClientConfigToResolvedParent owner) report which lane a write took.
// In the production build (no af1_counters tag) they have EMPTY bodies, so the
// compiler elides the calls entirely — zero residual instructions and, crucially,
// NO process-global counter state mutated on every secure write. That removes the
// unsynchronized-int data race that existed when the increments ran inline in
// production: concurrent /api/install + /api/init-client-config +
// /api/resolve-symlink-and-write writes from different goroutines no longer touch
// any shared counter.
//
// The real counters + increment bodies live in secure_write_counters_aftag.go,
// compiled ONLY under -tags=af1_counters (the AF-1 entry-count tests). A
// production binary therefore contains NO secureWritePathBasedStringEntryCount /
// secureWriteResolvedParentEntryCount symbol at all — verifiable with
// `go tool nm <bin> | grep secureWritePathBasedStringEntryCount` returning empty.
//
// This mirrors the state_paths_prod.go / state_paths_envfallback.go build-tag
// precedent in this package: a test-only mechanism gated out of shipped binaries
// at compile time, behind a dedicated single-purpose tag (NOT reusing
// test_state_path_env).

package api

// observeStringEntry records an entry into the path-based string lane
// (secureWriteClientConfigImpl). No-op in production.
func observeStringEntry() {}

// observeResolvedParentEntry records an entry into the shared resolved-parent
// owner (secureWriteClientConfigToResolvedParent). No-op in production.
func observeResolvedParentEntry() {}
