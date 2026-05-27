package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
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
// Flow (mirrors the native-http branch of `mcphub daemon` but with
// workspace-template arg expansion instead of a named-daemon spec
// lookup):
//
//  1. Validate flags (--port, --workspace, --server).
//  2. Load the server manifest; require kind=workspace-scoped + a
//     non-nil DaemonTemplate.
//  3. Resolve env (secret:KEY references via vault).
//  4. Build childArgs = m.BaseArgs ++ ExpandWorkspacePathTokens(
//     m.DaemonTemplate.ExtraArgsTemplate, <workspace path>). Append
//     `--port <internalPort>` where internalPort =
//     port + config.NativeHTTPInternalPortOffset.
//  5. Spawn the upstream native-http child via daemon.HTTPHost and
//     ListenAndServe an external port -> internal-port reverse proxy.
//  6. Standard shutdown semantics on cmd.Context().Done() or
//     ChildExited.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.2.
// Spec ref: docs/superpowers/specs/2026-05-20-serena-dynamic-pool.md §6.
func newDaemonSerenaProxyCmd() *cobra.Command {
	var (
		portFlag      int
		workspaceFlag string
		serverFlag    string
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

			raw, err := api.NewAPI().ManifestGet(serverFlag)
			if err != nil {
				return fmt.Errorf("load manifest %s: %w", serverFlag, err)
			}
			m, err := config.ParseManifest(bytes.NewReader([]byte(raw)))
			if err != nil {
				return err
			}
			if m.Name != serverFlag {
				return fmt.Errorf("manifest name %q does not match requested server %q", m.Name, serverFlag)
			}
			if m.Kind != config.KindWorkspaceScoped {
				return fmt.Errorf("serena-proxy requires kind=workspace-scoped manifest; got kind=%q", m.Kind)
			}
			if m.DaemonTemplate == nil {
				return fmt.Errorf("serena-proxy requires manifest with daemon_template block; got nil")
			}
			if m.Transport != config.TransportNativeHTTP {
				return fmt.Errorf("serena-proxy currently supports only transport=native-http; got %q", m.Transport)
			}

			// Resolve env (secret:KEY -> vault lookup).
			vault, _ := secrets.OpenVault(defaultKeyPath(), defaultVaultPath())
			resolver := secrets.NewResolver(vault, nil)
			env, err := resolver.ResolveMap(m.Env)
			if err != nil {
				return err
			}

			// Build the uvx (or whatever the manifest says) argv:
			// BaseArgs ++ template-expanded ExtraArgsTemplate. Token
			// expansion is the helper's job; we hand it the canonical
			// workspace path so any `${workspace.path}` reference inside
			// the template lands as the registry's canonical form.
			templateArgs := make([]string, 0, len(m.BaseArgs)+len(m.DaemonTemplate.ExtraArgsTemplate))
			templateArgs = append(templateArgs, m.BaseArgs...)
			templateArgs = append(templateArgs, m.DaemonTemplate.ExtraArgsTemplate...)
			childArgs := config.ExpandWorkspacePathTokens(templateArgs, canonical)

			internalPort := portFlag + config.NativeHTTPInternalPortOffset
			childArgs = append(childArgs, "--port", fmt.Sprintf("%d", internalPort))

			cmdPath := m.Command
			h, err := daemon.NewHTTPHost(daemon.HTTPHostConfig{
				Command:      cmdPath,
				Args:         childArgs,
				Env:          env,
				UpstreamPort: internalPort,
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
	return c
}
