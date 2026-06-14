// serena_client_reconcile.go — dynamic-pool client-reconcile to the
// constant /serena/mcp router endpoint.
//
// Phase 3 of the serena dynamic-pool migrate redesign (design
// docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md
// §5 finding #5; plan docs/superpowers/plans/2026-05-29-serena-migrate-redesign.md
// "Phase 3 — Client-reconcile to /serena/mcp").
//
// Problem: when serena moves to the dynamic-pool model the per-daemon
// localhost:9121 global daemon goes away, but managed clients still point
// their `serena` MCP entry at it. The /serena/mcp router on the GUI server
// is the new routing surface — it resolves the target workspace from a
// tool's path-arg against the live registry, so every client points at ONE
// constant URL, workspace-agnostic. This file rewrites each in-scope
// client's serena entry to that router URL BEFORE the legacy 9121 endpoint
// is removed, so a per-client rewrite failure leaves that client on the
// still-functional legacy endpoint rather than a dead URL.
//
// CRITICAL — this is NOT the G4 hub-resolver path (design claim #8). Serena
// client routing flows exclusively through the registry-driven /serena/mcp
// router. This file does NOT import or invoke
// BuildResolverSnapshotFromManifests / manifestHasScheduledDaemon and does
// NOT touch the G4 binding topology.
//
// This is a NEW, UNWIRED function: Phase 4 (the migrate command) calls it.
// It is not wired into any CLI command or migrate path here.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
)

// SerenaRouterURLPath is the constant path of the GUI-server router that
// fronts every per-workspace serena daemon. Clients point their serena MCP
// entry here (workspace-agnostic); the router resolves the workspace per
// request from the tool's path-arg (internal/gui/serena_router.go:146
// registers "/serena/mcp").
const SerenaRouterURLPath = "/serena/mcp"

// serenaEntryName is the MCP entry name every client uses for serena. It is
// the manifest server name; clients key their config map on it.
const serenaEntryName = "serena"

// defaultLegacySerenaPort is the legacy global serena daemon port that
// dynamic-pool clients must be moved OFF of. Used only when
// SerenaReconcileOpts.LegacyPort is zero.
const defaultLegacySerenaPort = 9121

// serenaReconcileClientSet is the O2 client set for the serena router
// rewrite — the clients the legacy serena manifest bound
// (servers/serena/manifest.yaml client_bindings) intersected, at call
// time, with the clients actually installed on the host. The set is fixed
// (not hard-coded per workstation): it mirrors the legacy serena binding
// surface. Order is stable for deterministic reports.
//
// Antigravity is in the set but takes the stdio-relay shape (relay → router)
// rather than a direct URL, per the descriptor-proxy design §5.
//
// This fixed set excludes the other relay-stdio adapter (zed) by
// construction — it mirrors only the legacy serena binding surface, which
// predates zed. The relay-stdio classification itself is owned by
// clients.IsRelayStdio (antigravity is correctly classified true there); the
// per-client relay handling below keys off that shape, not off this list.
func serenaReconcileClientSet() []string {
	return []string{
		"claude-code",
		"codex-cli",
		"cursor",
		"vscode",
		"gemini-cli",
		"qwen-cli",
		"antigravity",
	}
}

// readPidportFn is the test seam for parsing the GUI pidport file. It
// mirrors internal/gui.ReadPidport's "<PID> <PORT>\n" parse exactly so the
// runtime fail-closed contract is identical; the api package cannot import
// internal/gui directly (internal/gui imports internal/api, so the reverse
// would be an import cycle). Phase 4's caller in internal/cli — which CAN
// import both — may inject gui.ReadPidport via SerenaReconcileOpts.ReadPidport.
//
// Default: parseGUIPidportFile below.
var readPidportFn = parseGUIPidportFile

// parseGUIPidportFile reads "<PID> <PORT>\n" from path. Returns (0,0,err) on
// a missing file or any parse failure — byte-identical semantics to
// internal/gui.ReadPidport (single_instance.go:92-110).
func parseGUIPidportFile(path string) (pid, port int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed pidport %q", string(b))
	}
	pid, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse pid: %w", err)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse port: %w", err)
	}
	return pid, port, nil
}

// SerenaReconcileOpts configures one client-reconcile run. All discovery
// inputs are injectable so the function is testable without a real GUI and
// so Phase 4 can wire the gui-package primitives across the
// api->gui-can't-import-api boundary.
type SerenaReconcileOpts struct {
	// PidportPath is the absolute path to the GUI pidport file. Phase 4
	// supplies gui.PidportPath(); tests supply a temp path. Required — an
	// empty path fails closed (we never guess the router port).
	PidportPath string

	// ReadPidport parses the pidport file at PidportPath into (pid, port).
	// nil → the package default parseGUIPidportFile (byte-identical to
	// gui.ReadPidport). Phase 4 may inject gui.ReadPidport directly.
	ReadPidport func(path string) (pid, port int, err error)

	// VerifyIdentity binds the pidport to the listener before any router URL is
	// trusted. nil → defaultGUIPidportIdentityCheck, which PROVES at the OS
	// level that the recorded PID owns the loopback router socket: the recorded
	// PID must be alive, must be the OS-reported owner of the 127.0.0.1:<port>
	// LISTENING socket (netstat -ano), and that owner's image must be the
	// mcphub binary. It does NOT trust any PID the listener self-reports over
	// HTTP (that is forgeable from the world-readable pidport file — bot PR
	// #252 P1). Tests may inject a no-op when they are not exercising discovery.
	VerifyIdentity func(ctx context.Context, pid, port int) error

	// Ping confirms the GUI router is actually serving on the discovered port.
	// nil → defaultRouterReadinessPing (a loopback HEAD + initialize probe). A nil
	// return means live; any error fails the reconcile closed. Mirrors the
	// G4 reconcile's live-probe-before-rewrite posture
	// (internal/cli/install.go:348-374).
	Ping func(ctx context.Context, port int) error

	// Clients is the {name -> adapter} map to reconcile. nil →
	// clients.AllClients() (production). Tests inject hermetic adapters
	// (their config paths resolve under a redirected HOME/USERPROFILE).
	Clients map[string]clients.Client

	// ClientsInclude optionally narrows the in-scope set. Empty → the full
	// serenaReconcileClientSet() intersected with installed adapters.
	ClientsInclude []string

	// McphubExePath is the absolute path written into Antigravity's relay
	// `command` field. "" → canonicalMcphubPath() (the installed binary,
	// never a throwaway %TEMP%/dev-checkout path — same rationale as
	// MigrateFrom).
	McphubExePath string

	// LegacyPort is the legacy global serena daemon port to remove clients
	// off of after a successful router rewrite. 0 → defaultLegacySerenaPort
	// (9121).
	LegacyPort int

	// RemoveLegacy, when true, removes the legacy localhost:<LegacyPort>
	// serena entry from a client AFTER that client's router rewrite
	// succeeds (claim #9). When false, the rewrite happens but the legacy
	// entry (if distinct) is left in place. Because every in-scope client
	// uses the SAME entry name ("serena"), the router rewrite already
	// overwrites the legacy entry in-place for URL clients; RemoveLegacy is
	// the explicit knob for callers that key legacy + router under
	// different names or want the removal observable in the report.
	RemoveLegacy bool

	// BackupKeepN bounds the per-adapter rolling backup count. 0 → no
	// pruning (Backup semantics). Phase 4 supplies effectiveBackupKeepN().
	BackupKeepN int

	// DryRun reports the intended rewrites without touching any config
	// file. Discovery (pidport + ping) still runs so a dry-run surfaces the
	// "start the GUI first" failure too.
	DryRun bool
}

// ErrSerenaReconcileGUINotLive is the fail-closed sentinel returned when the
// GUI pidport is absent/stale or the readiness ping fails. Callers must NOT
// write any client entry when this fires — a guessed/spoofed router URL must
// never reach a client config (security: the loopback-only address + the
// readiness ping bound the URL to a live, local GUI).
var ErrSerenaReconcileGUINotLive = errors.New("serena client-reconcile: GUI not live (start `mcphub gui` first); refusing to write a guessed router URL")

// ReconcileSerenaClientsToRouter rewrites each in-scope client's serena MCP
// entry to the constant /serena/mcp router URL on the live GUI port, then
// (optionally) removes the legacy localhost:9121 entry — but ONLY after the
// router rewrite for that client has succeeded.
//
// GUI-port discovery is live-pidport + readiness ping, fail-closed: it reads
// the actual bound port from the pidport file (written only AFTER the
// listener is up — the persisted setting is WRONG for --port 0 / explicit
// flag launches), pings it, and on absent/stale pidport OR ping failure
// returns ErrSerenaReconcileGUINotLive with NO client writes.
//
// The result is a MigrateReport: Applied rows are successful (or dry-run
// intended) router rewrites; Failed rows carry the per-client error so a
// partial failure leaves that client on its still-functional legacy
// endpoint and is retryable. The returned error is non-nil only for a
// whole-run blocker (GUI not live).
//
// This function does NOT restart the supervisor, does NOT touch the disk
// manifest, and does NOT flow through the G4 hub resolver (claim #8). It is
// inert until Phase 4 wires it into the migrate command.
func ReconcileSerenaClientsToRouter(ctx context.Context, opts SerenaReconcileOpts) (*MigrateReport, error) {
	report := &MigrateReport{}

	// 1. Discover the live GUI port (fail-closed). This MUST happen before
	//    any client write so a stale/absent pidport or a dead listener
	//    never results in a guessed URL being persisted.
	port, err := discoverLiveGUIPort(ctx, opts)
	if err != nil {
		return nil, err
	}

	routerURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, SerenaRouterURLPath)

	legacyPort := opts.LegacyPort
	if legacyPort == 0 {
		legacyPort = defaultLegacySerenaPort
	}

	allClients := opts.Clients
	if allClients == nil {
		allClients = clients.AllClients()
	}

	// 2. Antigravity's relay `command` field needs the canonical installed
	//    mcphub path. Resolve once. A failure here is NOT fatal to the whole
	//    run — only Antigravity needs it, and its per-client attempt will
	//    surface the error.
	relayExePath := opts.McphubExePath
	if relayExePath == "" {
		if canonical, cerr := canonicalMcphubPath(); cerr == nil {
			relayExePath = canonical
		}
	}

	for _, clientName := range inScopeReconcileClients(opts.ClientsInclude) {
		adapter := allClients[clientName]
		if adapter == nil {
			// No adapter constructed on this host (e.g. UserHomeDir
			// failed). Silently skip — a Failed row would add noise
			// without a repairable cause, mirroring MigrateFrom.
			continue
		}

		entry := clients.MCPEntry{
			Name: serenaEntryName,
			URL:  routerURL,
			// Relay fields are consumed only by the Antigravity adapter;
			// URL adapters ignore them. RelayURL routes the relay at the
			// router via its --url escape hatch (the /serena/mcp router has
			// no per-daemon manifest port for the --server/--daemon form).
			RelayServer:  serenaEntryName,
			RelayDaemon:  "claude",
			RelayExePath: relayExePath,
			RelayURL:     routerURL,
		}

		if opts.DryRun {
			report.Applied = append(report.Applied, AppliedMigration{
				Server: serenaEntryName, Client: clientName, URL: routerURL,
			})
			continue
		}

		if !adapter.Exists() {
			// Client not installed on this machine — nothing to reconcile.
			// Skip quietly (mirrors MigrateFrom / Install).
			continue
		}

		// Back up before mutating, same discipline as MigrateFrom. The
		// returned path is threaded onto the Applied row so a partial-failure
		// caller (the serena migrate driver) can restore this client to its
		// pre-rewrite entry via RestoreSerenaReconcileApplied.
		backupPath, berr := adapter.BackupKeep(opts.BackupKeepN)
		if berr != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName, Err: berr.Error(),
			})
			continue
		}

		// 3. Router rewrite. On failure, DO NOT remove the legacy entry —
		//    the client stays on its still-functional legacy endpoint
		//    (claim #9) and the failure is reported for retry.
		if aerr := adapter.AddEntry(entry); aerr != nil {
			report.Failed = append(report.Failed, FailedMigration{
				Server: serenaEntryName, Client: clientName, Err: aerr.Error(),
			})
			continue
		}

		// 4. Record the managed-entries marker so a later demigrate can tell
		//    a mcphub-installed entry from an operator-owned one (demigrate
		//    symmetry — same RecordManagedEntry discipline MigrateFrom uses
		//    at migrate.go:175). Best-effort: a marker-write failure must NOT
		//    roll back the successful router rewrite (the operator's config is
		//    the load-bearing artifact; the marker is observability). The row
		//    is still Applied; the marker error is a soft warning.
		if recErr := RecordManagedEntry(clientName, serenaEntryName); recErr != nil {
			_ = LogHubMcpEvent("warn", "managed-entries-record-failed", map[string]any{
				"server": serenaEntryName,
				"client": clientName,
				"err":    recErr.Error(),
				"note":   "serena router-reconcile demigrate fallback for this entry will fail-closed until the marker is repopulated",
			})
		}

		// 5. Legacy-endpoint removal — ONLY after the rewrite above
		//    succeeded (claim #9). For URL clients the router rewrite
		//    already overwrote the same-named "serena" entry in place, so
		//    there is nothing distinct to remove; RemoveLegacy is honored
		//    for callers whose legacy entry lives under a different name or
		//    who want the ordering invariant exercised explicitly. A removal
		//    failure does NOT undo the successful rewrite — it is a soft
		//    warning, since the client is already on the router URL.
		if opts.RemoveLegacy {
			if rerr := removeLegacySerenaEntry(adapter, routerURL, legacyPort); rerr != nil {
				_ = LogHubMcpEvent("warn", "serena-legacy-endpoint-removal-failed", map[string]any{
					"server":      serenaEntryName,
					"client":      clientName,
					"legacy_port": legacyPort,
					"err":         rerr.Error(),
					"note":        "router rewrite already succeeded; client is on the /serena/mcp router, legacy entry cleanup deferred",
				})
			}
		}

		report.Applied = append(report.Applied, AppliedMigration{
			Server: serenaEntryName, Client: clientName, URL: routerURL, BackupPath: backupPath,
		})
	}

	return report, nil
}

// RestoreSerenaReconcileApplied undoes a partially-successful
// ReconcileSerenaClientsToRouter run by restoring every Applied client's
// serena entry from the per-client backup the reconcile captured immediately
// before its rewrite. It is the outer-rollback compensator the serena migrate
// driver runs when the reconcile reports per-client failures (report.Failed
// non-empty): the migrate must NOT proceed to the irreversible supervisor reap
// while only SOME clients point at the router, so the ones that succeeded are
// reverted to their pre-rewrite (legacy) entry and the whole run is aborted.
//
// allClients is the {name -> adapter} map to restore against (the same
// surface the reconcile rewrote); nil → clients.AllClients(). Restore is
// best-effort per client: a client whose adapter is missing, whose backup
// path was not recorded (dry-run / empty), or whose restore errors is
// collected into the returned joined error, but every other client is still
// attempted so one failure does not strand the rest on the router.
//
// CRITICAL — this restore uses RestoreEntryFromBackupForRollback, NOT the
// plain RestoreEntryFromBackup. The per-client backup captured before the
// reconcile rewrite is the client's PRE-RECONCILE state, which for a normal
// pre-cutover serena client IS the legacy hub entry (loopback
// http://localhost:9121/mcp for URL clients, or the `mcphub relay` form for
// Antigravity). RestoreEntryFromBackup defends the demigrate flow by
// REFUSING to write a hub-managed-shaped backup entry
// (ErrBackupEntryAlreadyMigrated) — which would make this abort-rollback
// FAIL and strand the already-rewritten clients on /serena/mcp even though
// the migration aborted (no dynamic-pool intent, no daemons). The rollback
// variant bypasses that guard to put the exact pre-reconcile bytes back; the
// demigrate guard stays in force for the normal demigrate flow.
func RestoreSerenaReconcileApplied(report *MigrateReport, allClients map[string]clients.Client) error {
	if report == nil {
		return nil
	}
	if allClients == nil {
		allClients = clients.AllClients()
	}
	var errs []error
	for _, app := range report.Applied {
		if app.BackupPath == "" {
			// No snapshot to restore from (dry-run row, or a producer that
			// does not capture a backup). Nothing to undo for this client.
			continue
		}
		adapter := allClients[app.Client]
		if adapter == nil {
			errs = append(errs, fmt.Errorf("restore %s/%s: no adapter on this host", app.Server, app.Client))
			continue
		}
		if rerr := adapter.RestoreEntryFromBackupForRollback(app.BackupPath, serenaEntryName); rerr != nil {
			errs = append(errs, fmt.Errorf("restore %s/%s from %s: %w", app.Server, app.Client, app.BackupPath, rerr))
		}
	}
	return errors.Join(errs...)
}

// inScopeReconcileClients returns the client names to reconcile: the full
// serenaReconcileClientSet() unless include narrows it. Narrowing preserves
// the canonical order and drops names not in the set (so a caller cannot
// widen the surface past the legacy serena bindings).
func inScopeReconcileClients(include []string) []string {
	full := serenaReconcileClientSet()
	if len(include) == 0 {
		return full
	}
	want := make(map[string]bool, len(include))
	for _, c := range include {
		want[c] = true
	}
	out := make([]string, 0, len(full))
	for _, c := range full {
		if want[c] {
			out = append(out, c)
		}
	}
	return out
}

// discoverLiveGUIPort reads the GUI pidport, confirms the GUI is live via a
// readiness ping, and returns the bound port. Fail-closed: an empty
// PidportPath, an unreadable/stale pidport, an out-of-range port, or a ping
// failure all return ErrSerenaReconcileGUINotLive (wrapped) so callers never
// write a guessed URL.
func discoverLiveGUIPort(ctx context.Context, opts SerenaReconcileOpts) (int, error) {
	if opts.PidportPath == "" {
		return 0, fmt.Errorf("%w: no pidport path provided", ErrSerenaReconcileGUINotLive)
	}
	readPidport := opts.ReadPidport
	if readPidport == nil {
		readPidport = readPidportFn
	}
	pid, port, err := readPidport(opts.PidportPath)
	if err != nil {
		return 0, fmt.Errorf("%w: read pidport %s: %v", ErrSerenaReconcileGUINotLive, opts.PidportPath, err)
	}
	// A 0 port is the well-known "auto-assign placeholder" the GUI writes
	// BEFORE its listener binds (internal/cli/gui.go) — it is never a usable
	// router port. Out-of-range values are corrupt metadata. Either way the
	// GUI is not yet serving a real port: fail closed.
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%w: pidport %s has no usable bound port (%d) — the GUI listener is not up", ErrSerenaReconcileGUINotLive, opts.PidportPath, port)
	}
	verifyIdentity := opts.VerifyIdentity
	if verifyIdentity == nil {
		verifyIdentity = defaultGUIPidportIdentityCheck
	}
	if ierr := verifyIdentity(ctx, pid, port); ierr != nil {
		return 0, fmt.Errorf("%w: pidport %s identity check failed for PID %d on port %d: %v", ErrSerenaReconcileGUINotLive, opts.PidportPath, pid, port, ierr)
	}
	ping := opts.Ping
	if ping == nil {
		ping = defaultRouterReadinessPing
	}
	if perr := ping(ctx, port); perr != nil {
		return 0, fmt.Errorf("%w: GUI on port %d did not answer the readiness ping: %v", ErrSerenaReconcileGUINotLive, port, perr)
	}
	return port, nil
}

// loopbackPortOwnerFn is the test seam for the OS-level port→owner-PID
// resolution. Production: loopbackPortOwnerPID (Windows: `netstat -ano` scan
// for the 127.0.0.1:<port> LISTENING owner; POSIX: fail-closed stub). Tests
// override it to inject a synthetic owner without a real listener, mirroring
// the file's existing readPidportFn / Ping / VerifyIdentity seam style.
var loopbackPortOwnerFn = loopbackPortOwnerPID

// guiImageForPIDFn is the test seam for the owner-PID → image-basename
// resolution. Production: guiImageForPID (Windows: procNameAndParent via
// wmic/PowerShell; POSIX: fail-closed stub). Tests override it to assert the
// foreign-image rejection without a real process table.
var guiImageForPIDFn = guiImageForPID

// defaultGUIPidportIdentityCheck PROVES, at the OS level, that the recorded
// pidport PID is the process that actually owns the loopback router socket —
// it does NOT trust any value the listener self-reports over HTTP.
//
// Why the self-reported path was insufficient (bot PR #252 P1): the GUI
// pidport file is world-readable and intentionally left on disk after the GUI
// exits. A local attacker that binds the stale loopback port can read the
// pidport file and echo its PID back from /api/ping, while processAlive(pid)
// only proves SOME process with that PID exists — not that it owns the socket.
// The HTTP-reported PID is therefore forgeable. The fix replaces it with an
// unforgeable OS binding:
//
//  1. processAlive(pid) — cheap pre-check: fail fast if the recorded PID is
//     already dead (the common stale-pidport case) before shelling out.
//  2. loopbackPortOwnerFn(port) — ask the OS who owns the 127.0.0.1:<port>
//     LISTENING socket. Require ownerPID == the recorded pidport pid. A
//     mismatch means a DIFFERENT process holds the port (stale GUI gone, port
//     reused by an attacker) → fail closed.
//  3. guiImageForPIDFn(ownerPID) — require the owning process's image basename
//     to be the mcphub binary (clients.IsMcphubBinary). A foreign image owning
//     the port → fail closed.
//
// The separate router-readiness probe (opts.Ping / defaultRouterReadinessPing)
// still runs AFTER this proof to confirm the router is actually serving; it is
// no longer the identity authority. Every failure path here is fail-closed:
// any uncertainty refuses the reconcile rather than trusting a guess.
func defaultGUIPidportIdentityCheck(ctx context.Context, pid, port int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pidport pid %d", pid)
	}
	// (1) Cheap liveness pre-check — fail fast on a dead recorded PID.
	alive, err := processAlive(pid)
	if err != nil {
		return fmt.Errorf("check recorded process liveness: %w", err)
	}
	if !alive {
		return fmt.Errorf("recorded process is not alive")
	}
	// (2) OS-level port-owner proof. The OWNER of the loopback LISTENING
	//     socket must be the recorded PID — not a value the listener reports
	//     about itself.
	ownerPID, ok, err := loopbackPortOwnerFn(port)
	if err != nil {
		// netstat failure OR the POSIX fail-closed stub. Refuse — never fall
		// back to a self-reported PID.
		return fmt.Errorf("resolve OS owner of loopback port %d: %w", port, err)
	}
	if !ok || ownerPID <= 0 {
		return fmt.Errorf("no process owns loopback LISTENING port %d (pidport is stale or the GUI listener is down)", port)
	}
	if ownerPID != pid {
		return fmt.Errorf("loopback port %d is owned by PID %d, not the recorded pidport PID %d (stale pidport / port reused by another process)", port, ownerPID, pid)
	}
	// (3) The owning process's image must be the mcphub binary. A foreign
	//     image holding the port fails closed even if it somehow matched the
	//     recorded PID number.
	image, ok := guiImageForPIDFn(ownerPID)
	if !ok {
		return fmt.Errorf("could not resolve the image of port-%d owner PID %d (OS-level identity proof unavailable)", port, ownerPID)
	}
	if !clients.IsMcphubBinary(image) {
		return fmt.Errorf("loopback port %d owner PID %d image %q is not the mcphub GUI binary (foreign process owns the router port)", port, ownerPID, image)
	}
	return nil
}

// defaultRouterReadinessPing confirms a live local GUI is serving on port by
// issuing a loopback HEAD request to the /serena/mcp router path. Any
// transport error fails closed. The router answers a HEAD on the same-origin
// path (a 4xx/5xx status still proves SOMETHING local is listening and
// serving that route); a connection-refused on a dead/stale port returns a
// transport error. This mirrors the G4 reconcile precedent of probing the
// bound port before rewriting client configs
// (internal/cli/install.go:348-374) and keeps the address loopback-only so a
// remote/spoofed endpoint can never satisfy it.
func defaultRouterReadinessPing(ctx context.Context, port int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, SerenaRouterURLPath)
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	// Verify this is actually the mcphub serena router, NOT just any local HTTP
	// server that happened to reuse a stale pidport's port (bot PR #248 P1). The
	// router answers a non-POST request (our HEAD) with 405 + `Allow: POST`
	// (internal/gui/serena_router.go:231-232) — a signature a random reused-port
	// server would not produce. A 200/404/other status means something ELSE is
	// listening here; fail closed so the reconcile never rewrites client configs
	// to an unrelated service (the prior "any HTTP response = live GUI" check
	// broke the fail-closed discovery guarantee).
	if resp.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(strings.ToUpper(resp.Header.Get("Allow")), "POST") {
		return fmt.Errorf("port %d responded but is not the mcphub serena router (HEAD %s -> status %d, Allow=%q; expected 405 with Allow: POST) — the GUI may not be up, or the pidport is stale and the port was reused by another service", port, SerenaRouterURLPath, resp.StatusCode, resp.Header.Get("Allow"))
	}
	// Step 2 (bot PR #248 P2): verify the router actually SERVES the MCP session
	// lifecycle before we point any client at it. The HEAD/405 check above only
	// proves the serena route exists; a real client's FIRST request is an MCP
	// `initialize` (no workspace path-arg), and the current router rejects any
	// POST without params.name — so a client pointed at it would fail at session
	// setup. This probe fails CLOSED until the router-completion phase synthesizes
	// the non-tool lifecycle, so the reconcile never rewrites a client to a router
	// that cannot complete `initialize`.
	return mcpInitializeProbe(ctx, port)
}

// mcpInitializeProbe POSTs a minimal MCP `initialize` request to the serena
// router and returns nil only if the router answers with a JSON-RPC RESULT
// (i.e. it serves the session lifecycle). A non-200 status, a JSON-RPC error, or
// a missing result means the router does not yet handle the non-tool lifecycle
// (the pre-router-completion state, where it only routes tool calls by
// params.name) → fail closed. Loopback-only; same 2s budget as the HEAD ping.
func mcpInitializeProbe(ctx context.Context, port int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, SerenaRouterURLPath)
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcphub-reconcile-probe","version":"0"}}}`
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(initBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("router at port %d does not serve the MCP lifecycle: initialize -> status %d (the /serena/mcp router must synthesize/handle initialize before clients are pointed at it; body=%.200s)", port, resp.StatusCode, string(raw))
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if jerr := json.Unmarshal(raw, &rpc); jerr != nil {
		return fmt.Errorf("router at port %d returned a non-JSON-RPC initialize response: %w", port, jerr)
	}
	if len(rpc.Error) > 0 || len(rpc.Result) == 0 {
		return fmt.Errorf("router at port %d rejected MCP initialize (no result; error=%s) — it does not yet serve the non-tool lifecycle (router-completion phase pending)", port, string(rpc.Error))
	}
	return nil
}

// removeLegacySerenaEntry removes a client's pre-dynamic-pool serena entry
// when it still points at the legacy localhost:<legacyPort> daemon and is
// NOT already the router entry we just wrote. This is the explicit
// legacy-cleanup step (claim #9), invoked ONLY after the router rewrite for
// the client succeeded.
//
// Because every in-scope client uses the same entry name ("serena"), the
// router AddEntry already overwrote the legacy entry in place for URL
// clients — so in the common case GetEntry now returns the router URL and
// this is a no-op. The guard below prevents deleting the freshly-written
// router entry: it removes only when the live entry still resolves to the
// legacy port.
func removeLegacySerenaEntry(adapter clients.Client, routerURL string, legacyPort int) error {
	cur, err := adapter.GetEntry(serenaEntryName)
	if err != nil {
		return err
	}
	if cur == nil {
		return nil // nothing to remove
	}
	// Never remove the router entry we just wrote.
	if cur.URL == routerURL {
		return nil
	}
	// Antigravity relay entries carry no URL; the router rewrite already
	// replaced its args, so leave it alone here.
	if cur.URL == "" {
		return nil
	}
	legacyMarker := fmt.Sprintf(":%d", legacyPort)
	if !strings.Contains(cur.URL, legacyMarker) {
		// The live entry points somewhere else (a user-configured remote, or
		// already-migrated state). Do not touch it — fail-closed against
		// deleting an entry we do not positively recognize as the legacy
		// daemon.
		return nil
	}
	return adapter.RemoveEntry(serenaEntryName)
}
