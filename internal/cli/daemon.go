package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"

	"github.com/spf13/cobra"
)

const (
	daemonOverlayAppliedEnvVar   = "MCPHUB_DAEMON_ENV_OVERLAY_APPLIED"
	daemonOverlayAppliedEnvValue = "supervisor"
	// daemonOverlayKeysEnvVar carries the comma-joined set of overlay keys
	// the supervisor applied (overlay-wins) into this wrapper's
	// environment before spawning it. It exists so the marker-present
	// reload-FAILURE path can reconstruct the overlay key set (and read
	// each key's already-expanded value back from os.Environ via
	// overlayValuesFromEnv) WITHOUT a successful overlay-file reload —
	// closing the both-keyed clobber where, on an unreadable overlay
	// file, a key present in BOTH the manifest and the cached operator
	// overlay would otherwise revert to the manifest default in cfg.Env
	// (Codex bot #268 daemon.go:380 P2 residual). The supervisor is the
	// only writer; like the APPLIED marker it is stripped from the
	// parent/merged env and re-appended with the trusted value so no
	// manifest/overlay row can spoof the key set.
	daemonOverlayKeysEnvVar = "MCPHUB_DAEMON_ENV_OVERLAY_KEYS"
	// daemonOverlayPathEnvVar is the dedicated mcphub-internal env var the
	// supervisor sets when spawning a descriptor whose launcher resolves the
	// operator env overlay file (daemon-env-overrides.yaml) and whose own
	// process env may redirect HOME / XDG_*_HOME. The supervisor merges the
	// serena manifest env (d.Env = clone of m.Env) into the serena-proxy
	// WRAPPER's cmd.Env, so a manifest that redirects HOME for the upstream
	// serena data dir also redirects the WRAPPER's HOME — and daemonOverlayEnv
	// resolves the overlay file via stateDirFunc() (HOME/XDG-based) on POSIX.
	// Without this channel the proxy would look for daemon-env-overrides.yaml
	// under the child's redirected home, miss it, and silently lose the
	// operator overlay (the clobber the serena-proxy-env-overlay fix closes).
	// The supervisor resolves the canonical overlay path ONCE (against its own
	// resolved state dir) and injects it here, AFTER the manifest/overlay env
	// merge so the child env cannot clobber it. daemonOverlayEnv reads it first
	// (resolveDaemonOverlayPath); an unset var falls back to the HOME/XDG state
	// dir, the correct behavior for a direct `mcphub daemon` invocation. This
	// is the overlay-file twin of MCPHUB_SUPERVISOR_INTENT_PATH (bot PR #403
	// r2; the same HOME-redirect immunity the intent-path channel already has).
	daemonOverlayPathEnvVar = "MCPHUB_DAEMON_ENV_OVERLAY_PATH"
	// daemonOverlayKeysSep is the join delimiter for daemonOverlayKeysEnvVar.
	// It must be valid INSIDE an environment-variable VALUE: a NUL byte is
	// NOT — the OS env block is NUL-terminated, so a NUL embedded in
	// cmd.Env's value truncates the variable (or fails the spawn) before the
	// daemon starts (Codex bot #268 P1). A comma is safe: overlay key NAMES
	// match [A-Za-z_][A-Za-z0-9_]* (no comma can appear), so the comma-joined
	// set splits back unambiguously.
	daemonOverlayKeysSep = ","
)

// reservedDaemonOverlayControlVars is the CANONICAL single-source list of every
// mcphub-reserved overlay control variable the supervisor injects at spawn time
// and the spawned wrapper TRUSTS in os.Environ() (APPLIED marker, KEYS set,
// PATH channel). It is the one owner of "which env vars are supervisor-only
// control plane"; stripAllDaemonOverlayControlVars drives off it so the
// strip-then-reappend discipline can never diverge per-var, and a future 4th
// control var only needs to be added HERE (plus its trusted re-append site),
// not at a new strip site. The constants above stay the source of truth for the
// names; this slice just enumerates them in one place. See
// stripAllDaemonOverlayControlVars for why the inherited values are untrusted.
var reservedDaemonOverlayControlVars = []string{
	daemonOverlayAppliedEnvVar,
	daemonOverlayKeysEnvVar,
	daemonOverlayPathEnvVar,
}

func newDaemonCmdReal() *cobra.Command {
	var server, daemonName string
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run or recover a daemon (the run path is invoked by the supervisor, not by humans)",
		// Hidden from `mcphub --help`: `daemon run` is spawned by the
		// supervisor, never typed. The one operator-facing subcommand,
		// `daemon recover <task>`, is quoted VERBATIM in the runtime
		// messages that call for it (supervise_squatter.go, the
		// bind-access-denied remedy), so hiding the parent costs no
		// discoverability. Still fully usable, incl. `daemon --help`.
		Hidden: true,
		Long: `Run a single mcp-local-hub daemon. This is the actual server process
that Task Scheduler launches per the scheduler task XML's <Exec>/<Command>
and <Arguments> fields. Not intended for interactive use.

Flow:
  1. Reads the server's manifest from the binary's //go:embed servers/
  2. Sets up env per manifest (including secret:KEY dereferencing)
  3. For native-http servers: launches the upstream binary directly
  4. For stdio-bridge servers: spawns child + multiplexes HTTP clients
     onto it via the in-process Go stdio-host
  5. Tees child stdout+stderr to %LOCALAPPDATA%\mcp-local-hub\logs\<s>-<d>.log
  6. Exits non-zero on unexpected child-process death — Task Scheduler's
     RestartOnFailure (3 retries × 1 min) auto-recovers

The scheduler task's XML is created by 'mcphub install'; you should never
need to call this manually.

See also: install, logs, restart, status.`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if server == "" || daemonName == "" {
				return fmt.Errorf("--server and --daemon are required")
			}
			// DM-3: capture launch failure into the daemon log file. Without
			// this wrap, pre-child errors (manifest open/parse, env resolve,
			// host construction, port bind, etc.) only reach mcphub's own
			// stderr — which Task Scheduler does NOT preserve. last_result=1
			// with no log entry leaves the user with no way to diagnose
			// (this happened with serena: never figured out the cause). The
			// wrap appends a timestamped diagnostic line so the cause
			// survives in the standard daemon log path. logPath must be
			// computed BEFORE the manifest load so manifest-open failures
			// also reach the log.
			logPath := filepath.Join(logBaseDir(), server+"-"+daemonName+".log")
			defer func() {
				if err != nil {
					writeLaunchFailure(logPath, server, daemonName, err)
				}
			}()
			// Validate the requested name before any path resolution.
			// Without this, a disk-fallback path that joins `server`
			// into a filesystem location would re-open the manifest-
			// name confused-deputy hardening from PR #51 S4.
			if err := api.CheckManifestName(server); err != nil {
				return err
			}
			// Load manifest bytes embed-first with disk fallback so a
			// GUI-created manifest can be installed and launched
			// immediately without rebuild. The fallback goes through
			// api.NewAPI().ManifestGet, which itself runs
			// CheckManifestName and the embed-first/disk-fallback
			// composition in one place. After parse, cross-check that
			// the manifest's own `name` field equals the requested
			// server: a disk manifest at servers/<server>/manifest.yaml
			// could legally name itself something else, and trusting
			// that name in scheduler/log paths would split-brain disk
			// vs embed lookups.
			raw, err := api.NewAPI().ManifestGet(server)
			if err != nil {
				return fmt.Errorf("load manifest %s: %w", server, err)
			}
			m, err := config.ParseManifest(bytes.NewReader([]byte(raw)))
			if err != nil {
				return err
			}
			if m.Name != server {
				return fmt.Errorf("manifest name %q does not match requested server %q (refuse to launch with mismatched identity)", m.Name, server)
			}
			var spec *config.DaemonSpec
			for i := range m.Daemons {
				if m.Daemons[i].Name == daemonName {
					spec = &m.Daemons[i]
					break
				}
			}
			if spec == nil {
				return fmt.Errorf("no daemon %q in %s manifest", daemonName, server)
			}
			// Resolve env. A vault that EXISTS but is unreadable is fatal ONLY
			// when this manifest actually uses secret refs — a secretless
			// server must not be bricked by a corrupt vault it never touches
			// (Codex #377 r5). A truly-absent vault → nil, secret refs optional.
			keyPath := defaultKeyPath()
			vaultPath := defaultVaultPath()
			vault, verr := secrets.OpenVaultOptional(keyPath, vaultPath)
			if verr != nil && secrets.HasSecretRef(m.Env) {
				return daemonSecretVaultFatalError(server, daemonName, keyPath, vaultPath, verr)
			}
			resolver := secrets.NewResolver(vault, nil) // TODO config.local.yaml in later task
			env, unsetEnv, err := daemonEnvWithOverlay(server, daemonName, m.Env, resolver)
			if err != nil {
				return err
			}
			// Build launch spec.
			childArgs := append([]string{}, m.BaseArgs...)
			childArgs = append(childArgs, spec.ExtraArgs...)
			// Resolve `command: mcphub` to the running binary's absolute path.
			// Manifests that wrap a hub-internal subcommand (e.g. lldb-bridge)
			// need the exact same exe that carries that subcommand; Go's exec
			// would otherwise use PATH and can find an older install of
			// mcphub whose subcommand set is out of date.
			cmdPath := m.Command
			if m.Command == "mcphub" {
				if self, err := os.Executable(); err == nil {
					cmdPath = self
				}
			}
			if m.Transport == config.TransportNativeHTTP {
				// Native-http path: upstream subprocess listens on an INTERNAL
				// port (spec.Port + api.NativeHTTPInternalPortOffset). mcphub's
				// HTTPHost listens on the external spec.Port and reverse-proxies
				// to upstream while applying the ProtocolBridge transforms
				// (inject synthetic __read_resource__/__list_prompts__/
				// __get_prompt__, rewrite their calls to the matching native
				// MCP methods). Internal ports are chosen by a fixed offset so
				// the mapping is predictable and stable across restarts.
				internalPort := spec.Port + config.NativeHTTPInternalPortOffset
				childArgs = append(childArgs, "--port", fmt.Sprintf("%d", internalPort))
				h, err := daemon.NewHTTPHost(daemon.HTTPHostConfig{
					Command:      cmdPath,
					Args:         childArgs,
					Env:          env,
					UnsetEnv:     unsetEnv,
					UpstreamPort: internalPort,
					LogPath:      logPath,
					// spec.Cwd is the manifest-declared per-daemon working
					// directory (validated absolute at parse time; empty means
					// inherit mcphub's own cwd). cmd.Dir is set from this in
					// the host's Start().
					WorkingDir: spec.Cwd,
				})
				if err != nil {
					return fmt.Errorf("NewHTTPHost: %w", err)
				}
				ctx := cmd.Context()
				if err := h.Start(ctx); err != nil {
					// h.Start can fail two ways: (a) cmd.Start() itself fails
					// (process never spawned, ProcessState nil → no suffix) or
					// (b) waitForReady saw childExited mid-probe (process did
					// spawn, ProcessState set by the watcher goroutine before
					// close(childExited), so formatChildExit gets pid+exit code).
					// Codex CLI review on PR #34 P2 — startup-before-ready was
					// previously a silent diagnostic hole.
					return fmt.Errorf("httphost.Start: %w%s", err, formatChildExit(h.ExitState()))
				}
				srv := &http.Server{
					Addr:              fmt.Sprintf("127.0.0.1:%d", spec.Port),
					Handler:           h.HTTPHandler(),
					ReadHeaderTimeout: 10 * time.Second,
					// WriteTimeout 0: SSE streams are long-lived; handler owns cancellation.
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
					// Upstream server died. Same recovery policy as stdio-bridge:
					// return non-zero → Task Scheduler's RestartOnFailure respawns.
					//
					// Capture the child's ProcessState BEFORE Stop() — exit code,
					// PID, and (on POSIX) signal info are the only diagnostic
					// we get when the subprocess crashed silently with no
					// stderr. Without this the launch-failure log line just
					// said "exited unexpectedly" with no way to tell controlled
					// sys.exit from native crash from parent kill.
					exitMsg := formatChildExit(h.ExitState())
					_ = h.Stop()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
					return fmt.Errorf("native-http upstream exited unexpectedly%s", exitMsg)
				}
			} else if m.Transport == config.TransportStdioBridge {
				// Native Go stdio-host: spawns the inner stdio MCP server as
				// a subprocess and exposes it on HTTP via the in-process host.
				// Replaces the previous npx supergateway wrapper (bridge.go),
				// removing the node/npm dependency from the runtime.
				h, err := daemon.NewStdioHost(daemon.HostConfig{
					Command:  cmdPath,
					Args:     childArgs,
					Env:      env,
					UnsetEnv: unsetEnv,
					LogPath:  logPath,
					// spec.Cwd is the manifest-declared per-daemon working
					// directory (validated absolute at parse time; empty means
					// inherit mcphub's own cwd). cmd.Dir is set from this in
					// the host's Start().
					WorkingDir: spec.Cwd,
				})
				if err != nil {
					return fmt.Errorf("NewStdioHost: %w", err)
				}
				ctx := cmd.Context()
				if err := h.Start(ctx); err != nil {
					return fmt.Errorf("host.Start: %w", err)
				}
				srv := &http.Server{
					Addr:              fmt.Sprintf("127.0.0.1:%d", spec.Port),
					Handler:           h.HTTPHandler(),
					ReadHeaderTimeout: 10 * time.Second,
					ReadTimeout:       30 * time.Second,
					// WriteTimeout intentionally 0 (unlimited): SSE streams are long-lived;
					// writes are bounded per-line via the handler's own select/context,
					// not by the server socket.
				}
				errCh := make(chan error, 1)
				go func() { errCh <- srv.ListenAndServe() }()
				select {
				case err := <-errCh:
					// Codex CLI xhigh re-review on 479cbc3 (P2 #4) and
					// kosyak 2026-05-06-claude-stop-error-durability-sham-fix.md:
					// Use h.LogSupervisorEvent so Stop errors land in the
					// rotated LogPath (durable across scheduled paths) AND
					// stderr (interactive). Cobra exit message goes through
					// os.Stderr alone, which scheduled paths can drop.
					stopErr := h.Stop()
					if stopErr != nil {
						h.LogSupervisorEvent(fmt.Sprintf("stop after http server error: %v (server err: %v)", stopErr, err))
					}
					if errors.Is(err, http.ErrServerClosed) {
						if stopErr != nil {
							return fmt.Errorf("http server closed; %w", stopErr)
						}
						return nil
					}
					if stopErr != nil {
						return fmt.Errorf("http server: %w; stop: %v", err, stopErr)
					}
					return fmt.Errorf("http server: %w", err)
				case <-ctx.Done():
					// Stop() first so handleSSE and handlePOST goroutines observe h.done
					// and return; then Shutdown can complete without waiting on long-lived SSE.
					stopErr := h.Stop()
					if stopErr != nil {
						h.LogSupervisorEvent(fmt.Sprintf("stop after ctx.Done graceful shutdown: %v", stopErr))
					}
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
					if stopErr != nil {
						return fmt.Errorf("graceful shutdown: %w", stopErr)
					}
					return nil
				case <-h.ChildExited():
					// Stdio child died unexpectedly (npx/uvx servers like memory,
					// sequential-thinking, time are known to exit silently after
					// serving N requests). Surface this to the scheduler by
					// returning a non-nil error from RunE so mcphub exits
					// non-zero; Windows Task Scheduler's RestartOnFailure policy
					// (3 retries, 1 minute apart — configured in install.go and
					// scheduler_windows.go) will re-launch the task, which
					// respawns the child. Scheduler owns the retry budget; we
					// do not add in-process respawn logic here.
					stopErr := h.Stop()
					exitMsg := fmt.Sprintf("child exited unexpectedly: %s", formatChildExit(h.ExitState()))
					if stopErr != nil {
						exitMsg += fmt.Sprintf("; stop: %v", stopErr)
					}
					h.LogSupervisorEvent(exitMsg)
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
					if stopErr != nil {
						return fmt.Errorf("stdio child exited unexpectedly; stop: %w", stopErr)
					}
					return fmt.Errorf("stdio child exited unexpectedly")
				}
			} else if m.Transport == config.TransportProcess {
				// Companion (kind=companion): a hub-managed NON-MCP process (e.g.
				// the excalidraw canvas Express server). No MCP host, no HTTP
				// listener, no port bind — run it raw from its package cwd
				// (spec.Cwd) and surface its exit to the supervisor for restart.
				// internal/daemon owns the env/console/log conventions
				// (RunProcess) so this stays a thin call.
				return daemon.RunProcess(cmd.Context(), daemon.ProcessConfig{
					Command:    cmdPath,
					Args:       childArgs,
					Env:        env,
					UnsetEnv:   unsetEnv,
					WorkingDir: spec.Cwd,
					LogPath:    logPath,
				})
			} else {
				return fmt.Errorf("unsupported transport %q", m.Transport)
			}
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	c.Flags().StringVar(&daemonName, "daemon", "", "daemon name within the server manifest")
	// Phase 3 workspace-scoped: lazy proxy subcommand the scheduler task
	// launches per (workspace, language). Hidden — users don't run it.
	c.AddCommand(newDaemonWorkspaceProxyCmd())
	// Phase D.2 / dynamic-pool serena: per-workspace native-http daemon
	// subcommand the supervisor launches per registered serena workspace.
	// Hidden — supervisor-intent.json descriptors point here.
	c.AddCommand(newDaemonSerenaProxyCmd())
	// P2b: operator recovery verb — reap a verified-own port squatter masking a
	// stuck/quarantined daemon, then force a respawn through the supervisor.
	c.AddCommand(newDaemonRecoverCmd())
	return c
}

func daemonSecretVaultFatalError(server, daemonName, keyPath, vaultPath string, err error) error {
	if errors.Is(err, api.ErrDaclOutsideAllowlist) {
		details := api.StateFileDACLRemediationDetailsFor(daemonVaultDACLRefusedPath(keyPath, vaultPath, err), err)
		sidText := ""
		if details.OffendingSID != "" {
			sidText = fmt.Sprintf(" offending SID %s.", details.OffendingSID)
		}
		return fmt.Errorf("daemon %s/%s: vault state-file DACL refused for %s.%s Remediate: %s Cause: %w",
			server, daemonName, details.Path, sidText, api.StateFileDACLRunbookPointer, err)
	}
	return fmt.Errorf("daemon %s/%s: %w", server, daemonName, err)
}

func daemonVaultDACLRefusedPath(keyPath, vaultPath string, err error) string {
	msg := err.Error()
	if strings.Contains(msg, vaultPath) {
		return vaultPath
	}
	if strings.Contains(msg, keyPath) {
		return keyPath
	}
	return keyPath
}

// daemonEnvWithOverlay returns the resolved child env map AND the list of
// declared keys to UNSET in the child (skipped optional secrets) — the host
// removes these from the inherited os.Environ() so the child sees them as
// truly absent, not present-but-empty (Codex #377).
func daemonEnvWithOverlay(server, daemonName string, manifestEnv map[string]string, resolver *secrets.Resolver) (map[string]string, []string, error) {
	if resolver == nil {
		return nil, nil, fmt.Errorf("resolve manifest env for %s/%s: resolver is nil", server, daemonName)
	}
	// Secrets are OPTIONAL by default (install-and-it-works): an unset
	// `secret:` ref must NOT block the spawn. Resolve best-effort — the
	// resolvable env vars are set, the unresolvable ones (a skipped/optional
	// secret) are OMITTED so the daemon still spawns and the SERVER reports
	// its own "missing required key" instead of mcphub failing cryptically.
	// ONLY secret: refs are optional; a missing $VAR/file: stays fatal.
	env, omitted, err := resolver.ResolveMapBestEffort(manifestEnv)
	if err != nil {
		return nil, nil, err
	}
	// A global daemon's canonical supervisor-intent task name is
	// `\mcp-local-hub-<server>-<daemon>`. Derive it here so the shared
	// overlay-merge owner stays the single source of the load/merge/unset
	// logic; the serena proxy reaches the same owner with ITS workspace-keyed
	// task name (daemon_serena.go), never reconstructing one from server/daemon.
	taskName := fmt.Sprintf(`\mcp-local-hub-%s-%s`, server, daemonName)
	return mergeResolvedDaemonEnvWithOverlay(taskName, env, omitted)
}

// mergeResolvedDaemonEnvWithOverlay is the task-name-keyed overlay-merge owner
// shared by the global daemon path (daemonEnvWithOverlay) and the serena
// per-workspace proxy (daemon_serena.go). Given an ALREADY-RESOLVED env map and
// the omitted-optional-secret map from ResolveMapBestEffort, it:
//
//  1. Loads + expands the operator overlay row for taskName.
//  2. Merges it over the resolved env (overlay WINS — mergeDaemonEnvMaps).
//  3. Computes the UNSET list (omitted optional secrets the overlay did NOT
//     supply) and warns once per genuinely-missing key.
//
// Returns the merged child env map and the list of keys to UNSET in the child
// (so the host strips them from the inherited os.Environ() — truly absent, not
// present-but-empty, no ambient-parent inheritance — Codex #377).
//
// Centralizing this here fixes serena-proxy-ignores-env-overlay: before, the
// serena proxy populated the child env from ResolveMapBestEffort ONLY and
// never merged the operator overlay the supervisor had already applied to the
// wrapper, so an operator override (e.g. SERENA_LOG_LEVEL) was silently
// dropped and an EnvRefs overlap key clobbered the overlay value.
func mergeResolvedDaemonEnvWithOverlay(taskName string, resolvedEnv, omittedSecrets map[string]string) (map[string]string, []string, error) {
	overlayEnv, err := daemonOverlayEnv(taskName)
	if err != nil {
		return nil, nil, err
	}
	merged := mergeDaemonEnvMaps(resolvedEnv, overlayEnv)
	// Warn + UNSET only for optional secrets the per-daemon overlay did NOT
	// supply. An omitted `secret:` ref whose key the overlay provides is NOT
	// actually missing: warning "spawning without it" would be false, and
	// adding it to UnsetEnv would be misleading (the merged env carries it).
	// Compute the warning AFTER the overlay merge (Codex #377 merge-gate P3).
	unset := make([]string, 0, len(omittedSecrets))
	for k, ref := range omittedSecrets {
		if _, ok := merged[k]; ok {
			continue
		}
		unset = append(unset, k)
		fmt.Fprintf(os.Stderr, "mcphub daemon %s: env %q (%s) is not set — spawning without it; set it via `mcphub secrets` (or the install secret prompt) if this server needs it.\n",
			taskName, k, ref)
	}
	return merged, unset, nil
}

func mergeDaemonEnvMaps(manifest, overlay map[string]string) map[string]string {
	if len(manifest) == 0 && len(overlay) == 0 {
		return map[string]string{}
	}
	merged := mergeDaemonEnv(nil, manifest, overlay)
	out := make(map[string]string, len(merged))
	for _, kv := range merged {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// resolveDaemonOverlayPath returns the operator env overlay file
// (daemon-env-overrides.yaml) path daemonOverlayEnv reads. It prefers the
// dedicated MCPHUB_DAEMON_ENV_OVERLAY_PATH control channel the supervisor
// injects (immune to the manifest/child env), falling back to the HOME/XDG
// state dir via stateDirFunc() when the var is unset.
//
// The channel exists because the serena-proxy WRAPPER process inherits the
// serena manifest env (the supervisor merges d.Env = clone of m.Env into the
// wrapper's cmd.Env). A manifest that redirects HOME / XDG_*_HOME for the
// upstream serena data dir therefore also redirects the WRAPPER's HOME, and on
// POSIX stateDirFunc() honors HOME/XDG — so without the channel the proxy would
// resolve daemon-env-overrides.yaml against the child's redirected home, miss
// it, and silently drop the operator overlay. This is the overlay-file twin of
// api.ResolveSupervisorIntentPathForProxy (bot PR #403 r2). The canonical
// shipped serena manifest does NOT redirect HOME (env: PYTHONUNBUFFERED only),
// so no shipped workspace hits the gap today — but the channel makes the load
// HOME-redirect-immune for any future manifest that does.
//
// An unset channel + direct `mcphub daemon` invocation resolves the overlay
// against the operator's own state dir, exactly as before.
func resolveDaemonOverlayPath() (string, error) {
	if p := os.Getenv(daemonOverlayPathEnvVar); p != "" {
		return p, nil
	}
	stateDir, err := stateDirFunc()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, overlayBaseName), nil
}

// daemonOverlayEnv loads + expands the operator env overlay row for the
// daemon identified by its CANONICAL supervisor-intent taskName (e.g.
// `\mcp-local-hub-<server>-<daemon>` for a global daemon, or
// `\mcp-local-hub-serena-<wskey>` for a serena per-workspace proxy). The
// caller supplies the authoritative task name; this owner never reconstructs
// it from server/daemon so a serena proxy (whose task name is workspace-keyed,
// not server-daemon-keyed) loads its own overlay row instead of a phantom one.
func daemonOverlayEnv(taskName string) (map[string]string, error) {
	overlayPath, err := resolveDaemonOverlayPath()
	if err != nil {
		return nil, fmt.Errorf("resolve state dir for env overlay: %w", err)
	}
	supervisorApplied := daemonOverlayAlreadyApplied(os.Environ())
	ov, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		// Supervisor-spawned wrapper (marker present): the supervisor
		// already loaded + expanded this same overlay row and merged the
		// result (overlay-wins) into THIS wrapper's environment before
		// spawning us; its own spawn path intentionally falls back to the
		// cached startup overlay on a corrupt / temporarily-unreadable
		// file (supervise.go "Load errors fall back to the cached startup
		// overlay"). A FATAL error here would kill every restarted
		// supervised daemon after an operator leaves
		// daemon-env-overrides.yaml malformed (or a transient
		// read-hardening failure hits) — even though the correct env is
		// ALREADY present in os.Environ(). Degrade gracefully: warn and
		// proceed with the already-applied env.
		//
		// The reload failed, so we cannot read the overlay file's key
		// set — but the supervisor injected that key set as
		// MCPHUB_DAEMON_ENV_OVERLAY_KEYS before spawning us. Reconstruct
		// the overlay map by reading each injected key's ALREADY-EXPANDED
		// value back from os.Environ() (overlayValuesFromEnv, the SAME
		// reader the success/marker path uses). That makes the operator
		// override WIN in cfg.Env even when the file is unreadable: for a
		// key present in BOTH the manifest and the cached overlay, the
		// reconstructed overlay value (last duplicate) beats the manifest
		// default StdioHost/HTTPHost would otherwise re-append after
		// os.Environ() (closes Codex bot #268 daemon.go:380 P2 — the r10
		// overlap-key clobber on an unreadable file). When the supervisor
		// applied NO overlay for this daemon, the injected key set is
		// empty and we proceed with manifest-only env (nil overlay map),
		// exactly as before.
		if supervisorApplied {
			injectedKeys := daemonOverlayKeysFromEnv(os.Environ())
			reconstructed := overlayMapFromInjectedKeys(injectedKeys, os.Environ())
			_ = api.LogHubMcpEvent("warn", "daemon-env-overlay-reload-degraded-supervised", map[string]any{
				"task_name":          taskName,
				"err":                err.Error(),
				"fallback":           "reconstructed overlay key set from os.Environ (overlay file unreadable)",
				"reconstructed_keys": len(reconstructed),
			})
			if len(reconstructed) == 0 {
				return nil, nil
			}
			return reconstructed, nil
		}
		// Direct `mcphub daemon` invocation (no marker): the operator
		// chose to run this daemon by hand and must see a malformed /
		// unreadable overlay surface as a fatal error.
		return nil, fmt.Errorf("load env overlay: %w", err)
	}
	overlayEnv := daemon_env_overlay.LookupOverlay(ov, taskName)
	if len(overlayEnv) == 0 {
		return nil, nil
	}
	// Supervisor-applied path (marker present in os.Environ): the
	// supervisor already ran ExpandParentPath over this same overlay row
	// and merged the result (overlay-wins) into THIS wrapper's environment
	// before spawning us. Re-expanding ${parent_path} here would double the
	// parent PATH (the wrapper's PATH already IS the expanded overlay PATH),
	// so r9 short-circuited by returning nil. But returning nil dropped the
	// overlay keys from cfg.Env entirely, and StdioHost spawns the upstream
	// child as append(os.Environ(), cfg.Env...) — so the manifest value in
	// cfg.Env (last duplicate wins) CLOBBERED the supervisor's overlay value
	// that was only present as an inherited parent entry (e.g. the memory
	// server's MEMORY_FILE_PATH override silently reverting to the manifest
	// default after restart — Codex bot #268 r10 P2).
	//
	// Fix: re-read each overlay key's ALREADY-EXPANDED value back from
	// os.Environ() (case-insensitive on Windows, exact on POSIX, mirroring
	// mergeDaemonEnv's PATH-collision normalizer). The overlay then WINS in
	// cfg.Env (no manifest clobber) WITHOUT re-expanding ${parent_path} (no
	// doubling). Keys somehow absent from os.Environ (cannot happen in
	// production — the supervisor always sets every overlay key — but
	// defends a hand-crafted env) fall back to expanding the literal value
	// so the overlay still wins and no raw ${parent_path} token leaks to the
	// child.
	if supervisorApplied {
		return overlayValuesFromEnv(overlayEnv, os.Environ()), nil
	}
	// Direct `mcphub daemon` invocation (no marker): expand ${parent_path}
	// against our own environment exactly once.
	expanded, err := daemon_env_overlay.ExpandParentPath(overlayEnv, os.Environ())
	if err != nil {
		_ = api.LogHubMcpEvent("warn", "daemon-env-overlay-parent-path-resolve-failed", map[string]any{
			"task_name": taskName,
			"err":       err.Error(),
		})
		return overlayEnv, nil
	}
	return expanded, nil
}

// overlayValuesFromEnv returns a map keyed by each overlay key (preserving
// the overlay's original key casing) whose value is read back from env —
// the supervisor-expanded value already present in the wrapper's
// environment. The env lookup is case-insensitive on Windows so an overlay
// `Path` key still finds a `PATH=` entry the supervisor wrote, matching
// mergeDaemonEnv's Windows PATH-family normalizer. A key with no matching
// env entry (degenerate; the supervisor always materializes every overlay
// key) falls back to expanding the literal overlay value against env so the
// overlay value still wins and no literal ${parent_path} token reaches the
// child.
func overlayValuesFromEnv(overlay map[string]string, env []string) map[string]string {
	out := make(map[string]string, len(overlay))
	var missing map[string]string
	for k := range overlay {
		if v, ok := lookupEnvValueCaseFold(env, k); ok {
			out[k] = v
			continue
		}
		if missing == nil {
			missing = map[string]string{}
		}
		missing[k] = overlay[k]
	}
	if len(missing) > 0 {
		if expanded, err := daemon_env_overlay.ExpandParentPath(missing, env); err == nil {
			for k, v := range expanded {
				out[k] = v
			}
		} else {
			for k, v := range missing {
				out[k] = v
			}
		}
	}
	return out
}

// overlayMapFromInjectedKeys reconstructs the overlay env map for the
// marker-present reload-FAILURE path. It takes the supervisor-injected
// overlay key set (each key spelled as the overlay stored it) and reads
// every key's ALREADY-EXPANDED value back from env via overlayValuesFromEnv
// — the SAME reader the success/marker path uses — so the rebuilt map's
// values match what the supervisor merged into os.Environ() at spawn time
// (case-insensitive PATH-family match on Windows). An empty key set yields
// an empty map so the caller can treat it as "no overlay for this daemon".
func overlayMapFromInjectedKeys(keys []string, env []string) map[string]string {
	if len(keys) == 0 {
		return map[string]string{}
	}
	keySet := make(map[string]string, len(keys))
	for _, k := range keys {
		keySet[k] = ""
	}
	return overlayValuesFromEnv(keySet, env)
}

// daemonOverlayKeysFromEnv returns the overlay key set the supervisor
// injected via daemonOverlayKeysEnvVar (the comma-joined value written at
// spawn time). The LAST matching entry wins, mirroring Go exec
// duplicate-key semantics so a supervisor-appended trusted value beats any
// earlier spoofed entry left in the inherited env. Empty segments (from a
// leading/trailing/duplicate separator) are dropped so a malformed value
// never yields an empty-named key. Returns nil when the var is absent or
// holds no non-empty key.
func daemonOverlayKeysFromEnv(env []string) []string {
	raw, ok := lookupEnvValueCaseFold(env, daemonOverlayKeysEnvVar)
	if !ok || raw == "" {
		return nil
	}
	var keys []string
	for _, k := range strings.Split(raw, daemonOverlayKeysSep) {
		if k == "" {
			continue
		}
		keys = append(keys, k)
	}
	return keys
}

// lookupEnvValueCaseFold returns the value of key in env (a "K=V" slice),
// matched case-insensitively on Windows and exactly on POSIX. The last
// matching entry wins, mirroring Go exec env duplicate-key semantics.
func lookupEnvValueCaseFold(env []string, key string) (string, bool) {
	winCaseFold := runtime.GOOS == "windows"
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if !ok {
			continue
		}
		if winCaseFold {
			if strings.EqualFold(k, key) {
				return v, true
			}
		} else if k == key {
			return v, true
		}
	}
	return "", false
}

func daemonOverlayAlreadyApplied(env []string) bool {
	return daemonOverlayMarkerValue(env) == daemonOverlayAppliedEnvValue
}

func appendDaemonOverlayAppliedMarker(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, daemonOverlayAppliedEnvVar) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, daemonOverlayAppliedEnvVar+"="+daemonOverlayAppliedEnvValue)
}

// appendDaemonOverlayKeys returns env with daemonOverlayKeysEnvVar set to
// the comma-joined overlay key set (each key spelled as the overlay stored
// it, so the wrapper's case-fold reader matches the supervisor-written
// os.Environ() entry on Windows). Strip-then-append mirrors
// appendDaemonOverlayAppliedMarker exactly: any pre-existing
// daemonOverlayKeysEnvVar entry — whether inherited from the parent shell
// or injected by a manifest/overlay row that names this reserved key — is
// removed before the trusted value is appended LAST, so no manifest/overlay
// value can spoof, inject, or drop keys. keys MUST be non-empty; callers
// gate on overlayApplied (len(overlayEnv) > 0) before invoking, exactly as
// they gate the APPLIED marker append.
func appendDaemonOverlayKeys(env []string, keys []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, daemonOverlayKeysEnvVar) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, daemonOverlayKeysEnvVar+"="+strings.Join(keys, daemonOverlayKeysSep))
}

// stripAllDaemonOverlayControlVars returns env with EVERY mcphub-reserved
// overlay control variable (reservedDaemonOverlayControlVars: the APPLIED
// marker, the KEYS set, AND the PATH channel) removed. It is the single
// inheritance-immunity owner the spawn closure calls ONCE — after seeding
// cmd.Env from os.Environ()/the manifest+overlay merge and BEFORE appending
// any TRUSTED control var — so no inherited or spoofed control var can ever
// survive into a wrapper's environment.
//
// Why every one of these is untrusted on inherit. The spawned wrapper TRUSTS
// each reserved var in its own os.Environ():
//   - APPLIED → daemonOverlayEnv treats the marker as "supervisor handled the
//     overlay upstream", switching an unreadable overlay file from FATAL to a
//     graceful degrade (and into the KEYS-reconstruction path).
//   - KEYS → on that unreadable-file path the wrapper RECONSTRUCTS the overlay
//     map from this set (daemonOverlayKeysFromEnv → overlayMapFromInjectedKeys),
//     applying each named key's os.Environ() value as a trusted overlay value.
//   - PATH → resolveDaemonOverlayPath reads it FIRST, resolving
//     daemon-env-overrides.yaml from this path instead of the HOME/XDG state dir.
//
// If ANY of these were left at whatever value the supervisor INHERITED from its
// own os.Environ() — a stale entry from a prior run, or a spoofed entry an
// attacker planted in the supervisor's environment, or a value a NON-serena
// wrapper's manifest seeded — the child would trust it: spoofed KEYS injects
// unrelated env as a phantom overlay; a spoofed PATH resolves the overlay from
// an attacker-chosen file. The supervisor is the ONLY legitimate writer of
// these reserved vars, so the spawn closure NEUTRALIZES all of them in one
// place, then re-appends exactly the TRUSTED vars for THIS spawn (APPLIED
// unconditionally; KEYS only when an overlay row applied; PATH only for a
// serena-proxy descriptor). This generalizes the prior per-var no-row KEYS
// strip into the whole-class fix (bot PR #403 r3 — the _PATH inheritance twin
// of the r2 _KEYS strip). Mirrors the strip-then-append discipline of
// appendDaemonOverlayKeys / appendDaemonOverlayAppliedMarker, minus the append.
func stripAllDaemonOverlayControlVars(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && isReservedDaemonOverlayControlVar(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isReservedDaemonOverlayControlVar reports whether key (an env-var NAME) is one
// of the supervisor-reserved overlay control vars. Case-insensitive on Windows
// (env keys are case-folded there) and exact on POSIX, matching the rest of the
// env helpers in this file. Driven off the canonical
// reservedDaemonOverlayControlVars list so the set never diverges.
func isReservedDaemonOverlayControlVar(key string) bool {
	for _, reserved := range reservedDaemonOverlayControlVars {
		if strings.EqualFold(key, reserved) {
			return true
		}
	}
	return false
}

func daemonOverlayMarkerValue(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && strings.EqualFold(k, daemonOverlayAppliedEnvVar) {
			return v
		}
	}
	return ""
}

// logBaseDir returns the per-OS directory for daemon logs.
// Windows: %LOCALAPPDATA%\mcp-local-hub\logs
// Linux/macOS: $XDG_STATE_HOME/mcp-local-hub/logs (or ~/.local/state/mcp-local-hub/logs)
func logBaseDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "logs")
}

// formatChildExit renders the child's ProcessState as a diagnostic
// suffix appended to "native-http upstream exited unexpectedly". The
// goal is to distinguish silent-exit failure modes that the log file
// alone cannot — Codex CLI consult, 2026-04-30:
//
//	exit code 0    — controlled sys.exit(0); upstream thinks it shut
//	                 down cleanly. Hints at swallowed exception.
//	exit code 1    — generic error path; matches Python's sys.exit(1).
//	signal=killed  — POSIX SIGKILL (parent kill or OOM).
//	signal=segfault — POSIX native crash.
//	0xC0000005 / 0xC000013A — Windows native crash or CTRL_BREAK.
//
// On POSIX, ProcessState.ExitCode() returns -1 for signal-terminated
// processes — that loses exactly the SIGKILL distinction the
// diagnostic is meant to capture. extractSignal pulls the signal name
// out of the WaitStatus on Unix; it is a no-op on Windows where the
// NTSTATUS is already encoded in ExitCode. Codex bot review on PR #33 P2.
//
// Returns "" when state is nil (process never spawned, still running,
// or Wait() not yet called) so the caller's Errorf format stays clean.
func formatChildExit(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	ec := state.ExitCode()
	// On Windows, native crashes surface as NTSTATUS reinterpreted as
	// int32 — e.g. 0xC0000005 (access violation) is exit_code=-1073741819
	// in decimal. Useless without hex. Emit exit_hex= when the value is
	// outside the typical 0-255 POSIX range. uint32 cast preserves the
	// NTSTATUS bit pattern. Codex CLI review on PR #34 P3.
	hex := ""
	if ec < 0 || ec > 255 {
		hex = fmt.Sprintf(" exit_hex=0x%x", uint32(ec))
	}
	return fmt.Sprintf(" (pid=%d exit_code=%d%s%s)", state.Pid(), ec, hex, extractSignal(state))
}

// writeLaunchFailure appends a timestamped failure diagnostic to the
// daemon's log file so Task Scheduler's last_result=1 isn't a black hole.
// Failures to open or write the diagnostic are silently dropped — we
// don't want this wrapper to compound the original launch error or to
// fail the deferred path with a panic. The line format is grep-able:
// `[mcphub-launch-failure <RFC3339-UTC> server=<s> daemon=<d>] <err>`.
func writeLaunchFailure(logPath, server, daemonName string, launchErr error) {
	if mkErr := os.MkdirAll(filepath.Dir(logPath), 0700); mkErr != nil {
		return
	}
	f, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if openErr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n[mcphub-launch-failure %s server=%s daemon=%s] %v\n",
		time.Now().UTC().Format(time.RFC3339), server, daemonName, launchErr)
}
