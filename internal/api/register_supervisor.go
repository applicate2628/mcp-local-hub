package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
		if legacyTaskDeleted && capturedPort > 0 {
			_ = killByPortFn(capturedPort, 5*time.Second)
		}
		if len(capturedPriorXML) > 0 {
			_ = sch.ImportXML(capturedTaskName, capturedPriorXML)
			_ = sch.Run(capturedTaskName)
			fmt.Fprintf(w, "  rollback: restored + restarted scheduler task %s\n", capturedTaskName)
		}
	})
	if len(priorXML) > 0 && port > 0 {
		_ = killByPortFn(port, 5*time.Second)
	}
	if err := sch.Delete(taskName); err != nil && !errors.Is(err, scheduler.ErrTaskNotFound) {
		return WorkspaceEntry{}, fmt.Errorf("delete legacy task %s before supervised promote: %w", taskName, err)
	}
	legacyTaskDeleted = true

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
			entryNameByClient[b.Client] = ResolveEntryName(reg, m.Name, lang, wsKey)
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
		priorEntry, _ := client.GetEntry(entryName)
		urlPath := b.URLPath
		if urlPath == "" {
			urlPath = "/mcp"
		}
		entry := clients.MCPEntry{
			Name: entryName,
			URL:  fmt.Sprintf("http://localhost:%d%s", port, urlPath),
		}
		if err := client.AddEntry(entry); err != nil {
			return WorkspaceEntry{}, fmt.Errorf("write %s entry: %w", b.Client, err)
		}
		clientRef := client
		savedPrior := priorEntry
		capturedName := entryName
		capturedClientName := b.Client
		*rollback = append(*rollback, func() {
			if savedPrior != nil {
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
	supervisorSpawnRequested := false
	*rollback = append(*rollback, func() {
		if supervisorSpawnRequested && port > 0 {
			_ = killByPortFn(port, 5*time.Second)
		}
		restoreIntent()
	})
	ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
	defer cancel()
	if _, err := registerSupervisorReconcileFn(ctx, true); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("supervisor reconcile after LSP intent write: %w", err)
	}
	supervisorSpawnRequested = true
	if err := proxyReadinessFn(port, 10*time.Second); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("proxy readiness on port %d: %w", port, err)
	}
	fmt.Fprintf(w, "✓ Supervisor-managed LSP proxy started: %s\n", LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang))
	a.recordRegisterIntentForTask(taskName, w)
	return composedEntry, nil
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
	if err := writeSupervisorIntentLockHeld(intentPath, desired); err != nil {
		return nil, fmt.Errorf("write supervisor-intent LSP row %s: %w", descriptor.TaskName, err)
	}

	return func() {
		if replaced {
			upsertSupervisorIntentDescriptor(intentPath, priorDescriptor)
			return
		}
		removeSupervisorIntentDescriptor(intentPath, descriptor.TaskName, !existed)
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
	taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang)
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
	desired.Daemons = kept
	desired.Version = 1
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSupervisorIntentLockHeld(intentPath, desired); err != nil {
		return nil, false, fmt.Errorf("write supervisor-intent without LSP row %s: %w", taskName, err)
	}

	return func() {
		upsertSupervisorIntentDescriptor(intentPath, removedDescriptor)
	}, true, nil
}

func removeSupervisorIntentDescriptor(path, taskName string, removeFileIfEmpty bool) {
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
	if !removed {
		return
	}
	desired.Daemons = kept
	if removeFileIfEmpty && len(desired.Daemons) == 0 && len(desired.MaintenanceTimers) == 0 && !desired.StrictMode {
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
	return &out
}
