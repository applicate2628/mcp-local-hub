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
//     workspace. A POST-COMMIT readiness/start failure does NOT roll back — the
//     workspace IS registered + in the committed intent, so the next call
//     resolves it and the daemon comes up under the supervisor; the router maps
//     the wrapped error to 503 and the client retries.
//
// TWO INSTALL MODES (bot PR #253 finding 1). The §7.1 gate inside
// InstallParsedManifest refuses to write a spec-bearing (runtime_spec) intent
// while a supervisor runs UNLESS the prior on-disk intent already carries
// runtime_spec (an older supervisor's DisallowUnknownFields watcher would reject
// the field). So auto-register branches on the prior intent + supervisor liveness:
//   - LIVE-ADD (prior intent already has a serena runtime_spec row): the running
//     supervisor is provably the new binary, so install (gate allows) then nudge
//     it to reconcile NOW (DialSupervisorIPCReconcile apply=true; the 60s
//     IntentWatcher poll is the backstop). No reap, no bounce.
//   - INTRODUCE (prior intent has NO runtime_spec): this is the FIRST serena
//     workspace (e.g. right after a zero-workspace Phase-4 migrate, which writes
//     a no-runtime_spec intent and does not reap/restart). The supervisor cannot
//     be proven runtime_spec-capable (it exposes no probeable version), so the
//     one-time cutover runs: REAP the (possibly old) supervisor — only when one
//     is running — so the spec-bearing write hits the gate with nothing running,
//     then INSTALL, then START the current binary which reconciles + spawns. This
//     bounces the supervisor + its daemons ONCE; every subsequent workspace is a
//     live-add. The reap/start primitives are Windows-only (injected by the CLI
//     via SetSerenaAutoRegisterCutoverPrimitives); on a build/platform where they
//     are not wired the introduce path fails loud (router → 503).
//
// CONCURRENCY (bot PR #253 finding 2). A process-global install mutex
// (serenaAutoRegisterInstallMu) serializes the register → re-read → install →
// commit-or-rollback critical section across ALL workspace roots. Without it two
// concurrent auto-registers for DIFFERENT roots would each install from their own
// stale registry snapshot and clobber each other's supervisor-intent rows
// (buildMergedSupervisorIntent REPLACES all serena rows with the passed snapshot).
// Holding the mutex from before the registry row is made visible through the
// install commit (or its rollback) guarantees each install fans out from the
// latest on-disk registry and no other root observes a half-committed row. It is
// released before the readiness probe (which touches neither registry nor intent).
// The per-key mutex below is the finer idempotency guard (same root → register
// exactly once); the install mutex is the coarser cross-root install serializer.
//
// This composition mirrors the Phase 4 migrate (runMigrateSerenaDynamicPool,
// internal/cli/migrate_serena.go): reap-first ordering, install-is-the-commit-
// point, recovery-restart-on-post-reap-failure. The install/reconcile/readiness/
// reap/start/liveness/prior-intent steps are seamed so the decision + rollback +
// concurrency logic is unit-testable without a live supervisor.
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

	// 3. Per-key idempotency guard. The keyed mutex serializes concurrent callers
	//    for the SAME workspace root so they register exactly once; the loser
	//    blocks here, then the post-acquire GetSerena re-read below returns the
	//    winner's entry. Use the CANONICALIZED root for the key so symlink aliases
	//    map to the same slot (on canonicalize failure — dir vanished mid-call —
	//    fall back to the raw cleaned root so the key stays deterministic).
	canonical := root
	if c, cErr := CanonicalWorkspacePath(root); cErr == nil {
		canonical = c
	}
	key := WorkspaceKey(canonical)
	unlockKey := lockSerenaAutoRegisterKey(key)
	defer unlockKey()

	regPath, err := DefaultRegistryPath()
	if err != nil {
		return nil, err
	}

	// 4. GLOBAL install mutex — serialize the register → install → commit/rollback
	//    critical section across ALL roots (bot PR #253 finding 2). Held from
	//    before the registry row is made visible through the install commit (or
	//    its rollback), released before the readiness probe.
	serenaAutoRegisterInstallMu.Lock()
	installMuReleased := false
	releaseInstallMu := func() {
		if installMuReleased {
			return
		}
		installMuReleased = true
		serenaAutoRegisterInstallMu.Unlock()
	}
	defer releaseInstallMu()

	// 5. Registry session under the cross-process flock: load, idempotent re-read,
	//    allocate a pool port, persist the row. The flock is released right after
	//    Save (the install/reap/start steps do not touch the registry); the
	//    install mutex above is what serializes roots across those steps.
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
	defer releaseReg()
	if err = reg.Load(); err != nil {
		return nil, err
	}

	// Idempotency: a concurrent winner (or a prior call) may have already
	// registered this workspace. Return the existing row unchanged.
	if existing, ok := reg.GetSerena(key); ok {
		releaseReg()
		e := existing
		return &e, nil
	}

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
	if err = reg.PutSerena(newEntry); err != nil {
		return nil, fmt.Errorf("serena auto-register: put serena row: %w", err)
	}
	if err = reg.Save(); err != nil {
		// Save is atomic (tempfile + rename): a failure leaves the prior on-disk
		// registry intact, so there is nothing to roll back.
		return nil, fmt.Errorf("serena auto-register: save registry: %w", err)
	}
	releaseReg()

	// The row is committed on disk + visible. From here every PRE-COMMIT error
	// path must remove it. rollback() runs SYNCHRONOUSLY while the install mutex is
	// still held (called inline, not deferred) so no concurrent root ever observes
	// the row in the intent after we have decided to drop it. It re-acquires the
	// registry flock itself (removeSerenaRowRelocking) and is idempotent via rowSaved.
	rowSaved := true
	rollback := func(cause error) (*WorkspaceEntry, error) {
		if rowSaved {
			rowSaved = false
			if rbErr := removeSerenaRowRelocking(regPath, key); rbErr != nil {
				return nil, fmt.Errorf("%w; AND the registry rollback also failed: %v", cause, rbErr)
			}
		}
		return nil, cause
	}

	// 6. Build the in-memory dynamic-pool manifest + take the FRESH install
	//    snapshot. reReadSerenaEntriesForInstall reloads the registry under a
	//    re-acquired flock so the install fans out from the current on-disk set —
	//    capturing any row a concurrent external `mcphub workspace register`
	//    committed after our Load (other auto-registers are blocked on the install
	//    mutex, so only an external writer can have raced).
	dyn, bErr := BuildInMemorySerenaDynamicPoolManifest(catalog)
	if bErr != nil {
		return rollback(fmt.Errorf("serena auto-register: build dynamic-pool manifest: %w", bErr))
	}
	installWorkspaces, rrErr := reReadSerenaEntriesForInstall(regPath)
	if rrErr != nil {
		return rollback(fmt.Errorf("serena auto-register: re-read registry before install: %w", rrErr))
	}

	// 7. Cutover decision — INSIDE the install mutex so a concurrent first-call for
	//    another root sees this call's committed intent and does NOT re-reap.
	priorHasSpec, phErr := autoRegisterPriorIntentHasSpecFn()
	if phErr != nil {
		return rollback(fmt.Errorf("serena auto-register: inspect prior supervisor intent for runtime_spec: %w", phErr))
	}
	supRunning := true // an undeterminable liveness probe → treat as running (never skip a needed reap)
	if running, srErr := autoRegisterSupervisorRunningFn(); srErr == nil {
		supRunning = running
	}
	needReap := !priorHasSpec && supRunning
	needStart := !priorHasSpec
	if (needReap || needStart) && (autoRegisterReapFn == nil || autoRegisterStartFn == nil) {
		return rollback(fmt.Errorf("serena auto-register: %s is the first serena workspace (the supervisor intent carries no runtime_spec yet), so a one-time supervisor cutover is required to introduce it — but the cutover primitives are not wired on this build/platform (supervisor reap/restart is Windows-only in v0.5.0)", root))
	}

	// 8. REAP first (introduce-while-running only) so the spec-bearing write hits
	//    the §7.1 gate with no supervisor running. A reap failure lands BEFORE the
	//    install commit → fail loud; nothing was killed (the reap failed) so the
	//    daemons stay up, and rollback() removes our row.
	if needReap {
		if reapErr := autoRegisterReapFn(ctx); reapErr != nil {
			return rollback(fmt.Errorf("serena auto-register: reap the running supervisor before introducing runtime_spec for %s: %w", root, reapErr))
		}
	}

	// 9. Install (live-add OR post-reap introduce). The §7.1 gate passes: either
	//    the prior intent already has runtime_spec (live-add), or we just reaped so
	//    no supervisor is running (introduce).
	if _, iErr := autoRegisterInstallParsedManifestFn(ctx, a, dyn, InstallParsedManifestOpts{
		Writer:     io.Discard,
		Workspaces: installWorkspaces,
	}); iErr != nil {
		cause := fmt.Errorf("serena auto-register: install dynamic-pool descriptor for %s: %w", root, iErr)
		// If we reaped, restart the supervisor so it restores the prior (still-on-
		// disk) intent — never leave no-supervisor-running. Then rollback our row.
		if needReap {
			if startErr := autoRegisterStartFn(ctx); startErr != nil {
				cause = fmt.Errorf("%w; AND the recovery supervisor restart after the reap also failed: %v — NO supervisor is running, run `mcphub supervise`", cause, startErr)
			}
		}
		return rollback(cause)
	}

	// 10. COMMIT POINT (mirror migrate step 9). Disarm the rollback — the intent
	//     now owns the row; rolling the registry back here would split-state.
	rowSaved = false

	// 11. Bring the new daemon live. INTRODUCE → START the supervisor (it cold-
	//     reconciles the committed intent and spawns). LIVE-ADD → the supervisor is
	//     already up; nudge it to reconcile NOW (best-effort; the 60s poll backstops).
	if needStart {
		if startErr := autoRegisterStartFn(ctx); startErr != nil {
			// POST-COMMIT start failure: fail loud, NO registry rollback (the intent
			// is the commit point). Audit, then return; the operator restarts the
			// supervisor and the committed intent is reconciled.
			emitWorkspaceAutoRegisteredEvent(root, key, port, newEntry.Languages)
			return nil, fmt.Errorf("serena auto-register: workspace %s registered and intent committed but the supervisor start failed: %w — run `mcphub supervise` so the current binary reconciles the committed intent (the registry is intentionally NOT rolled back: the intent is the commit point)", root, startErr)
		}
	} else if _, recErr := autoRegisterReconcileFn(ctx, true); recErr != nil {
		_ = recErr // non-fatal: the 60s IntentWatcher reconcile is the backstop
	}

	// Release the install mutex before the readiness probe — readiness touches
	// neither the registry nor the intent, so other roots may proceed.
	releaseInstallMu()

	// 12. Readiness: poll the allocated port's /mcp with a synthetic initialize.
	//     Ready → return the entry. A timeout AFTER the committed install does NOT
	//     roll back (the workspace is registered + in the intent; the next call
	//     resolves it and the daemon will be up) — return an error wrapping the
	//     readiness failure so the router maps it to 503 and the client retries.
	if rdErr := autoRegisterReadinessFn(port, serenaAutoRegisterReadinessTimeout); rdErr != nil {
		emitWorkspaceAutoRegisteredEvent(root, key, port, newEntry.Languages)
		return nil, fmt.Errorf("serena auto-register: workspace %s registered and intent committed, but the per-workspace daemon on port %d was not ready in time: %w (the workspace IS registered; retry the call — the supervisor is bringing the daemon up)", root, port, rdErr)
	}

	// 13. Audit: emit the success event (best-effort; never fatal).
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

// reReadSerenaEntriesForInstall reloads the registry under a freshly-acquired
// flock and returns the current serena rows — the authoritative snapshot the
// install fans out from (bot PR #253 finding 2). Always returns a NON-nil slice
// (a nil Workspaces would make InstallParsedManifest silently drop the server's
// existing daemon rows). The lock is always released before returning.
func reReadSerenaEntriesForInstall(regPath string) ([]WorkspaceEntry, error) {
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return nil, fmt.Errorf("re-acquire registry lock: %w", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return nil, fmt.Errorf("reload registry: %w", err)
	}
	entries := reg.SerenaEntries()
	if entries == nil {
		entries = []WorkspaceEntry{}
	}
	return entries, nil
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
// The install / immediate-reconcile / readiness / reap / start steps each
// require a real supervisor or a live network listener; they are routed through
// package-level function vars (defaulting to the real impls) so unit tests
// exercise the register + cutover-decision + rollback + idempotency + concurrency
// logic without a supervisor. Mirrors the migrate seam idiom (installParsedManifestFn
// / reconcileSerenaClientsFn / migrateSerenaStartFn in internal/cli/migrate_serena.go).

// autoRegisterInstallParsedManifestFn is the seam over (*API).InstallParsedManifest.
// Default calls the real installer; tests override it to inject install success/
// failure without reaching the scheduler / state-file pipeline.
var autoRegisterInstallParsedManifestFn = func(ctx context.Context, a *API, m *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
	return a.InstallParsedManifest(ctx, m, opts)
}

// autoRegisterReconcileFn is the seam over DialSupervisorIPCReconcile. Default
// nudges the running supervisor to reconcile NOW (live-add path); tests override
// it to assert it is called with apply=true (and that a transient/unavailable
// error is non-fatal).
var autoRegisterReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
	return DialSupervisorIPCReconcile(ctx, apply)
}

// autoRegisterReadinessFn is the seam over verifyProxyReady. Default polls the
// allocated port's /mcp; tests override it to return nil (ready) or an error
// (post-commit timeout, which must NOT roll back).
var autoRegisterReadinessFn = func(port int, timeout time.Duration) error {
	return verifyProxyReady(port, timeout)
}

// autoRegisterPriorIntentHasSpecFn reports whether the on-disk supervisor intent
// already carries a serena runtime_spec row (seam; tests override). A missing
// intent file → (false, nil) (nothing introduced yet). This drives the LIVE-ADD
// vs INTRODUCE branch (bot PR #253 finding 1).
var autoRegisterPriorIntentHasSpecFn = func() (bool, error) {
	sd, err := DaemonStateDir()
	if err != nil {
		return false, err
	}
	intent, err := ReadSupervisorIntent(filepath.Join(sd, supervisorIntentFileLeaf))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if intent == nil {
		return false, nil
	}
	return intent.HasRuntimeSpecRow(), nil
}

// autoRegisterSupervisorRunningFn reports whether a supervisor process is
// currently running (seam; tests override). Default: the cross-platform
// supervisor-lock liveness probe (the same signal the §7.1 install gate reads).
// Only consulted on the INTRODUCE branch to decide whether a reap is needed.
var autoRegisterSupervisorRunningFn = func() (bool, error) {
	sd, err := DaemonStateDir()
	if err != nil {
		return false, err
	}
	running, _, perr := SupervisorRunningUnderStateDir(sd)
	return running, perr
}

// autoRegisterReapFn / autoRegisterStartFn are the supervisor cutover primitives
// used on the INTRODUCE branch (bot PR #253 finding 1): reap the running
// supervisor before the first spec-bearing write, then start the current binary.
// They are nil until the CLI wires them via SetSerenaAutoRegisterCutoverPrimitives
// at GUI-server startup (the real reap/start live in internal/cli and are
// Windows-only; api cannot import cli). When nil, the introduce path fails loud.
var (
	autoRegisterReapFn  func(ctx context.Context) error
	autoRegisterStartFn func(ctx context.Context) error
)

// SetSerenaAutoRegisterCutoverPrimitives wires the supervisor reap/start used by
// AutoRegisterSerenaWorkspace when introducing the FIRST serena runtime_spec
// (the one-time cutover). Called once from CLI GUI-server startup. Passing nil
// for either clears the wiring (the introduce path then fails loud). Safe to call
// before the GUI starts serving; the package-global is read under the install
// mutex on the introduce branch.
func SetSerenaAutoRegisterCutoverPrimitives(reap, start func(ctx context.Context) error) {
	autoRegisterReapFn = reap
	autoRegisterStartFn = start
}

// --- per-workspace-key concurrency guard -----------------------------------

// serenaAutoRegisterInstallMu serializes the register → install → commit/rollback
// critical section across ALL workspace roots (bot PR #253 finding 2).
var serenaAutoRegisterInstallMu sync.Mutex

// serenaAutoRegisterKeyMu guards the serenaAutoRegisterKeyLocks map itself.
var serenaAutoRegisterKeyMu sync.Mutex

// serenaAutoRegisterKeyLocks holds one *sync.Mutex per in-flight workspace key
// so concurrent AutoRegisterSerenaWorkspace calls for the SAME root serialize
// (register exactly once; the loser re-reads the winner's row), while calls for
// DISTINCT roots proceed to the install mutex. Entries are intentionally never
// deleted: the key space is the bounded set of a single user's workspaces (<100),
// and a reaper would reintroduce a lost-wakeup race against an in-flight holder.
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
