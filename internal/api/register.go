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
	"fmt"
	"io"
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
	// 2.5 Ensure the shared weekly-refresh task exists. Idempotent (Delete
	// + Create under the hood) so it is safe to invoke on every Register.
	// Failure here is non-fatal — per-workspace registration must not be
	// blocked by a shared-task problem; surface a warning and carry on.
	// Placed AFTER argument validation so an invalid-language call
	// produces no scheduler side effects at all.
	if err := a.EnsureWeeklyRefreshTask(); err != nil {
		fmt.Fprintf(w, "warning: ensure shared weekly-refresh task: %v\n", err)
	}
	// 3. Acquire the registry lock.
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
	// 4. Per-language register with rollback stack.
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
		reg.Put(entry)
	}
	if err := reg.Save(); err != nil {
		runRollback()
		return report, fmt.Errorf("persist registry: %w", err)
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
	args := []string{
		"daemon", "workspace-proxy",
		"--port", fmt.Sprintf("%d", port),
		"--workspace", canonical,
		"--language", lang,
	}
	var priorXML []byte
	if xml, err := sch.ExportXML(taskName); err == nil {
		priorXML = xml
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
	capturedTaskName := taskName
	capturedPriorXML := priorXML
	*rollback = append(*rollback, func() {
		_ = sch.Delete(capturedTaskName)
		if len(capturedPriorXML) > 0 {
			_ = sch.ImportXML(capturedTaskName, capturedPriorXML)
			fmt.Fprintf(w, "  rollback: restored scheduler task %s\n", capturedTaskName)
		} else {
			fmt.Fprintf(w, "  rollback: deleted scheduler task %s\n", capturedTaskName)
		}
	})
	fmt.Fprintf(w, "\u2713 Scheduler task created: %s\n", taskName)
	// Start the proxy immediately. Logon-triggered tasks only fire at the
	// next logon, so without this call `mcphub register` would advertise a
	// dead port until the user reboots. Mirrors the Phase 2 install path
	// which invokes sch.Run right after Create. On Run failure we fall
	// back to the rollback stack, which will Delete the task (restoring
	// the prior XML, if any) and the registry row.
	if err := sch.Run(taskName); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("run task %s: %w", taskName, err)
	}
	fmt.Fprintf(w, "\u2713 Scheduler task started: %s\n", taskName)
	// 2. Write client entries.
	bindings := m.ClientBindings
	if len(bindings) == 0 {
		bindings = defaultClientBindings
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
		entryName, already := entryNameByClient[b.Client]
		if !already {
			entryName = ResolveEntryName(reg, m.Name, lang, wsKey)
			entryNameByClient[b.Client] = entryName
		}
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
	// 3. Compose registry entry.
	return WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      lang,
		Backend:       spec.Backend,
		Port:          port,
		TaskName:      taskName,
		ClientEntries: entryNameByClient,
		WeeklyRefresh: opts.WeeklyRefresh,
		Lifecycle:     LifecycleConfigured, // lazy-mode: proxy scheduled, backend NOT spawned
	}, nil
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
	canonical, err := CanonicalWorkspacePath(workspacePath)
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
				suffix := workspaceKey
				if len(suffix) > 4 {
					suffix = suffix[:4]
				}
				return base + "-" + suffix
			}
		}
	}
	return base
}

func sortedLanguageNames(m *config.ServerManifest) []string {
	out := make([]string, 0, len(m.Languages))
	for _, l := range m.Languages {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
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
