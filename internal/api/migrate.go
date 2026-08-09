package api

import (
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// MigrateOpts controls a migration invocation.
//
// Servers lists the server names to migrate; one manifest lookup per name
// happens inside MigrateFrom. ClientsInclude optionally narrows the set of
// clients whose configs are rewritten — empty means every client binding in
// the manifest is processed. DryRun reports the intended URL rewrites without
// touching any config file. ScanOpts currently carries ManifestDir for
// per-server manifest lookup; the client-path fields on ScanOpts are not
// consumed here because each client adapter resolves its own config path via
// os.UserHomeDir() at adapter-construction time.
type MigrateOpts struct {
	Servers        []string
	ClientsInclude []string
	DryRun         bool
	ScanOpts       ScanOpts
	// GUIPort is the LIVE GUI/hub listener port, set by the GUI-side caller
	// (realMigrator) so the dynamic-pool serena server is written with its
	// canonical /serena/mcp router URL (SerenaRouterClientURL) rather than the
	// legacy per-daemon 9121 URL from serena's still-legacy-shaped manifest. 0
	// means "unknown" (CLI / non-GUI caller): serena then falls back to the
	// generic per-daemon URL, since the router only exists while the GUI runs.
	GUIPort int
}

// MigrateReport holds per-(server, client) outcomes for a migration run.
// Applied rows describe successful rewrites (or intended rewrites in dry-run);
// Failed rows carry the error message for display. The CLI and GUI render the
// same report shape without interpreting it further.
type MigrateReport struct {
	Applied []AppliedMigration `json:"applied"`
	Failed  []FailedMigration  `json:"failed"`
}

// AppliedMigration is one actually rewritten (or dry-run-intended) (server,
// client) pair. An applied mutation with unconfirmed lock release also has a
// FailedMigration lifecycle row; the two collections are not disjoint.
type AppliedMigration struct {
	Server string `json:"server"`
	Client string `json:"client"`
	URL    string `json:"url"`
	// BackupPath is the per-client backup file written immediately BEFORE
	// the entry rewrite, capturing the pre-rewrite shape of this client's
	// config. Set by ReconcileSerenaClientsToRouter so a caller can undo a
	// partially-successful reconcile (restore each Applied client to its
	// pre-rewrite entry) via RestoreSerenaReconcileApplied — the serena
	// migrate driver uses this for its outer-rollback "reconcile failed on
	// some clients, restore the ones that succeeded before the point of no
	// return" path. Empty for producers that do not snapshot a backup
	// (e.g. MigrateFrom) or for dry-run rows; omitempty keeps the JSON
	// surface backward-compatible.
	BackupPath string `json:"backup_path,omitempty"`
	// restoreUnsafe is set only when the config mutation applied but its lock
	// release could not be confirmed. It is deliberately not serialized: the
	// in-process rollback owner consumes it to avoid reacquiring that same leaf.
	restoreUnsafe bool
}

// FailedMigration is one (server, client) pair that could not be migrated.
// Err is the string form of the underlying error so the report serialises
// cleanly to JSON.
type FailedMigration struct {
	Server string `json:"server"`
	Client string `json:"client"`
	Err    string `json:"err"`
}

// MigrateFrom rewrites stdio entries to hub-HTTP entries (or relay entries
// for Antigravity) for each (server, client) pair derived from the manifest
// bindings intersected with ClientsInclude. The operation is idempotent:
// adapters overwrite any existing entry with the same name, and re-running
// migration yields the same end state.
//
// Errors during manifest lookup, backup, or adapter write do not abort the
// entire run — they are captured in MigrateReport.Failed so partial progress
// remains observable. A server whose manifest cannot be opened produces one
// Failed row for that server and the migration continues with the next one.
//
// The returned error is always nil today; the signature reserves space for a
// future pre-flight check (e.g. "manifest dir missing") that legitimately
// blocks the whole run.
func (a *API) MigrateFrom(opts MigrateOpts) (*MigrateReport, error) {
	report := &MigrateReport{}
	allClients := clients.AllClients()
	// Read backups.keep_n once per migrate run — settings file IO is
	// cheap, but the loop below can fan out to N servers × M clients
	// and re-reading on every iteration would be wasteful. The adapter
	// prunes in-place after each fresh timestamped backup so the
	// on-disk set never exceeds keepN.
	keepN := a.effectiveBackupKeepN()

	includedClient := func(c string) bool {
		if len(opts.ClientsInclude) == 0 {
			return true
		}
		for _, x := range opts.ClientsInclude {
			if x == c {
				return true
			}
		}
		return false
	}

	for _, server := range opts.Servers {
		m, err := loadManifestForServer(opts.ScanOpts.ManifestDir, server)
		if err != nil {
			report.Failed = append(report.Failed, FailedMigration{Server: server, Err: err.Error()})
			continue
		}
		// Companion sink-guard: a kind=companion server is a hub-managed NON-MCP
		// process with no client routing. It has no client_bindings, so the
		// binding loop below is a no-op — but the synthesized-binding pass would
		// otherwise fabricate a binding from its primary daemon and write a bogus
		// client URL. Skip it outright (defense-in-depth atop the scan
		// source-filter that already keeps it out of the matrix).
		if m.Kind == config.KindCompanion {
			continue
		}
		// Track which clients a manifest binding already covers for this
		// server so the synthesized-binding pass below does not migrate a
		// client twice.
		boundClients := map[string]bool{}
		for _, binding := range m.ClientBindings {
			boundClients[binding.Client] = true
			if !includedClient(binding.Client) {
				continue
			}
			migrateOneBinding(report, allClients, m, server, binding, opts.DryRun, keepN, opts.GUIPort, clients.ClassifyClientMutation)
		}
		// Servers-matrix fix: the matrix renders a toggleable cell for
		// EVERY detected client, not only the manifest's static
		// client_bindings. A targeted client outside client_bindings was
		// previously a silent no-op (the iteration above skipped it).
		// Synthesize a binding for each such targeted client so Apply
		// actually writes the hub-HTTP entry. The synthesized binding
		// points at the server's primary daemon (the one named "default",
		// else the first daemon) with the canonical /mcp path. Eligibility
		// is gated to (a) a client explicitly named in ClientsInclude,
		// (b) NOT already covered by a manifest binding, (c) a known
		// adapter, (d) adapter.Exists() — so we never fabricate work for a
		// client the host does not have. Same per-binding body as a real
		// binding (DRY via migrateOneBinding).
		for _, client := range opts.ClientsInclude {
			if boundClients[client] {
				continue
			}
			adapter := allClients[client]
			if adapter == nil {
				continue
			}
			// DryRun reports the intended write without checking Exists()
			// (mirrors the real-binding path, which evaluates Exists()
			// only after the DryRun short-circuit). Skip the Exists()
			// gate here so DryRun stays a pure preview.
			if !opts.DryRun && !adapter.Exists() {
				continue
			}
			primaryDaemon, ok := primaryDaemonName(m)
			if !ok {
				// Manifest has no daemons at all — cannot synthesize a URL.
				// Surface a Failed row so the operator sees why this
				// targeted client produced nothing.
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: client,
					Err: fmt.Sprintf("manifest %s: no daemons declared; cannot migrate non-binding client %q", server, client),
				})
				continue
			}
			synth := config.ClientBinding{
				Client:  client,
				Daemon:  primaryDaemon,
				URLPath: "/mcp",
			}
			migrateOneBinding(report, allClients, m, server, synth, opts.DryRun, keepN, opts.GUIPort, clients.ClassifyClientMutation)
		}
	}
	return report, nil
}

// migrateOneBinding performs the per-(server, client) migrate for a single
// binding — real (from m.ClientBindings) or synthesized (for a targeted
// non-binding client). Ordinary outcomes append exactly one row to
// report.Applied or report.Failed; a skipped client appends none. The typed
// applied-release outcome is the exception: it records the actual change in
// Applied and its lifecycle failure in Failed. Manifest-bound and synthesized
// clients run this identical logic.
//
// The adapter's AddEntry overwrites any existing same-name entry wholesale
// (map-key assignment on every adapter; see addentry_overwrites note in the
// implementation report), so re-pointing a stale-port entry at the correct
// current daemon port is a plain idempotent overwrite.
func migrateOneBinding(
	report *MigrateReport,
	allClients map[string]clients.Client,
	m *config.ServerManifest,
	server string,
	binding config.ClientBinding,
	dryRun bool,
	keepN int,
	guiPort int,
	classify func(error) clients.ClientMutationSettlement,
) {
	adapter := allClients[binding.Client]
	if adapter == nil {
		// No adapter constructed on this host (e.g. UserHomeDir failed);
		// silently skip — a Failed row would add noise without a
		// repairable cause the user can act on.
		return
	}
	daemonPort, ok := findDaemonPort(m, binding.Daemon)
	if !ok {
		report.Failed = append(report.Failed, FailedMigration{
			Server: server, Client: binding.Client,
			Err: fmt.Sprintf("manifest %s: binding references unknown daemon %q", server, binding.Daemon),
		})
		return
	}
	urlPath := binding.URLPath
	if urlPath == "" {
		urlPath = "/mcp"
	}
	url := clients.HubLoopbackURL(daemonPort, urlPath)
	// serena is the dynamic-pool router-fronted server: its canonical client URL
	// is the constant /serena/mcp router on the LIVE GUI port — NOT the legacy
	// per-daemon 9121 URL the line above derives from serena's still-legacy-shaped
	// manifest. Without this, checking serena's matrix cell wrote a dead 9121 entry
	// (serena-client-revert-on-manifest-sync write-side). guiPort is 0 for a
	// CLI/non-GUI caller (the router only exists while the GUI runs); the dedicated
	// `mcphub migrate serena legacy-to-dynamic-pool` command owns that path.
	if IsSerenaServer(server) && guiPort > 0 {
		url = SerenaRouterClientURL(guiPort)
	}

	if dryRun {
		report.Applied = append(report.Applied, AppliedMigration{
			Server: server, Client: binding.Client, URL: url,
		})
		return
	}

	if !adapter.Exists() {
		// Client not installed on this machine — nothing to migrate.
		// Skip quietly: this mirrors Install's behavior for missing
		// clients and keeps the report focused on actual attempts.
		return
	}
	if _, err := adapter.BackupKeep(keepN); err != nil {
		report.Failed = append(report.Failed, FailedMigration{
			Server: server, Client: binding.Client, Err: err.Error(),
		})
		return
	}
	entry := clients.MCPEntry{
		Name:        server,
		URL:         url,
		RelayServer: server,
		RelayDaemon: binding.Daemon,
	}
	if clients.IsRelayStdio(binding.Client) {
		if IsSerenaServer(server) && guiPort > 0 && url == SerenaRouterClientURL(guiPort) {
			entry.RelayURL = url
		}
		// Relay-stdio adapters (antigravity, zed) spawn the
		// stdio relay from an absolute mcphub path persisted
		// into the client config, so AddEntry rejects an entry
		// with no RelayExePath. Anchor at the canonical
		// installed path, not at the running executable.
		// Otherwise a migrate invoked from a dev checkout or
		// %TEMP% build would persist a throwaway absolute path —
		// the next time that path disappears (cleanup, rebuild)
		// the client's MCP entry is silently broken. The
		// relay-stdio fact is owned by clients.IsRelayStdio so a
		// new relay adapter is covered here automatically.
		if canonical, err := canonicalMcphubPath(); err == nil {
			entry.RelayExePath = canonical
		}
	}
	if classify == nil {
		classify = clients.ClassifyClientMutation
	}
	var releaseErr error
	if err := adapter.AddEntry(entry); err != nil {
		if classify(err) != clients.ClientMutationAppliedReleaseUnconfirmed {
			report.Failed = append(report.Failed, FailedMigration{
				Server: server, Client: binding.Client, Err: err.Error(),
			})
			return
		}
		releaseErr = err
	}
	// PR #187 (B4 ownership marker): record this (client,
	// server) tuple in the managed-entries marker file so
	// Demigrate can later distinguish entries mcphub
	// installed from entries the user owned pre-mcphub.
	// Best-effort: a marker-write failure must NOT roll back
	// the successful AddEntry (operator's config is the
	// load-bearing artifact; the marker is observability
	// for the future demigrate path). A marker-only error does not add a Failed
	// row; it is surfaced as a soft warning via the standard hub-mcp event log.
	// The independent applied-release lifecycle failure, when present, is still
	// reported after the actual Applied row below.
	if recErr := RecordManagedEntry(binding.Client, server); recErr != nil {
		_ = LogHubMcpEvent("warn", "managed-entries-record-failed", map[string]any{
			"server": server,
			"client": binding.Client,
			"err":    recErr.Error(),
			"note":   "demigrate fallback for this entry will fail-closed until the marker is repopulated by a subsequent migrate",
		})
	}
	report.Applied = append(report.Applied, AppliedMigration{
		Server: server, Client: binding.Client, URL: url,
	})
	if releaseErr != nil {
		report.Failed = append(report.Failed, FailedMigration{
			Server: server, Client: binding.Client,
			Err: fmt.Sprintf("configuration applied; lock release unconfirmed: %v", releaseErr),
		})
	}
}

// primaryDaemonName returns the name of the manifest's primary daemon — the
// one named "default" if present, otherwise the first daemon in m.Daemons.
// Returns ("", false) when the manifest declares no daemons (dynamic-pool
// daemon_template manifests, or a malformed manifest), so callers can fail
// closed rather than synthesize a URL against a non-existent daemon. Used to
// derive the daemon for a synthesized binding when a targeted client is not
// in the manifest's static client_bindings.
func primaryDaemonName(m *config.ServerManifest) (string, bool) {
	if len(m.Daemons) == 0 {
		return "", false
	}
	for _, d := range m.Daemons {
		if d.Name == "default" {
			return d.Name, true
		}
	}
	return m.Daemons[0].Name, true
}

// loadManifestForServer opens and parses servers/<name>/manifest.yaml.
// Empty dir triggers the production embed-first path (servers.Manifests
// embed FS with disk fallback). A non-empty dir reads only from that
// directory — used by tests that inject hermetic manifest fixtures.
func loadManifestForServer(dir, name string) (*config.ServerManifest, error) {
	if dir == "" {
		data, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			return nil, err
		}
		return parseManifestForName(name, data)
	}
	data, err := os.ReadFile(filepath.Join(dir, name, "manifest.yaml"))
	if err != nil {
		return nil, err
	}
	return parseManifestForName(name, data)
}

// findDaemonPort returns the port of the named daemon from the manifest.
// Returns (0, false) when the name does not match any daemon, so callers can
// treat that as a manifest integrity error without a panic. Reuses the
// canonical findDaemon (install.go) so the manifest daemon-name match lives in
// exactly one place.
func findDaemonPort(m *config.ServerManifest, daemonName string) (int, bool) {
	d, ok := findDaemon(m, daemonName)
	if !ok {
		return 0, false
	}
	return d.Port, true
}
