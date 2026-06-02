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

	// 5b. Abort BEFORE any registration mutation if the auto-register context is
	//     already done (bot PR #253 P2). The router runs auto-register on a
	//     detached, up-to-45s context and CANCELS it when the client session is
	//     terminated (DELETE / idle-sweep) during the window; a terminated session
	//     must not allocate a pool port or write a registry row. ctx.Err() also
	//     covers the deadline. Nothing is saved yet, so this is a clean return.
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("serena auto-register: aborted before registering %s: %w (the client session was terminated or the deadline expired)", root, cerr)
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
		WorkspaceKey: key,
		// Store the CANONICAL (symlink-resolved) path, matching manual
		// registration (internal/cli/workspace_cmd.go:237). The resolver matches
		// an incoming absolute path's CanonicalWorkspacePath against stored rows
		// via canonicalizeWorkspacePath (which does NOT resolve symlinks), so a
		// row storing the unresolved `root` would never resolve and every call
		// would re-auto-register (bot PR #253 P2).
		WorkspacePath: canonical,
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
	// NOTE (bot PR #253 P2): the flock is intentionally NOT released here. It is
	// held continuously through the install commit (or rollback) below so the
	// not-yet-committed registry row is invisible to OTHER mcphub processes — a
	// concurrent migrate/install must not read this uncommitted row, commit a
	// supervisor intent for it, and then be left with a daemon whose registry row
	// our pre-commit rollback removed (split-state). The deferred releaseReg frees
	// the flock on any error path; the commit path frees it explicitly below.

	// The row is on disk under our held flock. Every PRE-COMMIT error path must
	// remove it. rollback() runs while the flock is STILL HELD (so no other process
	// ever observes the row we are about to drop) and operates on the held reg
	// directly — no re-lock. Idempotent via rowSaved.
	rowSaved := true
	rollback := func(cause error) (*WorkspaceEntry, error) {
		if rowSaved {
			rowSaved = false
			reg.RemoveSerena(key)
			if rbErr := reg.Save(); rbErr != nil {
				return nil, fmt.Errorf("%w; AND the registry rollback also failed: %v", cause, rbErr)
			}
		}
		return nil, cause
	}

	// 6. Build the in-memory dynamic-pool manifest + take the install snapshot.
	//    Because we hold the registry flock CONTINUOUSLY through the install (bot
	//    PR #253 P2), reg.SerenaEntries() is the authoritative current on-disk set —
	//    no concurrent in-process OR cross-process writer can have raced since our
	//    Load, so no separate re-read under a fresh lock is needed.
	dyn, bErr := BuildInMemorySerenaDynamicPoolManifest(catalog)
	if bErr != nil {
		return rollback(fmt.Errorf("serena auto-register: build dynamic-pool manifest: %w", bErr))
	}
	installWorkspaces := reg.SerenaEntries()
	if installWorkspaces == nil {
		installWorkspaces = []WorkspaceEntry{}
	}

	// 7. Cutover decision — INSIDE the install mutex so a concurrent first-call for
	//    another root sees this call's committed intent and does NOT re-reap.
	priorHasSpec, phErr := autoRegisterPriorIntentHasSpecFn()
	if phErr != nil {
		return rollback(fmt.Errorf("serena auto-register: inspect prior supervisor intent for runtime_spec: %w", phErr))
	}
	supRunning := true // undeterminable liveness → treat as running for the REAP decision (never skip a needed reap)
	livenessKnown := true
	if running, srErr := autoRegisterSupervisorRunningFn(); srErr == nil {
		supRunning = running
	} else {
		livenessKnown = false
	}
	needReap := !priorHasSpec && supRunning
	// needStart fires whenever the daemon must be brought live by a supervisor
	// start: INTRODUCING runtime_spec (!priorHasSpec); a live-add whose supervisor is
	// NOT running (!supRunning — e.g. it crashed after migration while the GUI stayed
	// up); OR a live-add whose liveness is UNDETERMINED (the probe errored — bot PR
	// #253 r5 P2). In the live-add-to-a-stopped/unknown-pool cases a best-effort IPC
	// reconcile would silently hit nothing, so readiness would time out and leave a
	// registered-but-never-spawned workspace; START brings it live (no reap — the
	// prior intent is already this binary's shape; a redundant start no-ops via the
	// supervisor.lock singleton when one is in fact running). The cutover-support gate
	// below then fails loud PRE-commit on a platform that cannot start.
	needStart := !priorHasSpec || !supRunning || !livenessKnown
	// PRE-COMMIT cutover-support gate (bot PR #253 P2). The CLI wires NON-nil
	// reap/start functions on EVERY platform, but off-Windows they are unsupported
	// stubs (defaultMigrateSerenaStart returns errSupervisorRestartUnsupported). A
	// nil-ONLY check would let the introduce path reap, commit the intent + the
	// registry row, and only THEN fail at the post-commit start — leaving a
	// spec-bearing intent the platform cannot bring live (a fresh host or
	// zero-workspace migration on Linux/macOS). So refuse BEFORE any reap/install
	// when the cutover is unwired OR the platform's start primitive is unsupported,
	// mirroring migrate's pre-commit migrateSerenaStartSupportedFn gate. rollback()
	// removes our row; nothing is committed.
	if needReap || needStart {
		if autoRegisterReapFn == nil || autoRegisterStartFn == nil ||
			autoRegisterStartSupportedFn == nil || !autoRegisterStartSupportedFn() {
			return rollback(fmt.Errorf("serena auto-register: %s is the first serena workspace (the supervisor intent carries no runtime_spec yet), so a one-time supervisor cutover (reap → install → start) is required to introduce it — but the supervisor reap/restart primitive is not supported on this build/platform (Windows-only in v0.5.0)", root))
		}
	}

	// commitCtx drives the irreversible cutover steps (reap → install → post-commit
	// start, plus the failPreCommit recovery-restart) once we pass the 7c session
	// gate below. It is DERIVED from ctx via context.WithoutCancel so a mid-cutover
	// session DELETE/idle-sweep (which cancels ctx through the router's watcher)
	// cannot abort them — a reaped-but-not-restarted supervisor leaves NO supervisor
	// running. The request ctx still gates the PRE-commit phase (7c + the install's
	// own commit-point ctx check, fix #1); commitCtx severs cancel ONLY for the
	// must-complete steps, bounded by serenaAutoRegisterCommitTimeout (bot PR #253 r6).
	// Invariant (docs/serena-lifecycle-invariants.md §2): a step that must complete
	// AFTER the reap goes on commitCtx; a step that must honor a session termination
	// goes on the request ctx. Placing a step on the wrong side reintroduces either a
	// half-cutover (no supervisor) or a terminated-session-still-mutates bug.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), serenaAutoRegisterCommitTimeout)
	defer commitCancel()

	// failPreCommit rolls back the registry row and — if we reaped a supervisor —
	// restarts it (via commitCtx) so the prior (still-on-disk) intent is reconciled
	// (never leave no-supervisor-running). Shared by the reap-fail and install-fail
	// paths AFTER a reap may have run. (The session-abort gate 7c below uses plain
	// rollback() because it runs BEFORE the reap, so the old supervisor is still up
	// and must NOT be restarted.)
	failPreCommit := func(cause error) (*WorkspaceEntry, error) {
		if needReap {
			if startErr := autoRegisterStartFn(commitCtx); startErr != nil {
				cause = fmt.Errorf("%w; AND the recovery supervisor restart after the reap also failed: %v — NO supervisor is running, run `mcphub supervise`", cause, startErr)
			}
		}
		return rollback(cause)
	}

	// 7c. Last session-liveness gate BEFORE the irreversible install commit (bot
	//     PR #253 P2). A session DELETE/sweep during the registry/cutover phase
	//     cancels ctx; abort here — before any reap or intent write — so a
	//     terminated session never lands a supervisor-intent daemon. No reap has
	//     run yet (the old supervisor is still up), so plain rollback() suffices.
	if cerr := ctx.Err(); cerr != nil {
		return rollback(fmt.Errorf("serena auto-register: the client session was terminated before %s reached the supervisor-intent commit: %w", root, cerr))
	}

	// 8. REAP first (introduce-while-running only) so the spec-bearing write hits
	//    the §7.1 gate with no supervisor running. A reap failure lands BEFORE the
	//    install commit → fail loud; nothing was killed (the reap failed) so the
	//    daemons stay up, and rollback() removes our row.
	if needReap {
		if reapErr := autoRegisterReapFn(commitCtx); reapErr != nil {
			return rollback(fmt.Errorf("serena auto-register: reap the running supervisor before introducing runtime_spec for %s: %w", root, reapErr))
		}
	}

	// 9. Install (live-add OR post-reap introduce). The §7.1 gate passes: either
	//    the prior intent already has runtime_spec (live-add), or we just reaped so
	//    no supervisor is running (introduce).
	// intentPath is intentionally discarded: the post-commit fan-out verify that
	// consumed it is gone (replaced by RequireWorkspaceKey, which guarantees presence
	// PRE-commit), and the commit point below keys off rowSaved, not the path.
	_, iErr := autoRegisterInstallParsedManifestFn(ctx, a, dyn, InstallParsedManifestOpts{
		Writer:     io.Discard,
		Workspaces: installWorkspaces,
		// RequireWorkspaceKey makes the install FAIL PRE-COMMIT if its stale-row
		// filter dropped our triggering workspace (its dir vanished between the Save
		// above and the merge). This REPLACES the old post-commit fan-out verify (bot
		// PR #253 r6 P2): a dropped workspace now errors BEFORE the intent write, so
		// the prior intent stays intact on disk and failPreCommit's recovery-restart
		// restores it — instead of committing an intent that drops it (which on the
		// FIRST introduce also replaced the legacy serena rows with nothing). The
		// install is passed the cancellable ctx so its own commit-point ctx check
		// (fix #1) honors a session terminated mid-install.
		RequireWorkspaceKey: key,
	})
	if iErr != nil {
		// Covers BOTH a genuine install failure AND the RequireWorkspaceKey
		// stale-drop (the install errored BEFORE the write, so the prior intent is
		// intact on disk): failPreCommit rolls our row back and — if we reaped —
		// recovery-restarts on that still-intact prior intent.
		return failPreCommit(fmt.Errorf("serena auto-register: install dynamic-pool descriptor for %s: %w", root, iErr))
	}

	// 10. COMMIT POINT (mirror migrate step 9). Disarm the rollback + release the
	//     flock — the intent now owns the row AND our daemon is in it; rolling the
	//     registry back here would split-state. Releasing the flock publishes the
	//     committed row to other processes.
	rowSaved = false
	releaseReg()

	// 11. Bring the new daemon live.
	//   - INTRODUCE or stopped/undetermined pool (needStart) → START the supervisor
	//     (it cold-reconciles the committed intent and spawns). The cutover-support
	//     gate above already verified the start primitive for this path.
	//   - LIVE-ADD (supervisor sampled running) → nudge it to reconcile NOW
	//     (best-effort; the 60s IntentWatcher poll backstops). BUT the liveness probe
	//     is a SNAPSHOT: the supervisor can EXIT between that probe and this reconcile
	//     (bot PR #253 r7 P2). An UNAVAILABLE reconcile means the committed intent has
	//     no running supervisor to spawn the daemon, so fall through to START one —
	//     the needStart outcome. (A reconcile error while the supervisor is still UP
	//     is a transient nudge failure; keep it non-fatal — the 60s poll backstops.)
	needPostCommitStart := needStart
	if !needStart {
		if _, recErr := autoRegisterReconcileFn(commitCtx, true); recErr != nil {
			if errors.Is(recErr, ErrSupervisorIPCUnavailable) {
				// Supervisor exited post-probe → it must be started. The live-add
				// decision skipped the cutover-support gate (it fires only for
				// needReap||needStart), so verify the start primitive HERE; an
				// unwired/unsupported platform cannot self-heal → fail loud.
				if autoRegisterStartFn == nil || autoRegisterStartSupportedFn == nil || !autoRegisterStartSupportedFn() {
					emitWorkspaceAutoRegisteredEvent(canonical, key, port, newEntry.Languages)
					return nil, fmt.Errorf("serena auto-register: workspace %s registered and intent committed, but the supervisor exited after the liveness probe and the start primitive is unavailable on this build/platform — run `mcphub supervise` so the current binary reconciles the committed intent", root)
				}
				needPostCommitStart = true
			}
			// else: the supervisor is up; the reconcile nudge failed transiently; the
			// 60s IntentWatcher reconcile is the backstop (non-fatal).
		}
	}
	if needPostCommitStart {
		if startErr := autoRegisterStartFn(commitCtx); startErr != nil {
			// POST-COMMIT start failure: fail loud, NO registry rollback (the intent
			// is the commit point). Audit, then return; the operator restarts the
			// supervisor and the committed intent is reconciled.
			emitWorkspaceAutoRegisteredEvent(canonical, key, port, newEntry.Languages)
			return nil, fmt.Errorf("serena auto-register: workspace %s registered and intent committed but the supervisor start failed: %w — run `mcphub supervise` so the current binary reconciles the committed intent (the registry is intentionally NOT rolled back: the intent is the commit point)", root, startErr)
		}
	}

	// Release the install mutex before the readiness probe — readiness touches
	// neither the registry nor the intent, so other roots may proceed.
	releaseInstallMu()

	// 12. Readiness: poll the allocated port's /mcp with a synthetic initialize,
	//     bounded by BOTH the fixed cold-start budget AND the router's REMAINING
	//     auto-register deadline (bot PR #253 P3) — a slow reap/install/start must
	//     not let this fixed-20s probe block past the handler's advertised 45s
	//     window. A timeout AFTER the committed install does NOT roll back (the
	//     workspace is registered + in the intent; the next call resolves it and
	//     the daemon will be up) — return an error the router maps to 503 so the
	//     client retries.
	readinessTimeout := serenaAutoRegisterReadinessTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < readinessTimeout {
			readinessTimeout = rem
		}
	}
	if readinessTimeout <= 0 {
		// The router's auto-register budget is already spent — do not block at all.
		emitWorkspaceAutoRegisteredEvent(canonical, key, port, newEntry.Languages)
		return nil, fmt.Errorf("serena auto-register: workspace %s registered and intent committed, but the auto-register deadline expired before the daemon on port %d became ready (the workspace IS registered; retry the call — the supervisor is bringing the daemon up)", root, port)
	}
	if rdErr := autoRegisterReadinessFn(port, readinessTimeout); rdErr != nil {
		emitWorkspaceAutoRegisteredEvent(canonical, key, port, newEntry.Languages)
		return nil, fmt.Errorf("serena auto-register: workspace %s registered and intent committed, but the per-workspace daemon on port %d was not ready in time: %w (the workspace IS registered; retry the call — the supervisor is bringing the daemon up)", root, port, rdErr)
	}

	// 13. Audit: emit the success event (best-effort; never fatal).
	emitWorkspaceAutoRegisteredEvent(canonical, key, port, newEntry.Languages)

	e := newEntry
	return &e, nil
}

// serenaAutoRegisterReadinessTimeout bounds the post-spawn readiness probe. It
// is generous enough for a cold `uvx`-spawned serena child (download + Python
// startup) on a warm cache but still bounded so a wedged spawn returns to the
// router (→ 503 + client retry) rather than blocking the call indefinitely.
const serenaAutoRegisterReadinessTimeout = 20 * time.Second

// serenaAutoRegisterCommitTimeout bounds the cutover steps (reap → install →
// post-commit start, plus the failPreCommit recovery-restart) once auto-register
// passes the 7c session gate (bot PR #253 r6 P2). Those steps run on commitCtx, a
// context DERIVED from the request ctx via context.WithoutCancel, so a mid-cutover
// session DELETE/idle-sweep (which cancels the request ctx) cannot abort a
// reap-then-not-restart half cutover. This timeout still bounds them: the start
// primitive self-verifies with a 30s IPC reconcile-ready poll, so 60s covers a
// reap + install + that poll.
const serenaAutoRegisterCommitTimeout = 60 * time.Second

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
	// Auto-register requires an ABSOLUTE path (bot PR #253 P1). A relative
	// tool-argument path is relative to the AGENT's environment, which the GUI
	// server cannot know; resolving it against the GUI's own cwd (filepath.Abs)
	// would discover the WRONG project (or none) — silently registering a
	// directory the agent never named. Refuse so the router returns not-found
	// instead. (Relative paths resolve only for ALREADY-registered workspaces,
	// via the resolver's per-root matching; a relative FIRST call cannot be
	// located and must not auto-register.)
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: %q is not an absolute path (auto-register cannot locate a relative path against the GUI's working directory)", ErrNotASerenaProject, p)
	}
	abs := filepath.Clean(p)
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
// .serena/project.yml. Only the language(s) matter here; every other serena
// field is left untouched on disk (auto-register never rewrites project.yml).
// Replicated minimally from internal/cli/workspace_cmd.go:745's serenaProjectYml
// (the api package cannot import cli). Both the modern plural `languages:` list
// and Serena's legacy singular `language:` scalar are read (bot PR #253 P2).
type serenaProjectYMLForAutoRegister struct {
	Languages []string `yaml:"languages"`
	Language  string   `yaml:"language"` // legacy singular form (`language: python`)
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
		cleaned = append(cleaned, strings.TrimSpace(l))
	}
	// Legacy singular fallback (bot PR #253 P2): an older Serena project.yml uses
	// `language: python` instead of the plural list. Auto-register exists to pick
	// up ALREADY-created Serena projects on first use, so a valid legacy project
	// must register, not 422. Only consult the scalar when the plural list yielded
	// nothing (a project carrying both is governed by its plural list).
	if len(cleaned) == 0 && strings.TrimSpace(doc.Language) != "" {
		cleaned = append(cleaned, strings.TrimSpace(doc.Language))
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("%w (%s)", ErrNoLanguages, path)
	}
	return cleaned, nil
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

// autoRegisterStartSupportedFn reports whether the platform's supervisor START
// primitive is actually WIRED (not merely a non-nil unsupported stub). nil or a
// false return → the introduce cutover refuses BEFORE committing the intent (bot
// PR #253 P2). Wired by the CLI to defaultMigrateSerenaStartSupported (true on
// Windows; false elsewhere in v0.5.0).
var autoRegisterStartSupportedFn func() bool

// SetSerenaAutoRegisterCutoverPrimitives wires the supervisor reap/start used by
// AutoRegisterSerenaWorkspace when introducing the FIRST serena runtime_spec
// (the one-time cutover), plus the start-supported predicate that gates the
// introduce path BEFORE any commit. Called once from CLI GUI-server startup.
// Passing nil for reap/start clears the wiring; a nil-or-false startSupported
// makes the introduce path fail loud pre-commit. Safe to call before the GUI
// starts serving; the package-globals are read under the install mutex on the
// introduce branch.
func SetSerenaAutoRegisterCutoverPrimitives(reap, start func(ctx context.Context) error, startSupported func() bool) {
	autoRegisterReapFn = reap
	autoRegisterStartFn = start
	autoRegisterStartSupportedFn = startSupported
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
