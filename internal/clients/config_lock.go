package clients

import (
	"fmt"
	"sync"

	"github.com/gofrs/flock"
)

// configMutexes holds one *sync.Mutex per config file path, created on first
// use. It serializes the read-modify-write of a single client config file
// WITHIN one process — the multi-tab GUI case where two AddEntry/RemoveEntry
// requests for the same client race. sync.Map is the right structure here:
// the key set (one entry per supported client config path) is tiny and
// effectively write-once, so LoadOrStore contention is negligible.
var configMutexes sync.Map // map[string]*sync.Mutex

// perPathMutex returns the process-wide *sync.Mutex for configPath, creating
// it on first request. Two callers racing on the same new path agree on a
// single mutex via LoadOrStore (the loser's freshly-allocated mutex is
// discarded), so the lock identity is stable for a given path.
func perPathMutex(configPath string) *sync.Mutex {
	if m, ok := configMutexes.Load(configPath); ok {
		return m.(*sync.Mutex)
	}
	m, _ := configMutexes.LoadOrStore(configPath, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// withConfigLock runs fn while holding BOTH an in-process per-path mutex and a
// cross-process advisory file lock ("<configPath>.lock"), so a single client
// config file's read-modify-write is serialized against every other writer —
// other goroutines in this process (multiple GUI tabs) AND other processes
// (the CLI and the GUI mutating the same client config concurrently).
//
// Lock order: in-process mutex FIRST, then flock. Release is reverse
// (flock unlocked by its deferred call, then the mutex), so the acquire/
// release ordering is symmetric and a panic in fn still unwinds both. Only
// ONE config file is ever locked per withConfigLock call, and the decorator
// never nests withConfigLock (the concrete adapter's internal cross-method
// calls bypass the decorator — see lockingClient), so no two config locks are
// ever held at once and the cross-file deadlock class is structurally absent.
//
// The flock is BLOCKING (mirrors api.Registry.Lock at
// internal/api/workspace_registry.go:200): the live GUI + CLI client-config
// write paths depend on these critical sections staying tight, so each
// mutating adapter method does exactly one open → mutate → atomic-write under
// the lock and nothing slow or interactive.
func withConfigLock(configPath string, fn func() error) error {
	mu := perPathMutex(configPath)
	mu.Lock()
	defer mu.Unlock()

	lockPath := configPath + ".lock"
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("config lock %s: %w", lockPath, err)
	}
	defer func() { _ = fl.Unlock() }()

	return fn()
}

// lockingClient decorates a Client so every MUTATING method serializes its
// read-modify-write of the underlying config file via withConfigLock. The
// 10 read-only methods are NOT overridden — they pass through to the embedded
// Client unchanged (they take no config-file write lock, matching the
// pre-decorator behavior).
//
// Re-entrancy safety: each override calls the CONCRETE adapter (l.Client),
// whose own internal cross-method calls (e.g. cursor/vscode/qwen/zed
// BackupKeep internally calling InitEmpty) dispatch on the concrete struct
// directly, NOT through this decorator — so they never re-enter withConfigLock
// and there is no self-deadlock on the same per-path mutex.
type lockingClient struct {
	Client // embedded: read-only methods + everything not overridden pass through
}

// newLockingClient wraps c so its mutating methods are config-file-locked.
// Every clients factory (NewX / AllClients) returns the wrapped adapter so the
// lock is in force for both the GUI (api) and CLI write paths.
func newLockingClient(c Client) Client {
	return &lockingClient{Client: c}
}

func (l *lockingClient) InitEmpty() (created bool, err error) {
	werr := withConfigLock(l.Client.ConfigPath(), func() error {
		created, err = l.Client.InitEmpty()
		return err
	})
	if werr != nil && err == nil {
		// A lock-acquire failure (not an InitEmpty failure) — surface it.
		return false, werr
	}
	return created, err
}

func (l *lockingClient) Backup() (string, error) {
	var path string
	err := withConfigLock(l.Client.ConfigPath(), func() error {
		var e error
		path, e = l.Client.Backup()
		return e
	})
	return path, err
}

func (l *lockingClient) BackupKeep(keepN int) (string, error) {
	var path string
	err := withConfigLock(l.Client.ConfigPath(), func() error {
		var e error
		path, e = l.Client.BackupKeep(keepN)
		return e
	})
	return path, err
}

func (l *lockingClient) Restore(backupPath string) error {
	return withConfigLock(l.Client.ConfigPath(), func() error {
		return l.Client.Restore(backupPath)
	})
}

func (l *lockingClient) AddEntry(entry MCPEntry) error {
	return withConfigLock(l.Client.ConfigPath(), func() error {
		return l.Client.AddEntry(entry)
	})
}

func (l *lockingClient) RemoveEntry(name string) error {
	return withConfigLock(l.Client.ConfigPath(), func() error {
		return l.Client.RemoveEntry(name)
	})
}

func (l *lockingClient) RestoreEntryFromBackup(backupPath, name string) error {
	return withConfigLock(l.Client.ConfigPath(), func() error {
		return l.Client.RestoreEntryFromBackup(backupPath, name)
	})
}

func (l *lockingClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return withConfigLock(l.Client.ConfigPath(), func() error {
		return l.Client.RestoreEntryFromBackupForRollback(backupPath, name)
	})
}
