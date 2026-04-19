package api

import (
	"bytes"
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
}

// MigrateReport holds per-(server, client) outcomes for a migration run.
// Applied rows describe successful rewrites (or intended rewrites in dry-run);
// Failed rows carry the error message for display. The CLI and GUI render the
// same report shape without interpreting it further.
type MigrateReport struct {
	Applied []AppliedMigration `json:"applied"`
	Failed  []FailedMigration  `json:"failed"`
}

// AppliedMigration is one successfully rewritten (or dry-run-intended)
// (server, client) pair.
type AppliedMigration struct {
	Server string `json:"server"`
	Client string `json:"client"`
	URL    string `json:"url"`
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
		for _, binding := range m.ClientBindings {
			if !includedClient(binding.Client) {
				continue
			}
			adapter := allClients[binding.Client]
			if adapter == nil {
				// No adapter constructed on this host (e.g. UserHomeDir failed);
				// silently skip — a Failed row would add noise without a
				// repairable cause the user can act on.
				continue
			}
			daemonPort, ok := findDaemonPort(m, binding.Daemon)
			if !ok {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client,
					Err: fmt.Sprintf("manifest %s: binding references unknown daemon %q", server, binding.Daemon),
				})
				continue
			}
			urlPath := binding.URLPath
			if urlPath == "" {
				urlPath = "/mcp"
			}
			url := fmt.Sprintf("http://localhost:%d%s", daemonPort, urlPath)

			if opts.DryRun {
				report.Applied = append(report.Applied, AppliedMigration{
					Server: server, Client: binding.Client, URL: url,
				})
				continue
			}

			if !adapter.Exists() {
				// Client not installed on this machine — nothing to migrate.
				// Skip quietly: this mirrors Install's behavior for missing
				// clients and keeps the report focused on actual attempts.
				continue
			}
			if _, err := adapter.Backup(); err != nil {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: err.Error(),
				})
				continue
			}
			entry := clients.MCPEntry{
				Name:        server,
				URL:         url,
				RelayServer: server,
				RelayDaemon: binding.Daemon,
			}
			if binding.Client == "antigravity" {
				// Anchor at the canonical installed path, not at the
				// running executable. Otherwise a migrate invoked from a
				// dev checkout or %TEMP% build would persist a
				// throwaway absolute path into Antigravity's config —
				// the next time that path disappears (cleanup, rebuild)
				// Antigravity's MCP entry is silently broken.
				if canonical, err := canonicalMcphubPath(); err == nil {
					entry.RelayExePath = canonical
				}
			}
			if err := adapter.AddEntry(entry); err != nil {
				report.Failed = append(report.Failed, FailedMigration{
					Server: server, Client: binding.Client, Err: err.Error(),
				})
				continue
			}
			report.Applied = append(report.Applied, AppliedMigration{
				Server: server, Client: binding.Client, URL: url,
			})
		}
	}
	return report, nil
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
		return config.ParseManifest(bytes.NewReader(data))
	}
	f, err := os.Open(filepath.Join(dir, name, "manifest.yaml"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return config.ParseManifest(f)
}

// findDaemonPort returns the port of the named daemon from the manifest.
// Returns (0, false) when the name does not match any daemon, so callers can
// treat that as a manifest integrity error without a panic.
func findDaemonPort(m *config.ServerManifest, daemonName string) (int, bool) {
	for _, d := range m.Daemons {
		if d.Name == daemonName {
			return d.Port, true
		}
	}
	return 0, false
}
