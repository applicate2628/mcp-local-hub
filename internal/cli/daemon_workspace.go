package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/servers"

	"github.com/spf13/cobra"
)

// newDaemonWorkspaceProxyCmd returns the `mcphub daemon workspace-proxy`
// subcommand the scheduler task launches per registered (workspace, language).
//
// Not intended for interactive use — the user runs `mcphub register` which
// creates the Task Scheduler entry that invokes this command at login. Kept
// `Hidden: true` to stay out of the default help output.
//
// Flow:
//  1. Validate flags (--port, --workspace, --language).
//  2. Canonicalize the workspace path and compute its 8-char key.
//  3. Load the registry and confirm (key, language) is registered.
//  4. Load the mcp-language-server manifest from the embedded FS and find
//     the matching LanguageSpec.
//  5. Construct a BackendLifecycle matching the language's backend kind.
//  6. Build LazyProxyConfig and hand it to daemon.LazyProxy.
//  7. Install SIGINT/SIGTERM handler that triggers graceful shutdown.
//  8. ListenAndServe; on shutdown, Stop the proxy within 5s.
func newDaemonWorkspaceProxyCmd() *cobra.Command {
	var (
		portFlag         int
		workspaceFlag    string
		languageFlag     string
		serverFlag       string
		registryOverride string
	)
	c := &cobra.Command{
		Use:   "workspace-proxy",
		Short: "Launch the lazy proxy for one (workspace, language) tuple",
		Long: `Internal subcommand invoked by the scheduler task created by
'mcphub register'. Answers initialize/tools/list synthetically from the
embedded catalog and materializes the heavy backend on the first tools/call.

The scheduler task passes --workspace, --language, and --port; the proxy
reads the registry to confirm the tuple is registered and the manifest to
construct the backend lifecycle. Human invocation is not supported.`,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if portFlag <= 0 {
				return fmt.Errorf("--port is required and must be > 0")
			}
			if workspaceFlag == "" || languageFlag == "" {
				return fmt.Errorf("--workspace and --language are required")
			}
			if serverFlag == "" {
				serverFlag = "mcp-language-server"
			}
			canonical, err := api.CanonicalWorkspacePath(workspaceFlag)
			if err != nil {
				return fmt.Errorf("canonical workspace path: %w", err)
			}
			wsKey := api.WorkspaceKey(canonical)

			regPath := registryOverride
			if regPath == "" {
				regPath, err = api.DefaultRegistryPath()
				if err != nil {
					return fmt.Errorf("registry path: %w", err)
				}
			}
			reg := api.NewRegistry(regPath)
			// Acquire the registry flock BEFORE the existence check and hold
			// it through Bind so `mcphub unregister` can't race us out of
			// the registry between check and listener.Bind. Once Bind returns
			// the port is actually listening, so a post-unlock unregister's
			// kill-by-port will find and terminate the proxy.
			unlock, err := reg.Lock()
			if err != nil {
				return fmt.Errorf("registry lock: %w", err)
			}
			releaseUnlock := func() {
				if unlock != nil {
					unlock()
					unlock = nil
				}
			}
			defer releaseUnlock()
			if err := reg.Load(); err != nil {
				return fmt.Errorf("load registry: %w", err)
			}
			if _, ok := reg.Get(wsKey, languageFlag); !ok {
				return fmt.Errorf("not registered: workspace %s language %s (key %s)",
					canonical, languageFlag, wsKey)
			}

			// Load the manifest from the embedded FS and locate the language spec.
			f, err := servers.Manifests.Open(serverFlag + "/manifest.yaml")
			if err != nil {
				return fmt.Errorf("open embedded manifest %s: %w", serverFlag, err)
			}
			defer f.Close()
			m, err := config.ParseManifest(f)
			if err != nil {
				return fmt.Errorf("parse manifest: %w", err)
			}
			var spec config.LanguageSpec
			for _, l := range m.Languages {
				if l.Name == languageFlag {
					spec = l
					break
				}
			}
			if spec.Name == "" {
				return fmt.Errorf("manifest %s lacks language %q", serverFlag, languageFlag)
			}

			logPath := filepath.Join(logBaseDir(),
				fmt.Sprintf("lsp-%s-%s.log", wsKey, languageFlag))
			lc := buildWorkspaceBackendLifecycle(spec, canonical, languageFlag, logPath)
			if lc == nil {
				return fmt.Errorf("unsupported backend %q for language %q", spec.Backend, languageFlag)
			}

			proxy := daemon.NewLazyProxy(daemon.LazyProxyConfig{
				WorkspaceKey:  wsKey,
				WorkspacePath: canonical,
				Language:      languageFlag,
				BackendKind:   spec.Backend,
				Port:          portFlag,
				Lifecycle:     lc,
				RegistryPath:  regPath,
			})

			// SIGINT / SIGTERM triggers graceful shutdown. Bound to a fresh
			// channel rather than cmd.Context() so the goroutine captures
			// the specific signals without re-running anything on ctx cancel.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)

			// Bind under the still-held registry lock so unregister cannot
			// remove our row between the Get check above and the port being
			// actually listening. Once the listener is bound, it is safe to
			// release the lock — a subsequent unregister's kill-by-port will
			// find the listening socket and terminate us.
			if err := proxy.Bind(); err != nil {
				return fmt.Errorf("bind proxy: %w", err)
			}
			releaseUnlock()

			errCh := make(chan error, 1)
			go func() { errCh <- proxy.Serve() }()

			select {
			case <-sigCh:
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := proxy.Stop(shutdownCtx); err != nil {
					fmt.Fprintf(os.Stderr, "warn: proxy stop: %v\n", err)
				}
				return nil
			case err := <-errCh:
				// ListenAndServe returned — either a bind error or a Stop
				// happened. http.ErrServerClosed is the clean-shutdown signal.
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return fmt.Errorf("lazy proxy: %w", err)
			case <-cmd.Context().Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = proxy.Stop(shutdownCtx)
				return nil
			}
		},
	}
	c.Flags().IntVar(&portFlag, "port", 0, "TCP port to bind (required; allocated by `mcphub register`)")
	c.Flags().StringVar(&workspaceFlag, "workspace", "", "absolute workspace path (required)")
	c.Flags().StringVar(&languageFlag, "language", "", "language name matching a manifest entry (required)")
	c.Flags().StringVar(&serverFlag, "server", "mcp-language-server", "embedded manifest to read LanguageSpec from")
	// Hidden override for tests and for operators repointing at a non-default
	// registry layout. Users should never touch this.
	c.Flags().StringVar(&registryOverride, "registry", "", "override registry YAML path (test/ops)")
	_ = c.Flags().MarkHidden("registry")
	return c
}

// buildWorkspaceBackendLifecycle returns the matching BackendLifecycle for
// a LanguageSpec. Returns nil for unknown backend kinds — the caller emits
// a clear error.
func buildWorkspaceBackendLifecycle(spec config.LanguageSpec, canonicalWorkspace, language, logPath string) daemon.BackendLifecycle {
	switch spec.Backend {
	case "gopls-mcp":
		extra := append([]string(nil), spec.ExtraFlags...)
		if len(extra) == 0 {
			extra = []string{"mcp"}
		}
		return daemon.NewGoplsMCPStdio(daemon.GoplsMCPStdioConfig{
			WrapperCommand: spec.LspCommand,
			ExtraArgs:      extra,
			Workspace:      canonicalWorkspace,
			LogPath:        logPath,
		})
	case "mcp-language-server":
		args := []string{"-workspace", canonicalWorkspace, "-lsp", spec.LspCommand}
		if len(spec.ExtraFlags) > 0 {
			args = append(args, "--")
			args = append(args, spec.ExtraFlags...)
		}
		return daemon.NewMcpLanguageServerStdio(daemon.McpLanguageServerStdioConfig{
			WrapperCommand: "mcp-language-server",
			WrapperArgs:    args,
			Workspace:      canonicalWorkspace,
			Language:       language,
			LogPath:        logPath,
			LSPCommand:     spec.LspCommand, // enables pre-flight for LSP-missing → LifecycleMissing
		})
	default:
		return nil
	}
}
