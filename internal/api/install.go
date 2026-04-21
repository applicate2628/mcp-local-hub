package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
	"mcp-local-hub/internal/secrets"
)

// mcphubShortName is the bare executable name. Used for Antigravity relay
// entries (subprocess spawners like Node's child_process do honor PATH) and
// for the install preflight "is mcphub on PATH?" check.
var mcphubShortName = func() string {
	if runtime.GOOS == "windows" {
		return "mcphub.exe"
	}
	return "mcphub"
}()

// canonicalMcphubPath returns the absolute path at which `mcphub setup`
// installs the binary: ~/.local/bin/mcphub.exe (Windows) or
// ~/.local/bin/mcphub (Linux/macOS). Scheduler tasks use this path as their
// <Command> because Windows Task Scheduler's CreateProcess call sets
// lpApplicationName — which skips PATH search entirely — so a bare
// "mcphub.exe" Command fails with ERROR_FILE_NOT_FOUND even when PATH
// contains the canonical dir. The path is user-canonical (depends only on
// $HOME / %USERPROFILE%), not dev-location-specific: moving or rebuilding
// the binary and re-running `mcphub setup` keeps scheduler tasks valid
// without any rewrite.
func canonicalMcphubPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "bin", mcphubShortName), nil
}

// Plan describes the side effects that `mcp install --server X` would produce.
// Returned by BuildPlan and rendered by `install --dry-run`.
type Plan struct {
	Server         string
	SchedulerTasks []ScheduledTaskPlan
	ClientUpdates  []ClientUpdatePlan
}

type ScheduledTaskPlan struct {
	Name    string
	Command string
	Args    []string
	Trigger string // human-readable
}

type ClientUpdatePlan struct {
	Client     string
	Path       string
	Action     string // "add" | "replace"
	URL        string
	DaemonName string // manifest daemon this binding points at (for relay-aware adapters)
}

// InstallOpts controls an install invocation.
type InstallOpts struct {
	Server       string
	DaemonFilter string // empty = all daemons in the manifest
	DryRun       bool
	Writer       io.Writer // progress output destination; nil = os.Stderr
}

// InstallAllOpts controls a bulk install.
type InstallAllOpts struct {
	ManifestDir string
	DryRun      bool
	Writer      io.Writer
}

// InstallResult is one row in an InstallAll report.
type InstallResult struct {
	Server string
	Err    error
}

// UninstallReport summarizes what Uninstall actually did. Callers (CLI/GUI)
// render this however they like; the API itself does not print.
type UninstallReport struct {
	Server          string
	TasksDeleted    []string
	TaskDeleteWarns []string
	ClientsUpdated  []string
	ClientWarns     []string
}

// refuseWorkspaceScopedInstall returns a clear error when someone tries to
// `mcphub install --server mcp-language-server`. Workspace-scoped manifests
// require `mcphub register <workspace> [language...]` — there is no implicit
// install semantic for them because the (workspace, language) tuples that a
// workspace-scoped server needs cannot be inferred from the manifest alone.
// Callers may pass a writer for a human-friendly surface; the returned error
// is the machine-readable signal.
func refuseWorkspaceScopedInstall(m *config.ServerManifest, w io.Writer) error {
	if m.Kind != config.KindWorkspaceScoped {
		return nil
	}
	if w != nil {
		fmt.Fprintf(w, "Server %q is workspace-scoped; use `mcphub register <workspace> [language...]` instead of `mcphub install`.\n", m.Name)
	}
	return fmt.Errorf("server %q is workspace-scoped; use `mcphub register <workspace> [language...]`", m.Name)
}

// Install performs the full install flow for one server: reads manifest,
// runs preflight, builds plan, creates scheduler tasks, writes client configs,
// starts daemons.
func (a *API) Install(opts InstallOpts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	// 1. Load manifest (embed FS first, disk fallback for dev flow).
	//    The canonical installed binary resolves manifests from its
	//    embedded FS so an install launched from any cwd finds the same
	//    10 servers the daemon sees — previously install opened disk
	//    and failed or saw a stale subset.
	data, err := loadManifestYAMLEmbedFirst(opts.Server)
	if err != nil {
		return fmt.Errorf("load manifest %s: %w", opts.Server, err)
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return err
	}
	// 1a. Reject workspace-scoped manifests at the Install entrypoint.
	// These require the explicit per-workspace `mcphub register` flow.
	if err := refuseWorkspaceScopedInstall(m, w); err != nil {
		return err
	}
	// 2. Preflight.
	if err := Preflight(m, opts.DaemonFilter); err != nil {
		return err
	}
	// 3. Build plan.
	plan, err := BuildPlan(m, opts.DaemonFilter)
	if err != nil {
		return err
	}
	// 4. Dry-run?
	if opts.DryRun {
		return printPlanTo(w, plan)
	}
	// 5. Execute.
	return executeInstallTo(w, m, plan)
}

// InstallAll is the production entry point for bulk install. Reads
// the authoritative server list from the embed FS (with disk union
// for dev flow) so the canonical installed binary behaves identically
// regardless of cwd or whether a dev source tree sits nearby.
//
// Workspace-scoped manifests are skipped silently — not a failure, just
// not this command's job. Such servers require the explicit per-workspace
// `mcphub register` flow; a notice is emitted to w so the user knows why
// those names were omitted.
func (a *API) InstallAll(dryRun bool, w io.Writer) []InstallResult {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return []InstallResult{{Err: err}}
	}
	var results []InstallResult
	var skipped []string
	for _, name := range names {
		// Probe the manifest kind cheaply. A parse error here is not
		// fatal — the normal install path will surface the same error
		// with its usual wrapping — so we fall through on failure.
		if data, derr := loadManifestYAMLEmbedFirst(name); derr == nil {
			if mf, perr := config.ParseManifest(bytes.NewReader(data)); perr == nil && mf.Kind == config.KindWorkspaceScoped {
				skipped = append(skipped, name)
				continue
			}
		}
		err := a.installUsingEmbedFirst(InstallOpts{
			Server: name,
			DryRun: dryRun,
			Writer: w,
		})
		results = append(results, InstallResult{Server: name, Err: err})
	}
	if len(skipped) > 0 && w != nil {
		fmt.Fprintf(w, "Skipped %d workspace-scoped manifest(s); use `mcphub register` instead: %v\n",
			len(skipped), skipped)
	}
	return results
}

// InstallAllFrom installs every manifest under the explicit opts.ManifestDir.
// Retained for test hermetic-filesystem use and legacy callers that pass
// a tempdir; production uses InstallAll which consults the embed FS.
//
// Workspace-scoped manifests in the directory are skipped silently (same
// contract as InstallAll).
func (a *API) InstallAllFrom(opts InstallAllOpts) []InstallResult {
	var results []InstallResult
	entries, err := os.ReadDir(opts.ManifestDir)
	if err != nil {
		return []InstallResult{{Err: err}}
	}
	var skipped []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestPath := filepath.Join(opts.ManifestDir, e.Name(), "manifest.yaml")
		if _, err := os.Stat(manifestPath); err != nil {
			continue
		}
		// Skip workspace-scoped manifests — same contract as InstallAll.
		if f, oerr := os.Open(manifestPath); oerr == nil {
			mf, perr := config.ParseManifest(f)
			_ = f.Close()
			if perr == nil && mf.Kind == config.KindWorkspaceScoped {
				skipped = append(skipped, e.Name())
				continue
			}
		}
		err := a.installFromManifestDir(InstallOpts{
			Server: e.Name(),
			DryRun: opts.DryRun,
			Writer: opts.Writer,
		}, opts.ManifestDir)
		results = append(results, InstallResult{Server: e.Name(), Err: err})
	}
	if len(skipped) > 0 && opts.Writer != nil {
		fmt.Fprintf(opts.Writer, "Skipped %d workspace-scoped manifest(s); use `mcphub register` instead: %v\n",
			len(skipped), skipped)
	}
	return results
}

// installUsingEmbedFirst is the install entry that loads the manifest
// via loadManifestYAMLEmbedFirst. Full Preflight is intentionally
// downgraded for the bulk-install case (ports belonging to a server
// already installed on this machine would otherwise abort the batch),
// but the subset of checks that are always relevant — command
// availability, canonical mcphub, and secret references — still runs
// so a broken manifest fails fast instead of leaving partial state.
func (a *API) installUsingEmbedFirst(opts InstallOpts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	data, err := loadManifestYAMLEmbedFirst(opts.Server)
	if err != nil {
		return fmt.Errorf("load manifest %s: %w", opts.Server, err)
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if err := preflightBulkSubset(m); err != nil {
		return err
	}
	plan, err := BuildPlan(m, opts.DaemonFilter)
	if err != nil {
		return err
	}
	if opts.DryRun {
		return printPlanTo(w, plan)
	}
	return executeInstallTo(w, m, plan)
}

// preflightBulkSubset is the subset of Preflight that is always safe
// to run in --all / bulk install mode: command + canonical mcphub
// + secret references. Port-in-use checks are skipped because they
// would reject every server already running on the host.
func preflightBulkSubset(m *config.ServerManifest) error {
	if _, err := exec.LookPath(m.Command); err != nil {
		return fmt.Errorf("command %q not found on PATH: %w", m.Command, err)
	}
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		return fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalPath, err)
	}
	if _, err := exec.LookPath(mcphubShortName); err != nil {
		return fmt.Errorf("%s not on PATH — run `mcphub setup`: %w", mcphubShortName, err)
	}
	return checkSecretRefs(m.Env)
}

// installFromManifestDir is Install-like but with an explicit manifestDir
// override. Used by InstallAllFrom so tests can point at a tempdir without
// mutating global executable-path state. Runs the same preflight subset
// as installUsingEmbedFirst (command + canonical mcphub + secrets) —
// port checks are skipped because bulk installs legitimately coexist
// with already-running sibling daemons.
func (a *API) installFromManifestDir(opts InstallOpts, manifestDir string) error {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	manifestPath := filepath.Join(manifestDir, opts.Server, "manifest.yaml")
	f, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", manifestPath, err)
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
	if err != nil {
		return err
	}
	if err := preflightBulkSubset(m); err != nil {
		return err
	}
	plan, err := BuildPlan(m, opts.DaemonFilter)
	if err != nil {
		return err
	}
	if opts.DryRun {
		return printPlanTo(w, plan)
	}
	return executeInstallTo(w, m, plan)
}

// Status returns the current scheduler view of all mcp-local-hub tasks,
// enriched with Server/Daemon/Port parsed from manifest, plus PID/RAM/Uptime
// for Running tasks when the OS introspection layer is available (Windows,
// populated by internal/api/processes.go at init). NextRun is surfaced as a
// raw backend-specific string (the locale-formatted time schtasks emits on
// Windows, empty elsewhere); callers that need a parsed time.Time should
// re-query the scheduler directly.
func (a *API) Status() ([]DaemonStatus, error) {
	return a.StatusWithOpts(StatusOpts{})
}

// StatusWithHealth is the pre-M5 shim kept for backwards compatibility. New
// callers should prefer StatusWithOpts.
func (a *API) StatusWithHealth(probeHealth bool) ([]DaemonStatus, error) {
	return a.StatusWithOpts(StatusOpts{ProbeHealth: probeHealth})
}

// StatusOpts bundles the flags governing Status enrichment. ProbeHealth
// toggles the synthetic initialize + tools/list round-trip. When
// ForceMaterialize is also true, workspace-scoped rows receive an
// additional no-op tools/call that triggers real backend materialization
// (the proxy writes LifecycleActive/Missing/Failed to the registry, which
// the enrichment then reloads onto the row).
//
// ForceMaterialize requires ProbeHealth. StatusWithOpts returns
// ErrForceMaterializeRequiresHealth when that invariant is violated —
// both the CLI help and the `--force-materialize` flag description promise
// this dependency. Allowing the flag in isolation would either be a
// silent no-op (confusing) or trigger materialization without the
// operator asking for the accompanying probe.
type StatusOpts struct {
	ProbeHealth      bool
	ForceMaterialize bool
}

// ErrForceMaterializeRequiresHealth enforces the documented `--health`
// prerequisite for `--force-materialize`. Callers should surface this
// verbatim to the end user.
var ErrForceMaterializeRequiresHealth = errors.New("--force-materialize requires --health")

// StatusWithOpts is Status + optional MCP-level probes. When
// opts.ProbeHealth is true, the function POSTs initialize + tools/list to
// each Running daemon's /mcp endpoint and fills DaemonStatus.Health. When
// opts.ForceMaterialize is also true, workspace-scoped rows get an
// additional no-op tools/call that drives the lazy proxy through
// materialization and records the resulting 5-state lifecycle in the
// registry (which this function then reloads onto the row).
//
// Disabled by default because the probe adds 1-3 s of per-daemon HTTP
// round-trips — acceptable for an interactive command but wasteful for
// repeated polling.
func (a *API) StatusWithOpts(opts StatusOpts) ([]DaemonStatus, error) {
	if opts.ForceMaterialize && !opts.ProbeHealth {
		return nil, ErrForceMaterializeRequiresHealth
	}
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		return nil, err
	}
	result := make([]DaemonStatus, 0, len(tasks))
	for _, t := range tasks {
		result = append(result, DaemonStatus{
			TaskName:   t.Name,
			State:      t.State,
			LastResult: int32(t.LastResult),
			NextRun:    t.NextRun,
		})
	}
	// Empty dir → enrichStatus uses the embed-first resolution path so
	// `mcphub status` from %TEMP% sees the same server set that the
	// daemon sees. Registry path is best-effort; if DefaultRegistryPath
	// errors (no $HOME, etc.), workspace-scoped rows get the task-name
	// parse only — their Lifecycle column shows "-" rather than a value.
	regPath, regErr := DefaultRegistryPath()
	if regErr != nil {
		regPath = ""
	}
	enrichStatusWithRegistry(result, "", regPath)
	if opts.ProbeHealth {
		probeDaemonHealth(result)
	}
	if opts.ForceMaterialize {
		forceMaterializeWorkspaceScoped(result, regPath)
	}
	return result, nil
}

// forceMaterializeProbe is the hook StatusWithOpts uses when
// opts.ForceMaterialize is true. Production path is sendForceMaterializeTools
// which actually POSTs a no-op tools/call over HTTP; tests replace this
// variable so they can assert "Materialize was triggered" without spinning
// up a real HTTP server.
//
// The result string is captured into the registry as LastError when
// non-empty; a nil error + empty string means the probe returned a valid
// JSON-RPC response (either success or JSON-RPC error — classification
// happens inside the hook).
var forceMaterializeProbe = sendForceMaterializeTools

// forceMaterializeWorkspaceScoped walks rows and for every workspace-scoped
// entry (non-empty Language), sends a real no-op tools/call via the
// forceMaterializeProbe hook. The proxy will drive the backend through
// materialization and record the 5-state lifecycle in the registry.
// After every row is probed, the registry is reloaded and the rows'
// Lifecycle + LastError + timestamp fields are refreshed.
func forceMaterializeWorkspaceScoped(rows []DaemonStatus, regPath string) {
	var wg sync.WaitGroup
	for i := range rows {
		if rows[i].Language == "" || rows[i].Port == 0 {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			forceMaterializeProbe(rows[idx].Port, rows[idx].Backend)
		}(i)
	}
	wg.Wait()
	// Reload the registry once so every workspace-scoped row sees the
	// proxy's post-probe lifecycle + timestamp writes.
	if regPath == "" {
		return
	}
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return
	}
	// Normalize leading "\" on both sides: Windows Task Scheduler's List
	// returns tasks with a leading "\" (e.g. "\mcp-local-hub-lsp-abc-python"),
	// whereas the registry stores the bare "mcp-local-hub-lsp-abc-python" form.
	// Without this the post-probe refresh silently misses every workspace-scoped
	// row on Windows even though the proxy has already written the new state.
	normalizeTaskName := func(s string) string { return strings.TrimPrefix(s, "\\") }
	byTask := make(map[string]WorkspaceEntry, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		byTask[normalizeTaskName(e.TaskName)] = e
	}
	for i := range rows {
		if rows[i].Language == "" {
			continue
		}
		if e, ok := byTask[normalizeTaskName(rows[i].TaskName)]; ok {
			rows[i].Lifecycle = e.Lifecycle
			rows[i].LastMaterializedAt = e.LastMaterializedAt
			rows[i].LastToolsCallAt = e.LastToolsCallAt
			rows[i].LastError = e.LastError
		}
	}
}

// sendForceMaterializeTools POSTs a safe no-op tools/call to
// http://127.0.0.1:<port>/mcp so the lazy proxy triggers its own backend
// materialization. Tool choice per backend:
//
//	mcp-language-server → "hover" (safe, read-only; diagnostics alternative
//	  requires a loaded file that may not exist in the workspace yet)
//	gopls-mcp           → "go_workspace" (no-arg diagnostic equivalent)
//	other               → "tools/call" with a benign empty name; the proxy
//	  materializes before validating the tool name, so any valid
//	  JSON-RPC shape is enough to drive materialization
//
// The function does not interpret the response body. The proxy records
// LifecycleActive on success, LifecycleMissing/LifecycleFailed on failure,
// and the caller reloads the registry to observe whichever was written.
func sendForceMaterializeTools(port int, backend string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	toolName := "hover"
	switch backend {
	case "gopls-mcp":
		toolName = "go_workspace"
	}
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":101,"method":"tools/call","params":{"name":%q,"arguments":{}}}`,
		toolName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// probeDaemonHealth fills DaemonStatus.Health for every Running row
// with a Port. The protocol: POST initialize (stream OR json Accept),
// capture Mcp-Session-Id, POST tools/list, count tools in the
// response. Any transport or JSON-RPC error is captured as Err with
// OK=false. Runs concurrently across rows to keep total time bounded.
//
// Workspace-scoped lazy proxies answer both initialize and tools/list
// synthetically from the embedded tool catalog without spawning the
// heavy backend. The probe therefore verifies the proxy is alive but
// says nothing about the underlying LSP binary. We tag those rows with
// Source="proxy-synthetic" so the CLI layer can distinguish them from
// a global-daemon row where a successful probe implies the upstream
// MCP server is also alive.
func probeDaemonHealth(rows []DaemonStatus) {
	var wg sync.WaitGroup
	for i := range rows {
		if rows[i].State != "Running" || rows[i].Port == 0 {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h := singleHealthProbeFn(rows[idx].Port)
			// Mark lazy-proxy probes by task-name structure, not by
			// registry-populated Language. Language can be empty when
			// registry enrichment fails (missing/corrupt file) even
			// though the task is clearly a lazy proxy. Without the
			// Source tag the CLI would show "OK (N)" as if a real
			// backend validated, when only the synthetic tools/list
			// responded — misleading during incidents.
			if h != nil && IsLazyProxyTaskName(rows[idx].TaskName) {
				h.Source = "proxy-synthetic"
			}
			rows[idx].Health = h
		}(i)
	}
	wg.Wait()
}

// singleHealthProbe does the initialize → tools/list sequence against
// 127.0.0.1:<port>/mcp with a 3 s total deadline. Returns a
// populated HealthProbe either way — the CLI decides whether OK or
// Err is the user-visible signal.
// singleHealthProbeFn is the test seam for singleHealthProbe. Production
// callers go through it; tests swap the var so the HTTP round-trip path
// is not exercised and the probe result is deterministic.
var singleHealthProbeFn = singleHealthProbe

func singleHealthProbe(port int) *HealthProbe {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	client := &http.Client{Timeout: 3 * time.Second}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcphub-health","version":"1"}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return &HealthProbe{Err: "initialize: " + err.Error()}
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &HealthProbe{Err: fmt.Sprintf("initialize: HTTP %d", resp.StatusCode)}
	}

	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(listBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req2.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp2, err := client.Do(req2)
	if err != nil {
		return &HealthProbe{Err: "tools/list: " + err.Error()}
	}
	defer resp2.Body.Close()
	raw, _ := io.ReadAll(resp2.Body)
	payload := raw
	// SSE-wrapped response: pull JSON out of the first data: line.
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload = []byte(strings.TrimPrefix(line, "data: "))
			break
		}
	}
	var parsed struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return &HealthProbe{Err: "tools/list: parse: " + err.Error()}
	}
	if len(parsed.Error) > 0 {
		return &HealthProbe{Err: "tools/list: " + string(parsed.Error)}
	}
	return &HealthProbe{OK: true, ToolCount: len(parsed.Result.Tools)}
}

// Uninstall removes all scheduler tasks and client entries for a server.
// It never prints; the returned UninstallReport carries the outcome for
// CLI/GUI rendering.
func (a *API) Uninstall(server string) (*UninstallReport, error) {
	data, err := loadManifestYAMLEmbedFirst(server)
	if err != nil {
		return nil, fmt.Errorf("load manifest %s: %w", server, err)
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	report := &UninstallReport{Server: m.Name}
	// Delete all tasks that begin with our prefix.
	prefix := "mcp-local-hub-" + m.Name
	tasks, err := sch.List(prefix)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if err := sch.Delete(t.Name); err != nil {
			report.TaskDeleteWarns = append(report.TaskDeleteWarns, fmt.Sprintf("delete %s: %v", t.Name, err))
		} else {
			report.TasksDeleted = append(report.TasksDeleted, t.Name)
		}
	}
	// Remove client entries.
	allClients := clients.AllClients()
	for _, b := range m.ClientBindings {
		client := allClients[b.Client]
		if client == nil || !client.Exists() {
			continue
		}
		if err := client.RemoveEntry(m.Name); err != nil {
			report.ClientWarns = append(report.ClientWarns, fmt.Sprintf("remove %s from %s: %v", m.Name, b.Client, err))
			continue
		}
		report.ClientsUpdated = append(report.ClientsUpdated, b.Client)
	}
	return report, nil
}

// BuildPlan translates a manifest into concrete intended actions.
// If daemonFilter is non-empty, only that daemon and its referencing client
// bindings are included; weekly refresh is skipped because a partial install
// does not imply a full-server restart. An unknown daemonFilter is an error
// surfaced before any side effects.
func BuildPlan(m *config.ServerManifest, daemonFilter string) (*Plan, error) {
	if daemonFilter != "" {
		if _, ok := findDaemon(m, daemonFilter); !ok {
			return nil, fmt.Errorf("no daemon %q in manifest %s", daemonFilter, m.Name)
		}
	}
	// Scheduler tasks reference the canonical ~/.local/bin/mcphub.exe
	// path (not dev location). See canonicalMcphubPath for the rationale.
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return nil, err
	}
	p := &Plan{Server: m.Name}
	// Scheduler tasks — one per daemon (global) or lazy (workspace-scoped).
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-" + d.Name,
			Command: canonicalPath,
			Args:    []string{"daemon", "--server", m.Name, "--daemon", d.Name},
			Trigger: "At logon",
		})
	}
	// Weekly refresh restarts the whole server, so it only makes sense for full installs.
	if m.WeeklyRefresh && daemonFilter == "" {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-weekly-refresh",
			Command: canonicalPath,
			Args:    []string{"restart", "--server", m.Name},
			Trigger: "Weekly Sun 03:00",
		})
	}
	// Client updates — one per binding; with a filter, only bindings pointing at the chosen daemon.
	for _, b := range m.ClientBindings {
		if daemonFilter != "" && b.Daemon != daemonFilter {
			continue
		}
		daemon, ok := findDaemon(m, b.Daemon)
		if !ok {
			return nil, fmt.Errorf("binding references unknown daemon %q", b.Daemon)
		}
		path, err := clientConfigPath(b.Client)
		if err != nil {
			return nil, err
		}
		urlPath := b.URLPath
		if urlPath == "" {
			urlPath = "/mcp"
		}
		url := fmt.Sprintf("http://localhost:%d%s", daemon.Port, urlPath)
		p.ClientUpdates = append(p.ClientUpdates, ClientUpdatePlan{
			Client:     b.Client,
			Path:       path,
			Action:     "add/replace",
			URL:        url,
			DaemonName: b.Daemon,
		})
	}
	return p, nil
}

func findDaemon(m *config.ServerManifest, name string) (config.DaemonSpec, bool) {
	for _, d := range m.Daemons {
		if d.Name == name {
			return d, true
		}
	}
	return config.DaemonSpec{}, false
}

// Preflight verifies install preconditions. Returns first error found.
// Called by Install before any side effects.
//
// daemonFilter must match the same filter used by BuildPlan — only daemons
// that the install will actually (re)create have their ports checked. Without
// this alignment, a partial install would fail preflight whenever sibling
// daemons (already running from a prior install) occupy their assigned ports,
// even though those ports are not being touched by the current invocation.
func Preflight(m *config.ServerManifest, daemonFilter string) error {
	// 1. Command available.
	if _, err := exec.LookPath(m.Command); err != nil {
		return fmt.Errorf("command %q not found on PATH: %w", m.Command, err)
	}
	// 2. Canonical mcphub must exist — scheduler tasks reference
	// ~/.local/bin/mcphub.exe by absolute path because Windows Task
	// Scheduler's CreateProcess call skips PATH lookup. Antigravity
	// relay entries still use the short name (Node's child_process
	// honors PATH), so both checks apply.
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		return fmt.Errorf("%s not present — run `mcphub setup` once to install the canonical binary: %w", canonicalPath, err)
	}
	if _, err := exec.LookPath(mcphubShortName); err != nil {
		return fmt.Errorf("%s not on PATH — run `mcphub setup` once to add ~/.local/bin to PATH: %w", mcphubShortName, err)
	}
	// 3. Ports free — only for daemons in the filtered scope.
	//
	// For native-http transports the daemon binds TWO ports: the external
	// client-facing spec.Port, and the internal spec.Port+10000 where the
	// upstream subprocess listens (see cli/daemon.go native-http branch).
	// Both must be free at install time; otherwise the install writes
	// scheduler and client-config entries that immediately fail to start.
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		if portInUse(d.Port) {
			return fmt.Errorf("port %d already in use (needed for daemon %s/%s)", d.Port, m.Name, d.Name)
		}
		if m.Transport == config.TransportNativeHTTP {
			internal := d.Port + config.NativeHTTPInternalPortOffset
			if portInUse(internal) {
				return fmt.Errorf("internal port %d already in use (needed for native-http upstream of %s/%s; external=%d, internal=external+%d)",
					internal, m.Name, d.Name, d.Port, config.NativeHTTPInternalPortOffset)
			}
		}
	}
	// 4. Secret references resolve. Any `secret:<key>` in manifest.Env
	// must already exist in the vault — otherwise the daemon would
	// spawn, fail to start on the missing env var, and the user would
	// chase a cryptic subprocess error. Failing here surfaces the real
	// cause (missing secret) before any side effect is applied.
	if err := checkSecretRefs(m.Env); err != nil {
		return err
	}
	return nil
}

// checkSecretRefs resolves every manifest env value and fails fast on
// the first missing secret. Only probes secret: refs — file:/literal/
// $VAR values are left to the resolver at daemon launch (they have
// different failure modes we don't want to pre-empt here).
func checkSecretRefs(env map[string]string) error {
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		// No vault yet — only fail if at least one secret: ref is
		// declared. Manifests with no secret refs should install
		// cleanly on a fresh machine without any secrets setup.
		for k, v := range env {
			if strings.HasPrefix(v, "secret:") {
				return fmt.Errorf("env[%s]=%q requires a secrets vault; run `mcphub secrets set %s` first (vault open failed: %v)",
					k, v, strings.TrimPrefix(v, "secret:"), err)
			}
		}
		return nil
	}
	resolver := secrets.NewResolver(vault, nil)
	for k, v := range env {
		if !strings.HasPrefix(v, "secret:") {
			continue
		}
		if _, err := resolver.Resolve(v); err != nil {
			return fmt.Errorf("env[%s]=%q: %w (run `mcphub secrets set %s` to provide it)",
				k, v, err, strings.TrimPrefix(v, "secret:"))
		}
	}
	return nil
}

// portInUse returns true if a listener on the given port accepts connections.
func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func printPlanTo(w io.Writer, p *Plan) error {
	fmt.Fprintf(w, "Install plan for server %q (dry-run):\n\n", p.Server)
	fmt.Fprintf(w, "  Scheduler tasks to create (%d):\n", len(p.SchedulerTasks))
	for _, t := range p.SchedulerTasks {
		fmt.Fprintf(w, "    \u2022 %s  [%s]\n        %s %v\n", t.Name, t.Trigger, t.Command, t.Args)
	}
	fmt.Fprintf(w, "\n  Client configs to update (%d):\n", len(p.ClientUpdates))
	for _, u := range p.ClientUpdates {
		fmt.Fprintf(w, "    \u2022 %s (%s)\n        %s  \u2192  %s\n", u.Client, u.Path, u.Action, u.URL)
	}
	fmt.Fprintln(w, "\nNo changes made.")
	_ = clients.Client(nil) // keep import live for later tasks
	return nil
}

func executeInstallTo(w io.Writer, m *config.ServerManifest, p *Plan) error {
	sch, err := scheduler.New()
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	// WorkingDirectory for the scheduler task: anchor at ~/.local/bin/
	// (same directory as the canonical mcphub binary). Using os.Getwd()
	// baked the dev-checkout cwd into the task XML — later invocations
	// from any other cwd broke because scheduler-spawned processes
	// inherited a stale cwd that no longer existed (e.g. R:\Temp\build
	// from a throwaway install run). ~/.local/bin is guaranteed to
	// exist (canonicalMcphubPath just confirmed it) and doesn't rot.
	canonical, err := canonicalMcphubPath()
	if err != nil {
		return err
	}
	workDir := filepath.Dir(canonical)

	// Rollback stack: accumulate compensating operations as side effects
	// are applied. On mid-sequence failure, pop and run them in reverse
	// so a failed install does not leave the system in a half-configured
	// state (scheduler tasks for a server whose client entries were never
	// written, or vice-versa).
	var rollback []func()
	runRollback := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}

	// 1. Create scheduler tasks.
	for _, t := range p.SchedulerTasks {
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          t.Command,
			Args:             t.Args,
			WorkingDir:       workDir,
			RestartOnFailure: true,
		}
		if t.Trigger == "At logon" {
			spec.LogonTrigger = true
		} else if t.Trigger == "Weekly Sun 03:00" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		}
		// Snapshot any existing task before replacing it so rollback can
		// put it back. Prior to this, Delete-before-Create made install
		// idempotent but a mid-sequence failure left the user with
		// NOTHING — the old task was gone and the new one never got
		// created. ExportXML gives us the full Task Scheduler XML of
		// whatever was there; rollback feeds it to ImportXML to restore.
		var priorXML []byte
		if xml, err := sch.ExportXML(spec.Name); err == nil {
			priorXML = xml
		}
		_ = sch.Delete(spec.Name)
		if err := sch.Create(spec); err != nil {
			runRollback()
			return fmt.Errorf("create task %s: %w", spec.Name, err)
		}
		taskName := spec.Name
		savedXML := priorXML // capture for closure
		rollback = append(rollback, func() {
			_ = sch.Delete(taskName)
			if len(savedXML) > 0 {
				if err := sch.ImportXML(taskName, savedXML); err == nil {
					fmt.Fprintf(w, "  rollback: restored prior scheduler task %s\n", taskName)
					return
				}
			}
			fmt.Fprintf(w, "  rollback: deleted scheduler task %s\n", taskName)
		})
		fmt.Fprintf(w, "\u2713 Scheduler task created: %s\n", spec.Name)
	}
	// 2. Backup + update client configs.
	// Populate relay-related fields so adapters for stdio-only clients
	// (e.g. Antigravity) can produce their `command`+`args` entry shape
	// invoking `mcphub.exe relay`. HTTP-native adapters ignore these fields.
	// We reference mcphub by short name and let the client's PATH resolve it;
	// this lets the user move or rebuild mcphub without rewriting every
	// client config.
	allClients := clients.AllClients()
	for _, u := range p.ClientUpdates {
		client := allClients[u.Client]
		if client == nil {
			runRollback()
			return fmt.Errorf("unknown client %q in binding", u.Client)
		}
		if !client.Exists() {
			fmt.Fprintf(w, "\u26a0 Client %s not installed on this machine \u2014 skipping\n", u.Client)
			continue
		}
		// Snapshot the prior entry BEFORE backing up the file or adding
		// the new one. This is the piece that makes rollback atomic on
		// reinstall/replace: if the install fails downstream and the
		// entry already existed with a different URL or relay config,
		// we AddEntry(prior) to restore — instead of RemoveEntry, which
		// would leave the client with no entry at all.
		priorEntry, _ := client.GetEntry(m.Name)

		bak, err := client.Backup()
		if err != nil {
			runRollback()
			return fmt.Errorf("backup %s: %w", u.Client, err)
		}
		fmt.Fprintf(w, "  backup: %s\n", bak)
		entry := clients.MCPEntry{
			Name:         m.Name,
			URL:          u.URL,
			RelayServer:  m.Name,
			RelayDaemon:  u.DaemonName,
			RelayExePath: mcphubShortName,
		}
		if err := client.AddEntry(entry); err != nil {
			runRollback()
			return fmt.Errorf("add entry to %s: %w", u.Client, err)
		}
		// Compensating op: restore the PRIOR entry (if any) or remove
		// the entry we just added (if this was a first-time install).
		// Wholesale-restoring the backup file is still avoided — a
		// concurrent install of a different server would lose its entry
		// if we did that. Entry-level capture+restore keeps the rollback
		// surgical while preserving the full prior state of THIS server.
		clientRef := client
		entryName := m.Name
		savedPrior := priorEntry
		rollback = append(rollback, func() {
			if savedPrior != nil {
				if err := clientRef.AddEntry(*savedPrior); err == nil {
					fmt.Fprintf(w, "  rollback: restored prior %s entry in %s\n", entryName, u.Client)
					return
				}
			}
			_ = clientRef.RemoveEntry(entryName)
			fmt.Fprintf(w, "  rollback: removed %s entry from %s\n", entryName, u.Client)
		})
		fmt.Fprintf(w, "\u2713 %s \u2192 %s\n", u.Client, u.URL)
	}
	// 3. Start daemons immediately (without waiting for next logon).
	for _, t := range p.SchedulerTasks {
		// Skip weekly refresh — it's triggered on schedule, not on install.
		if t.Trigger != "At logon" {
			continue
		}
		if err := sch.Run(t.Name); err != nil {
			fmt.Fprintf(w, "\u26a0 failed to start %s immediately: %v (will start at next logon)\n", t.Name, err)
		} else {
			fmt.Fprintf(w, "\u2713 Started: %s\n", t.Name)
		}
	}
	fmt.Fprintln(w, "\nInstall complete.")
	return nil
}

// clientConfigPath returns the absolute path to the named client's config.
// Private helper owned by the api package; a parallel copy lives in cli for
// commands that do not yet call through api (secrets, rollback).
func clientConfigPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch name {
	case "claude-code":
		return filepath.Join(home, ".claude.json"), nil
	case "codex-cli":
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "gemini-cli":
		return filepath.Join(home, ".gemini", "settings.json"), nil
	case "antigravity":
		return filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), nil
	default:
		return "", fmt.Errorf("unknown client %q (expected claude-code | codex-cli | gemini-cli | antigravity)", name)
	}
}

// Stop stops a running daemon without removing its scheduler task or client
// entries. For each matching task it kills the daemon process by port
// FIRST (schtasks /End only terminates the task's launch action; the
// spawned daemon process keeps the port bound until killed) and then
// calls sch.Stop to clean up the scheduler state. Returns a per-task
// result set so callers can surface partial failures without bailing
// after the first row.
func (a *API) Stop(server, daemonFilter string) ([]RestartResult, error) {
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := listTasksForServer(sch, server)
	if err != nil {
		return nil, err
	}
	ports := manifestPortMap("")
	wsByTask := workspaceTasksByName()
	var results []RestartResult
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		if daemonFilter != "" {
			wantSuffix := "-" + daemonFilter
			if !strings.HasSuffix(normalized, wantSuffix) {
				continue
			}
		}
		if strings.Contains(t.Name, "weekly-refresh") {
			continue // schedule-only task, nothing to kill
		}
		port := portForTask(normalized, ports, wsByTask)
		if port != 0 {
			if err := killDaemonByPort(port, 5*time.Second); err != nil {
				results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
				continue
			}
		}
		_ = sch.Stop(t.Name)
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// Restart kills the live daemons for one server (+ optional daemon
// filter) by port and re-runs their scheduler tasks. The --server
// counterpart of RestartAll: same semantics, narrower scope.
func (a *API) Restart(server, daemonFilter string) ([]RestartResult, error) {
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := listTasksForServer(sch, server)
	if err != nil {
		return nil, err
	}
	ports := manifestPortMap("")
	wsByTask := workspaceTasksByName()
	var results []RestartResult
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		if daemonFilter != "" {
			wantSuffix := "-" + daemonFilter
			if !strings.HasSuffix(normalized, wantSuffix) {
				continue
			}
		}
		if strings.Contains(t.Name, "weekly-refresh") {
			continue
		}
		port := portForTask(normalized, ports, wsByTask)
		if port != 0 {
			if err := killDaemonByPort(port, 5*time.Second); err != nil {
				results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
				continue
			}
		}
		_ = sch.Stop(t.Name)
		if err := sch.Run(t.Name); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: err.Error()})
			continue
		}
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// listTasksForServer returns every scheduler task whose name maps to
// the given server. For global servers that means the classic
// "mcp-local-hub-<server>-" prefix. For workspace-scoped servers (the
// manifest's Kind == workspace-scoped) the per-(workspace, language)
// proxy tasks use "mcp-local-hub-lsp-<key>-<lang>" with NO server slug
// in the name — this helper also queries that prefix so `mcphub stop
// --server mcp-language-server` and `mcphub restart --server
// mcp-language-server` actually target those daemons. Without the
// extended match, workspace-scoped proxies survive every server-scoped
// maintenance command.
func listTasksForServer(sch scheduler.Scheduler, server string) ([]scheduler.TaskStatus, error) {
	primary, err := sch.List("mcp-local-hub-" + server + "-")
	if err != nil {
		return nil, err
	}
	if !serverIsWorkspaceScoped(server) {
		return primary, nil
	}
	lsp, err := sch.List("mcp-local-hub-lsp-")
	if err != nil {
		// Don't fail the entire Stop/Restart if the secondary list
		// errors — log would be nice but we're in API layer; return
		// the primary result set so at least global tasks are handled.
		return primary, nil
	}
	return append(primary, lsp...), nil
}

// serverIsWorkspaceScoped returns true iff the given server name refers
// to a manifest with Kind == workspace-scoped. Misses (unknown manifest,
// load error) return false — classic behavior.
func serverIsWorkspaceScoped(server string) bool {
	data, err := loadManifestYAMLEmbedFirst(server)
	if err != nil {
		return false
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return false
	}
	return m.Kind == config.KindWorkspaceScoped
}

// workspaceTasksByName returns a (taskName → WorkspaceEntry) map from
// the current registry. Used by Stop/Restart to find the right port for
// workspace-scoped lazy-proxy tasks (their ports live in the registry,
// not the manifest). Nil on registry load failure — callers treat it as
// empty and fall back to manifest ports.
func workspaceTasksByName() map[string]WorkspaceEntry {
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return nil
	}
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return nil
	}
	out := make(map[string]WorkspaceEntry, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		out[strings.TrimPrefix(e.TaskName, "\\")] = e
	}
	return out
}

// portForTask resolves the port for a scheduler task name, trying the
// workspace-scoped registry first (for lazy-proxy tasks) and then
// falling back to the manifest port map (for global daemons).
func portForTask(taskName string, ports map[string]map[string]int, wsByTask map[string]WorkspaceEntry) int {
	if e, ok := wsByTask[taskName]; ok && e.Port != 0 {
		return e.Port
	}
	srv, dmn := parseTaskName(taskName)
	if p, ok := ports[srv][dmn]; ok {
		return p
	}
	return ports[srv]["default"]
}

// RestartResult is one row in a RestartAll report.
type RestartResult struct {
	TaskName string
	Err      string
}

// RestartAll stops+starts every scheduler task under our prefix. Returns a
// per-task result list with any errors.
//
// Why we don't rely on scheduler.Stop alone: the task's action (spawning
// the daemon) finishes in milliseconds; the scheduler immediately flips
// the task back to "Ready". `schtasks /End` therefore finds no running
// task instance and silently succeeds, while the daemon process keeps
// running. A subsequent `schtasks /Run` tries to spawn a second daemon,
// hits the bound port, and dies — so the user ends up with the original
// stale daemon they wanted to replace. We have to kill the daemon
// process by port first.
func (a *API) RestartAll() ([]RestartResult, error) {
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		return nil, err
	}
	ports := manifestPortMap("")
	var results []RestartResult
	for _, t := range tasks {
		// Skip weekly-refresh — scheduled, not restarted.
		if strings.Contains(t.Name, "weekly-refresh") {
			continue
		}
		srv, dmn := parseTaskName(t.Name)
		port := ports[srv][dmn]
		if port == 0 {
			port = ports[srv]["default"]
		}
		if err := killDaemonByPort(port, 5*time.Second); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
			continue
		}
		_ = sch.Stop(t.Name) // no-op for completed actions; preserve for the edge case of a mid-launch task
		if err := sch.Run(t.Name); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: err.Error()})
			continue
		}
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// StopAll stops every running scheduler task under our prefix. Leaves tasks
// in place (scheduler will relaunch them at next LogonTrigger unless also
// uninstalled). Kills the daemon process by port (see RestartAll comment
// on why scheduler.Stop alone isn't enough). Returns per-task results so
// the CLI can report failures.
func (a *API) StopAll() ([]RestartResult, error) {
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		return nil, err
	}
	ports := manifestPortMap("")
	var results []RestartResult
	for _, t := range tasks {
		// Skip weekly-refresh — schedule-only task; Stop has no effect anyway.
		if strings.Contains(t.Name, "weekly-refresh") {
			continue
		}
		srv, dmn := parseTaskName(t.Name)
		port := ports[srv][dmn]
		if port == 0 {
			port = ports[srv]["default"]
		}
		if err := killDaemonByPort(port, 5*time.Second); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
			continue
		}
		_ = sch.Stop(t.Name)
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// killByPortFn is a test seam for killDaemonByPort. Production callers go
// through this indirection so the Unregister and WeeklyRefreshAll code
// paths can be unit-tested without spawning real processes bound to ports.
// Tests assign a fake in their setup and restore the default in defer.
var killByPortFn = killDaemonByPort

// killDaemonByPort finds the process listening on 127.0.0.1:port, kills
// its whole tree with taskkill /F /T, and polls until the port is free.
// Returns nil when nothing is listening (nothing to kill).
//
// /T is critical: our hub.exe spawns npx/uvx which spawn node/python.
// Killing only hub.exe leaves the grandchildren running and occupying
// the child-stdin side of the pipe.
func killDaemonByPort(port int, timeout time.Duration) error {
	if lookupProcess == nil || port == 0 {
		return nil
	}
	pid, _, _, ok := lookupProcess(port)
	if !ok {
		return nil
	}
	out, err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F", "/T").CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, _, _, stillUp := lookupProcess(port); !stillUp {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port %d still bound after %s", port, timeout)
}
