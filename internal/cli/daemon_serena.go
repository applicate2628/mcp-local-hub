package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"

	"github.com/spf13/cobra"
)

// newDaemonSerenaProxyCmd returns the `mcphub daemon serena-proxy`
// subcommand the supervisor launches per registered serena workspace.
//
// Not intended for interactive use - it is the CLI entrypoint that
// supervisor-intent.json descriptors emitted by
// api.BuildSupervisorDaemonsForSerena point at. Hidden from default
// help.
//
// Flow (descriptor-driven — the proxy reads its MATERIALIZED runtime
// spec off its own supervisor-intent descriptor and NEVER re-reads the
// server manifest; design
// docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md §4):
//
//  1. Validate flags (--port, --workspace, --server, --task-name).
//  2. Canonicalize workspace; compute wsKey; open per-workspace log.
//  3. Load OWN descriptor by --task-name from supervisor-intent.json;
//     assert the descriptor/flag consistency contract (§3.2) and the
//     RuntimeSpec nil/unsupported-version fail-loud guard (§4). NO
//     manifest fallback — a missing/inconsistent spec fails loud.
//  4. Resolve env (secret:KEY references via vault) over spec.EnvRefs.
//  5. childArgs = spec.ChildArgs ++ [--port, spec.UpstreamPort]. The
//     spec already carries the expanded --project <workspace> and the
//     appended --context <value>; the proxy adds only the internal port.
//  6. Spawn the upstream native-http child via daemon.HTTPHost and
//     ListenAndServe an external port -> internal-port reverse proxy.
//  7. Standard shutdown semantics on cmd.Context().Done() or ChildExited.
//
// The kind/template/transport gates that used to live here moved to
// build/install time (BuildSupervisorDaemonsForSerena + the
// InstallParsedManifest native-http contract gate).
func newDaemonSerenaProxyCmd() *cobra.Command {
	var (
		portFlag      int
		workspaceFlag string
		serverFlag    string
		taskNameFlag  string
	)
	c := &cobra.Command{
		Use:    "serena-proxy",
		Short:  "Launch a per-workspace serena native-http daemon",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if portFlag <= 0 {
				return fmt.Errorf("--port is required and must be > 0")
			}
			if workspaceFlag == "" {
				return fmt.Errorf("--workspace is required")
			}
			if serverFlag == "" {
				serverFlag = "serena"
			}
			if taskNameFlag == "" {
				return fmt.Errorf("--task-name is required (the proxy looks up its own supervisor-intent descriptor by task name)")
			}
			if err := api.CheckManifestName(serverFlag); err != nil {
				return err
			}

			canonical, err := api.CanonicalWorkspacePath(workspaceFlag)
			if err != nil {
				return fmt.Errorf("canonical workspace path: %w", err)
			}
			wsKey := api.WorkspaceKey(canonical)

			// Per-workspace log file so the GUI Logs picker can attribute
			// each serena instance separately. Mirrors the lsp-<wsKey>-<lang>.log
			// naming the LSP workspace-proxy uses.
			logPath := filepath.Join(logBaseDir(), fmt.Sprintf("%s-%s.log", serverFlag, wsKey))
			defer func() {
				if err != nil {
					writeLaunchFailure(logPath, serverFlag, "serena-"+wsKey, err)
				}
			}()

			// Load the materialized runtime spec off THIS daemon's own
			// supervisor-intent descriptor (looked up by --task-name) and
			// enforce the descriptor/flag consistency contract + the
			// nil/unsupported-version fail-loud guard. NO manifest read,
			// NO manifest fallback (design §4) — a missing/inconsistent
			// spec fails loud rather than re-reading the embedded legacy
			// kind:global manifest (the defect this redesign kills).
			spec, err := loadSerenaProxyRuntimeSpec(taskNameFlag, canonical, portFlag)
			if err != nil {
				return err
			}

			// Resolve env (secret:KEY -> vault lookup) over the spec's raw
			// env refs (cleartext-free on disk; resolved in-process here).
			// Secrets are OPTIONAL (install-and-it-works): best-effort so a
			// workspace-scoped daemon with a skipped `secret:` ref still spawns
			// (env var omitted) instead of failing — matching the global-daemon
			// path (Codex #377). $VAR/file: refs stay fatal.
			vault, _ := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			resolver := secrets.NewResolver(vault, nil)
			// Best-effort: a skipped optional `secret:` ref is omitted (spawn
			// proceeds) rather than fatal; $VAR/file: refs stay fatal. The
			// omitted keys are passed as UnsetEnv so the host removes them from
			// the child's inherited os.Environ() — truly absent, not
			// present-but-empty, and no ambient-parent inheritance (Codex #377).
			env, omittedSecrets, err := resolver.ResolveMapBestEffort(spec.EnvRefs)
			if err != nil {
				return err
			}
			unsetEnv := make([]string, 0, len(omittedSecrets))
			for k := range omittedSecrets {
				unsetEnv = append(unsetEnv, k)
			}

			// Final child argv: the spec's fully-materialized ChildArgs
			// (already carrying expanded --project + appended --context)
			// plus the internal upstream port the child binds.
			childArgs := serenaProxyChildArgs(spec)

			h, err := daemon.NewHTTPHost(daemon.HTTPHostConfig{
				Command:      spec.ChildCommand,
				Args:         childArgs,
				Env:          env,
				UnsetEnv:     unsetEnv,
				UpstreamPort: spec.UpstreamPort,
				LogPath:      logPath,
			})
			if err != nil {
				return fmt.Errorf("NewHTTPHost: %w", err)
			}

			ctx := cmd.Context()
			if err := h.Start(ctx); err != nil {
				return fmt.Errorf("httphost.Start: %w%s", err, formatChildExit(h.ExitState()))
			}

			srv := &http.Server{
				Addr:              fmt.Sprintf("127.0.0.1:%d", portFlag),
				Handler:           h.HTTPHandler(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()
			select {
			case err := <-errCh:
				_ = h.Stop()
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return fmt.Errorf("http server: %w", err)
			case <-ctx.Done():
				_ = h.Stop()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
				return nil
			case <-h.ChildExited():
				exitMsg := formatChildExit(h.ExitState())
				_ = h.Stop()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
				return fmt.Errorf("native-http upstream exited unexpectedly%s", exitMsg)
			}
		},
	}
	c.Flags().IntVar(&portFlag, "port", 0, "TCP port to bind on 127.0.0.1 (required; per-workspace from registry)")
	c.Flags().StringVar(&workspaceFlag, "workspace", "", "absolute workspace path (required)")
	c.Flags().StringVar(&serverFlag, "server", "serena", "manifest name to load (defaults to serena)")
	c.Flags().StringVar(&taskNameFlag, "task-name", "", "canonical supervisor-intent task name of THIS daemon (required; the proxy reads its own RuntimeSpec descriptor by this key)")
	return c
}

// loadSerenaProxyRuntimeSpec loads the serena-proxy's OWN supervisor-intent
// descriptor by taskName and returns its materialized RuntimeSpec, enforcing
// the fail-loud boundary defenses (design §4) and the descriptor/flag
// consistency contract (design §3.2).
//
// It NEVER reads the server manifest. Every failure path returns an
// operator-actionable error and the caller exits non-zero; there is no silent
// fallback to a manifest read (which would re-introduce the embed-shadow
// defect this redesign exists to kill).
//
//   - descriptor not found for taskName            -> fail loud (names the task)
//   - RuntimeSpec nil (pre-spec / stale row)        -> fail loud ("reinstall …")
//   - SpecVersion unsupported by this binary        -> fail loud (names the version)
//   - --port != RuntimeSpec.ExternalPort            -> fail loud (§3.2; names port)
//   - canonical(--workspace) != RuntimeSpec.WorkspacePath -> fail loud (§3.2; names workspace)
//
// canonicalWorkspace MUST already be the canonical form of the --workspace
// flag; flagPort is the raw --port value. The caller resolves both before
// calling.
func loadSerenaProxyRuntimeSpec(taskName, canonicalWorkspace string, flagPort int) (*api.DaemonRuntimeSpec, error) {
	// Resolve the control-plane intent path via the supervisor-injected env
	// channel (MCPHUB_SUPERVISOR_INTENT_PATH), falling back to
	// DefaultSupervisorIntentPath only when the channel is unset. The channel is
	// IMMUNE to the manifest/child env — the supervisor sets it AFTER the env
	// merge — so a serena-proxy whose manifest env redirects HOME / XDG_*_HOME
	// for the upstream serena data dir still finds its real descriptor instead
	// of resolving the path against the child's home (bot PR #246 P2).
	intentPath, err := api.ResolveSupervisorIntentPathForProxy()
	if err != nil {
		return nil, fmt.Errorf("resolve supervisor-intent path: %w", err)
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		return nil, fmt.Errorf("read supervisor-intent %s: %w", intentPath, err)
	}
	d := intent.FindSupervisorDaemonByTaskName(taskName)
	if d == nil {
		return nil, fmt.Errorf("serena-proxy: no supervisor-intent descriptor for task %q (reinstall the serena dynamic pool)", taskName)
	}
	if d.RuntimeSpec == nil {
		return nil, fmt.Errorf("serena-proxy: descriptor %q carries no runtime_spec (pre-redesign or stale row); reinstall the serena dynamic pool", taskName)
	}
	spec := d.RuntimeSpec
	if spec.SpecVersion != api.DaemonRuntimeSpecVersion {
		return nil, fmt.Errorf("serena-proxy: unsupported runtime spec_version %d for task %q (this binary supports %d); reinstall the serena dynamic pool", spec.SpecVersion, taskName, api.DaemonRuntimeSpecVersion)
	}

	// Descriptor/flag consistency contract (§3.2): argv, top-level descriptor
	// fields, and the RuntimeSpec must be one self-consistent unit. Any
	// disagreement fails loud — no silent reconcile to one side.
	if d.TaskName != taskName {
		return nil, fmt.Errorf("serena-proxy: descriptor task name %q does not match --task-name %q", d.TaskName, taskName)
	}
	if flagPort != spec.ExternalPort || flagPort != d.Port {
		return nil, fmt.Errorf("serena-proxy: --port %d disagrees with descriptor (runtime_spec.external_port=%d, descriptor.port=%d) for task %q; reinstall the serena dynamic pool", flagPort, spec.ExternalPort, d.Port, taskName)
	}
	if canonicalWorkspace != spec.WorkspacePath || canonicalWorkspace != d.Workspace {
		return nil, fmt.Errorf("serena-proxy: --workspace %q disagrees with descriptor (runtime_spec.workspace_path=%q, descriptor.workspace=%q) for task %q; reinstall the serena dynamic pool", canonicalWorkspace, spec.WorkspacePath, d.Workspace, taskName)
	}
	return spec, nil
}

// serenaProxyChildArgs returns the final upstream child argv:
// spec.ChildArgs ++ [--port, spec.UpstreamPort]. spec.ChildArgs already
// carries the expanded --project <workspace> and the appended --context
// <value> (materialized at build time); the proxy only appends the internal
// port the child binds. PURE: it clones spec.ChildArgs so the spec is not
// mutated.
func serenaProxyChildArgs(spec *api.DaemonRuntimeSpec) []string {
	out := make([]string, 0, len(spec.ChildArgs)+2)
	out = append(out, spec.ChildArgs...)
	out = append(out, "--port", strconv.Itoa(spec.UpstreamPort))
	return out
}
