package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/config"
)

// Phase 5 — serena auto-register-on-miss.
//
// AutoRegisterSerenaWorkspace registers a serena workspace at runtime when a
// `/serena/mcp` tool call arrives for a project path that has a
// `.serena/project.yml` marker but no registered serena workspace yet. The GUI
// router (internal/gui/serena_router.go) calls this on the
// ErrWorkspaceNotFound branch, then forwards the original call to the
// freshly-spawned per-workspace daemon.
//
// CONTRACT (do not change without updating the router caller + the deps seam in
// internal/gui/serena_router.go's serenaRouterDeps.AutoRegisterFn):
//   - absPath: the tool-argument path the agent called serena from.
//   - returns the registered *WorkspaceEntry on success (router then forwards).
//   - returns ErrNotASerenaProject when absPath has no `.serena/project.yml`
//     ancestor marker (router → keep the existing 503 not-found; this is the
//     DoS bound — an attacker cannot register a path with no marker they own).
//   - returns ErrNoLanguages when the marker exists but lists no languages
//     (router → HTTP 422).
//   - returns any other error for install/spawn/readiness failure (router →
//     HTTP 503); the implementation rolls back the registry row for a
//     PRE-COMMIT failure so a failed auto-register leaves no half-registered
//     workspace. A POST-COMMIT readiness failure does NOT roll back — the
//     workspace IS registered + in the committed intent, so the next call
//     resolves it and the daemon comes up under the supervisor reconciler; the
//     router maps the wrapped error to 503 and the client retries.
//
// IMPLEMENTATION (Phase 5 Part B) composes, in order, under a per-wsKey
// concurrency guard so concurrent calls for the same path register exactly once:
//  1. ancestor-walk for `.serena/project.yml` (ErrNotASerenaProject if absent) +
//     read its `languages:` (ErrNoLanguages if empty);
//  2. registry write under flock with re-read idempotency (GetSerena → return
//     existing if a concurrent winner already registered; else AllocateSerenaPort
//     + PutSerena + Save), arming a remove-row rollback;
//  3. BuildInMemorySerenaDynamicPoolManifest + InstallParsedManifest with
//     Workspaces: reg.SerenaEntries() (the §7.1 gate now ALLOWS this live-add
//     because the prior intent already carries runtime_spec post-cutover);
//  4. DialSupervisorIPCReconcile(ctx, true) for an immediate spawn (not the 60s
//     IntentWatcher poll);
//  5. readiness probe on the allocated port's /mcp (verifyProxyReady pattern);
//  6. audit `workspace-auto-registered` {path, languages, port, ...}.
//
// This composition is a SIMPLER SUBSET of the Phase 4 migrate
// (runMigrateSerenaDynamicPool, internal/cli/migrate_serena.go) because
// auto-register runs while the supervisor is ALREADY RUNNING (post-cutover): no
// reap (no old supervisor to kill), no supervisor start (it is already up — an
// immediate reconcile spawns the new daemon now). Spawn is Windows-only (the
// supervisor start primitive is a no-op stub elsewhere); the decision/rollback
// logic is cross-platform and the install/reconcile/readiness steps are seamed
// so the happy path is unit-testable without a live supervisor.
func (a *API) AutoRegisterSerenaWorkspace(ctx context.Context, absPath string) (entry *WorkspaceEntry, err error) {
	// 1. Resolve the workspace root: clean/abs absPath, then walk up ancestors
	//    for `.serena/project.yml`. The found directory IS the WorkspacePath.
	//    No marker found → ErrNotASerenaProject (the DoS bound — NEVER register
	//    an arbitrary unmarked path).
	root, err := resolveSerenaProjectRoot(absPath)
	if err != nil {
		return nil, err
	}

	// 2. Read <root>/.serena/project.yml languages. Empty/missing → ErrNoLanguages.
	languages, err := readSerenaProjectYMLLanguages(root)
	if err != nil {
		return nil, err
	}

	// 3. Concurrency guard + idempotency. The keyed mutex serializes concurrent
	//    callers for the SAME workspace root so they register exactly once; the
	//    loser blocks here, then the post-acquire GetSerena re-read below returns
	//    the winner's entry. Use the CANONICALIZED root for the key so symlink
	//    aliases map to the same guard slot (CanonicalWorkspacePath resolves the
	//    full path; on failure — e.g. the dir vanished mid-call — fall back to
	//    the raw cleaned root so the key is still deterministic).
	canonical := root
	if c, cErr := CanonicalWorkspacePath(root); cErr == nil {
		canonical = c
	}
	key := WorkspaceKey(canonical)

	unlockKey := lockSerenaAutoRegisterKey(key)
	defer unlockKey()

	// Load the registry under its cross-process flock. Hold the flock across the
	// registry mutation only (alloc + PutSerena + Save); the install/reconcile/
	// readiness steps that follow do NOT touch the registry, so we release the
	// flock right after Save so the install does not hold it and the rollback can
	// re-acquire it cleanly. The per-key mutex already blocks concurrent same-root
	// callers; a distinct concurrent root takes a distinct mutex slot but shares
	// this one flock, so releasing promptly avoids serializing unrelated roots
	// across the multi-second install.
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	regUnlock, err := reg.Lock()
	if err != nil {
		return nil, err
	}
	regReleased := false
	releaseReg := func() {
		if regReleased {
			return
		}
		regReleased = true
		regUnlock()
	}
	// Belt-and-suspenders: release on any early-return error path BEFORE the
	// explicit release fires. Idempotent, so the post-Save explicit release is
	// authoritative and this deferred call is a harmless no-op after it.
	defer releaseReg()
	if err := reg.Load(); err != nil {
		return nil, err
	}

	// Idempotency: a concurrent winner (or a prior call) may have already
	// registered this workspace. Return the existing row unchanged.
	if existing, ok := reg.GetSerena(key); ok {
		releaseReg()
		e := existing
		return &e, nil
	}

	// 4. Allocate a pool port + register. Load the serena catalog manifest, take
	//    the EFFECTIVE port pool (Phase 2 owner — never fails closed on the
	//    legacy kind:global embed), allocate a free port, build + persist the row.
	catalog, err := loadSerenaCatalogManifest()
	if err != nil {
		return nil, fmt.Errorf("serena auto-register: load serena catalog manifest: %w", err)
	}
	pool, err := EffectiveSerenaPortPool(catalog)
	if err != nil {
		return nil, fmt.Errorf("serena auto-register: resolve effective serena port pool: %w", err)
	}
	port, err := reg.AllocateSerenaPort(pool)
	if err != nil {
		return nil, fmt.Errorf("serena auto-register: allocate serena pool port: %w", err)
	}
	newEntry := WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: root,
		Language:      SerenaLanguageSentinel,
		Backend:       SerenaServerName,
		Port:          port,
		TaskName:      fmt.Sprintf("mcp-local-hub-serena-%s", key),
		RegisteredAt:  time.Now().UTC(),
		RegisteredVia: "auto-detect",
		Languages:     append([]string(nil), languages...),
	}
	if err := reg.PutSerena(newEntry); err != nil {
		return nil, fmt.Errorf("serena auto-register: put serena row: %w", err)
	}
	if err := reg.Save(); err != nil {
		// Save is atomic (tempfile + rename): a failure leaves the prior on-disk
		// registry intact, so there is nothing to roll back. The deferred
		// releaseReg frees the still-held flock. Do NOT arm the relocking rollback
		// here — it would deadlock re-acquiring the flock this goroutine still holds.
		return nil, fmt.Errorf("serena auto-register: save registry: %w", err)
	}

	// The registry mutation is committed on disk. Arm the PRE-COMMIT rollback
	// (remove the row we just added) and release the flock so the install does
	// not hold it. The rollback re-acquires the flock itself.
	rollbackArmed := true
	defer func() {
		if err == nil || !rollbackArmed {
			return
		}
		if rbErr := removeSerenaRowRelocking(regPath, key); rbErr != nil {
			err = fmt.Errorf("%w; AND the registry rollback also failed: %v", err, rbErr)
		}
	}()
	releaseReg()

	// 5. Build the in-memory dynamic-pool manifest + install it (live-add). The
	//    Workspaces snapshot is the FULL current serena set (reg.SerenaEntries(),
	//    which now includes the row we just saved) so InstallParsedManifest
	//    preserves every existing daemon row and adds ours. The §7.1 gate ALLOWS
	//    this write because the prior on-disk intent already carries runtime_spec
	//    (post-cutover) — the gate fires only when INTRODUCING runtime_spec.
	dyn, err := BuildInMemorySerenaDynamicPoolManifest(catalog)
	if err != nil {
		return nil, fmt.Errorf("serena auto-register: build dynamic-pool manifest: %w", err)
	}
	installWorkspaces := reg.SerenaEntries()
	if installWorkspaces == nil {
		// A nil snapshot would silently drop the server's existing daemon rows;
		// pass a non-nil slice. (Cannot happen here — we just PutSerena'd a row —
		// but the contract requires non-nil, so guard defensively.)
		installWorkspaces = []WorkspaceEntry{}
	}
	if _, err = autoRegisterInstallParsedManifestFn(ctx, a, dyn, InstallParsedManifestOpts{
		Writer:     io.Discard,
		Workspaces: installWorkspaces,
	}); err != nil {
		// PRE-COMMIT failure: InstallParsedManifest's own inner stack already
		// undid its scheduler/client/intent side effects; the deferred rollback
		// above removes our registry row. Return wrapped (router → 503).
		return nil, fmt.Errorf("serena auto-register: install dynamic-pool descriptor for %s: %w", root, err)
	}

	// 6. DISARM the rollback the instant the install returns success — that intent
	//    write is the commit point (mirror migrate step 9). After commit, rolling
	//    back the registry would create split-state: the intent has the row but
	//    the registry would not, so the router could not resolve the workspace.
	rollbackArmed = false

	// 7. Immediate spawn: ask the (already-running) supervisor to reconcile NOW so
	//    the new daemon comes up immediately instead of on the 60s IntentWatcher
	//    poll. A SUPERVISOR_STARTING / transient / unavailable response is NOT
	//    fatal — the 60s reconcile is the backstop — so do not abort; continue to
	//    the readiness probe. (Best-effort: the committed intent is the source of
	//    truth; reconcile only accelerates the spawn.)
	if _, recErr := autoRegisterReconcileFn(ctx, true); recErr != nil {
		// Non-fatal: fall through to readiness. The supervisor will pick up the
		// committed intent on its next poll even if this immediate nudge failed.
		_ = recErr
	}

	// 8. Readiness: poll the allocated port's /mcp with a synthetic initialize.
	//    Ready → return the entry. A timeout AFTER the committed install does NOT
	//    roll back (the workspace is registered + in the intent; the next call
	//    resolves it and the daemon will be up) — return an error wrapping the
	//    readiness failure so the router maps it to 503 and the client retries.
	if rdErr := autoRegisterReadinessFn(port, serenaAutoRegisterReadinessTimeout); rdErr != nil {
		err = fmt.Errorf("serena auto-register: workspace %s registered and intent committed, but the per-workspace daemon on port %d was not ready in time: %w (the workspace IS registered; retry the call — the supervisor is bringing the daemon up)", root, port, rdErr)
		// Emit the audit even on the readiness-timeout path: the registration +
		// install DID commit, so the operator should still see the event.
		emitWorkspaceAutoRegisteredEvent(root, key, port, newEntry.Languages)
		return nil, err
	}

	// 9. Audit: emit the success event (best-effort; never fatal).
	emitWorkspaceAutoRegisteredEvent(root, key, port, newEntry.Languages)

	e := newEntry
	return &e, nil
}

// serenaAutoRegisterReadinessTimeout bounds the post-spawn readiness probe. It
// is generous enough for a cold `uvx`-spawned serena child (download + Python
// startup) on a warm cache but still bounded so a wedged spawn returns to the
// router (→ 503 + client retry) rather than blocking the call indefinitely.
const serenaAutoRegisterReadinessTimeout = 20 * time.Second

// resolveSerenaProjectRoot cleans+absolutizes p, then walks UP the ancestor
// chain looking for the first directory containing `.serena/project.yml`. That
// directory is the workspace root (the path the per-workspace serena daemon is
// `--project`-ed at). Returns ErrNotASerenaProject when no ancestor carries the
// marker — the load-bearing DoS bound (an unmarked arbitrary path is never
// auto-registered).
//
// We cannot import serena_routing's resolver.go ancestor walker (it imports
// api → cycle), so this is the api-package-local walk. It mirrors that walker's
// shape: start at the cleaned absolute path, test `<dir>/.serena/project.yml`
// at each level, stop at the filesystem root.
func resolveSerenaProjectRoot(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("%w: empty path", ErrNotASerenaProject)
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("serena auto-register: resolve path %q: %w", p, err)
	}
	dir := abs
	for {
		marker := filepath.Join(dir, ".serena", "project.yml")
		if fi, statErr := os.Stat(marker); statErr == nil && !fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding the marker.
			return "", fmt.Errorf("%w (searched %q and every ancestor up to the root)", ErrNotASerenaProject, abs)
		}
		dir = parent
	}
}

// serenaProjectYMLForAutoRegister is the minimal shape consumed from
// .serena/project.yml. Only the languages list matters here; every other serena
// field is left untouched on disk (auto-register never rewrites project.yml).
// Replicated minimally from internal/cli/workspace_cmd.go:745's serenaProjectYml
// (the api package cannot import cli).
type serenaProjectYMLForAutoRegister struct {
	Languages []string `yaml:"languages"`
}

// readSerenaProjectYMLLanguages reads <root>/.serena/project.yml and returns its
// declared languages. A missing/empty languages list → ErrNoLanguages (the
// marker exists but no serena descriptor can be synthesized). A read or
// YAML-parse failure propagates verbatim. Empty-string entries are dropped so a
// `languages: [""]` list does not masquerade as a valid declaration.
func readSerenaProjectYMLLanguages(root string) ([]string, error) {
	path := filepath.Join(root, ".serena", "project.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("serena auto-register: read %s: %w", path, err)
	}
	var doc serenaProjectYMLForAutoRegister
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("serena auto-register: parse %s: %w", path, err)
	}
	cleaned := make([]string, 0, len(doc.Languages))
	for _, l := range doc.Languages {
		if strings.TrimSpace(l) == "" {
			continue
		}
		cleaned = append(cleaned, l)
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("%w (%s)", ErrNoLanguages, path)
	}
	return cleaned, nil
}

// removeSerenaRowRelocking is the PRE-COMMIT registry rollback compensator. The
// flock was released after the auto-register Save (so the multi-second install
// did not hold it), so this re-acquires the flock itself, reloads the current
// on-disk registry, drops the serena row for key, and saves. It is SURGICAL: it
// removes ONLY the one workspace key we added (RemoveSerena is a no-op if a
// concurrent unregister already dropped it), leaving every other row — including
// any concurrent serena registration for a different key — untouched. The lock
// is always released before returning (no leaked lock).
func removeSerenaRowRelocking(regPath, key string) error {
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return fmt.Errorf("auto-register rollback: re-acquire registry lock: %w", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return fmt.Errorf("auto-register rollback: reload registry: %w", err)
	}
	reg.RemoveSerena(key)
	if err := reg.Save(); err != nil {
		return fmt.Errorf("auto-register rollback: save registry: %w", err)
	}
	return nil
}

// emitWorkspaceAutoRegisteredEvent writes a best-effort `workspace-auto-registered`
// audit event to supervisor-events.log. Mirrors the api-package emit idiom used
// by emitStaleWorkspaceSkippedEvent / emitSpecBearingInstallRefusedEvent
// (install_parsed_manifest.go): resolve the state dir, open the canonical
// supervisor event log, emit, close. A failure to resolve/open/emit is silently
// non-fatal — the audit is observability, not a gate.
func emitWorkspaceAutoRegisteredEvent(workspacePath, key string, port int, languages []string) {
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		return
	}
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      SupervisorEventSeverityInfo,
		Source:        "reconcile",
		Event:         "workspace-auto-registered",
		TaskName:      SerenaTaskNameForWorkspace(workspacePath),
		Body: map[string]any{
			"path":          workspacePath,
			"workspace_key": key,
			"port":          port,
			"languages":     languages,
		},
	})
}

// loadSerenaCatalogManifest loads + parses the embedded serena catalog manifest
// (honoring the MCPHUB_MANIFEST_DIR_OVERRIDE test seam, so tests read a seeded
// temp manifest with no embed leakage). It is the catalog input to both
// EffectiveSerenaPortPool and BuildInMemorySerenaDynamicPoolManifest. Mirrors
// internal/cli/migrate_serena.go:162's loadSerenaManifestForMigrateFn (which the
// api package cannot import — cli imports api, not the reverse). A package-level
// var so tests can override the catalog source without seeding a manifest dir.
var loadSerenaCatalogManifest = func() (*config.ServerManifest, error) {
	raw, err := NewAPI().ManifestGet(SerenaServerName)
	if err != nil {
		return nil, fmt.Errorf("load serena manifest: %w", err)
	}
	m, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse serena manifest: %w", err)
	}
	return m, nil
}

// --- Test seams ------------------------------------------------------------
//
// The install / immediate-reconcile / readiness steps each require a real
// supervisor or a live network listener; they are routed through package-level
// function vars (defaulting to the real impls) so unit tests exercise the
// register + rollback + idempotency + concurrency logic without a supervisor.
// Mirrors the migrate seam idiom (installParsedManifestFn / reconcileSerenaClientsFn
// / migrateSerenaStartFn in internal/cli/migrate_serena.go).

// autoRegisterInstallParsedManifestFn is the seam over (*API).InstallParsedManifest.
// Default calls the real installer; tests override it to inject install success/
// failure without reaching the scheduler / state-file pipeline.
var autoRegisterInstallParsedManifestFn = func(ctx context.Context, a *API, m *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
	return a.InstallParsedManifest(ctx, m, opts)
}

// autoRegisterReconcileFn is the seam over DialSupervisorIPCReconcile. Default
// nudges the running supervisor to reconcile NOW; tests override it to assert
// it is called with apply=true (and that a transient/unavailable error is
// non-fatal).
var autoRegisterReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
	return DialSupervisorIPCReconcile(ctx, apply)
}

// autoRegisterReadinessFn is the seam over verifyProxyReady. Default polls the
// allocated port's /mcp; tests override it to return nil (ready) or an error
// (post-commit timeout, which must NOT roll back).
var autoRegisterReadinessFn = func(port int, timeout time.Duration) error {
	return verifyProxyReady(port, timeout)
}

// --- per-workspace-key concurrency guard -----------------------------------

// serenaAutoRegisterKeyMu guards the serenaAutoRegisterKeyLocks map itself.
var serenaAutoRegisterKeyMu sync.Mutex

// serenaAutoRegisterKeyLocks holds one *sync.Mutex per in-flight workspace key
// so concurrent AutoRegisterSerenaWorkspace calls for the SAME root serialize
// (register exactly once; the loser re-reads the winner's row), while calls for
// DISTINCT roots proceed in parallel. Entries are intentionally never deleted:
// the key space is the bounded set of a single user's workspaces (<100), and a
// reaper would reintroduce a lost-wakeup race against an in-flight holder.
var serenaAutoRegisterKeyLocks = map[string]*sync.Mutex{}

// lockSerenaAutoRegisterKey acquires the per-key mutex for key and returns its
// unlock function. The map lookup/insert is guarded by serenaAutoRegisterKeyMu;
// the per-key mutex is then acquired OUTSIDE that guard so a long-running holder
// for one key never blocks map access for another key.
func lockSerenaAutoRegisterKey(key string) func() {
	serenaAutoRegisterKeyMu.Lock()
	mu, ok := serenaAutoRegisterKeyLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		serenaAutoRegisterKeyLocks[key] = mu
	}
	serenaAutoRegisterKeyMu.Unlock()

	mu.Lock()
	return func() { mu.Unlock() }
}

// ErrNotASerenaProject is returned by AutoRegisterSerenaWorkspace when the
// requested path has no `.serena/project.yml` ancestor marker, so it is not an
// auto-registrable serena project. The router maps this to the existing
// workspace-not-found response (NOT an auto-register) — it is the load-bearing
// DoS bound against registering arbitrary attacker-supplied paths.
var ErrNotASerenaProject = errors.New("serena auto-register: path is not a serena project (no .serena/project.yml marker)")

// ErrNoLanguages is returned by AutoRegisterSerenaWorkspace when the
// `.serena/project.yml` marker exists but declares no languages, so no serena
// descriptor can be synthesized. The router maps this to HTTP 422.
var ErrNoLanguages = errors.New("serena auto-register: .serena/project.yml declares no languages")
