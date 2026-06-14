package secrets

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// WithVaultLock runs fn while holding a cross-process exclusive file lock on
// "<vaultPath>.lock". It is the cross-process counterpart to the in-process
// vaultMutex held by internal/api's secrets wrappers: vaultMutex serializes
// vault read-modify-write inside a single process (multiple GUI tabs), while
// this flock serializes the same OpenVault → mutate → save sequence ACROSS
// processes (the GUI's api layer and the CLI mutating the vault at the same
// time). Without it, two processes can each OpenVault (read the full map),
// then each save (write the full map), and the second writer silently
// overwrites the first — the last-write-wins data loss documented in
// work-items/bugs/a3a-vault-concurrent-edit-lww.md.
//
// The lock is BLOCKING (mirrors api.Registry.Lock at
// internal/api/workspace_registry.go:200): callers must keep the critical
// section tight — open, mutate, save — and must NOT hold it across an
// interactive prompt or editor invocation, or a concurrent process's vault
// write blocks for the whole interaction. The lock file is created under the
// vault's own directory (created 0700 if missing) so it inherits the same
// owner-only ACL boundary as secrets.age itself.
//
// The lock is always released before WithVaultLock returns, on every path
// (fn error, fn panic via the deferred Unlock, or success). fn's error is
// returned verbatim so callers' typed-error mapping is preserved; a failure
// to ACQUIRE the lock is returned wrapped.
func WithVaultLock(vaultPath string, fn func() error) error {
	if dir := filepath.Dir(vaultPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("vault lock: mkdir %s: %w", dir, err)
		}
	}
	lockPath := vaultPath + ".lock"
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("vault lock %s: %w", lockPath, err)
	}
	defer func() { _ = fl.Unlock() }()
	return fn()
}
