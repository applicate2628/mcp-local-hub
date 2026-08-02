package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

var registerSupervisorReconcileFn = DialSupervisorIPCReconcile

// LSPTaskNameForWorkspaceLanguage returns the bare task identity used by
// registry rows and legacy scheduler tasks for one workspace-scoped LSP proxy.
func LSPTaskNameForWorkspaceLanguage(wsKey, lang string) string {
	return fmt.Sprintf("mcp-local-hub-lsp-%s-%s", wsKey, lang)
}

// LSPIntentTaskNameForWorkspaceLanguage returns the canonical
// supervisor-intent task identity for one workspace-scoped LSP proxy.
func LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang string) string {
	return canonicalIntentTaskKey(LSPTaskNameForWorkspaceLanguage(wsKey, lang))
}

// supervisorPostWriteDeps is the transaction-local dependency bundle for the
// existing post-AddEntry supervised chain. It is deliberately private and
// concrete: this seam exists only to make every provisional-write failure
// deterministic without replacing production owners process-wide.
type supervisorPostWriteDeps struct {
	upsertIntent       func(WorkspaceEntry, string) (compensation, error)
	writeRunningIntent func(string, stopIntentCompensationSink) (string, error)
	reconcile          func(context.Context, bool) (ReconcileResponse, error)
	readiness          func(int, time.Duration) error
}

// normalizeSupervisorPostWriteDeps captures production owners at Register call
// entry and fills any omitted test dependency. The returned value is complete
// and immutable for that invocation even if an older package test seam changes
// later in the process.
func normalizeSupervisorPostWriteDeps(a *API, provided supervisorPostWriteDeps) supervisorPostWriteDeps {
	resolved := supervisorPostWriteDeps{
		upsertIntent:       a.upsertLSPSupervisorIntent,
		writeRunningIntent: a.writeRegisterRunningIntentForTask,
		reconcile:          registerSupervisorReconcileFn,
		readiness:          proxyReadinessFn,
	}
	if provided.upsertIntent != nil {
		resolved.upsertIntent = provided.upsertIntent
	}
	if provided.writeRunningIntent != nil {
		resolved.writeRunningIntent = provided.writeRunningIntent
	}
	if provided.reconcile != nil {
		resolved.reconcile = provided.reconcile
	}
	if provided.readiness != nil {
		resolved.readiness = provided.readiness
	}
	return resolved
}

// enrollSupervisorIntentUndo is the single participant-admission boundary for
// every registration caller that mutates supervisor-intent. A non-nil undo is
// transaction-owned even when the mutation also returned an error, because the
// writer may have failed after a partial durable change.
func enrollSupervisorIntentUndo(transaction *registrationTransaction, label string, undo compensation) {
	if undo != nil {
		transaction.AddCompensation(label, undo)
	}
}

// BuildSupervisorDaemonForLSP materializes a supervisor-owned descriptor for
// the workspace-proxy launcher. The supervisor owns the mcphub proxy process;
// the proxy then owns the real stdio LSP backend through StdioHost.
func BuildSupervisorDaemonForLSP(entry WorkspaceEntry, mcphubBinaryPath string) SupervisorDaemon {
	taskName := LSPIntentTaskNameForWorkspaceLanguage(entry.WorkspaceKey, entry.Language)
	return SupervisorDaemon{
		TaskName: taskName,
		Server:   "mcp-language-server",
		Daemon:   fmt.Sprintf("lsp-%s-%s", entry.WorkspaceKey, entry.Language),
		Command:  mcphubBinaryPath,
		Args: []string{
			"daemon", "workspace-proxy",
			"--port", fmt.Sprintf("%d", entry.Port),
			"--workspace", entry.WorkspacePath,
			"--language", entry.Language,
		},
		Workspace: entry.WorkspacePath,
		Port:      entry.Port,
		// LSP workspace-proxy children bind their port promptly (before the
		// heavy stdio LSP backend materializes lazily on the first tools/call),
		// but the same slow-bind allowance the serena-proxy builder stamps
		// (#488, P1b) is applied here so a proxy that is momentarily slow to
		// bind post-spawn is not terminate-first-then-respawned by the liveness
		// sweep. The field exists on SupervisorDaemon since #488; 120s matches
		// the serena-proxy precedent (supervisor_intent_build.go).
		StartupBindDeadlineSeconds: 120,
	}
}

// LSPRegistryRowBacksDescriptor reports whether the supervisor-intent LSP
// workspace-proxy descriptor d still has a backing registry row in
// workspaces.yaml — i.e. whether `mcphub daemon workspace-proxy --workspace
// <path> --language <lang>` would find its (workspace_key, language) entry
// instead of exiting 1 "not registered". The supervisor reconciler calls this
// (via the Reconciler.LSPRegistryHasRow seam) to EXCLUDE orphaned LSP
// descriptors from the spawn-desired set rather than spawn-and-quarantine them.
//
// This single-descriptor form loads (locks + reads + parses
// workspaces.yaml) on every call — fine for a one-off lookup, but a
// reconcile pass calling it once per LSP daemon repeats that full
// lock+load for every descriptor. A caller iterating many descriptors in
// one pass (the supervisor reconciler) should instead load the registry
// once with OpenLSPRegistryForReconcile and call
// LSPRegistryRowBacksDescriptorIn for each descriptor against that single
// loaded registry. See LSPRegistryRowBacksDescriptorIn for the shared
// predicate and fail-open contract; this wrapper just adds the per-call
// load around it.
func LSPRegistryRowBacksDescriptor(d SupervisorDaemon) bool {
	reg, ok := OpenLSPRegistryForReconcile()
	if !ok {
		// Load/lock failure — fail OPEN (never suppress a legitimate spawn
		// on a transient registry hiccup).
		return true
	}
	return LSPRegistryRowBacksDescriptorIn(d, reg)
}

// OpenLSPRegistryForReconcile loads workspaces.yaml ONCE (brief-retry lock,
// read, parse) and returns the loaded *Registry for repeated
// LSPRegistryRowBacksDescriptorIn lookups within one reconcile pass. The
// second return is false when the registry path can't be resolved, the
// lock can't be acquired, or the load fails — callers must treat a false
// ok the same way LSPRegistryRowBacksDescriptor's single-call form does:
// fail OPEN (do not exclude any descriptor) rather than guess.
//
// The returned Registry holds an in-memory snapshot; it does not need to
// stay locked for the lookups (Get is a pure in-memory map read), so this
// releases the flock immediately after Load succeeds — a long-held lock
// across an entire reconcile pass would otherwise contend with concurrent
// registry writers (workspace register/unregister) for no benefit.
func OpenLSPRegistryForReconcile() (*Registry, bool) {
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return nil, false
	}
	reg := NewRegistry(regPath)
	unlock, ok, err := tryLockRegistryBrief(reg)
	if err != nil || !ok {
		return nil, false
	}
	loadErr := reg.Load()
	releaseErr := unlock()
	if releaseErr != nil {
		fmt.Fprintf(os.Stderr, "open LSP registry for reconcile: release registry lock %s: %v\n", regPath, releaseErr)
	}
	if loadErr != nil || releaseErr != nil {
		return nil, false
	}
	return reg, true
}

// LSPRegistryRowBacksDescriptorIn is the registry-injected form of
// LSPRegistryRowBacksDescriptor: same predicate, same fail-open contract,
// but against an already-loaded *Registry (e.g. from
// OpenLSPRegistryForReconcile) instead of locking + loading
// workspaces.yaml on every call. Use this when checking many descriptors
// against the same registry snapshot in one pass.
//
// The (workspace_path, language) pair is read from the descriptor's flat
// argv (--workspace / --language), falling back to the Workspace field for
// the path. The lookup mirrors the unregister path's canonical/legacy-key
// tolerance: the canonical EvalSymlinks key is tried first, then the
// pre-symlink legacy key, so a row written under either canonicalization
// scheme is found. A non-LSP descriptor or a descriptor missing its
// language returns true (fail OPEN — never suppress a legitimate spawn;
// the worst case is the pre-fix behavior of spawning a row that then fails
// loud, which is strictly safer than silently dropping a backed daemon).
func LSPRegistryRowBacksDescriptorIn(d SupervisorDaemon, reg *Registry) bool {
	lang := lspDescriptorArgValue(d.Args, "--language")
	if lang == "" {
		return true
	}
	wsPath := lspDescriptorArgValue(d.Args, "--workspace")
	if wsPath == "" {
		wsPath = d.Workspace
	}
	if wsPath == "" {
		return true
	}
	if reg == nil {
		return true
	}
	// Canonical (EvalSymlinks) key first, then the legacy pre-symlink key —
	// matching unregisterWithManifest's two-key tolerance so a row written
	// under either scheme is recognized.
	if canonical, cerr := CanonicalWorkspacePathForCleanup(wsPath); cerr == nil {
		if _, found := reg.Get(WorkspaceKey(canonical), lang); found {
			return true
		}
	}
	if legacy, lerr := CanonicalWorkspacePathLegacyCompat(wsPath); lerr == nil {
		if _, found := reg.Get(WorkspaceKey(legacy), lang); found {
			return true
		}
	}
	return false
}

// lspDescriptorArgValue returns the value following the first occurrence of
// flag in a flat `--flag value` argv, or "" if absent.
func lspDescriptorArgValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func killObservedLiveLSPProxy(port int, taskName string, observedLive bool) error {
	if !observedLive || port <= 0 || killByPortFn == nil {
		return nil
	}
	if err := killByPortFn(port, 5*time.Second); err != nil {
		return fmt.Errorf("kill legacy LSP proxy on port %d (task %s): %w", port, taskName, err)
	}
	return nil
}

func (a *API) registerOneLanguageSupervised(
	m *config.ServerManifest,
	spec config.LanguageSpec,
	canonical, wsKey, lang string,
	opts RegisterOpts,
	reg *Registry,
	sch testScheduler,
	allClients map[string]registerClient,
	bindings []config.ClientBinding,
	w io.Writer,
	transaction *registrationTransaction,
) (result registeredLanguageResult, err error) {
	unlock, err := reg.Lock()
	if err != nil {
		return registeredLanguageResult{}, fmt.Errorf("acquire registry lock: %w", err)
	}
	lockToken := transaction.AddFinalizer("release supervised registry lock for "+wsKey+"/"+lang, unlock)
	if err := reg.Load(); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("load registry: %w", err)
	}

	prior, had := reg.Get(wsKey, lang)
	var port int
	if had {
		port = prior.Port
	} else {
		p, err := AllocatePort(reg, *m.PortPool)
		if err != nil {
			return registeredLanguageResult{}, err
		}
		port = p
	}
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, lang)
	canonicalExe, err := canonicalMcphubPath()
	if err != nil {
		return registeredLanguageResult{}, err
	}
	if _, err := os.Stat(canonicalExe); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
	}

	var priorXML []byte
	if xml, err := sch.ExportXML(taskName); err == nil {
		priorXML = xml
	} else if !errors.Is(err, scheduler.ErrTaskNotFound) {
		return registeredLanguageResult{}, fmt.Errorf("export prior task %s: %w", taskName, err)
	}
	capturedTaskName := taskName
	capturedPriorXML := priorXML
	capturedPort := port
	legacyTaskTouched := false
	transaction.AddCompensation("restore supervised legacy task "+capturedTaskName, func() error {
		if !legacyTaskTouched {
			return nil
		}
		var joined error
		if capturedPort > 0 {
			if killErr := killByPortFn(capturedPort, 5*time.Second); killErr != nil {
				joined = errors.Join(joined, fmt.Errorf("kill replacement proxy on port %d: %w", capturedPort, killErr))
			}
		}
		if len(capturedPriorXML) > 0 {
			if importErr := sch.ImportXML(capturedTaskName, capturedPriorXML); importErr != nil {
				joined = errors.Join(joined, fmt.Errorf("restore prior task %s: %w", capturedTaskName, importErr))
			}
			if runErr := sch.Run(capturedTaskName); runErr != nil {
				joined = errors.Join(joined, fmt.Errorf("restart prior task %s: %w", capturedTaskName, runErr))
			}
			if joined == nil {
				fmt.Fprintf(w, "  rollback: restored + restarted scheduler task %s\n", capturedTaskName)
			}
		}
		return joined
	})
	if (had || len(priorXML) > 0) && port > 0 {
		legacyTaskTouched = true
		legacyPortReady := proxyReadinessFn(port, lspExistingProxyProbeTimeout) == nil
		if err := killObservedLiveLSPProxy(port, taskName, legacyPortReady); err != nil {
			return registeredLanguageResult{}, err
		}
	}
	if len(priorXML) > 0 {
		legacyTaskTouched = true
	}
	if err := sch.Delete(taskName); err != nil && !errors.Is(err, scheduler.ErrTaskNotFound) {
		return registeredLanguageResult{}, fmt.Errorf("delete legacy task %s before supervised promote: %w", taskName, err)
	}

	entryNameByClient := map[string]string{}
	if had {
		for k, v := range prior.ClientEntries {
			entryNameByClient[k] = v
		}
	}
	for _, b := range bindings {
		client, ok := allClients[b.Client]
		if !ok || !client.Exists() {
			continue
		}
		if _, already := entryNameByClient[b.Client]; !already {
			entryNameByClient[b.Client] = resolveWorkspaceScopedLSPEntryName(reg, m.Name, lang, wsKey)
		}
	}

	weeklyRefresh := resolveWeeklyRefresh(a, opts)
	if had {
		weeklyRefresh = prior.WeeklyRefresh
	}
	composedEntry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      lang,
		Backend:       spec.Backend,
		Port:          port,
		TaskName:      taskName,
		ClientEntries: entryNameByClient,
		WeeklyRefresh: weeklyRefresh,
		Lifecycle:     LifecycleConfigured,
	}
	capturedRegKey := wsKey
	capturedRegLang := lang
	capturedHad := had
	capturedPrior := prior
	transaction.AddCompensation("restore supervised registry row "+capturedRegKey+"/"+capturedRegLang, func() error {
		// The client-update phase can hold a second registry lock when it fails.
		// This rollback re-enters the registry through TryLock, so release that
		// owned lock first while leaving the transaction's unrelated finalizers
		// and compensations in their existing order.
		releaseErr := transaction.Release(lockToken)
		restoreErr := restoreRegistryRowForRollback(reg.path, capturedRegKey, capturedRegLang, capturedPrior, capturedHad)
		return errors.Join(releaseErr, restoreErr)
	})
	if err := reg.PutLSP(composedEntry); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("register: composed LSP-row write rejected: %w", err)
	}
	if err := reg.Save(); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("persist registry: %w", err)
	}
	if err := transaction.Release(lockToken); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("release supervised registry lock before client updates: %w", err)
	}

	unlock, err = reg.Lock()
	if err != nil {
		return registeredLanguageResult{}, fmt.Errorf("re-acquire registry lock: %w", err)
	}
	lockToken = transaction.AddFinalizer("release supervised registry lock before client updates for "+wsKey+"/"+lang, unlock)
	if err := reg.Load(); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("reload registry: %w", err)
	}
	if _, ok := reg.Get(wsKey, lang); !ok {
		return registeredLanguageResult{}, fmt.Errorf("registry entry disappeared before client updates for %s/%s", wsKey, lang)
	}
	provisionalReceipts, err := writeRegisteredClientEntries(bindings, allClients, entryNameByClient, port, lang, w, transaction)
	if err != nil {
		return registeredLanguageResult{}, err
	}
	if err := transaction.Release(lockToken); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("release supervised registry lock after client updates: %w", err)
	}
	postWrite := opts.supervisorPostWriteDeps
	if postWrite.upsertIntent == nil || postWrite.writeRunningIntent == nil || postWrite.reconcile == nil || postWrite.readiness == nil {
		return registeredLanguageResult{}, errors.New("supervised register post-write dependencies were not normalized")
	}

	restoreIntent, err := postWrite.upsertIntent(composedEntry, canonicalExe)
	enrollSupervisorIntentUndo(
		transaction,
		"restore supervised intent "+LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang),
		restoreIntent,
	)
	if err != nil {
		return registeredLanguageResult{}, err
	}
	if canonicalTaskName, err := postWrite.writeRunningIntent(
		taskName,
		transaction.AddCompensation,
	); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("clear register stop for %s before supervisor reconcile: %w", canonicalTaskName, err)
	}
	// The reconcile may spawn the replacement and then return an error. Arm the
	// no-op-safe kill before invoking it, after the running-intent restoration
	// participant, so rollback first stops the possible replacement and then
	// restores the exact prior stop state.
	transaction.AddCompensation("kill possible supervised proxy "+taskName, func() error {
		if port > 0 {
			if killErr := killByPortFn(port, 5*time.Second); killErr != nil {
				return fmt.Errorf("kill possible supervised proxy on port %d: %w", port, killErr)
			}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
	defer cancel()
	// Stop is cleared (above) — the daemon is now spawn-desired. From here the
	// reconcile can bind the port, so the rollback must own a port kill.
	if _, err := postWrite.reconcile(ctx, true); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("supervisor reconcile after LSP intent write: %w", err)
	}
	if err := postWrite.readiness(port, 10*time.Second); err != nil {
		return registeredLanguageResult{}, fmt.Errorf("proxy readiness on port %d: %w", port, err)
	}
	transaction.AddSuccessOutput(
		"supervised proxy started "+LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang),
		w,
		"✓ Supervisor-managed LSP proxy started: %s\n",
		LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang),
	)
	return registeredLanguageResult{Entry: composedEntry, Receipts: provisionalReceipts}, nil
}

func resolveWorkspaceScopedLSPEntryName(reg *Registry, serverName, language, workspaceKey string) string {
	name := ResolveEntryName(reg, serverName, language, workspaceKey)
	if name != LSPRouterEntryName(language) {
		return name
	}
	short := workspaceKey
	if len(short) > 4 {
		short = short[:4]
	}
	candidate := name + "-" + short
	if entryNameTakenByOtherWorkspace(reg, candidate, workspaceKey) {
		return name + "-" + workspaceKey
	}
	return candidate
}

func (a *API) upsertLSPSupervisorIntent(entry WorkspaceEntry, mcphubBinaryPath string) (undo compensation, err error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, unlockErr))
		}
	}()

	prior, existed, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return nil, err
	}
	desired := cloneSupervisorIntentFile(prior)
	descriptor := BuildSupervisorDaemonForLSP(entry, mcphubBinaryPath)
	kept := desired.Daemons[:0]
	replaced := false
	var priorDescriptor SupervisorDaemon
	for _, daemon := range desired.Daemons {
		if daemon.TaskName != descriptor.TaskName {
			kept = append(kept, daemon)
			continue
		}
		replaced = true
		priorDescriptor = daemon
	}
	desired.Daemons = append(kept, descriptor)
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	// Capture any prior stop artifact for this task through both the canonical
	// and bare key forms. A stop or absent-only watermark keyed under the bare
	// legacy/external/migrated form would be missed by a raw canonical-only
	// index, so the rollback closure below would restore the descriptor without
	// the artifact and revive a deliberately-stopped daemon.
	priorArtifacts := supervisorStopArtifactsForTask(desired, descriptor.TaskName)
	if replaced {
		undo = func() error {
			return upsertSupervisorIntentDescriptorAndStop(intentPath, priorDescriptor, priorArtifacts)
		}
	} else {
		undo = func() error {
			return removeSupervisorIntentDescriptorAndStop(intentPath, descriptor.TaskName, !existed, priorArtifacts)
		}
	}
	if err := writeSupervisorIntentLockHeld(intentPath, desired); err != nil {
		return undo, fmt.Errorf("write supervisor-intent LSP row %s: %w", descriptor.TaskName, err)
	}
	return undo, nil
}

// lspSupervisorIntentDescriptorExists reports whether supervisor-intent.json
// currently carries a descriptor for the given (workspaceKey, language) LSP
// proxy. Used by the auto-register fast path to distinguish "port responds AND
// the supervisor owns the proxy" (true fast-return) from "port responds but no
// supervisor ownership" (legacy/orphan proxy that needs promotion).
func lspSupervisorIntentDescriptorExists(wsKey, lang string) (exists bool, err error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return false, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return false, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, unlockErr))
		}
	}()
	current, existed, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return false, err
	}
	if !existed || current == nil {
		return false, nil
	}
	taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang)
	for _, d := range current.Daemons {
		if d.TaskName == taskName {
			return true, nil
		}
	}
	return false, nil
}

func (a *API) removeLSPSupervisorIntent(wsKey, lang string) (compensation, bool, error) {
	return a.removeSupervisorIntentDescriptorForTask(LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang))
}

// RemoveSerenaSupervisorIntentForWorkspace removes the supervisor-owned Serena
// per-workspace descriptor that pairs with a removed Serena registry row and
// nudges a running supervisor to reconcile immediately. A live-supervisor
// reconcile failure restores the descriptor and any matching stop tombstone so
// the failed unregister is reversible.
func (a *API) RemoveSerenaSupervisorIntentForWorkspace(workspacePath string) (bool, error) {
	taskName := SerenaTaskNameForWorkspace(workspacePath)
	descriptor, found, err := supervisorIntentDescriptorForTask(taskName)
	if err != nil || !found {
		return false, err
	}
	// Resolve the EFFECTIVE port through the owner, not the raw descriptor.Port: a
	// legacy Port=0 serena-proxy row still binds the `--port` from its argv (F5 no
	// longer persists it into the field), so keying the pre-removal force-kill on the
	// raw field would skip the kill for exactly that row — the descriptor is removed
	// but the live child keeps squatting its port → the lost-own-child class this PR
	// closes elsewhere (bot PR #505 r5 completeness sweep; mirrors daemon_recover.go).
	if port, ok := EffectiveDaemonPort(descriptor); ok && port != 0 && forceKillByPortFn != nil {
		outcome, killErr := forceKillByPortFn(port, 5*time.Second)
		if killErr != nil && outcome != portKillNoListener && outcome != portKillIdentityMismatch {
			return false, fmt.Errorf("kill live serena proxy on port %d before removing %s: %w", port, taskName, killErr)
		}
	}
	restoreSupervisorIntent, removed, err := a.removeSupervisorIntentDescriptorForTask(taskName)
	if err != nil || !removed {
		return removed, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
	_, err = registerSupervisorReconcileFn(ctx, true)
	cancel()
	if err != nil {
		if !errors.Is(err, ErrSupervisorIPCUnavailable) {
			restoreErr := error(nil)
			if restoreSupervisorIntent != nil {
				restoreErr = restoreSupervisorIntent()
			}
			return true, errors.Join(
				fmt.Errorf("supervisor reconcile after removing %s failed while supervisor is alive; retry unregister: %w", taskName, err),
				labeledTransactionError("compensation", "restore supervisor intent "+taskName, restoreErr),
			)
		}
		// ErrSupervisorIPCUnavailable does NOT by itself mean the teardown is
		// durable. The error fires for BOTH "no supervisor at all" (benign — the
		// on-disk descriptor removal is final, nothing respawns the daemon) AND
		// "a supervisor holds supervisor.lock but its IPC listener is wedged"
		// (dangerous — that live supervisor still has the now-removed descriptor
		// in memory, so its reaper keeps or respawns the orphaned Serena daemon,
		// while the caller proceeds to delete the registry row; on retry there is
		// no row left to drive the paired teardown). Probe the flock-authoritative
		// lock owner to tell the two apart — the SAME live-owner probe seam
		// demoteIPCUnavailableWhenOwnerAlive uses on the removed-target kill path.
		// Unlike the install nudge we do NOT start the autostart owner here: this
		// is a teardown, not a bring-up, so there is no owner to start.
		if alive, pid, probeErr := serenaSupervisorOwnerAliveOnIPCUnavailable(); probeErr != nil || alive {
			restoreErr := error(nil)
			if restoreSupervisorIntent != nil {
				restoreErr = restoreSupervisorIntent()
			}
			if probeErr != nil {
				// Cannot resolve / probe the lock owner → unknown liveness. A live
				// wedged supervisor is the dangerous case, so fail closed: restore
				// the descriptor and make the caller retry rather than orphan a daemon.
				return true, errors.Join(
					fmt.Errorf("supervisor reconcile after removing %s reported IPC unavailable and the lock-owner probe failed (%v); resolve the supervisor state then retry unregister: %w", taskName, probeErr, err),
					labeledTransactionError("compensation", "restore supervisor intent "+taskName, restoreErr),
				)
			}
			return true, errors.Join(
				fmt.Errorf("supervisor reconcile after removing %s reported IPC unavailable but supervisor lock owner pid=%d is alive (IPC wedged); the live supervisor still tracks the daemon; run `mcphub restart` or kill the wedged process, then retry unregister: %w", taskName, pid, err),
				labeledTransactionError("compensation", "restore supervisor intent "+taskName, restoreErr),
			)
		}
		// No live lock owner: IPC really is unavailable because no supervisor is
		// running. The on-disk removal is durable and nothing will respawn the
		// daemon, so the teardown succeeded.
	}
	return true, nil
}

// serenaSupervisorOwnerAliveOnIPCUnavailable probes the flock-authoritative
// supervisor lock owner so RemoveSerenaSupervisorIntentForWorkspace can demote a
// reconcile ErrSupervisorIPCUnavailable into a failed teardown when a live but
// wedged supervisor still owns the removed descriptor in memory. It reuses the
// SAME installSupervisorRunningProbeFn + DaemonStateDir() seam as
// demoteIPCUnavailableWhenOwnerAlive (install_parsed_manifest.go). A non-nil
// probeErr means the owner liveness is unknown; callers MUST fail closed
// (treat as alive) on probeErr.
func serenaSupervisorOwnerAliveOnIPCUnavailable() (alive bool, pid int, probeErr error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return false, 0, fmt.Errorf("resolve state dir before serena-teardown owner probe: %w", err)
	}
	running, ownerPID, err := installSupervisorRunningProbeFn(stateDir)
	if err != nil {
		return false, 0, fmt.Errorf("probe supervisor before serena teardown: %w", err)
	}
	return running, ownerPID, nil
}

func supervisorIntentDescriptorForTask(taskName string) (descriptor SupervisorDaemon, found bool, err error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return SupervisorDaemon{}, false, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return SupervisorDaemon{}, false, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, unlockErr))
		}
	}()
	current, existed, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return SupervisorDaemon{}, false, err
	}
	if !existed || current == nil {
		return SupervisorDaemon{}, false, nil
	}
	for _, daemon := range current.Daemons {
		if daemon.TaskName == taskName {
			return daemon, true, nil
		}
	}
	return SupervisorDaemon{}, false, nil
}

func (a *API) removeSupervisorIntentDescriptorForTask(taskName string) (undo compensation, removed bool, err error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, false, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return nil, false, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, unlockErr))
		}
	}()

	prior, _, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return nil, false, err
	}
	desired := cloneSupervisorIntentFile(prior)
	kept := desired.Daemons[:0]
	removed = false
	var removedDescriptor SupervisorDaemon
	for _, daemon := range desired.Daemons {
		if daemon.TaskName == taskName {
			removed = true
			removedDescriptor = daemon
			continue
		}
		kept = append(kept, daemon)
	}
	if !removed {
		return func() error { return nil }, false, nil
	}
	priorArtifacts := supervisorStopArtifactsForTask(desired, taskName)
	desired.Daemons = kept
	// Descriptor removal owns the matching stop tombstone too: this mirrors the
	// install prune invariant that row removed => its stop entry goes with it.
	// Keeping both changes in one intent write avoids a re-register window where
	// reconcile observes the re-added descriptor but an old stop still suppresses
	// readiness before the reversible running-intent writer can clear it.
	removedDescriptors := []SupervisorDaemon{removedDescriptor}
	desired.Stops = pruneStopsForRemovedSupervisorTargets(desired.Stops, removedDescriptors)
	desired.LegacyStopWatermarks = pruneLegacyStopWatermarksForRemovedSupervisorTargets(desired.LegacyStopWatermarks, removedDescriptors)
	undo = func() error {
		return upsertSupervisorIntentDescriptorAndStop(intentPath, removedDescriptor, priorArtifacts)
	}
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSupervisorIntentLockHeld(intentPath, desired); err != nil {
		return undo, false, fmt.Errorf("write supervisor-intent without LSP row %s: %w", taskName, err)
	}
	return undo, true, nil
}

type supervisorStopArtifacts struct {
	stop         DaemonIntent
	hasStop      bool
	watermark    DaemonIntent
	hasWatermark bool
}

func supervisorStopArtifactsForTask(intent *SupervisorIntentFile, taskName string) supervisorStopArtifacts {
	if intent == nil {
		return supervisorStopArtifacts{}
	}
	stop, hasStop := supervisorStopForTask(intent.Stops, taskName)
	watermark, hasWatermark := supervisorStopForTask(intent.LegacyStopWatermarks, taskName)
	return supervisorStopArtifacts{
		stop:         stop,
		hasStop:      hasStop,
		watermark:    watermark,
		hasWatermark: hasWatermark,
	}
}

func (a supervisorStopArtifacts) hasAny() bool {
	return a.hasStop || a.hasWatermark
}

func supervisorStopForTask(stops map[string]DaemonIntent, taskName string) (DaemonIntent, bool) {
	if len(stops) == 0 {
		return DaemonIntent{}, false
	}
	canonicalTaskName := canonicalIntentTaskKey(taskName)
	if stop, ok := stops[canonicalTaskName]; ok {
		return stop, true
	}
	return DaemonIntent{}, false
}

func restoreSupervisorStopArtifacts(desired *SupervisorIntentFile, taskName string, artifacts supervisorStopArtifacts) {
	key := canonicalIntentTaskKey(taskName)
	if artifacts.hasStop {
		if desired.Stops == nil {
			desired.Stops = map[string]DaemonIntent{}
		}
		desired.Stops[key] = artifacts.stop
		return
	}
	if artifacts.hasWatermark {
		if desired.LegacyStopWatermarks == nil {
			desired.LegacyStopWatermarks = map[string]DaemonIntent{}
		}
		desired.LegacyStopWatermarks[key] = artifacts.watermark
	}
}

func upsertSupervisorIntentDescriptorAndStop(path string, descriptor SupervisorDaemon, artifacts supervisorStopArtifacts) (err error) {
	lock := flock.New(path + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("supervisor-intent rollback flock %s: %w", path+supervisorIntentLockSuffix, err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release supervisor-intent rollback flock %s: %w", path+supervisorIntentLockSuffix, unlockErr))
		}
	}()
	current, _, err := readSupervisorIntentForMerge(path)
	if err != nil {
		return err
	}
	desired := cloneSupervisorIntentFile(current)
	kept := desired.Daemons[:0]
	for _, daemon := range desired.Daemons {
		if daemon.TaskName != descriptor.TaskName {
			kept = append(kept, daemon)
		}
	}
	desired.Daemons = append(kept, descriptor)
	restoreSupervisorStopArtifacts(desired, descriptor.TaskName, artifacts)
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return writeSupervisorIntentLockHeld(path, desired)
}

func removeSupervisorIntentDescriptorAndStop(path, taskName string, removeFileIfEmpty bool, artifacts supervisorStopArtifacts) (err error) {
	lock := flock.New(path + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("supervisor-intent rollback flock %s: %w", path+supervisorIntentLockSuffix, err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release supervisor-intent rollback flock %s: %w", path+supervisorIntentLockSuffix, unlockErr))
		}
	}()
	current, existed, err := readSupervisorIntentForMerge(path)
	if err != nil || !existed {
		return err
	}
	desired := cloneSupervisorIntentFile(current)
	kept := desired.Daemons[:0]
	removed := false
	for _, daemon := range desired.Daemons {
		if daemon.TaskName == taskName {
			removed = true
			continue
		}
		kept = append(kept, daemon)
	}
	if !removed && !artifacts.hasAny() {
		return nil
	}
	desired.Daemons = kept
	removedDescriptor := SupervisorDaemon{TaskName: taskName}
	desired.LegacyStopWatermarks = pruneLegacyStopWatermarksForRemovedSupervisorTargets(desired.LegacyStopWatermarks, []SupervisorDaemon{removedDescriptor})
	restoreSupervisorStopArtifacts(desired, taskName, artifacts)
	if removeFileIfEmpty && len(desired.Daemons) == 0 && len(desired.MaintenanceTimers) == 0 && !desired.StrictMode && len(desired.Stops) == 0 && len(desired.LegacyStopWatermarks) == 0 {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		return nil
	}
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return writeSupervisorIntentLockHeld(path, desired)
}

func cloneSupervisorIntentFile(in *SupervisorIntentFile) *SupervisorIntentFile {
	if in == nil {
		return &SupervisorIntentFile{Version: 1}
	}
	out := *in
	out.Daemons = append([]SupervisorDaemon(nil), in.Daemons...)
	for i := range out.Daemons {
		out.Daemons[i].Args = append([]string(nil), in.Daemons[i].Args...)
		out.Daemons[i].Env = cloneStringMap(in.Daemons[i].Env)
		if in.Daemons[i].RuntimeSpec != nil {
			spec := *in.Daemons[i].RuntimeSpec
			spec.ChildArgs = append([]string(nil), in.Daemons[i].RuntimeSpec.ChildArgs...)
			spec.EnvRefs = cloneStringMap(in.Daemons[i].RuntimeSpec.EnvRefs)
			out.Daemons[i].RuntimeSpec = &spec
		}
	}
	out.MaintenanceTimers = append([]MaintenanceTimer(nil), in.MaintenanceTimers...)
	for i := range out.MaintenanceTimers {
		out.MaintenanceTimers[i].Args = append([]string(nil), in.MaintenanceTimers[i].Args...)
		if in.MaintenanceTimers[i].Enabled != nil {
			enabled := *in.MaintenanceTimers[i].Enabled
			out.MaintenanceTimers[i].Enabled = &enabled
		}
	}
	// E2 stops sub-block: `out := *in` copies only the map HEADER — a
	// mutation through the clone would alias the caller's map. Deep-clone.
	if in.Stops != nil {
		out.Stops = make(map[string]DaemonIntent, len(in.Stops))
		for k, v := range in.Stops {
			out.Stops[k] = v
		}
	}
	if in.LegacyStopWatermarks != nil {
		out.LegacyStopWatermarks = make(map[string]DaemonIntent, len(in.LegacyStopWatermarks))
		for k, v := range in.LegacyStopWatermarks {
			out.LegacyStopWatermarks[k] = v
		}
	}
	return &out
}
