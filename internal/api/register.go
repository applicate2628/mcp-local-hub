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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// defaultClientBindings is the implicit client binding set used when a
// workspace-scoped manifest does not declare client_bindings. Matches the
// default install clients that support per-entry URLs. Opt-in clients and
// Antigravity's stdio-relay model are intentionally excluded from the implicit
// workspace-scoped write set.
var defaultClientBindings = []config.ClientBinding{
	{Client: "claude-code", URLPath: "/mcp"},
	{Client: "codex-cli", URLPath: "/mcp"},
	{Client: "cursor", URLPath: "/mcp"},
}

// RegisterOpts controls a Register invocation.
type RegisterOpts struct {
	// WeeklyRefreshExplicit selects between two interpretation modes:
	//   - true:  use WeeklyRefresh literally (caller has decided).
	//   - false: ignore WeeklyRefresh; read daemons.weekly_refresh_default
	//            from settings and use that. Memo D1 (opt-in knob).
	// CLI surface: --weekly-refresh / --no-weekly-refresh both flip this
	// to true; absent flag leaves it false (knob path).
	WeeklyRefreshExplicit bool

	// WeeklyRefresh is the value persisted on each created entry when
	// WeeklyRefreshExplicit==true. Ignored otherwise.
	WeeklyRefresh bool

	// SupervisedProxy routes workspace LSP proxies through supervisor-intent
	// instead of the legacy per-language Windows Scheduled Task path. The
	// supervisor path makes the proxy process a Job-protected child of the
	// hub supervisor, matching Serena's daemon ownership model.
	SupervisedProxy bool

	Writer io.Writer // progress output; nil = os.Stderr
}

// resolveWeeklyRefresh picks the effective WeeklyRefresh value for a
// new entry per memo D1: explicit caller override beats the persisted
// knob; absent explicit override, read daemons.weekly_refresh_default
// from settings (default "false").
//
// Error/empty handling: if SettingsGet returns an error (settings file
// absent on first install, or unreadable due to corruption), the
// fallback is false (the opt-out default). A corrupt-YAML error is
// treated the same as "key absent" and is not surfaced here; the
// caller may surface it separately if diagnosability is required.
func resolveWeeklyRefresh(a *API, opts RegisterOpts) bool {
	if opts.WeeklyRefreshExplicit {
		return opts.WeeklyRefresh
	}
	v, err := a.SettingsGet("daemons.weekly_refresh_default")
	if err != nil || v == "" {
		return false
	}
	return v == "true"
}

// RegisterReport summarizes what Register actually created.
type RegisterReport struct {
	Workspace    string           `json:"workspace"`
	WorkspaceKey string           `json:"workspace_key"`
	Entries      []WorkspaceEntry `json:"entries"`
	Warnings     []string         `json:"warnings,omitempty"`
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
	m, err := parseManifestForName("mcp-language-server", data)
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
	var schedulerUnavailableErr error
	if err != nil {
		if schedulerUnavailableError(err) {
			opts.SupervisedProxy = true
			schedulerUnavailableErr = err
			sch = legacyTaskUnavailableScheduler{}
		} else {
			return nil, err
		}
	}
	// 3.1 Ensure the shared weekly-refresh task exists only for the legacy
	// scheduler-backed path. Supervised registration owns its lifecycle
	// through the supervisor intent file; touching the shared scheduler task
	// there would mutate durable state before per-language preconditions
	// such as legacy-port kill/adoption have been proven.
	if !opts.SupervisedProxy {
		if err := a.EnsureWeeklyRefreshTask(); err != nil {
			fmt.Fprintf(w, "warning: ensure shared weekly-refresh task: %v\n", err)
		}
	}
	allClients := clientsAllForRegister()
	var rollback []func()
	runRollback := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}
	report := &RegisterReport{Workspace: canonical, WorkspaceKey: wsKey}
	if schedulerUnavailableErr != nil {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("scheduler unavailable (%v); using supervised LSP proxy path and skipping legacy task handling", schedulerUnavailableErr))
		if err := preflightSchedulerlessRegisterSupervisor(); err != nil {
			return report, err
		}
	}
	for _, lang := range languages {
		entry, err := a.registerOneLanguage(m, canonical, wsKey, lang, opts, reg, sch, allClients, w, &rollback)
		if err != nil {
			runRollback()
			return report, fmt.Errorf("register language %s: %w", lang, err)
		}
		report.Entries = append(report.Entries, entry)
	}
	report.Warnings = append(report.Warnings, a.cleanupDirectLanguageServerEntriesAfterRegister(bySpec, languages, canonical, allClients, w)...)
	return report, nil
}

func preflightSchedulerlessRegisterSupervisor() error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
	defer cancel()
	if _, err := registerSupervisorReconcileFn(ctx, false); err != nil {
		return fmt.Errorf("schedulerless LSP register requires a running supervisor before writing intent: %w; run `mcphub supervise` from another shell and retry", err)
	}
	return nil
}

func (a *API) cleanupDirectLanguageServerEntriesAfterRegister(
	bySpec map[string]config.LanguageSpec,
	languages []string,
	canonicalWorkspace string,
	allClients map[string]registerClient,
	w io.Writer,
) []string {
	aliases := map[string]bool{}
	for _, lang := range languages {
		spec, ok := bySpec[lang]
		if !ok {
			continue
		}
		addLSPCleanupAlias(aliases, lang)
		addLSPCleanupAlias(aliases, spec.LspCommand)
	}
	if len(aliases) == 0 {
		return nil
	}

	keepN := a.EffectiveBackupKeepN()
	var warnings []string
	for clientName, client := range allClients {
		if client == nil || !client.Exists() {
			continue
		}
		entries, err := client.FindStdioLanguageServerEntries()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s direct LSP scan failed: %v", clientName, err))
			continue
		}
		matches := matchingDirectLanguageServerEntries(entries, aliases, canonicalWorkspace)
		if aliases["go"] || aliases["gopls"] {
			stdioEntries, err := client.AllStdioEntries()
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s direct stdio scan failed: %v", clientName, err))
			} else {
				matches = append(matches, matchingDirectGoplsMCPEntries(stdioEntries, canonicalWorkspace)...)
			}
		}
		matches = dedupeDirectLanguageServerEntries(matches)
		if len(matches) == 0 {
			continue
		}
		backupPath, err := client.BackupKeep(keepN)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s direct LSP backup failed: %v", clientName, err))
			continue
		}
		fmt.Fprintf(w, "✓ %s backup before direct LSP cleanup: %s\n", clientName, backupPath)
		for _, entry := range matches {
			if err := client.RemoveEntry(entry.Name); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s remove direct LSP entry %s failed: %v", clientName, entry.Name, err))
				continue
			}
			fmt.Fprintf(w, "✓ %s removed direct LSP entry %s (--lsp %s)\n", clientName, entry.Name, entry.Language)
		}
	}
	return warnings
}

func addLSPCleanupAlias(aliases map[string]bool, value string) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "" {
		aliases[value] = true
	}
}

func matchingDirectLanguageServerEntries(entries []clients.LanguageServerStdioEntry, aliases map[string]bool, canonicalWorkspace string) []clients.LanguageServerStdioEntry {
	var out []clients.LanguageServerStdioEntry
	for _, entry := range entries {
		if lspCleanupAliasMatches(entry.Language, aliases) && directEntryWorkspaceMatches(entry.Args, canonicalWorkspace) {
			out = append(out, entry)
		}
	}
	return out
}

func matchingDirectGoplsMCPEntries(entries []clients.StdioEntry, canonicalWorkspace string) []clients.LanguageServerStdioEntry {
	var out []clients.LanguageServerStdioEntry
	for _, entry := range entries {
		if !isCommandBasename(entry.Command, "gopls") || !stringSliceContainsFold(entry.Args, "mcp") {
			continue
		}
		if !directEntryWorkspaceMatches(entry.Args, canonicalWorkspace) {
			continue
		}
		out = append(out, clients.LanguageServerStdioEntry{
			Name:     entry.Name,
			Command:  entry.Command,
			Language: "gopls",
			Args:     entry.Args,
		})
	}
	return out
}

func directEntryWorkspaceMatches(args []string, canonicalWorkspace string) bool {
	raw := directEntryWorkspaceArg(args)
	if raw == "" {
		return false
	}
	canonical, err := CanonicalWorkspacePathForCleanup(raw)
	if err != nil {
		return false
	}
	return canonical == canonicalWorkspace
}

func directEntryWorkspaceArg(args []string) string {
	for i, arg := range args {
		for _, flag := range []string{"--workspace", "-workspace", "--dir", "-dir", "--directory", "-directory"} {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
			if value, ok := strings.CutPrefix(arg, flag+"="); ok {
				return value
			}
		}
	}
	return ""
}

func dedupeDirectLanguageServerEntries(entries []clients.LanguageServerStdioEntry) []clients.LanguageServerStdioEntry {
	seen := map[string]bool{}
	var out []clients.LanguageServerStdioEntry
	for _, entry := range entries {
		if entry.Name == "" || seen[entry.Name] {
			continue
		}
		seen[entry.Name] = true
		out = append(out, entry)
	}
	return out
}

func lspCleanupAliasMatches(value string, aliases map[string]bool) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	return aliases[value]
}

func isCommandBasename(command, want string) bool {
	base := strings.ReplaceAll(command, "\\", "/")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	return base == want
}

func stringSliceContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
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
	if opts.SupervisedProxy {
		return a.registerOneLanguageSupervised(m, spec, canonical, wsKey, lang, opts, reg, sch, allClients, w, rollback)
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
		//
		// B.1: this is an LSP-row write; PutLSP enforces the @-prefix gate.
		if err := reg.PutLSP(WorkspaceEntry{WorkspaceKey: wsKey, WorkspacePath: canonical, Language: lang, Port: port}); err != nil {
			return WorkspaceEntry{}, fmt.Errorf("register: tentative LSP-row write rejected: %w", err)
		}
		capturedKey := wsKey
		capturedLang := lang
		*rollback = append(*rollback, func() {
			reg.Remove(capturedKey, capturedLang)
		})
	}
	taskName := LSPTaskNameForWorkspaceLanguage(wsKey, lang)
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
	startedNewTask := false
	*rollback = append(*rollback, func() {
		// Kill the running proxy BEFORE deleting the task. On Windows,
		// sch.Delete removes the task definition but does NOT terminate
		// the already-started process. If sch.Run above succeeded and
		// a later step (client-config write, registry save) failed, the
		// rollback stack runs — without this kill, an orphan proxy would
		// keep the allocated port bound and break immediate re-register
		// attempts. killByPortFn is a no-op if nothing is listening.
		if startedNewTask && capturedPort > 0 {
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
		Name:        taskName,
		Description: fmt.Sprintf("mcp-local-hub: workspace %s lang %s", canonical, lang),
		Command:     canonicalExe,
		Args:        args,
		// WorkingDir is the canonical workspace path, NOT the install
		// directory. Two reasons:
		//
		// 1. LSP backends (clangd, rust-analyzer, gopls, …) expect cwd to
		//    be the project root for compile_commands.json / Cargo.toml /
		//    go.mod discovery. Running them from ~/.local/bin/ broke that.
		//
		// 2. v0.3.0-blockers bug #1: Go 1.19+ exec.LookPath enforces
		//    CVE-2022-30580 — refuses to return a cwd-relative match. The
		//    install dir ~/.local/bin/ may contain a stale copy of the
		//    wrapper binary (mcp-language-server.exe), and Windows lookup
		//    semantics check cwd FIRST, so the cwd-relative match
		//    shadows the on-PATH copy and triggers ErrDot. Setting cwd
		//    to the workspace removes the shadow: the workspace
		//    contains source files only, never the wrapper binary, so
		//    LookPath falls through to PATH and finds the canonical
		//    install (~/go/bin or wherever the user has it).
		WorkingDir:       canonical,
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
			entryNameByClient[b.Client] = resolveWorkspaceScopedLSPEntryName(reg, m.Name, lang, wsKey)
		}
	}
	// On re-register (idempotent path, had == true), preserve the prior
	// weekly_refresh value. Otherwise a user who previously registered
	// with --no-weekly-refresh would have it silently re-enabled by any
	// later `mcphub register` invocation. A caller that wants to CHANGE
	// the setting on re-register must use a dedicated path (e.g., a
	// future `mcphub workspaces set weekly-refresh=...`).
	// Memo D1: knob-aware default. Explicit caller flag still wins.
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
	// B.1: composedEntry is an LSP-row write; PutLSP enforces the @-prefix
	// gate (Language is the per-LSP language string, never the sentinel).
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
	startedNewTask = true
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
	// Task 10 plan \u00a765: AFTER scheduler create + sch.Run + readiness
	// PASS, record Desired=running intent + workspace-registered audit
	// entry. Audit / intent failures are logged + tolerated \u2014 the
	// workspace is already registered (registry on disk + scheduler
	// task created + proxy started + readiness probe passed).
	a.recordRegisterIntentForTask(taskName, w)
	// Phase 3: re-acquire flock before client config writes. Client
	// adapters perform read-modify-write updates, so these writes must be
	// serialized against concurrent register/unregister operations.
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
	m, err := parseManifestForName("mcp-language-server", data)
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
	// Legacy fallback: registry rows written before EvalSymlinks-based
	// canonicalization landed used the abs+clean+drive-normalized form
	// only. If the new key has no rows, retry against the legacy key
	// once so a one-shot symlink-migration unregister works.
	legacyCanonical, err := CanonicalWorkspacePathLegacyCompat(workspacePath)
	if err != nil {
		return nil, err
	}
	legacyWSKey := WorkspaceKey(legacyCanonical)
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
	// B.1: default unregister removes only LSP rows; serena rows live under
	// SerenaLanguageSentinel and are removed via `--backend serena` /
	// `--backend all` (mapped to RemoveByBackend in the CLI). Using
	// ListByWorkspaceLSP centralises the sentinel filter at both the
	// canonical-key lookup and the legacy-key fallback.
	existing := reg.ListByWorkspaceLSP(wsKey)
	activeWSKey := wsKey
	if len(existing) == 0 && legacyWSKey != wsKey {
		existing = reg.ListByWorkspaceLSP(legacyWSKey)
		if len(existing) > 0 {
			activeWSKey = legacyWSKey
		}
	}
	if len(existing) == 0 {
		return nil, fmt.Errorf("workspace %s (key %s) is not registered", canonical, wsKey)
	}
	targets := languages
	if len(targets) == 0 {
		for _, e := range existing {
			targets = append(targets, e.Language)
		}
	}
	allClients := clientsAllForRegister()
	report := &UnregisterReport{Workspace: canonical, WorkspaceKey: activeWSKey}
	sch, schErr := schedulerNewForRegister()
	// Non-Windows backends return not-implemented; supervised LSP rows have no
	// scheduled task to delete there, so tolerate absence and skip only the
	// legacy-task Delete below.
	if schErr != nil {
		if schedulerUnavailableError(schErr) {
			sch = nil
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("scheduler unavailable (%v); skipping legacy task deletion (supervised rows have none)", schErr))
		} else {
			return nil, schErr
		}
	}
	for _, lang := range targets {
		entry, ok := reg.Get(activeWSKey, lang)
		if !ok {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("language %s not registered for workspace %s", lang, canonical))
			continue
		}
		intentTaskName := LSPIntentTaskNameForWorkspaceLanguage(activeWSKey, lang)
		if _, supervisorManaged, err := a.removeLSPSupervisorIntent(activeWSKey, lang); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("remove supervisor intent %s: %v", intentTaskName, err))
			continue
		} else if supervisorManaged {
			ctx, cancel := context.WithTimeout(context.Background(), DefaultReconcileTimeout)
			if _, err := registerSupervisorReconcileFn(ctx, true); err != nil {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("supervisor reconcile after removing %s: %v", intentTaskName, err))
			} else {
				fmt.Fprintf(w, "✓ removed supervisor intent %s\n", intentTaskName)
			}
			cancel()
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
		if sch != nil {
			if err := sch.Delete(entry.TaskName); err != nil {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("delete task %s: %v", entry.TaskName, err))
			} else {
				fmt.Fprintf(w, "\u2713 deleted scheduler task %s\n", entry.TaskName)
			}
		}
		// 2. Remove client entries.
		for clientName, entryName := range entry.ClientEntries {
			client, ok := allClients[clientName]
			if !ok || !client.Exists() {
				continue
			}
			if shouldPreserveSharedLSPRouterEntry(client, entryName, lang) {
				fmt.Fprintf(w, "✓ preserved shared LSP router entry %s in %s\n", entryName, clientName)
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
		reg.Remove(activeWSKey, lang)
		report.Removed = append(report.Removed, lang)
	}
	if err := reg.Save(); err != nil {
		return report, fmt.Errorf("persist registry: %w", err)
	}
	return report, nil
}

func shouldPreserveSharedLSPRouterEntry(client registerClient, entryName, language string) bool {
	if entryName != LSPRouterEntryName(language) {
		return false
	}
	live, err := client.GetEntry(entryName)
	if err != nil {
		return false
	}
	return entryIsLSPRouterForLanguage(live, language)
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

type legacyTaskUnavailableScheduler struct{}

func (legacyTaskUnavailableScheduler) Create(scheduler.TaskSpec) error {
	return errors.New("scheduler unavailable for legacy task creation")
}
func (legacyTaskUnavailableScheduler) Delete(string) error { return scheduler.ErrTaskNotFound }
func (legacyTaskUnavailableScheduler) Run(string) error    { return scheduler.ErrTaskNotFound }
func (legacyTaskUnavailableScheduler) ExportXML(string) ([]byte, error) {
	return nil, scheduler.ErrTaskNotFound
}
func (legacyTaskUnavailableScheduler) ImportXML(string, []byte) error {
	return errors.New("scheduler unavailable for legacy task restore")
}

// registerClient is the subset of clients.Client the register path
// consumes. Lets tests substitute an in-memory map.
type registerClient interface {
	Exists() bool
	BackupKeep(keepN int) (string, error)
	AddEntry(clients.MCPEntry) error
	RemoveEntry(name string) error
	GetEntry(name string) (*clients.MCPEntry, error)
	AllStdioEntries() ([]clients.StdioEntry, error)
	FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error)
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

func (a realClientAdapter) Exists() bool { return a.c.Exists() }
func (a realClientAdapter) BackupKeep(keepN int) (string, error) {
	return a.c.BackupKeep(keepN)
}
func (a realClientAdapter) AddEntry(e clients.MCPEntry) error { return a.c.AddEntry(e) }
func (a realClientAdapter) RemoveEntry(name string) error     { return a.c.RemoveEntry(name) }
func (a realClientAdapter) GetEntry(name string) (*clients.MCPEntry, error) {
	return a.c.GetEntry(name)
}
func (a realClientAdapter) AllStdioEntries() ([]clients.StdioEntry, error) {
	return a.c.AllStdioEntries()
}
func (a realClientAdapter) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return a.c.FindStdioLanguageServerEntries()
}
