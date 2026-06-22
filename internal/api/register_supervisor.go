package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/clients"
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
// The (workspace_path, language) pair is read from the descriptor's flat argv
// (--workspace / --language), falling back to the Workspace field for the path.
// The lookup mirrors the unregister path's canonical/legacy-key tolerance:
// the canonical EvalSymlinks key is tried first, then the pre-symlink legacy
// key, so a row written under either canonicalization scheme is found. A
// non-LSP descriptor, a descriptor missing its language, or any registry
// read/lock failure returns true (fail OPEN — never suppress a legitimate spawn
// on a transient registry hiccup; the worst case is the pre-fix behavior of
// spawning a row that then fails loud, which is strictly safer than silently
// dropping a backed daemon).
func LSPRegistryRowBacksDescriptor(d SupervisorDaemon) bool {
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
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return true
	}
	reg := NewRegistry(regPath)
	unlock, ok, err := tryLockRegistryBrief(reg)
	if err != nil || !ok {
		return true
	}
	defer unlock()
	if err := reg.Load(); err != nil {
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
	w io.Writer,
	rollback *[]func(),
) (WorkspaceEntry, error) {
	unlock, err := reg.Lock()
	if err != nil {
		return WorkspaceEntry{}, fmt.Errorf("acquire registry lock: %w", err)
	}
	releaseUnlock := func() {
		if unlock != nil {
			unlock()
			unlock = nil
		}
	}
	defer releaseUnlock()
	if err := reg.Load(); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("load registry: %w", err)
	}

	prior, had := reg.Get(wsKey, lang)
	var port int
	if had {
		port = prior.Port
	} else {
		p, err := AllocatePort(reg, *m.PortPool)
		if err != nil {
			return WorkspaceEntry{}, err
		}
		port = p
	}
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, lang)
	canonicalExe, err := canonicalMcphubPath()
	if err != nil {
		return WorkspaceEntry{}, err
	}
	if _, err := os.Stat(canonicalExe); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
	}

	var priorXML []byte
	if xml, err := sch.ExportXML(taskName); err == nil {
		priorXML = xml
	} else if !errors.Is(err, scheduler.ErrTaskNotFound) {
		return WorkspaceEntry{}, fmt.Errorf("export prior task %s: %w", taskName, err)
	}
	capturedTaskName := taskName
	capturedPriorXML := priorXML
	capturedPort := port
	legacyTaskDeleted := false
	*rollback = append(*rollback, func() {
		if !legacyTaskDeleted {
			return
		}
		if capturedPort > 0 {
			_ = killByPortFn(capturedPort, 5*time.Second)
		}
		if len(capturedPriorXML) > 0 {
			_ = sch.ImportXML(capturedTaskName, capturedPriorXML)
			_ = sch.Run(capturedTaskName)
			fmt.Fprintf(w, "  rollback: restored + restarted scheduler task %s\n", capturedTaskName)
		}
	})
	if (had || len(priorXML) > 0) && port > 0 {
		legacyPortReady := proxyReadinessFn(port, lspExistingProxyProbeTimeout) == nil
		if err := killObservedLiveLSPProxy(port, taskName, legacyPortReady); err != nil {
			return WorkspaceEntry{}, err
		}
	}
	if err := sch.Delete(taskName); err != nil && !errors.Is(err, scheduler.ErrTaskNotFound) {
		return WorkspaceEntry{}, fmt.Errorf("delete legacy task %s before supervised promote: %w", taskName, err)
	}
	legacyTaskDeleted = len(priorXML) > 0

	bindingsPre := m.ClientBindings
	if len(bindingsPre) == 0 {
		bindingsPre = defaultClientBindings
	}
	entryNameByClient := map[string]string{}
	if had {
		for k, v := range prior.ClientEntries {
			entryNameByClient[k] = v
		}
	}
	for _, b := range bindingsPre {
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
	if err := reg.PutLSP(composedEntry); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("register: composed LSP-row write rejected: %w", err)
	}
	if err := reg.Save(); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("persist registry: %w", err)
	}
	capturedRegKey := wsKey
	capturedRegLang := lang
	capturedHad := had
	capturedPrior := prior
	*rollback = append(*rollback, func() {
		unlock, err := reg.Lock()
		if err != nil {
			fmt.Fprintf(w, "  rollback: could not lock registry for %s/%s: %v\n", capturedRegKey, capturedRegLang, err)
			return
		}
		defer unlock()
		if err := reg.Load(); err != nil {
			fmt.Fprintf(w, "  rollback: could not reload registry for %s/%s: %v\n", capturedRegKey, capturedRegLang, err)
			return
		}
		if capturedHad {
			reg.Put(capturedPrior)
			_ = reg.Save()
			fmt.Fprintf(w, "  rollback: restored prior registry entry %s/%s\n", capturedRegKey, capturedRegLang)
			return
		}
		reg.Remove(capturedRegKey, capturedRegLang)
		_ = reg.Save()
		fmt.Fprintf(w, "  rollback: removed registry entry %s/%s\n", capturedRegKey, capturedRegLang)
	})
	releaseUnlock()

	unlock, err = reg.Lock()
	if err != nil {
		return WorkspaceEntry{}, fmt.Errorf("re-acquire registry lock: %w", err)
	}
	defer releaseUnlock()
	if err := reg.Load(); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("reload registry: %w", err)
	}
	if _, ok := reg.Get(wsKey, lang); !ok {
		return WorkspaceEntry{}, fmt.Errorf("registry entry disappeared before client updates for %s/%s", wsKey, lang)
	}
	for _, b := range bindingsPre {
		client, ok := allClients[b.Client]
		if !ok || !client.Exists() {
			continue
		}
		entryName := entryNameByClient[b.Client]
		// Snapshot the prior entry; a GetEntry error MUST abort before the
		// AddEntry below (bot PR #420 finding 1, data-loss). A multi-layer
		// adapter (mimocode) can confirm a write-target prior yet still fail
		// reading a malformed lower layer, returning (nil, err); dropping the
		// error would let AddEntry overwrite and the nil-prior rollback branch
		// DELETE the operator's entry. The caller runs the accumulated
		// *rollback on this returned error, so no local runRollback is needed.
		priorEntry, err := client.GetEntry(entryName)
		if err != nil {
			return WorkspaceEntry{}, fmt.Errorf("snapshot prior %s entry in %s: %w", entryName, b.Client, err)
		}
		urlPath := b.URLPath
		if urlPath == "" {
			urlPath = "/mcp"
		}
		entry := clients.MCPEntry{
			Name: entryName,
			URL:  fmt.Sprintf("http://127.0.0.1:%d%s", port, urlPath),
		}
		if err := client.AddEntry(entry); err != nil {
			return WorkspaceEntry{}, fmt.Errorf("write %s entry: %w", b.Client, err)
		}
		clientRef := client
		savedPrior := priorEntry
		capturedName := entryName
		capturedClientName := b.Client
		*rollback = append(*rollback, func() {
			// See install.go rollback: a mimo prior sourced BELOW the write target
			// (config.json) or from the ~/.claude.json import must NOT be copied up
			// (permanent shadow + import-credential leak — bot PR #420 finding 1).
			// Take REMOVE for those; the zero value (every other adapter / an
			// at-or-above mimo prior) copies up.
			if savedPrior != nil && !savedPrior.SourceBelowWriteTarget {
				_ = clientRef.AddEntry(*savedPrior)
				fmt.Fprintf(w, "  rollback: restored prior %s entry in %s\n", capturedName, capturedClientName)
				return
			}
			_ = clientRef.RemoveEntry(capturedName)
			fmt.Fprintf(w, "  rollback: removed %s entry from %s\n", capturedName, capturedClientName)
		})
		fmt.Fprintf(w, "✓ %s → %s (entry %s)\n", b.Client, entry.URL, entryName)
	}
	releaseUnlock()

	restoreIntent, err := a.upsertLSPSupervisorIntent(composedEntry, canonicalExe)
	if err != nil {
		return WorkspaceEntry{}, err
	}
	// reconcileAttempted gates the rollback kill-by-port. The spawn TRIGGER is
	// not proxy readiness — it is the
	// stop being cleared (writeRegisterRunningIntentForTask makes the daemon
	// spawn-desired) PLUS the reconcile nudge (registerSupervisorReconcileFn).
	// A reconcile that SPAWNS the daemon and THEN errors returns before any
	// post-readiness flag is set, so gating the kill on a late flag would skip
	// the kill and leave restoreIntent()'s descriptor removal to orphan a live
	// daemon on the port. Flip this true once the stop is cleared and the
	// reconcile is about to fire; killByPort is a no-op when nothing is bound, so
	// killing on a reconcile that never actually spawned is harmless.
	reconcileAttempted := false
	*rollback = append(*rollback, func() {
		if reconcileAttempted && port > 0 {
			_ = killByPortFn(port, 5*time.Second)
		}
		restoreIntent()
	})
	if canonicalTaskName, err := a.writeRegisterRunningIntentForTask(taskName); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("clear register stop for %s before supervisor reconcile: %w", canonicalTaskName, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
	defer cancel()
	// Stop is cleared (above) — the daemon is now spawn-desired. From here the
	// reconcile can bind the port, so the rollback must own a port kill.
	reconcileAttempted = true
	if _, err := registerSupervisorReconcileFn(ctx, true); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("supervisor reconcile after LSP intent write: %w", err)
	}
	if err := proxyReadinessFn(port, 10*time.Second); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("proxy readiness on port %d: %w", port, err)
	}
	fmt.Fprintf(w, "✓ Supervisor-managed LSP proxy started: %s\n", LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang))
	a.recordRegisterIntentForTask(taskName, w)
	return composedEntry, nil
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

func (a *API) upsertLSPSupervisorIntent(entry WorkspaceEntry, mcphubBinaryPath string) (func(), error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()

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
	// Capture any prior stop tombstone for this task through both the canonical
	// and bare key forms — a stop keyed under the bare (legacy/external/migrated)
	// form would be missed by a raw canonical-only index, so the rollback closure
	// below would restore the descriptor WITHOUT the tombstone and revive a
	// deliberately-stopped daemon. Mirrors removeSupervisorIntentDescriptorForTask.
	// Capture any prior stop tombstone for this task through both the canonical
	// and bare key forms — a stop keyed under the bare (legacy/external/migrated)
	// form would be missed by a raw canonical-only index, so the rollback closure
	// below would restore the descriptor WITHOUT the tombstone and revive a
	// deliberately-stopped daemon. Mirrors removeSupervisorIntentDescriptorForTask.
	priorStop, hadPriorStop := supervisorStopForTask(desired.Stops, descriptor.TaskName)
	if err := writeSupervisorIntentLockHeld(intentPath, desired); err != nil {
		return nil, fmt.Errorf("write supervisor-intent LSP row %s: %w", descriptor.TaskName, err)
	}

	return func() {
		if replaced {
			upsertSupervisorIntentDescriptorAndStop(intentPath, priorDescriptor, priorStop, hadPriorStop)
			return
		}
		removeSupervisorIntentDescriptorAndStop(intentPath, descriptor.TaskName, !existed, priorStop, hadPriorStop)
	}, nil
}

// lspSupervisorIntentDescriptorExists reports whether supervisor-intent.json
// currently carries a descriptor for the given (workspaceKey, language) LSP
// proxy. Used by the auto-register fast path to distinguish "port responds AND
// the supervisor owns the proxy" (true fast-return) from "port responds but no
// supervisor ownership" (legacy/orphan proxy that needs promotion).
func lspSupervisorIntentDescriptorExists(wsKey, lang string) (bool, error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return false, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return false, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()
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

func (a *API) removeLSPSupervisorIntent(wsKey, lang string) (func(), bool, error) {
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
	if port := descriptor.Port; port != 0 && forceKillByPortFn != nil {
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
			if restoreSupervisorIntent != nil {
				restoreSupervisorIntent()
			}
			return true, fmt.Errorf("supervisor reconcile after removing %s failed while supervisor is alive; restored supervisor intent descriptor; retry unregister: %w", taskName, err)
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
			if restoreSupervisorIntent != nil {
				restoreSupervisorIntent()
			}
			if probeErr != nil {
				// Cannot resolve / probe the lock owner → unknown liveness. A live
				// wedged supervisor is the dangerous case, so fail closed: restore
				// the descriptor and make the caller retry rather than orphan a daemon.
				return true, fmt.Errorf("supervisor reconcile after removing %s reported IPC unavailable and the lock-owner probe failed (%v); restored supervisor intent descriptor; resolve the supervisor state then retry unregister: %w", taskName, probeErr, err)
			}
			return true, fmt.Errorf("supervisor reconcile after removing %s reported IPC unavailable but supervisor lock owner pid=%d is alive (IPC wedged); the live supervisor still tracks the daemon, so restored supervisor intent descriptor; run `mcphub restart` or kill the wedged process, then retry unregister: %w", taskName, pid, err)
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

func supervisorIntentDescriptorForTask(taskName string) (SupervisorDaemon, bool, error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return SupervisorDaemon{}, false, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return SupervisorDaemon{}, false, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()
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

func (a *API) removeSupervisorIntentDescriptorForTask(taskName string) (func(), bool, error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, false, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return nil, false, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()

	prior, _, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return nil, false, err
	}
	desired := cloneSupervisorIntentFile(prior)
	kept := desired.Daemons[:0]
	removed := false
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
		return func() {}, false, nil
	}
	priorStop, hadPriorStop := supervisorStopForTask(desired.Stops, taskName)
	desired.Daemons = kept
	// Descriptor removal owns the matching stop tombstone too: this mirrors the
	// install prune invariant that row removed => its stop entry goes with it.
	// Keeping both changes in one intent write avoids a re-register window where
	// reconcile observes the re-added descriptor but an old stop still suppresses
	// readiness before recordRegisterIntentForTask can clear it.
	desired.Stops = pruneStopsForRemovedSupervisorTargets(desired.Stops, []SupervisorDaemon{removedDescriptor})
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSupervisorIntentLockHeld(intentPath, desired); err != nil {
		return nil, false, fmt.Errorf("write supervisor-intent without LSP row %s: %w", taskName, err)
	}

	return func() {
		upsertSupervisorIntentDescriptorAndStop(intentPath, removedDescriptor, priorStop, hadPriorStop)
	}, true, nil
}

func supervisorStopForTask(stops map[string]DaemonIntent, taskName string) (DaemonIntent, bool) {
	if len(stops) == 0 {
		return DaemonIntent{}, false
	}
	canonicalTaskName := canonicalIntentTaskKey(taskName)
	if stop, ok := stops[canonicalTaskName]; ok {
		return stop, true
	}
	if bareTaskName := strings.TrimPrefix(canonicalTaskName, `\`); bareTaskName != canonicalTaskName {
		if stop, ok := stops[bareTaskName]; ok {
			return stop, true
		}
	}
	return DaemonIntent{}, false
}

func upsertSupervisorIntentDescriptorAndStop(path string, descriptor SupervisorDaemon, stop DaemonIntent, restoreStop bool) {
	lock := flock.New(path + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return
	}
	defer func() { _ = lock.Unlock() }()
	current, _, err := readSupervisorIntentForMerge(path)
	if err != nil {
		return
	}
	desired := cloneSupervisorIntentFile(current)
	kept := desired.Daemons[:0]
	for _, daemon := range desired.Daemons {
		if daemon.TaskName != descriptor.TaskName {
			kept = append(kept, daemon)
		}
	}
	desired.Daemons = append(kept, descriptor)
	if restoreStop {
		if desired.Stops == nil {
			desired.Stops = map[string]DaemonIntent{}
		}
		desired.Stops[canonicalIntentTaskKey(descriptor.TaskName)] = stop
	}
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeSupervisorIntentLockHeld(path, desired)
}

func removeSupervisorIntentDescriptor(path, taskName string, removeFileIfEmpty bool) {
	removeSupervisorIntentDescriptorAndStop(path, taskName, removeFileIfEmpty, DaemonIntent{}, false)
}

func removeSupervisorIntentDescriptorAndStop(path, taskName string, removeFileIfEmpty bool, stop DaemonIntent, restoreStop bool) {
	lock := flock.New(path + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return
	}
	defer func() { _ = lock.Unlock() }()
	current, existed, err := readSupervisorIntentForMerge(path)
	if err != nil || !existed {
		return
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
	if !removed && !restoreStop {
		return
	}
	desired.Daemons = kept
	if restoreStop {
		if desired.Stops == nil {
			desired.Stops = map[string]DaemonIntent{}
		}
		desired.Stops[canonicalIntentTaskKey(taskName)] = stop
	}
	if removeFileIfEmpty && len(desired.Daemons) == 0 && len(desired.MaintenanceTimers) == 0 && !desired.StrictMode && len(desired.Stops) == 0 {
		_ = os.Remove(path)
		return
	}
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeSupervisorIntentLockHeld(path, desired)
}

func upsertSupervisorIntentDescriptor(path string, descriptor SupervisorDaemon) {
	lock := flock.New(path + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return
	}
	defer func() { _ = lock.Unlock() }()
	current, _, err := readSupervisorIntentForMerge(path)
	if err != nil {
		return
	}
	desired := cloneSupervisorIntentFile(current)
	kept := desired.Daemons[:0]
	for _, daemon := range desired.Daemons {
		if daemon.TaskName != descriptor.TaskName {
			kept = append(kept, daemon)
		}
	}
	desired.Daemons = append(kept, descriptor)
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeSupervisorIntentLockHeld(path, desired)
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
	return &out
}
