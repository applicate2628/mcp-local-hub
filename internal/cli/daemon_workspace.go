package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
		hardCapFlag      int
		idleTTLFlag      time.Duration
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
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if portFlag <= 0 {
				return fmt.Errorf("--port is required and must be > 0")
			}
			if workspaceFlag == "" || languageFlag == "" {
				return fmt.Errorf("--workspace and --language are required")
			}
			if serverFlag == "" {
				serverFlag = "mcp-language-server"
			}
			// DM-3 mirror: capture launch failure into the lazy-proxy log
			// file. Same reasoning as newDaemonCmdReal — Task Scheduler
			// records last_result=1 but loses mcphub's stderr, so without
			// this wrap registry/manifest/bind errors leave no diagnostic
			// trail.
			//
			// Codex r1 P2 fix: install the defer BEFORE
			// CanonicalWorkspacePath so stale workspace registrations
			// (path moved/deleted) also produce a diagnostic line — that
			// is the most common scheduler-failure path and the one with
			// the worst observability gap. logPath starts as a
			// pre-canonicalization fallback (`lazy-proxy-<lang>-pre.log`)
			// and gets refined to `lsp-<wsKey>-<lang>.log` once wsKey is
			// known. The deferred closure captures `logPath` by reference
			// so the refinement is visible at fire time.
			logPath := filepath.Join(logBaseDir(),
				fmt.Sprintf("lazy-proxy-%s-pre.log", languageFlag))
			daemonLabel := "lazy-proxy-" + languageFlag
			defer func() {
				if err != nil {
					writeLaunchFailure(logPath,
						"mcp-language-server",
						daemonLabel,
						err)
				}
			}()
			// Pre-existing orphan class (deep-sec P3-b, note-only — NOT fixed
			// here). This uses the STRICT canonicalizer, which fails when the
			// workspace dir was DELETED after registration; the proxy then exits
			// non-zero ("canonical workspace path") and the supervisor churns it
			// toward quarantine. The reconcile orphan-exclusion guard does NOT
			// suppress this case: that guard keys on api.LSPRegistryRowBacksDescriptor,
			// which uses the TOLERANT CanonicalWorkspacePathForCleanup (best-effort
			// on a missing dir) and therefore still FINDS the backing row, so the
			// descriptor is NOT classified as an orphan and is still spawned. The
			// strict/tolerant asymmetry (this spawn path vs. the predicate) is the
			// gap. Closing it (e.g. excluding descriptors whose workspace dir is
			// gone, or auto-unregistering on deleted-dir) is a separate tracked
			// follow-up, intentionally out of scope for this PR.
			canonical, err := api.CanonicalWorkspacePath(workspaceFlag)
			if err != nil {
				return fmt.Errorf("canonical workspace path: %w", err)
			}
			wsKey := api.WorkspaceKey(canonical)
			// Legacy key for one-shot migration of registry rows
			// created before EvalSymlinks-based canonicalization
			// landed. Computed up-front so the registry-load fallback
			// below has it without re-reading the filesystem.
			legacyCanonical, err := api.CanonicalWorkspacePathLegacyCompat(workspaceFlag)
			if err != nil {
				return fmt.Errorf("legacy canonical workspace path: %w", err)
			}
			legacyWSKey := api.WorkspaceKey(legacyCanonical)
			// Refine to the canonical lsp-<wsKey>-<lang>.log path now
			// that wsKey is known; subsequent failures (registry, bind,
			// manifest, …) land in the same file the proxy itself
			// writes, so the GUI Logs picker can surface them too.
			// We intentionally use the PRIMARY wsKey here for the
			// initial bootstrap path so a fresh install always lands
			// in the new-style filename. If we fall back to the legacy
			// key below, logPath is re-refined post-fallback so the
			// proxy and the cli agree on the file.
			logPath = filepath.Join(logBaseDir(),
				fmt.Sprintf("lsp-%s-%s.log", wsKey, languageFlag))
			daemonLabel = "lsp-" + wsKey + "-" + languageFlag

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
			defer func() { api.ReleaseAndJoin(&err, unlock) }()
			if err := reg.Load(); err != nil {
				return fmt.Errorf("load registry: %w", err)
			}
			entry, ok := reg.Get(wsKey, languageFlag)
			activeWSKey := wsKey
			if !ok && legacyWSKey != wsKey {
				entry, ok = reg.Get(legacyWSKey, languageFlag)
				if ok {
					activeWSKey = legacyWSKey
					// Re-refine logPath/daemonLabel so the proxy
					// and the cli agree on the same log file. A
					// proxy launched by a scheduler task created
					// under the legacy key writes to the legacy
					// filename; without this re-refine, our own
					// error logging would split into the new-key
					// filename and the operator would miss them.
					logPath = filepath.Join(logBaseDir(),
						fmt.Sprintf("lsp-%s-%s.log", activeWSKey, languageFlag))
					daemonLabel = "lsp-" + activeWSKey + "-" + languageFlag
				}
			}
			if !ok {
				return fmt.Errorf("not registered: workspace %s language %s (key %s)",
					canonical, languageFlag, wsKey)
			}
			// Scheduler task args and the registry are two sources of truth
			// that can drift (stale task XML, manual edits, partial recovery).
			// If --port mismatches entry.Port, coming up anyway would bind a
			// port clients and status-metadata don't expect, producing a
			// silently-broken registration that still looks healthy in
			// `mcphub workspaces`.
			//
			// FIX-3 (P1-2 root): distinguish a STALE REGISTRY ROW from a genuine
			// mis-registration. When supervisor-intent (the authority the supervisor
			// spawns this proxy FROM) agrees with our --port and ONLY the registry row
			// disagrees, the divergence is the ephemeral-collision self-heal's
			// crash/double-fault residue (registry=newPort / intent=oldPort, or the
			// inverse). Returning a plain exit-1 there BRICKS the LSP daemon forever —
			// the self-heal keys on exit-3, so it never re-drives, and the crash loop
			// marches to quarantine → parole → same exit-1 → re-quarantine. Classify it
			// as bind-refused-equivalent (exit-3) so the supervisor reallocates a fresh
			// pool port and rewrites BOTH stores consistently. A genuine mis-registration
			// (our --port itself is stale relative to intent, or intent has no matching
			// descriptor) keeps the original fail-closed exit-1 ("run mcphub register")
			// so it is never swept into a self-heal loop.
			if entry.Port != portFlag {
				var intent *api.SupervisorIntentFile
				if ip, perr := api.ResolveSupervisorIntentPathForProxy(); perr == nil {
					// nil on any read error → classify falls back to exit-1 (fail-closed).
					intent, _ = api.ReadSupervisorIntent(ip)
				}
				return classifyLSPPortMismatch(intent, workspaceFlag, languageFlag, portFlag, entry.Port, activeWSKey)
			}
			// Re-assert Configured lifecycle on startup using the already-
			// loaded Registry (no new flock acquisition — we hold it).
			// Bind does the same thing via PutLifecycle would deadlock
			// the outer flock on Windows' non-reentrant LockFileEx.
			entry.Lifecycle = api.LifecycleConfigured
			entry.LastError = ""
			reg.Put(entry)
			if err := reg.Save(); err != nil {
				return fmt.Errorf("persist configured state: %w", err)
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

			// Reuse the canonical logPath (refined above to
			// lsp-<wsKey>-<lang>.log after canonicalization) so the
			// backend's child stdout/stderr and any pre-Serve
			// mcphub-side diagnostic land in the same file. Two log
			// files for one daemon would split the log dropdown in
			// the GUI and confuse "where did the failure go?"
			// debugging.
			lc := buildWorkspaceBackendLifecycle(spec, canonical, languageFlag, logPath)
			if lc == nil {
				return fmt.Errorf("unsupported backend %q for language %q", spec.Backend, languageFlag)
			}

			proxy := daemon.NewLazyProxy(daemon.LazyProxyConfig{
				WorkspaceKey:          activeWSKey,
				WorkspacePath:         canonical,
				Language:              languageFlag,
				BackendKind:           spec.Backend,
				Port:                  portFlag,
				Lifecycle:             lc,
				RegistryPath:          regPath,
				MaterializedHardCap:   hardCapFlag,
				IdleBackendTTL:        idleTTLFlag,
				IdleBackendCheckEvery: 0,
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
				// Bind refused because a foreign process holds this pool port
				// (WSAEADDRINUSE/WSAEACCES) → exit exitBindRefused so the
				// supervisor's ephemeral-collision self-heal reallocates a fresh
				// pool port instead of crash-looping to quarantine. Any other
				// bind failure keeps the existing cobra exit-1 (genuine crash).
				return bindRefusedExit(fmt.Errorf("bind proxy: %w", err))
			}
			releaseErr := unlock()
			unlock = nil
			if releaseErr != nil {
				return fmt.Errorf("release registry lock after proxy bind: %w", releaseErr)
			}

			errCh := make(chan error, 1)
			go func() { errCh <- proxy.Serve() }()

			select {
			case <-sigCh:
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := proxy.Stop(shutdownCtx); err != nil {
					// codex deep-sec PR #164 P2 closure: the warn line
					// goes through daemon.DaemonDiagWriter (os.Stderr
					// only on TTY) and ALSO appends to the lazy-proxy
					// logPath so scheduler-spawned daemons (which
					// suppress stderr) still record the stop error.
					fmt.Fprintf(daemon.DaemonDiagWriter(), "warn: proxy stop: %v\n", err)
					writeLaunchFailure(logPath, "lazy-proxy", daemonLabel, fmt.Errorf("proxy stop: %w", err))
				}
				return nil
			case err := <-errCh:
				// Serve returned — either a bind error or a Stop happened.
				// http.ErrServerClosed is the clean-shutdown signal.
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				// Unexpected Serve error. Tear down the materialized
				// backend (if any) before returning so scheduler restart
				// doesn't leak orphaned LSP subprocesses holding the
				// workspace-proxy port / stdin pipes. Stop is idempotent
				// and bounded by the 5s shutdown context; the error
				// returned to Task Scheduler still reflects the original
				// Serve failure.
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = proxy.Stop(shutdownCtx)
				cancel()
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
	c.Flags().IntVar(&hardCapFlag, "materialized-hard-cap", daemon.DefaultLSPMaterializedHardCap, "maximum concurrently materialized LSP backends; 0 disables the cap")
	c.Flags().DurationVar(&idleTTLFlag, "idle-backend-ttl", daemon.DefaultLSPIdleBackendTTL, "stop materialized LSP backend after this idle duration; 0 disables idle reaping (the cold-start probation watchdog stays active)")
	// Hidden override for tests and for operators repointing at a non-default
	// registry layout. Users should never touch this.
	c.Flags().StringVar(&registryOverride, "registry", "", "override registry YAML path (test/ops)")
	_ = c.Flags().MarkHidden("materialized-hard-cap")
	_ = c.Flags().MarkHidden("idle-backend-ttl")
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

// classifyLSPPortMismatch (FIX-3) decides how a workspace-LSP proxy reacts when its
// registry-row port disagrees with its own --port. It returns a SELF-HEALING exit-3
// error (daemonBindRefusedExitError) when supervisor-intent — the authority the
// supervisor spawns this proxy from — AGREES with our --port (so only the registry
// row is stale, the ephemeral-collision self-heal's crash/double-fault residue), and
// the original fail-closed exit-1 error otherwise (a genuine mis-registration, or any
// case where intent could not confirm our --port). intent may be nil (read failed) →
// exit-1. This is bounded to the dynamic-pool workspace-LSP proxy by construction:
// only `daemon workspace-proxy` reaches this code, and lspIntentDescriptorPort matches
// solely IsWorkspaceLSPProxyDescriptor rows, so a fixed-daemon mis-registration can
// never be swept into the self-heal loop.
func classifyLSPPortMismatch(intent *api.SupervisorIntentFile, workspacePath, language string, portFlag, registryPort int, activeWSKey string) error {
	if p, ok := lspIntentDescriptorPort(intent, workspacePath, language); ok && p == portFlag {
		return &daemonBindRefusedExitError{err: fmt.Errorf(
			"registry row for (%s, %s) has stale port %d while supervisor-intent (the spawn authority) agrees with --port=%d; treating as bind-refused-equivalent (exit %d) so the supervisor reallocates a fresh pool port and reconciles both stores",
			activeWSKey, language, registryPort, portFlag, exitBindRefused)}
	}
	return fmt.Errorf("port mismatch: --port=%d but registry entry for (%s, %s) has port %d; run `mcphub register` to reconcile",
		portFlag, activeWSKey, language, registryPort)
}

// lspIntentDescriptorPort returns the --port of the supervisor-intent workspace-LSP
// descriptor matching (workspacePath, language), or (0,false) when absent/unparseable.
// The match is exact on the descriptor's --workspace / --language argv tokens: for a
// supervisor-spawned proxy our own flags ARE that descriptor's argv verbatim, so an
// exact string match uniquely finds our own row. A missing intent, a missing matching
// row, or a non-integer --port all yield (0,false) → the caller keeps exit-1.
func lspIntentDescriptorPort(intent *api.SupervisorIntentFile, workspacePath, language string) (int, bool) {
	if intent == nil {
		return 0, false
	}
	for i := range intent.Daemons {
		d := intent.Daemons[i]
		if !api.IsWorkspaceLSPProxyDescriptor(d) {
			continue
		}
		if descriptorArgValue(d.Args, "--workspace") != workspacePath ||
			descriptorArgValue(d.Args, "--language") != language {
			continue
		}
		if raw := descriptorArgValue(d.Args, "--port"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				return n, true
			}
		}
		return 0, false
	}
	return 0, false
}

// descriptorArgValue returns the value token following the FIRST occurrence of flag
// in a `daemon <kind> …` descriptor's args, or "" when absent. Scoped to i>=2 (past
// the "daemon <kind>" prefix) to mirror api.descriptorArgPort's scan discipline, so a
// stray leading token can never be mistaken for a flag.
func descriptorArgValue(args []string, flag string) string {
	for i := 2; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
