package clients

import (
	"fmt"
	"os"
	"path/filepath"
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

	// Ensure the write-target parent dir exists BEFORE creating the advisory
	// flock (bot PR #420 finding 3, r15). The flock file lives at
	// "<configPath>.lock" INSIDE the write-target dir, so flock.New(...).Lock()
	// fails outright when that dir is absent — BEFORE any mutating adapter method
	// body (e.g. BackupKeep's own os.MkdirAll, mimocode.go) ever runs, leaving the
	// adapter-internal create structurally unreachable through this wrapper. A
	// MiMoCode profile active ONLY via a MIMOCODE_CONFIG_DIR overlay (or
	// MIMOCODE_CONFIG / inline content) with the GLOBAL ~/.config/mimocode dir
	// absent reports Exists()==true, so install / register / GUI Apply proceed to a
	// mutating call here — and without this create they would fail at the lock and
	// abort. This is the SINGLE owner of "the flock needs its parent dir to exist"
	// on the WRITE side; the READ side already handles the same constraint in
	// withConfigReadLock below.
	//
	// Runs under the per-path mutex already held above, so intra-process racers on
	// a new path are serialized (the create is idempotent for the cross-process
	// case). The production SecureCreateParentDirForConfigLock creates missing
	// components mode 0o700 (NOT 0o755): a fresh mcphub-created config dir with no
	// operator mode to preserve, and the secure-write parent-dir gate rejects
	// group/world bits on POSIX — a 0o755 dir would make a subsequent strict-mode
	// (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) SecureWriteClientConfig reject the very
	// dir just created. The FILE write itself stays hardened by the unchanged
	// WriteConfigFile / SecureWriteClientConfig pipeline (handle-relative DACL/mode,
	// atomic rename, symlink refusal); this governs only the parent dir.
	//
	// SECURITY (bot PR #420 finding 1 + r16 P1 + r17 finding P2a): the create/verify
	// goes through SecureCreateParentDir, NOT a blind os.MkdirAll. In production
	// internal/api/init() swaps SecureCreateParentDir to
	// api.SecureCreateParentDirForConfigLock, which descends the parent chain from
	// the VOLUME ROOT component-by-component, refusing any symlink / reparse-point
	// component (existing OR missing). It is called UNCONDITIONALLY — NOT only when
	// the dir is absent (the r17 P2a fix). The earlier IsNotExist-guarded form
	// skipped the descent when the parent dir ALREADY EXISTED, so an existing
	// SYMLINKED / reparse-point parent dir was never refused: flock.New(configPath +
	// ".lock") below would then create the lock file THROUGH the symlinked parent
	// (at an attacker-chosen target) before the later hardened SecureWriteClientConfig
	// could refuse it. Running the secure descent unconditionally verifies every
	// EXISTING component O_NOFOLLOW-relative and refuses a symlinked parent up front,
	// closing that pre-flock TOCTOU. The descent is idempotent for a clean existing
	// real-dir chain (every component opens as a real dir and returns nil). The test
	// default (fallbackSecureCreateParentDir) stays a plain idempotent MkdirAll,
	// safe only in the t.TempDir() sandboxes adapter tests use.
	// Normalize the write-target parent to an ABSOLUTE path BEFORE the secure
	// create (bot PR #420 r18 LOW/MEDIUM finding). Two adapters have a documented
	// BARE-RELATIVE fallback config path — copilot-cli's "mcp-config.json" and
	// qoder's "mcp-settings.json" (their defaultXConfigPath() degrades to a bare
	// basename when os.UserHomeDir() fails, matching the fail-safe posture
	// AllClients uses). For such a path filepath.Dir(configPath) is ".", and the
	// PRODUCTION secure creator (api.SecureCreateParentDirForConfigLock →
	// secureCreateParentDirAnywhereImpl) rejects "." / any non-absolute dir up
	// front ("no volume name" / "not absolute"), so r18's unconditional-secure-
	// create (the P2a symlink-gap fix) regressed those adapters' writes to a hard
	// failure at this chokepoint. filepath.Abs resolves the bare-relative dir
	// against the process cwd so the secure descent runs against a real absolute
	// chain — which restores copilot-cli/qoder writes WITHOUT weakening the P2a
	// guarantee for real absolute config paths: filepath.Abs is a Clean-only
	// no-op on an already-absolute dir, so the symlink-refusing volume-root descent
	// still applies unchanged to every absolute parent (no IsNotExist re-guard, no
	// skip — the gap stays closed). The flock + the actual file write below use the
	// SAME process cwd to resolve the original relative configPath, so the secured
	// absolute dir and the lock/write target are the same physical directory.
	if dir := filepath.Dir(configPath); dir != "" {
		absDir, absErr := filepath.Abs(dir)
		if absErr != nil {
			return fmt.Errorf("config lock %s: resolve parent dir %q to absolute: %w", configPath+".lock", dir, absErr)
		}
		if mkErr := SecureCreateParentDir(absDir); mkErr != nil {
			return fmt.Errorf("config lock %s: create parent dir: %w", configPath+".lock", mkErr)
		}
	}

	lockPath := configPath + ".lock"
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("config lock %s: %w", lockPath, err)
	}
	defer func() { _ = fl.Unlock() }()

	return fn()
}

// withConfigReadLock is the read-selection variant of withConfigLock used by
// the backup-READ paths (LatestBackupPath / BackupsNewestFirst /
// LegacyBackupsNewestFirst / the backup-file predicates). It serializes those
// reads against the backup-WRITE path on the same key.
//
// Difference from withConfigLock: when configPath's parent directory does not
// exist, the advisory flock cannot be created (its file lives in that missing
// dir). A missing parent dir means there are no backup files AND no writer can
// be mid-write (writeBackup's callers create the dir before writing), so there
// is nothing to serialize against — run fn under the in-process mutex only and
// let its own os.IsNotExist handling return the documented empty/absent result.
// The in-process mutex is still taken so two goroutines in this process agree
// on ordering even in the missing-dir case.
func withConfigReadLock(configPath string, fn func() error) error {
	if _, err := os.Stat(filepath.Dir(configPath)); err != nil && os.IsNotExist(err) {
		mu := perPathMutex(configPath)
		mu.Lock()
		defer mu.Unlock()
		return fn()
	}
	return withConfigLock(configPath, fn)
}

// lockingClient decorates a Client so every MUTATING method serializes its
// read-modify-write of the underlying config file via withConfigLock. The
// backup-READ selection/inspection methods (LatestBackupPath,
// BackupContainsEntry, BackupEntryIsHubManaged) are ALSO serialized under the
// same per-path lock so the demigrate selection cannot observe a backup
// directory/file mid-write (b1). The remaining read-only methods (Name,
// ConfigPath, Exists, GetEntry, AllStdioEntries, FindStdioLanguageServerEntries)
// are NOT overridden — they pass through to the embedded Client unchanged.
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

// The three backup-READ overrides below close the b1 residual race: the
// backup-write path (Backup/BackupKeep/writeBackup) is already serialized by
// the decorator, but the backup-read SELECTION + per-file inspection used by
// the demigrate flow (LatestBackupPath to pick a path; BackupContainsEntry /
// BackupEntryIsHubManaged to classify it) read the same backup directory and
// files with no lock. Concurrent with a Backup/BackupKeep writer those reads
// could observe a half-written timestamped backup or a mid-pruned directory
// view, so the demigrate selection could pick (or classify) a torn file.
//
// Re-entrancy is safe: no locked WRITE method on this decorator calls these
// read methods (each write override calls only the same-named concrete
// method, whose body never re-enters the read selection), so wrapping them in
// withConfigLock cannot self-deadlock on the same per-path mutex.

func (l *lockingClient) LatestBackupPath() (path string, ok bool, err error) {
	werr := withConfigReadLock(l.Client.ConfigPath(), func() error {
		path, ok, err = l.Client.LatestBackupPath()
		return err
	})
	if werr != nil && err == nil {
		return "", false, werr
	}
	return path, ok, err
}

func (l *lockingClient) BackupContainsEntry(backupPath, name string) (has bool, err error) {
	werr := withConfigReadLock(l.Client.ConfigPath(), func() error {
		has, err = l.Client.BackupContainsEntry(backupPath, name)
		return err
	})
	if werr != nil && err == nil {
		return false, werr
	}
	return has, err
}

func (l *lockingClient) BackupEntryIsHubManaged(backupPath, name string) (managed bool, err error) {
	werr := withConfigReadLock(l.Client.ConfigPath(), func() error {
		managed, err = l.Client.BackupEntryIsHubManaged(backupPath, name)
		return err
	})
	if werr != nil && err == nil {
		return false, werr
	}
	return managed, err
}

// RemovableStdioEntries forwards the OPTIONAL Client method of the same name to
// the wrapped concrete client, so the register-path inner type-assert
// (realClientAdapter.RemovableStdioEntries: a.c.(interface{ RemovableStdioEntries... }))
// actually reaches it. Without this forwarder the assert is made against the
// lockingClient, which embeds the Client INTERFACE — and RemovableStdioEntries is
// NOT on that interface (only *mimoCodeClient implements it) — so the assert
// failed and the cleanup path silently fell back to the merged-view
// AllStdioEntries(), making the write-target/effective-enabled removability
// discrimination INERT for the production (NewMimoCode → newLockingClient) chain.
//
// It is a plain pass-through (NO config lock): the wrapped method it forwards to
// (AllStdioEntries / RemovableStdioEntries) is a read-only method that the
// decorator does not lock — same posture as the embedded AllStdioEntries
// pass-through — so wrapping it in withConfigReadLock would add no consistency
// the un-overridden AllStdioEntries already lacks. It dispatches on the CONCRETE
// l.Client, not the embedded interface, so it routes to *mimoCodeClient's own
// RemovableStdioEntries. Non-mimo clients have no such method → fall back to
// their own AllStdioEntries (backward-compatible: identical to pre-forwarder
// behavior, where the register inner assert also fell back to AllStdioEntries).
func (l *lockingClient) RemovableStdioEntries() ([]StdioEntry, error) {
	if c, ok := l.Client.(interface {
		RemovableStdioEntries() ([]StdioEntry, error)
	}); ok {
		return c.RemovableStdioEntries()
	}
	return l.Client.AllStdioEntries()
}

// ActiveStdioEntriesExcludingWriteTarget / ActiveLanguageServerEntriesExcludingWriteTarget
// forward the two OPTIONAL post-removal active readers to the wrapped concrete
// client so the register-path inner type-assert reaches *mimoCodeClient's own
// methods (same forwarding reason as RemovableStdioEntries above; without these
// the assert is made against the embedded Client interface and silently fails).
// Plain pass-through, no config lock — read-only, same posture as
// RemovableStdioEntries. NON-mimo clients have no such method → return (nil, nil):
// an EMPTY post-removal survivor set means "nothing re-emerges", the correct
// default for a single-file adapter where RemoveEntry deletes the sole definition.
// The caller-side workspace-scoped recheck (register.go) then never blocks for a
// non-mimo client — behavior-unchanged.
func (l *lockingClient) ActiveStdioEntriesExcludingWriteTarget(name string) ([]StdioEntry, error) {
	if c, ok := l.Client.(interface {
		ActiveStdioEntriesExcludingWriteTarget(string) ([]StdioEntry, error)
	}); ok {
		return c.ActiveStdioEntriesExcludingWriteTarget(name)
	}
	return nil, nil
}

func (l *lockingClient) ActiveLanguageServerEntriesExcludingWriteTarget(name string) ([]LanguageServerStdioEntry, error) {
	if c, ok := l.Client.(interface {
		ActiveLanguageServerEntriesExcludingWriteTarget(string) ([]LanguageServerStdioEntry, error)
	}); ok {
		return c.ActiveLanguageServerEntriesExcludingWriteTarget(name)
	}
	return nil, nil
}

// RemovableStdioCandidatesWriteTargetOwned / FindStdioLanguageServerCandidatesWriteTargetOwned
// forward the OPTIONAL workspace-aware register-grain candidate sources (branch (a)
// + managed-only) to the wrapped concrete client (only *mimoCodeClient implements
// them; same forwarding reason as RemovableStdioEntries above). Plain pass-through,
// no config lock (read-only). The register grain's removableStdioEntriesForDirectCleanup /
// findStdioLanguageServerCandidatesForDirectCleanup wrappers fall back to the
// conservative full-survivor methods for any client lacking these, so a non-mimo
// adapter is behavior-unchanged.
func (l *lockingClient) RemovableStdioCandidatesWriteTargetOwned() ([]StdioEntry, error) {
	if c, ok := l.Client.(interface {
		RemovableStdioCandidatesWriteTargetOwned() ([]StdioEntry, error)
	}); ok {
		return c.RemovableStdioCandidatesWriteTargetOwned()
	}
	return l.Client.AllStdioEntries()
}

func (l *lockingClient) FindStdioLanguageServerCandidatesWriteTargetOwned() ([]LanguageServerStdioEntry, error) {
	if c, ok := l.Client.(interface {
		FindStdioLanguageServerCandidatesWriteTargetOwned() ([]LanguageServerStdioEntry, error)
	}); ok {
		return c.FindStdioLanguageServerCandidatesWriteTargetOwned()
	}
	return l.Client.FindStdioLanguageServerEntries()
}
