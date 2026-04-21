// Package api — Register/Unregister for workspace-scoped MCP servers.
//
// Lazy-mode contract (M3 of the 2026-04-20 workspace-scoped plan):
//   - Register creates one scheduler task per (workspace, language) whose
//     command is `mcphub daemon workspace-proxy --port <p> --workspace <ws>
//     --language <lang>`. The proxy itself answers initialize/tools/list
//     synthetically and materializes the heavy backend on first tools/call.
//   - NO LSP binary preflight at register time. Missing binaries surface
//     later at first tools/call via LifecycleMissing.
//   - Each new registry entry starts with Lifecycle=LifecycleConfigured.
//     The proxy itself may re-stamp this on startup, but Register
//     pre-seeds it so `mcphub workspaces` shows a sensible state
//     immediately.
//   - Rollback: if any per-language step fails, every side effect applied
//     so far is reversed in LIFO order (client entries, scheduler tasks,
//     port allocations, registry entries).
//   - Default-all when caller passes an empty languages slice.
package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// defaultClientBindings is the implicit client binding set used when a
// workspace-scoped manifest does not declare client_bindings. Matches the
// three HTTP-native clients that support per-entry URLs. Antigravity (a
// stdio-relay client) is intentionally excluded; its relay model presumes
// a single (server, daemon) tuple which workspace-scoped entries do not
// have (decision recorded in the plan's Self-Review §7).
var defaultClientBindings = []config.ClientBinding{
	{Client: "codex-cli", URLPath: "/mcp"},
	{Client: "claude-code", URLPath: "/mcp"},
	{Client: "gemini-cli", URLPath: "/mcp"},
}

// RegisterOpts controls a Register invocation.
type RegisterOpts struct {
	WeeklyRefresh bool      // persist weekly_refresh=true on each created entry
	Writer        io.Writer // progress output; nil = os.Stderr
}

// RegisterReport summarizes what Register actually created.
type RegisterReport struct {
	Workspace    string           `json:"workspace"`
	WorkspaceKey string           `json:"workspace_key"`
	Entries      []WorkspaceEntry `json:"entries"`
}

// UnregisterReport summarizes what Unregister actually removed.
type UnregisterReport struct {
	Workspace    string   `json:"workspace"`
	WorkspaceKey string   `json:"workspace_key"`
	Removed      []string `json:"removed"` // language names
	Warnings     []string `json:"warnings,omitempty"`
}

// Register ensures workspace-scoped lazy proxies exist for each requested
// language in workspacePath. An empty languages slice defaults to every
// language declared in the manifest.
//
// Lazy mode: this function DOES NOT preflight LSP binaries. Missing LSP
// binaries are surfaced later at first tools/call via LifecycleMissing.
//
// Side effects per language (rolled back on later failure):
//  1. Allocate port from registry (first-free in the manifest's pool).
//  2. Create scheduler task whose command is
//     `mcphub daemon workspace-proxy --port <p> --workspace <ws> --language <lang>`.
//  3. Write managed entries into each client config (codex-cli, claude-code,
//     gemini-cli by default, or whatever the manifest declares in
//     client_bindings).
//
// Registry is saved once at the end; a mid-loop failure leaves the registry
// untouched on disk.
func (a *API) Register(workspacePath string, languages []string, opts RegisterOpts) (*RegisterReport, error) {
	data, err := loadManifestYAMLEmbedFirst("mcp-language-server")
	if err != nil {
		return nil, fmt.Errorf("load manifest mcp-language-server: %w", err)
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return a.registerWithManifest(m, workspacePath, languages, opts)
}

// registerWithManifest is the test-seam variant: production callers pass
// through Register (which loads the embedded manifest); tests inject a
// synthetic manifest to exercise rollback and edge cases hermetically.
func (a *API) registerWithManifest(m *config.ServerManifest, workspacePath string, languages []string, opts RegisterOpts) (*RegisterReport, error) {
	if m.Kind != config.KindWorkspaceScoped {
		return nil, fmt.Errorf("manifest %s: not workspace-scoped", m.Name)
	}
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	// 0. Canonical workspace + key.
	canonical, err := CanonicalWorkspacePath(workspacePath)
	if err != nil {
		return nil, err
	}
	wsKey := WorkspaceKey(canonical)
	// 1. Default-all when caller passed an empty slice. Sort for
	// deterministic iteration order — tests and rollback ordering both
	// depend on it.
	if len(languages) == 0 {
		for _, l := range m.Languages {
			languages = append(languages, l.Name)
		}
		sort.Strings(languages)
	}
	// 2. Validate every requested language is declared in the manifest
	// BEFORE any side effect. Unknown language → manifest-integrity error.
	bySpec := map[string]config.LanguageSpec{}
	for _, l := range m.Languages {
		bySpec[l.Name] = l
	}
	for _, lang := range languages {
		if _, ok := bySpec[lang]; !ok {
			return nil, fmt.Errorf("unknown language %q (manifest %s supports: %v)",
				lang, m.Name, sortedLanguageNames(m))
		}
	}
	// 2.4 Preflight the canonical mcphub binary BEFORE any scheduler
	// side effect. Register's per-language loop does the same check
	// inside registerOneLanguage, but EnsureWeeklyRefreshTask (below)
	// would fire first and could leak a shared "mcp-local-hub-workspace-
	// weekly-refresh" task pointing at a missing binary if setup wasn't
	// run yet. Fail fast instead — the user sees the same "run mcphub
	// setup once" message install uses, no orphan shared state created.
	canonicalExeForPreflight, err := canonicalMcphubPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(canonicalExeForPreflight); err != nil {
		return nil, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExeForPreflight, err)
	}
	// 2.5 Ensure the shared weekly-refresh task exists. Idempotent (Delete
	// + Create under the hood) so it is safe to invoke on every Register.
	// Failure here is non-fatal — per-workspace registration must not be
	// blocked by a shared-task problem; surface a warning and carry on.
	// Placed AFTER argument validation AND the canonical-mcphub preflight
	// so neither invalid-language calls nor setup-skipped installs
	// produce scheduler side effects at all.
	if err := a.EnsureWeeklyRefreshTask(); err != nil {
		fmt.Fprintf(w, "warning: ensure shared weekly-refresh task: %v\n", err)
	}
	// 3. Per-language register with rollback stack.
	//
	// Registry flock lifetime is scoped to `registerOneLanguage`'s Phase 1
	// (port alloc + task create + registry write) and is explicitly
	// released BEFORE `sch.Run` triggers the proxy subprocess. Holding it
	// across sch.Run + readiness probe deadlocks the proxy: the proxy's
	// own `reg.Lock()` in `daemon_workspace.go` would block until the
	// readiness probe times out, then register's rollback would delete
	// the registry entry, and the unblocked proxy would exit with
	// "not registered". Rollback closures that touch the registry each
	// re-acquire the lock themselves.
	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	sch, err := schedulerNewForRegister()
	if err != nil {
		return nil, err
	}
	allClients := clientsAllForRegister()
	var rollback []func()
	runRollback := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}
	report := &RegisterReport{Workspace: canonical, WorkspaceKey: wsKey}
	for _, lang := range languages {
		entry, err := a.registerOneLanguage(m, canonical, wsKey, lang, opts, reg, sch, allClients, w, &rollback)
		if err != nil {
			runRollback()
			return report, fmt.Errorf("register language %s: %w", lang, err)
		}
		report.Entries = append(report.Entries, entry)
	}
	return report, nil
}

// registerOneLanguage is the per-language unit of work. It (a) allocates a
// free port (or reuses the existing one for idempotent re-register),
// (b) creates the scheduler task, (c) writes each client entry, and
// accumulates rollback closures in order. Returns the entry ready to be
// Put in the registry.
func (a *API) registerOneLanguage(
	m *config.ServerManifest,
	canonical, wsKey, lang string,
	opts RegisterOpts,
	reg *Registry,
	sch testScheduler,
	allClients map[string]registerClient,
	w io.Writer,
	rollback *[]func(),
) (WorkspaceEntry, error) {
	var spec config.LanguageSpec
	for _, l := range m.Languages {
		if l.Name == lang {
			spec = l
			break
		}
	}
	// Phase 1: registry write window — acquire flock, load current state,
	// do all port/task/registry work, release flock BEFORE sch.Run so the
	// spawned proxy subprocess can acquire it. The rollback closures that
	// touch registry each re-acquire the flock themselves so rollback is
	// safe whether it runs during Phase 1 (flock still held) or Phase 2/3
	// (flock released).
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
	// Reuse existing entry port (idempotent re-register) or allocate new.
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
		// Tentatively pin the port into the registry's in-memory set so
		// subsequent AllocatePort calls within the same Register loop don't
		// return the same port again.
		reg.Put(WorkspaceEntry{WorkspaceKey: wsKey, WorkspacePath: canonical, Language: lang, Port: port})
		capturedKey := wsKey
		capturedLang := lang
		*rollback = append(*rollback, func() {
			reg.Remove(capturedKey, capturedLang)
		})
	}
	taskName := fmt.Sprintf("mcp-local-hub-lsp-%s-%s", wsKey, lang)
	// 1. Create scheduler task (or replace — snapshot the prior XML so
	// rollback can restore it).
	canonicalExe, err := canonicalMcphubPath()
	if err != nil {
		return WorkspaceEntry{}, err
	}
	// Verify the canonical mcphub binary actually exists before creating a
	// scheduler task pointing at it. Without this preflight, a fresh user
	// who skipped `mcphub setup` could get a successful-looking register
	// that persists registry/client state for a non-existent binary path;
	// Windows schtasks /run only starts the task and never verifies the
	// action actually executed, so the registration appears to succeed
	// while no proxy ever comes up. Install does the same preflight in
	// installUsingEmbedFirst (see install.go:298-300).
	if _, err := os.Stat(canonicalExe); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
	}
	args := []string{
		"daemon", "workspace-proxy",
		"--port", fmt.Sprintf("%d", port),
		"--workspace", canonical,
		"--language", lang,
	}
	var priorXML []byte
	if xml, err := sch.ExportXML(taskName); err == nil {
		priorXML = xml
	} else if !errors.Is(err, scheduler.ErrTaskNotFound) {
		// Only "task not found" is safe to ignore — any other export error
		// (permission, scheduler service down, XML corruption) means we do
		// NOT have a reliable priorXML snapshot. Proceeding would Delete
		// the existing task and leave rollback unable to restore it on a
		// later failure, turning a recoverable re-register error into a
		// persistent outage.
		return WorkspaceEntry{}, fmt.Errorf("export prior task %s: %w", taskName, err)
	}
	// Register the scheduler rollback BEFORE the destructive Delete+Create
	// so a Create failure on a re-register path does not orphan the old
	// task. Previously the rollback was appended after Create: a transient
	// Create error left the prior task deleted with no restoration, turning
	// a recoverable registration error into a hard workspace outage.
	capturedTaskName := taskName
	capturedPriorXML := priorXML
	capturedPort := port
	*rollback = append(*rollback, func() {
		// Kill the running proxy BEFORE deleting the task. On Windows,
		// sch.Delete removes the task definition but does NOT terminate
		// the already-started process. If sch.Run above succeeded and
		// a later step (client-config write, registry save) failed, the
		// rollback stack runs — without this kill, an orphan proxy would
		// keep the allocated port bound and break immediate re-register
		// attempts. killByPortFn is a no-op if nothing is listening.
		if capturedPort > 0 {
			_ = killByPortFn(capturedPort, 5*time.Second)
		}
		_ = sch.Delete(capturedTaskName)
		if len(capturedPriorXML) > 0 {
			_ = sch.ImportXML(capturedTaskName, capturedPriorXML)
			// Restart the prior proxy. Without this, re-register rollback
			// would restore the old task definition but leave no process
			// running (we just killed the live proxy above), turning a
			// recoverable re-register error into a hard outage for the
			// language until next logon/manual restart.
			_ = sch.Run(capturedTaskName)
			fmt.Fprintf(w, "  rollback: restored + restarted scheduler task %s\n", capturedTaskName)
		} else {
			fmt.Fprintf(w, "  rollback: deleted scheduler task %s\n", capturedTaskName)
		}
	})
	// Destructive replace: prior task (if any) is Deleted and the new task
	// Created. A Create failure triggers runRollback which fires the
	// closure above to restore priorXML (or no-op if there was no prior).
	//
	// Kill the currently-running proxy FIRST when replacing. Windows Task
	// Scheduler's Delete does NOT terminate the running child — without
	// this kill, the old proxy keeps `port` bound and sch.Run below fails
	// to bind. Only meaningful when we're actually replacing (priorXML
	// non-empty); on a first-time registration the port is unbound.
	if len(priorXML) > 0 && port > 0 {
		_ = killByPortFn(port, 5*time.Second)
	}
	_ = sch.Delete(taskName)
	taskSpec := scheduler.TaskSpec{
		Name:             taskName,
		Description:      fmt.Sprintf("mcp-local-hub: workspace %s lang %s", canonical, lang),
		Command:          canonicalExe,
		Args:             args,
		WorkingDir:       filepath.Dir(canonicalExe),
		RestartOnFailure: true,
		LogonTrigger:     true,
	}
	if err := sch.Create(taskSpec); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("create task %s: %w", taskName, err)
	}
	fmt.Fprintf(w, "\u2713 Scheduler task created: %s\n", taskName)
	// Pre-compute client entry names so the registry entry can be fully
	// composed BEFORE we start the proxy. The daemon launched by sch.Run
	// loads workspaces.yaml on startup and exits if its (workspaceKey,
	// language) is absent — persisting-before-Run closes that race.
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
	// On re-register (idempotent path, had == true), preserve the prior
	// weekly_refresh value. Otherwise a user who previously registered
	// with --no-weekly-refresh would have it silently re-enabled by any
	// later `mcphub register` invocation, since opts.WeeklyRefresh
	// defaults to true in the CLI flow. A caller that wants to CHANGE
	// the setting on re-register must use a dedicated path (e.g., a
	// future `mcphub workspaces set weekly-refresh=...`).
	weeklyRefresh := opts.WeeklyRefresh
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
	reg.Put(composedEntry)
	if err := reg.Save(); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("persist registry: %w", err)
	}
	capturedRegKey := wsKey
	capturedRegLang := lang
	capturedHad := had
	capturedPrior := prior
	*rollback = append(*rollback, func() {
		// Rollback may fire at any phase — before or after Phase 1 releases
		// the flock — so re-acquire it here. If acquisition fails (extreme
		// cases: registry path unreachable, concurrent holder deadlocked),
		// log and continue so sibling rollback closures still run.
		unlock, err := reg.Lock()
		if err != nil {
			fmt.Fprintf(w, "  rollback: could not lock registry for %s/%s: %v\n",
				capturedRegKey, capturedRegLang, err)
			return
		}
		defer unlock()
		if err := reg.Load(); err != nil {
			fmt.Fprintf(w, "  rollback: could not reload registry for %s/%s: %v\n",
				capturedRegKey, capturedRegLang, err)
			return
		}
		if capturedHad {
			// Re-register rollback: restore the prior (workspace, language)
			// entry. Simply removing would leave the scheduler task
			// (possibly restored from priorXML and restarted) pointing at
			// a missing registry row, which workspace-proxy treats as
			// "not registered" and exits — turning a recoverable
			// re-register failure into a persistent outage.
			reg.Put(capturedPrior)
			_ = reg.Save()
			fmt.Fprintf(w, "  rollback: restored prior registry entry %s/%s\n", capturedRegKey, capturedRegLang)
			return
		}
		reg.Remove(capturedRegKey, capturedRegLang)
		_ = reg.Save()
		fmt.Fprintf(w, "  rollback: removed registry entry %s/%s\n", capturedRegKey, capturedRegLang)
	})

	// Phase 1 complete: the registry row is persisted to disk. Release the
	// flock BEFORE sch.Run so the proxy subprocess launched by the scheduler
	// task can acquire it. Holding the flock through sch.Run + readiness
	// probe deadlocks the proxy: daemon_workspace.go's reg.Lock() blocks on
	// us, its port never binds, our readiness probe times out, and we then
	// roll back the registry row the already-blocked proxy was waiting to
	// read. Net result: "error: not registered" from the proxy and a
	// consistent 10s register failure. Regression-guarded by
	// TestRegisterOneLanguage_ReleasesFlockBeforeSchRun.
	releaseUnlock()

	// Start the proxy. Registry is already persisted, so daemon startup
	// finds the entry. Logon-triggered tasks only fire at the next logon,
	// so this sch.Run prevents the port from advertising dead until reboot.
	if err := sch.Run(taskName); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("run task %s: %w", taskName, err)
	}
	// Verify readiness. Windows schtasks /Run only triggers the task
	// action; it does not report whether the action actually succeeded.
	// Without this probe, a bad binary path, port contention, startup
	// crash, or task-XML drift would produce a successful-looking
	// register whose client configs point at a dead port. The probe
	// polls 127.0.0.1:<port>/mcp with synthetic initialize until it
	// succeeds OR the bounded timeout elapses. Rollback stack fires
	// on timeout so registry / scheduler / client entries do not leak
	// for a proxy that never came up.
	if err := proxyReadinessFn(port, 10*time.Second); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("proxy readiness on port %d: %w", port, err)
	}
	fmt.Fprintf(w, "\u2713 Scheduler task started: %s\n", taskName)
	// 2. Write client entries. Names + entry were pre-composed above;
	// this loop just pushes entries into each client's config and
	// registers per-client rollbacks.
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
		fmt.Fprintf(w, "\u2713 %s \u2192 %s (entry %s)\n", b.Client, entry.URL, entryName)
	}
	return composedEntry, nil
}

// Unregister removes scheduler tasks, client-config entries, and registry
// rows for the named languages in workspacePath. If languages is empty/nil,
// every language for the workspace is removed. Cleanup is atomic in the
// sense that the registry is saved once at the end; scheduler and client
// operations are best-effort and captured in Warnings.
//
// Unknown workspaces (no entries matching workspace_key) return an error;
// unknown individual languages inside an otherwise-known workspace surface
// as warnings so the caller gets a best-effort teardown.
func (a *API) Unregister(workspacePath string, languages []string) (*UnregisterReport, error) {
	data, err := loadManifestYAMLEmbedFirst("mcp-language-server")
	if err != nil {
		return nil, fmt.Errorf("load manifest mcp-language-server: %w", err)
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return a.unregisterWithManifest(m, workspacePath, languages, os.Stderr)
}

func (a *API) unregisterWithManifest(m *config.ServerManifest, workspacePath string, languages []string, w io.Writer) (*UnregisterReport, error) {
	// Use the existence-tolerant variant: the operator may be cleaning up
	// a registration whose workspace directory has since been deleted,
	// moved, or is on an unavailable drive. Without this weakening, an
	// orphaned scheduler task / client entry / registry row survives
	// forever because the user cannot run `mcphub unregister` against a
	// missing path. Registration paths still use the strict form.
	canonical, err := CanonicalWorkspacePathForCleanup(workspacePath)
	if err != nil {
		return nil, err
	}
	wsKey := WorkspaceKey(canonical)
	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return nil, err
	}
	existing := reg.ListByWorkspace(wsKey)
	if len(existing) == 0 {
		return nil, fmt.Errorf("workspace %s (key %s) is not registered", canonical, wsKey)
	}
	targets := languages
	if len(targets) == 0 {
		for _, e := range existing {
			targets = append(targets, e.Language)
		}
	}
	sch, err := schedulerNewForRegister()
	if err != nil {
		return nil, err
	}
	allClients := clientsAllForRegister()
	report := &UnregisterReport{Workspace: canonical, WorkspaceKey: wsKey}
	for _, lang := range targets {
		entry, ok := reg.Get(wsKey, lang)
		if !ok {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("language %s not registered for workspace %s", lang, canonical))
			continue
		}
		// 1. Kill any live proxy bound to this language's port BEFORE we
		// Delete the scheduler task. sch.Delete removes the task record
		// but does NOT terminate the running child — without this kill,
		// the proxy keeps the port bound until the next reboot, which
		// breaks immediate re-register and leaves the registry/scheduler
		// disagreeing with what's actually on the network. Errors are
		// downgraded to warnings because a successful kill-on-absent
		// (nothing listening) is expected for cold workspaces and MUST
		// not fail the teardown path.
		if killByPortFn != nil && entry.Port != 0 {
			if err := killByPortFn(entry.Port, 5*time.Second); err != nil {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("kill proxy on port %d (task %s): %v",
						entry.Port, entry.TaskName, err))
			}
		}
		// 2. Remove scheduler task. Task Scheduler's Delete is the
		// supported way to stop a logon-triggered task from respawning.
		// The kill-by-port above already terminated any live proxy; this
		// Delete prevents it from being re-launched at next logon.
		if err := sch.Delete(entry.TaskName); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("delete task %s: %v", entry.TaskName, err))
		} else {
			fmt.Fprintf(w, "\u2713 deleted scheduler task %s\n", entry.TaskName)
		}
		// 2. Remove client entries.
		for clientName, entryName := range entry.ClientEntries {
			client, ok := allClients[clientName]
			if !ok || !client.Exists() {
				continue
			}
			if err := client.RemoveEntry(entryName); err != nil {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("remove entry %s from %s: %v", entryName, clientName, err))
			} else {
				fmt.Fprintf(w, "\u2713 removed %s entry from %s\n", entryName, clientName)
			}
		}
		// 3. Drop registry row.
		reg.Remove(wsKey, lang)
		report.Removed = append(report.Removed, lang)
	}
	if err := reg.Save(); err != nil {
		return report, fmt.Errorf("persist registry: %w", err)
	}
	return report, nil
}

// ResolveEntryName returns the client-config entry name to use for a given
// (server, language, workspaceKey). The default name is "<server>-<lang>"
// (e.g. mcp-language-server-python). If a DIFFERENT workspace in the
// registry already owns that base name, append "-<4hex>" from the
// workspace key. If the SAME workspace owns it (idempotent re-register),
// return the base name — we never rename an existing managed entry.
func ResolveEntryName(reg *Registry, serverName, language, workspaceKey string) string {
	base := serverName + "-" + language
	// Walk every registry entry; any OTHER workspace using the base name
	// → collision, suffix ours. Our own prior entry → idempotent, same
	// name.
	for _, e := range reg.Workspaces {
		for _, name := range e.ClientEntries {
			if name == base && e.WorkspaceKey != workspaceKey {
				short := workspaceKey
				if len(short) > 4 {
					short = short[:4]
				}
				candidate := base + "-" + short
				// Two workspaces sharing the first 4 hex chars of their
				// keys AND colliding on the same language would otherwise
				// get the same candidate name, causing one register to
				// overwrite the other's client entry. Fall back to the
				// full 8-char key when the short form is also taken.
				if entryNameTakenByOtherWorkspace(reg, candidate, workspaceKey) {
					return base + "-" + workspaceKey
				}
				return candidate
			}
		}
	}
	return base
}

// entryNameTakenByOtherWorkspace returns true iff some registry entry
// (other than our own workspaceKey) already uses the given client entry
// name. Used by ResolveEntryName to escape short-suffix collisions.
func entryNameTakenByOtherWorkspace(reg *Registry, candidate, workspaceKey string) bool {
	for _, e := range reg.Workspaces {
		if e.WorkspaceKey == workspaceKey {
			continue
		}
		for _, name := range e.ClientEntries {
			if name == candidate {
				return true
			}
		}
	}
	return false
}

func sortedLanguageNames(m *config.ServerManifest) []string {
	out := make([]string, 0, len(m.Languages))
	for _, l := range m.Languages {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

// proxyReadinessFn is the test seam for verifyProxyReady. Production
// callers go through this hook. Tests override it to return nil
// immediately (skip the real HTTP probe against the fake scheduler
// whose Run doesn't actually spawn a proxy) or to inject failures.
var proxyReadinessFn = verifyProxyReady

// verifyProxyReady polls 127.0.0.1:<port>/mcp with a minimal MCP
// initialize request until the proxy answers OR the bounded timeout
// elapses. Returns nil on first successful 200 response, error
// otherwise. 200ms polling interval balances quick-success latency
// against thrashing the port during slower startups. Called right
// after sch.Run so register can error-and-rollback instead of
// reporting a successful registration whose proxy never came up.
func verifyProxyReady(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"mcphub-register-readiness","version":"1.0.0"},"capabilities":{}}}`)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build readiness request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		lastErr = fmt.Errorf("readiness probe status %d", resp.StatusCode)
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout after %s", timeout)
	}
	return lastErr
}

// --- Test seams ---------------------------------------------------------
//
// The register path depends on a scheduler backend, a map of client
// adapters, and a registry file path. All three are injected through
// package-scoped function hooks that default to the real production
// implementations. Tests assign replacements via newRegisterHarness and
// restore them in defer.

// testScheduler is the subset of scheduler.Scheduler the register/unregister
// paths use. Keeping the interface narrow makes fake implementations
// trivial.
type testScheduler interface {
	Create(spec scheduler.TaskSpec) error
	Delete(name string) error
	Run(name string) error
	ExportXML(name string) ([]byte, error)
	ImportXML(name string, xml []byte) error
}

// registerClient is the subset of clients.Client the register path
// consumes. Lets tests substitute an in-memory map.
type registerClient interface {
	Exists() bool
	AddEntry(clients.MCPEntry) error
	RemoveEntry(name string) error
	GetEntry(name string) (*clients.MCPEntry, error)
}

// Package-level hooks — nil in production (fall back to real schedulers /
// clients); tests assign replacements via newRegisterHarness and restore
// them in defer.
var (
	testSchedulerFactory     func() (testScheduler, error)
	testClientFactory        func() map[string]registerClient
	testRegistryPathOverride string
)

func schedulerNewForRegister() (testScheduler, error) {
	if testSchedulerFactory != nil {
		return testSchedulerFactory()
	}
	real, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	return realSchedulerAdapter{real}, nil
}

func clientsAllForRegister() map[string]registerClient {
	if testClientFactory != nil {
		return testClientFactory()
	}
	out := map[string]registerClient{}
	for name, c := range clients.AllClients() {
		out[name] = realClientAdapter{c}
	}
	return out
}

func registryPathForRegister() (string, error) {
	if testRegistryPathOverride != "" {
		return testRegistryPathOverride, nil
	}
	return DefaultRegistryPath()
}

type realSchedulerAdapter struct{ s scheduler.Scheduler }

func (a realSchedulerAdapter) Create(spec scheduler.TaskSpec) error  { return a.s.Create(spec) }
func (a realSchedulerAdapter) Delete(name string) error              { return a.s.Delete(name) }
func (a realSchedulerAdapter) Run(name string) error                 { return a.s.Run(name) }
func (a realSchedulerAdapter) ExportXML(name string) ([]byte, error) { return a.s.ExportXML(name) }
func (a realSchedulerAdapter) ImportXML(name string, xml []byte) error {
	return a.s.ImportXML(name, xml)
}

type realClientAdapter struct{ c clients.Client }

func (a realClientAdapter) Exists() bool                      { return a.c.Exists() }
func (a realClientAdapter) AddEntry(e clients.MCPEntry) error { return a.c.AddEntry(e) }
func (a realClientAdapter) RemoveEntry(name string) error     { return a.c.RemoveEntry(name) }
func (a realClientAdapter) GetEntry(name string) (*clients.MCPEntry, error) {
	return a.c.GetEntry(name)
}
