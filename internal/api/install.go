package api

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

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
	DaemonFilter string    // empty = all daemons in the manifest
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

// Install performs the full install flow for one server: reads manifest,
// runs preflight, builds plan, creates scheduler tasks, writes client configs,
// starts daemons.
func (a *API) Install(opts InstallOpts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	// 1. Load manifest.
	manifestPath := filepath.Join(defaultManifestDir(), opts.Server, "manifest.yaml")
	f, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", manifestPath, err)
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
	if err != nil {
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

// InstallAll is the production entry point for bulk install using
// defaultManifestDir().
func (a *API) InstallAll(dryRun bool, w io.Writer) []InstallResult {
	return a.InstallAllFrom(InstallAllOpts{
		ManifestDir: defaultManifestDir(),
		DryRun:      dryRun,
		Writer:      w,
	})
}

// InstallAllFrom installs every manifest under ManifestDir. It continues past
// individual failures and returns the per-server result list so callers can
// display a summary.
func (a *API) InstallAllFrom(opts InstallAllOpts) []InstallResult {
	var results []InstallResult
	entries, err := os.ReadDir(opts.ManifestDir)
	if err != nil {
		return []InstallResult{{Err: err}}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(opts.ManifestDir, e.Name(), "manifest.yaml")); err != nil {
			continue
		}
		err := a.installFromManifestDir(InstallOpts{
			Server: e.Name(),
			DryRun: opts.DryRun,
			Writer: opts.Writer,
		}, opts.ManifestDir)
		results = append(results, InstallResult{Server: e.Name(), Err: err})
	}
	return results
}

// installFromManifestDir is Install-like but with an explicit manifestDir
// override. Used by InstallAllFrom so tests can point at a tempdir without
// mutating global executable-path state. Preflight is intentionally omitted
// here — bulk installs may run against manifests whose sibling daemons are
// already occupying their ports from a prior install, which would otherwise
// abort the entire batch.
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
	enrichStatus(result, defaultManifestDir())
	return result, nil
}

// Uninstall removes all scheduler tasks and client entries for a server.
// It never prints; the returned UninstallReport carries the outcome for
// CLI/GUI rendering.
func (a *API) Uninstall(server string) (*UninstallReport, error) {
	manifestPath := filepath.Join(defaultManifestDir(), server, "manifest.yaml")
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := config.ParseManifest(f)
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
	exe, err := os.Executable()
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
			Command: exe,
			Args:    []string{"daemon", "--server", m.Name, "--daemon", d.Name},
			Trigger: "At logon",
		})
	}
	// Weekly refresh restarts the whole server, so it only makes sense for full installs.
	if m.WeeklyRefresh && daemonFilter == "" {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    "mcp-local-hub-" + m.Name + "-weekly-refresh",
			Command: exe,
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
	// 2. Ports free — only for daemons in the filtered scope.
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		if portInUse(d.Port) {
			return fmt.Errorf("port %d already in use (needed for daemon %s/%s)", d.Port, m.Name, d.Name)
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
	repoDir, err := os.Getwd()
	if err != nil {
		return err
	}
	// 1. Create scheduler tasks.
	for _, t := range p.SchedulerTasks {
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          t.Command,
			Args:             t.Args,
			WorkingDir:       repoDir,
			RestartOnFailure: true,
		}
		if t.Trigger == "At logon" {
			spec.LogonTrigger = true
		} else if t.Trigger == "Weekly Sun 03:00" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		}
		// Delete any previous instance so Create is idempotent.
		_ = sch.Delete(spec.Name)
		if err := sch.Create(spec); err != nil {
			return fmt.Errorf("create task %s: %w", spec.Name, err)
		}
		fmt.Fprintf(w, "\u2713 Scheduler task created: %s\n", spec.Name)
	}
	// 2. Backup + update client configs.
	// Populate relay-related fields so adapters for stdio-only clients
	// (e.g. Antigravity) can produce their `command`+`args` entry shape
	// invoking `mcphub.exe relay`. HTTP-native adapters ignore these fields.
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	allClients := clients.AllClients()
	for _, u := range p.ClientUpdates {
		client := allClients[u.Client]
		if client == nil {
			return fmt.Errorf("unknown client %q in binding", u.Client)
		}
		if !client.Exists() {
			fmt.Fprintf(w, "\u26a0 Client %s not installed on this machine \u2014 skipping\n", u.Client)
			continue
		}
		bak, err := client.Backup()
		if err != nil {
			return fmt.Errorf("backup %s: %w", u.Client, err)
		}
		fmt.Fprintf(w, "  backup: %s\n", bak)
		entry := clients.MCPEntry{
			Name:         m.Name,
			URL:          u.URL,
			RelayServer:  m.Name,
			RelayDaemon:  u.DaemonName,
			RelayExePath: exePath,
		}
		if err := client.AddEntry(entry); err != nil {
			return fmt.Errorf("add entry to %s: %w", u.Client, err)
		}
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
// entries. Equivalent to `schtasks /End /TN <name>` for each task matching
// the server (+ daemon filter if set).
func (a *API) Stop(server, daemonFilter string) error {
	sch, err := scheduler.New()
	if err != nil {
		return err
	}
	tasks, err := sch.List("mcp-local-hub-" + server + "-")
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if daemonFilter != "" {
			wantSuffix := "-" + daemonFilter
			if !strings.HasSuffix(strings.TrimPrefix(t.Name, "\\"), wantSuffix) {
				continue
			}
		}
		if err := sch.Stop(t.Name); err != nil {
			return fmt.Errorf("stop %s: %w", t.Name, err)
		}
	}
	return nil
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
	ports := manifestPortMap(defaultManifestDir())
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
	ports := manifestPortMap(defaultManifestDir())
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

