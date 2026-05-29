// Package cli — `mcphub migrate serena legacy-to-dynamic-pool`
// (Phase D.3 of docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md).
//
// The driver migrates the serena server from its legacy 2-daemon (or
// unified-intermediate) `kind: global` manifest to the dynamic-pool
// `kind: workspace-scoped` + daemon_template shape, fanning out one
// supervisor-intent daemon per registered serena workspace.
//
// Command placement: `serena` is attached as a subcommand of the
// existing `mcphub migrate <server>...` command. Because the existing
// `mcphub migrate serena [more]` stdio→HTTP migrate usage would
// otherwise be shadowed by the new subcommand (cobra resolves a
// matching subcommand name before treating it as a positional arg — a
// behavior verified empirically before this wiring), the `serena`
// subcommand carries its OWN RunE that delegates to the existing
// migrate logic (re-prepending the consumed `serena` token + mirroring
// the migrate flags). Only `mcphub migrate serena legacy-to-dynamic-pool`
// routes to the dynamic-pool driver.
//
// Rollback composition (plan §D.3 "Outer/inner rollback composition"):
// this driver owns the OUTER stack covering ONLY the manifest write
// (step 5) and the registry alloc/save (step 6). api.InstallParsedManifest
// owns the INNER stack for scheduler tasks + per-client config + intent
// write (steps that map to executeInstallTo). When InstallParsedManifest
// returns an error, its inner stack has ALREADY undone its own steps; the
// outer stack then undoes only manifest + registry. The outer stack never
// pushes the inner's undos, so there is no double-undo.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// serenaManifestServerName is the server-name segment of the on-disk
// serena manifest path (<manifest-dir>/serena/manifest.yaml).
const serenaManifestServerName = "serena"

// installParsedManifestFn is the package-level test seam over
// api.InstallParsedManifest. Production resolves to the API method; the
// rollback tests override it to inject an intent-write failure without
// reaching the real scheduler / state-file pipeline. Production callers
// MUST NOT reassign it.
var installParsedManifestFn = func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
	return a.InstallParsedManifest(ctx, m, opts)
}

// serenaSourceState classifies the detected source shape of the on-disk
// serena manifest. The migration is only defined for the legacy and
// intermediate shapes; already-migrated is a no-op and malformed is a
// hard error.
type serenaSourceState int

const (
	serenaSourceMalformed serenaSourceState = iota
	serenaSourceLegacy2Daemon
	serenaSourceUnifiedIntermediate
	serenaSourceAlreadyMigrated
)

func (s serenaSourceState) String() string {
	switch s {
	case serenaSourceLegacy2Daemon:
		return "legacy-2-daemon"
	case serenaSourceUnifiedIntermediate:
		return "unified-intermediate"
	case serenaSourceAlreadyMigrated:
		return "already-migrated"
	default:
		return "malformed"
	}
}

// detectSerenaSourceState classifies a parsed serena manifest per the
// D.3 source-state table. Refuse-on-malformed: any shape outside the
// three recognized states returns serenaSourceMalformed.
func detectSerenaSourceState(m *config.ServerManifest) (serenaSourceState, error) {
	switch {
	case len(m.Daemons) == 0 && m.DaemonTemplate != nil:
		return serenaSourceAlreadyMigrated, nil
	case m.DaemonTemplate == nil && len(m.Daemons) == 2 &&
		daemonNamed(m.Daemons, "claude") && daemonNamed(m.Daemons, "codex"):
		return serenaSourceLegacy2Daemon, nil
	case m.DaemonTemplate == nil && len(m.Daemons) == 1 && daemonNamed(m.Daemons, "unified"):
		return serenaSourceUnifiedIntermediate, nil
	default:
		return serenaSourceMalformed, fmt.Errorf(
			"serena manifest is in an unrecognized state (daemons=%d, daemon_template_present=%t) — "+
				"manual reconciliation required: the migration only supports the legacy 2-daemon "+
				"(claude+codex), unified-intermediate (single `unified`), or already-migrated "+
				"(daemon_template only) shapes",
			len(m.Daemons), m.DaemonTemplate != nil)
	}
}

// daemonNamed reports whether daemons contains an entry with the given name.
func daemonNamed(daemons []config.DaemonSpec, name string) bool {
	for _, d := range daemons {
		if d.Name == name {
			return true
		}
	}
	return false
}

// newMigrateSerenaCmd builds the `serena` subcommand of `migrate`.
//
// It preserves the existing `mcphub migrate serena [more] [flags]`
// stdio→HTTP behavior via a delegating RunE (so the subcommand does not
// shadow the documented usage), and attaches the
// `legacy-to-dynamic-pool` dynamic-pool migration as a child.
func newMigrateSerenaCmd() *cobra.Command {
	var clientsFlag string
	var dryRun, jsonOut bool
	c := &cobra.Command{
		Use:   "serena [server]...",
		Short: "serena-specific migration helpers",
		Long: `Subcommand group for serena migrations.

With no further subcommand, ` + "`mcphub migrate serena [more] [flags]`" + ` behaves
exactly like ` + "`mcphub migrate serena [more] [flags]`" + ` did before this command
existed: it rewrites the serena (and any additional listed servers') client
entries from stdio to hub HTTP. The dynamic-pool migration lives under the
` + "`legacy-to-dynamic-pool`" + ` subcommand.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Re-prepend the consumed "serena" token so the delegate
			// migrates the serena server plus any additional servers.
			servers := append([]string{serenaManifestServerName}, args...)
			return runStdioMigrate(cmd, servers, clientsFlag, dryRun, jsonOut)
		},
	}
	c.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated subset of clients (default: every binding in the manifest)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change, don't write")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	c.AddCommand(newMigrateSerenaDynamicPoolCmd())
	return c
}

// newMigrateSerenaDynamicPoolCmd builds
// `mcphub migrate serena legacy-to-dynamic-pool`.
func newMigrateSerenaDynamicPoolCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "legacy-to-dynamic-pool",
		Short: "Migrate serena from legacy 2-daemon/unified to the dynamic per-workspace pool",
		Long: `Rewrite servers/serena/manifest.yaml from its legacy global 2-daemon
(claude+codex) or unified-intermediate shape to the dynamic-pool
kind=workspace-scoped + daemon_template form, then fan out one
supervisor-intent daemon per registered serena workspace.

Idempotent: re-running against an already-migrated manifest is a no-op
(exit 0, no writes).

Prerequisites: at least one serena workspace must be registered first
(mcphub workspace register <path>). The supervisor picks up the new
intent on its next IntentWatcher tick (within 60s); no restart is
required.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrateSerenaDynamicPool(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

// runMigrateSerenaDynamicPool is the dynamic-pool migration driver.
//
// err is named so the deferred outer-rollback closure can inspect and
// rewrite it (composite-error contract).
func runMigrateSerenaDynamicPool(ctx context.Context, w io.Writer) (err error) {
	manifestPath := filepath.Join(api.WritableManifestDir(), serenaManifestServerName, "manifest.yaml")

	// 1. Read + parse the on-disk serena manifest; detect source state.
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read serena manifest %s: %w", manifestPath, err)
	}
	m, err := config.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("parse serena manifest %s: %w", manifestPath, err)
	}
	state, err := detectSerenaSourceState(m)
	if err != nil {
		return err
	}

	// 2. already-migrated → no-op, exit 0, zero writes.
	if state == serenaSourceAlreadyMigrated {
		fmt.Fprintln(w, "serena manifest is already migrated to the dynamic pool; nothing to do.")
		return nil
	}

	// 3. Acquire registry flock; snapshot serena workspaces.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return err
	}
	workspaces := reg.SerenaEntries()

	// 4. Refuse on empty workspace registry.
	if len(workspaces) == 0 {
		return fmt.Errorf(
			"no serena workspaces registered — register at least one workspace before migration " +
				"(mcphub workspace register <path> --backend serena --languages <list>)")
	}

	// Open the audit log up front so both success and rollback paths can
	// emit. A nil log (open failure) degrades to no audit but does not
	// abort the migration.
	events := openMigrateAuditLog(w)

	// OUTER rollback stack — owns ONLY step 5 (manifest write) and step 6
	// (registry alloc/save). The inner InstallParsedManifest stack owns
	// scheduler/client/intent undos; the outer stack never pushes those,
	// so there is no double-undo.
	var rollback []func() error
	defer func() {
		if err == nil {
			return
		}
		var rbErrs []error
		for i := len(rollback) - 1; i >= 0; i-- {
			if e := rollback[i](); e != nil {
				rbErrs = append(rbErrs, e)
			}
		}
		if len(rbErrs) > 0 {
			failed := make([]string, 0, len(rbErrs))
			for _, e := range rbErrs {
				failed = append(failed, e.Error())
			}
			if events != nil {
				_ = events.Emit(api.SupervisorEvent{
					Severity: "error",
					Source:   "migration",
					Event:    "rollback-incomplete",
					Body: map[string]any{
						"primary_error": err.Error(),
						"failed_undos":  failed,
					},
				})
			}
			err = fmt.Errorf("%w; rollback also failed: %v", err, errors.Join(rbErrs...))
		}
	}()

	// 5. Rewrite the manifest body (drop daemons[], add daemon_template).
	newBody, err := buildDynamicPoolManifest(m)
	if err != nil {
		return err
	}
	manifestBackup, err := snapshotManifest(manifestPath)
	if err != nil {
		return err
	}
	rollback = append(rollback, func() error { return restoreManifest(manifestPath, manifestBackup) })
	if err := writeNewManifest(manifestPath, newBody); err != nil {
		return err
	}

	// 6. Allocate a serena port per workspace + Save. Snapshot the
	// registry BEFORE allocation so the undo restores the exact prior
	// rows (ports + any other field) and re-persists them.
	portsBefore := snapshotRegistry(reg)
	if err := allocateSerenaPorts(reg, workspaces); err != nil {
		return err
	}
	rollback = append(rollback, func() error { return restoreRegistry(reg, portsBefore) })
	if err := reg.Save(); err != nil {
		return err
	}

	// Re-read the freshly-allocated serena rows so the fan-out and the
	// audit event see the assigned ports.
	allocated := reg.SerenaEntries()

	// Parse the rewritten manifest so InstallParsedManifest receives the
	// dynamic-pool shape (DaemonTemplate != nil) it fans out on.
	newManifest, err := config.ParseManifest(bytes.NewReader(newBody))
	if err != nil {
		return err
	}

	// 7. Install the parsed manifest. The inner InstallParsedManifest
	// stack owns scheduler/client/intent undos; on error here, those are
	// already undone and the outer stack fires for manifest + registry
	// only. No ClientsInclude / IncludeAllClients: the dynamic-pool
	// manifest carries no client_bindings (the per-workspace router owns
	// routing), so the only side effect is the per-workspace
	// supervisor-intent fan-out driven by opts.Workspaces.
	// StartAfterWrite=false defers daemon spawn to the reconciler.
	if _, ierr := installParsedManifestFn(ctx, api.NewAPI(), newManifest, api.InstallParsedManifestOpts{
		Writer:          w,
		Workspaces:      allocated,
		StartAfterWrite: false,
	}); ierr != nil {
		err = ierr
		return err
	}

	// 8. Emit the success audit event.
	if events != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "migration",
			Event:    "serena-dynamic-pool-migration",
			Body: map[string]any{
				"source_state":      state.String(),
				"target_workspaces": workspacePaths(allocated),
				"allocated_ports":   workspacePorts(allocated),
			},
		})
	}

	// 9. Success — clear rollback so the deferred undo is a no-op.
	rollback = nil
	fmt.Fprintln(w, "supervisor will pick up new intent within 60s (next IntentWatcher tick); no restart required.")
	return nil
}

// openMigrateAuditLog resolves the supervisor-events.log path under the
// active state dir and opens it. A failure degrades to nil (no audit) +
// a warning; it never aborts the migration.
func openMigrateAuditLog(w io.Writer) *api.SupervisorEventLog {
	stateDir, err := stateDirFunc()
	if err != nil {
		fmt.Fprintf(w, "warning: cannot resolve state dir for audit log: %v\n", err)
		return nil
	}
	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		fmt.Fprintf(w, "warning: cannot open supervisor-events.log: %v\n", err)
		return nil
	}
	return events
}

// dynamicPoolManifestOut is the YAML output shape for a rewritten
// dynamic-pool serena manifest. It is a PURPOSE-BUILT emit struct (not
// config.ServerManifest) because that struct's url/headers/daemons/etc.
// fields lack omitempty, so marshaling it directly emits empty `url:` /
// `headers:` keys that ParseManifest's key-presence pass then rejects
// under transport=native-http. This struct emits ONLY the keys a valid
// dynamic-pool manifest carries.
//
// client_bindings are intentionally NOT emitted: in the dynamic-pool
// model the per-workspace router (Phase F) owns all client routing, and
// the legacy static bindings reference the claude/codex daemons that the
// migration drops — keeping them would make BuildPlanWithOpts reject the
// manifest ("binding references unknown daemon"). Plan §G.2 ("client
// bindings become unused in dynamic-pool, router handles all bindings").
type dynamicPoolManifestOut struct {
	Name           string                 `yaml:"name"`
	Kind           string                 `yaml:"kind"`
	Transport      string                 `yaml:"transport"`
	Command        string                 `yaml:"command"`
	BaseArgs       []string               `yaml:"base_args,omitempty"`
	Env            map[string]string      `yaml:"env,omitempty"`
	DaemonTemplate dynamicPoolTemplateOut `yaml:"daemon_template"`
	WeeklyRefresh  bool                   `yaml:"weekly_refresh"`
}

type dynamicPoolTemplateOut struct {
	Context           string          `yaml:"context"`
	PortPool          config.PortPool `yaml:"port_pool"`
	ExtraArgsTemplate []string        `yaml:"extra_args_template"`
}

// buildDynamicPoolManifest renders the dynamic-pool manifest body from
// the legacy/intermediate source manifest. It preserves identity fields
// (name, transport, command, base_args, env) and replaces the daemons[]
// block with a daemon_template carrying the canonical serena port pool +
// ${workspace.path} project arg. client_bindings are dropped (see the
// dynamicPoolManifestOut doc).
func buildDynamicPoolManifest(src *config.ServerManifest) ([]byte, error) {
	out := dynamicPoolManifestOut{
		Name:      src.Name,
		Kind:      config.KindWorkspaceScoped,
		Transport: src.Transport,
		Command:   src.Command,
		BaseArgs:  src.BaseArgs,
		Env:       src.Env,
		DaemonTemplate: dynamicPoolTemplateOut{
			Context:           serenaDynamicPoolContext,
			PortPool:          config.PortPool{Start: serenaDynamicPoolPortStart, End: serenaDynamicPoolPortEnd},
			ExtraArgsTemplate: []string{"--project", config.WorkspacePathToken},
		},
		WeeklyRefresh: src.WeeklyRefresh,
	}

	body, err := yaml.Marshal(&out)
	if err != nil {
		return nil, fmt.Errorf("marshal dynamic-pool manifest: %w", err)
	}
	// Defensive: round-trip through the validator so a malformed rewrite
	// fails the migration before it touches disk.
	if _, perr := config.ParseManifest(bytes.NewReader(body)); perr != nil {
		return nil, fmt.Errorf("rewritten dynamic-pool manifest failed validation: %w", perr)
	}
	return body, nil
}

// Dynamic-pool template defaults. The port pool matches the
// serena dynamic-pool window the D.1 schema + workspace_cmd register
// flow allocate from.
const (
	serenaDynamicPoolContext   = "ide-assistant"
	serenaDynamicPoolPortStart = 9400
	serenaDynamicPoolPortEnd   = 9499
)

// ---------------------------------------------------------------------------
// Named helpers (plan §D.3 v7).
// ---------------------------------------------------------------------------

// snapshotManifest reads the current manifest bytes for rollback restore.
func snapshotManifest(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot manifest %s: %w", path, err)
	}
	return data, nil
}

// restoreManifest writes the backup bytes back to path (atomic).
func restoreManifest(path string, backup []byte) error {
	if err := writeNewManifest(path, backup); err != nil {
		return fmt.Errorf("restore manifest %s: %w", path, err)
	}
	return nil
}

// snapshotRegistry returns a deep-enough copy of the registry's
// workspace rows for rollback restore. WorkspaceEntry is a value type
// whose only reference fields (ClientEntries map, Languages slice) are
// not mutated by allocateSerenaPorts, so a shallow per-element copy of
// the slice is sufficient to restore the pre-allocation port state.
func snapshotRegistry(reg *api.Registry) []api.WorkspaceEntry {
	return append([]api.WorkspaceEntry(nil), reg.Workspaces...)
}

// restoreRegistry resets the registry rows to the snapshot and persists.
func restoreRegistry(reg *api.Registry, snapshot []api.WorkspaceEntry) error {
	reg.Workspaces = append([]api.WorkspaceEntry(nil), snapshot...)
	if err := reg.Save(); err != nil {
		return fmt.Errorf("restore registry: %w", err)
	}
	return nil
}

// allocateSerenaPorts assigns a serena pool port to every workspace row
// that does not already have one, writing the assignment back into the
// registry via PutSerena. Idempotent for rows that already carry a port.
func allocateSerenaPorts(reg *api.Registry, workspaces []api.WorkspaceEntry) error {
	pool := config.PortPool{Start: serenaDynamicPoolPortStart, End: serenaDynamicPoolPortEnd}
	for _, ws := range workspaces {
		// Re-read the live row (the registry is the source of truth; the
		// passed snapshot may be stale relative to in-loop mutations).
		cur, ok := reg.GetSerena(ws.WorkspaceKey)
		if !ok {
			continue
		}
		if cur.Port > 0 {
			continue
		}
		port, err := reg.AllocateSerenaPort(pool)
		if err != nil {
			return fmt.Errorf("allocate serena port for workspace %s: %w", cur.WorkspacePath, err)
		}
		cur.Port = port
		if cur.TaskName == "" {
			cur.TaskName = "mcp-local-hub-serena-" + cur.WorkspaceKey
		}
		if err := reg.PutSerena(cur); err != nil {
			return fmt.Errorf("persist allocated port for workspace %s: %w", cur.WorkspacePath, err)
		}
	}
	return nil
}

// writeNewManifest writes body to path atomically (temp + rename),
// mirroring Registry.Save / autostart.atomicWriteFile. The serena
// manifest dir already exists (the source manifest was read from it).
func writeNewManifest(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir manifest dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp manifest: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp manifest: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp manifest: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp manifest into place: %w", err)
	}
	return nil
}

// workspacePaths / workspacePorts extract the audit-event body slices.
func workspacePaths(entries []api.WorkspaceEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.WorkspacePath)
	}
	return out
}

func workspacePorts(entries []api.WorkspaceEntry) []int {
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Port)
	}
	return out
}
