package api

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"mcp-local-hub/internal/clients"
)

const (
	lspManifestServerName = "mcp-language-server"
	lspRouterURLTemplate  = "http://127.0.0.1:%d/lsp/%s/mcp"
)

// LSPClientRouterOpts controls the client-config reconcile that points
// eligible present MCP clients at the workspace-agnostic LSP router.
type LSPClientRouterOpts struct {
	// GUIPort is the configured GUI/router port. Zero means read the
	// validated gui_server.port setting through SettingsGet.
	GUIPort int
	// Languages optionally narrows the manifest language set. Empty means
	// every language declared by servers/mcp-language-server/manifest.yaml.
	Languages []string
	// Clients optionally injects the client adapter map for tests. Nil means
	// clients.AllClients().
	Clients map[string]clients.Client
	// BackupKeepN optionally overrides the per-client backup retention count.
	// Zero means EffectiveBackupKeepN().
	BackupKeepN int
	// McphubExePath is used only by relay-shaped clients such as Antigravity.
	// Empty means canonicalMcphubPath().
	McphubExePath string
}

type LSPClientRouterBackup struct {
	Client string
	Path   string
}

type LSPClientRouterChange struct {
	Client    string
	Language  string
	EntryName string
	URL       string
}

type LSPClientRouterFailure struct {
	Client    string
	Language  string
	EntryName string
	Op        string
	Err       string
}

// LSPClientRouterReport summarizes client-config mutations. Registry rows are
// intentionally not included because this reconcile never creates or deletes
// per-(workspace, language) registrations; existing rows are warm
// preregistrations that the /lsp/<lang>/mcp router can reuse.
type LSPClientRouterReport struct {
	Backups  []LSPClientRouterBackup
	Applied  []LSPClientRouterChange
	Removed  []LSPClientRouterChange
	Restored []LSPClientRouterChange
	Failed   []LSPClientRouterFailure
}

type LSPRouterClientStatus struct {
	Client          string
	ConfigPath      string
	Disabled        bool
	ExistingEntries []string
	MissingEntries  []string
}

type lspClientRouterOp struct {
	kind      string
	language  string
	entryName string
	backup    string
	entry     clients.MCPEntry
}

// EnsureLSPRouterClientEntries ensures every effectively-enabled present
// client has one mcp-language-server-<language> entry pointing at the GUI LSP
// router. Effective enablement is the single rule for setup writes:
// (client is in clients.default_install OR its live config has mcphub evidence)
// AND client is not in clients.lsp_router_disabled. Live mcphub evidence means
// an existing manifest-managed entry matching mcphub's install shape or an
// existing hub-owned mcp-language-server-* router/per-project entry.
// Existing per-project entries that point at registry-owned proxy ports are
// migrated away after a per-client backup. The workspace registry is kept
// intact; those rows are harmless warm preregistrations.
func (a *API) EnsureLSPRouterClientEntries(opts LSPClientRouterOpts) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return report, err
	}
	port, err := a.lspRouterGUIPort(opts.GUIPort)
	if err != nil {
		return report, err
	}
	regEntries, err := loadLSPRouterRegistryEntries()
	if err != nil {
		return report, err
	}
	portsByLanguage := lspRegistryPortsByLanguage(regEntries)
	disabledClients, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		return report, err
	}
	enabledClients, err := a.ClientInstallEnabledSet()
	if err != nil {
		return report, err
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	keepN := opts.BackupKeepN
	if keepN == 0 {
		keepN = a.EffectiveBackupKeepN()
	}

	for _, clientName := range sortedLSPClientNames(clientMap) {
		adapter := clientMap[clientName]
		if adapter == nil || !adapter.Exists() || disabledClients[clientName] {
			continue
		}
		if !enabledClients[clientName] {
			hasEvidence, err := clientHasLSPRouterEnablementEvidence(clientName, adapter, languages, regEntries, portsByLanguage)
			if err != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, "", "", "enablement", err))
				continue
			}
			if !hasEvidence {
				continue
			}
		}
		ops := make([]lspClientRouterOp, 0, len(languages))
		for _, language := range languages {
			targetName := LSPRouterEntryName(language)
			targetURL := LSPRouterURL(port, language)
			current, err := adapter.GetEntry(targetName)
			if err != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, language, targetName, "read", err))
				continue
			}
			if !entryMatchesLSPRouter(current, targetURL) {
				if current != nil && !entryIsHubOwnedLSPClientEntry(current, language, portsByLanguage[language]) {
					report.Failed = append(report.Failed, lspFailure(clientName, language, targetName, "ownership",
						errors.New("live entry is not hub-owned; refusing to overwrite")))
					continue
				}
				entry, err := lspRouterMCPEntryForClient(opts, adapter, targetName, targetURL)
				if err != nil {
					report.Failed = append(report.Failed, lspFailure(clientName, language, targetName, "prepare", err))
					continue
				}
				ops = append(ops, lspClientRouterOp{
					kind:      "add",
					language:  language,
					entryName: targetName,
					entry:     entry,
				})
			}

			for _, legacyName := range lspLegacyCandidateEntryNames(regEntries, language, clientName) {
				if legacyName == targetName {
					continue
				}
				legacy, err := adapter.GetEntry(legacyName)
				if err != nil {
					report.Failed = append(report.Failed, lspFailure(clientName, language, legacyName, "read", err))
					continue
				}
				if legacy == nil || !entryPointsAtLegacyLSPPort(legacy, portsByLanguage[language]) {
					continue
				}
				// Leave registry ClientEntries on the legacy name so rollback
				// can reconstruct pre-router entries; GUI reads recognize the
				// shared router name separately for visibility.
				ops = append(ops, lspClientRouterOp{
					kind:      "remove",
					language:  language,
					entryName: legacyName,
				})
			}
		}
		applyLSPRouterOps(opts, adapter, clientName, keepN, ops, report)
	}
	return report, lspRouterReportError(report, "lsp client router wiring")
}

func clientHasLSPRouterEnablementEvidence(
	clientName string,
	adapter clients.Client,
	languages []string,
	regEntries []WorkspaceEntry,
	portsByLanguage map[string]map[int]bool,
) (bool, error) {
	for _, language := range languages {
		targetName := LSPRouterEntryName(language)
		live, err := adapter.GetEntry(targetName)
		if err != nil {
			return false, fmt.Errorf("read %s entry %s: %w", clientName, targetName, err)
		}
		if entryIsHubOwnedLSPClientEntry(live, language, portsByLanguage[language]) {
			return true, nil
		}
		for _, legacyName := range lspLegacyCandidateEntryNames(regEntries, language, clientName) {
			if legacyName == targetName {
				continue
			}
			legacy, err := adapter.GetEntry(legacyName)
			if err != nil {
				return false, fmt.Errorf("read %s entry %s: %w", clientName, legacyName, err)
			}
			if entryPointsAtLegacyLSPPort(legacy, portsByLanguage[language]) {
				return true, nil
			}
		}
	}

	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return false, fmt.Errorf("list manifests for %s enablement evidence: %w", clientName, err)
	}
	for _, server := range names {
		data, err := loadManifestYAMLEmbedFirst(server)
		if err != nil {
			return false, fmt.Errorf("load manifest %s for %s enablement evidence: %w", server, clientName, err)
		}
		m, err := parseManifestForName(server, data)
		if err != nil {
			return false, fmt.Errorf("parse manifest %s for %s enablement evidence: %w", server, clientName, err)
		}
		for _, binding := range m.ClientBindings {
			if strings.TrimSpace(binding.Client) != clientName {
				continue
			}
			live, err := adapter.GetEntry(server)
			if err != nil {
				return false, fmt.Errorf("read %s entry %s: %w", clientName, server, err)
			}
			if live == nil {
				break
			}
			if matched, _ := liveEntryMatchesManifestBinding(live, server, binding, m); matched {
				return true, nil
			}
		}
	}
	return false, nil
}

// RollbackLSPRouterClientEntries reconstructs the pre-router per-workspace
// LSP entries from preserved registry rows, then removes router entries that
// are no longer needed. It deliberately does not select "latest backup": later
// setup or GUI-port operations may have created newer router-containing
// backups, so backup ordering is not a deterministic pre-router marker.
// The current router state is still backed up before any mutation so rollback
// itself remains reversible through the normal backup files.
func (a *API) RollbackLSPRouterClientEntries(opts LSPClientRouterOpts) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return report, err
	}
	regEntries, err := loadLSPRouterRegistryEntries()
	if err != nil {
		return report, err
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	keepN := opts.BackupKeepN
	if keepN == 0 {
		keepN = a.EffectiveBackupKeepN()
	}

	for _, clientName := range sortedLSPClientNames(clientMap) {
		adapter := clientMap[clientName]
		if adapter == nil || !adapter.Exists() {
			continue
		}
		var ops []lspClientRouterOp
		for _, language := range languages {
			routerName := LSPRouterEntryName(language)
			legacyNames := map[string]bool{}
			for _, regEntry := range regEntries {
				if regEntry.Language != language || regEntry.Port <= 0 {
					continue
				}
				entryName := ""
				if regEntry.ClientEntries != nil {
					entryName = strings.TrimSpace(regEntry.ClientEntries[clientName])
				}
				if entryName == "" {
					continue
				}
				legacyNames[entryName] = true
				targetURL := clients.HubLoopbackURL(regEntry.Port, "/mcp")
				live, readErr := adapter.GetEntry(entryName)
				if readErr != nil {
					report.Failed = append(report.Failed, lspFailure(clientName, language, entryName, "read", readErr))
					continue
				}
				if entryMatchesURL(live, targetURL) {
					continue
				}
				if live != nil && !entryIsHubOwnedLSPClientEntry(live, language, map[int]bool{regEntry.Port: true}) {
					report.Failed = append(report.Failed, lspFailure(clientName, language, entryName, "ownership",
						errors.New("live entry is not hub-owned; refusing to overwrite")))
					continue
				}
				if op, ok, blocked := lspRollbackRestoreOpFromBackup(adapter, clientName, language, entryName, targetURL, report); blocked {
					continue
				} else if ok {
					ops = append(ops, op)
					continue
				}
				entry, prepErr := lspLegacyMCPEntryForClient(opts, adapter, entryName, targetURL)
				if prepErr != nil {
					report.Failed = append(report.Failed, lspFailure(clientName, language, entryName, "prepare", prepErr))
					continue
				}
				ops = append(ops, lspClientRouterOp{
					kind:      "add",
					language:  language,
					entryName: entryName,
					entry:     entry,
				})
			}
			live, readErr := adapter.GetEntry(routerName)
			if readErr != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, language, routerName, "read", readErr))
				continue
			}
			if entryIsLSPRouterForLanguage(live, language) && !legacyNames[routerName] {
				ops = append(ops, lspClientRouterOp{
					kind:      "remove",
					language:  language,
					entryName: routerName,
				})
			}
		}
		applyLSPRouterOps(opts, adapter, clientName, keepN, ops, report)
	}
	return report, lspRouterReportError(report, "lsp client router rollback")
}

// RollbackLSPRouterClientEntriesForClient removes this client's shared
// mcp-language-server-<language> router entries without restoring legacy
// per-workspace entries and without touching sibling clients.
func (a *API) RollbackLSPRouterClientEntriesForClient(clientName string, opts LSPClientRouterOpts) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	clientName = strings.TrimSpace(clientName)
	if clientName == "" {
		return report, fmt.Errorf("client name is required")
	}
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return report, err
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	adapter, ok := clientMap[clientName]
	if !ok {
		return report, fmt.Errorf("unknown client %q", clientName)
	}
	if adapter == nil || !adapter.Exists() {
		return report, nil
	}
	keepN := opts.BackupKeepN
	if keepN == 0 {
		keepN = a.EffectiveBackupKeepN()
	}

	ops := make([]lspClientRouterOp, 0, len(languages))
	for _, language := range languages {
		entryName := LSPRouterEntryName(language)
		live, err := adapter.GetEntry(entryName)
		if err != nil {
			report.Failed = append(report.Failed, lspFailure(clientName, language, entryName, "read", err))
			continue
		}
		if live == nil {
			continue
		}
		if !entryIsLSPRouterForLanguage(live, language) {
			report.Failed = append(report.Failed, lspFailure(clientName, language, entryName, "ownership",
				errors.New("live entry is not a hub-owned LSP router entry; refusing to remove")))
			continue
		}
		ops = append(ops, lspClientRouterOp{
			kind:      "remove",
			language:  language,
			entryName: entryName,
		})
	}
	applyLSPRouterOps(opts, adapter, clientName, keepN, ops, report)
	return report, lspRouterReportError(report, "lsp client router per-client rollback")
}

// LSPRouterClientStatuses reports the current shared-router entry presence for
// every present client config. It is read-only and shares the same language
// manifest, disabled-list, adapter map, and router-entry detector as ensure.
func (a *API) LSPRouterClientStatuses(opts LSPClientRouterOpts) ([]LSPRouterClientStatus, error) {
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return nil, err
	}
	disabledClients, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		return nil, err
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}

	statuses := make([]LSPRouterClientStatus, 0, len(clientMap))
	for _, clientName := range sortedLSPClientNames(clientMap) {
		adapter := clientMap[clientName]
		if adapter == nil || !adapter.Exists() {
			continue
		}
		status := LSPRouterClientStatus{
			Client:     clientName,
			ConfigPath: adapter.ConfigPath(),
			Disabled:   disabledClients[clientName],
		}
		for _, language := range languages {
			entryName := LSPRouterEntryName(language)
			live, err := adapter.GetEntry(entryName)
			if err != nil {
				return statuses, fmt.Errorf("read %s entry %s: %w", clientName, entryName, err)
			}
			if entryIsLSPRouterForLanguage(live, language) {
				status.ExistingEntries = append(status.ExistingEntries, entryName)
				continue
			}
			status.MissingEntries = append(status.MissingEntries, entryName)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// LSPRouterEntryName is the canonical client-config entry name for one
// manifest language.
func LSPRouterEntryName(language string) string {
	return lspManifestServerName + "-" + language
}

// LSPRouterURL is the canonical GUI-router URL written into client configs.
func LSPRouterURL(guiPort int, language string) string {
	return fmt.Sprintf(lspRouterURLTemplate, guiPort, language)
}

func (a *API) lspRouterGUIPort(port int) (int, error) {
	if port == 0 {
		setting, err := a.SettingsGet("gui_server.port")
		if err != nil {
			return 0, fmt.Errorf("read gui_server.port: %w", err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(setting))
		if err != nil || n < 1024 || n > 65535 {
			return 0, fmt.Errorf("gui_server.port resolved to invalid value %q", setting)
		}
		port = n
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("GUI port %d is outside 1..65535", port)
	}
	return port, nil
}

func loadLSPRouterLanguages(requested []string) ([]string, error) {
	data, err := loadManifestYAMLEmbedFirst(lspManifestServerName)
	if err != nil {
		return nil, fmt.Errorf("load manifest %s: %w", lspManifestServerName, err)
	}
	m, err := parseManifestForName(lspManifestServerName, data)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	all := make([]string, 0, len(m.Languages))
	for _, spec := range m.Languages {
		if spec.Name == "" {
			continue
		}
		known[spec.Name] = true
		all = append(all, spec.Name)
	}
	if len(requested) == 0 {
		return all, nil
	}
	out := make([]string, 0, len(requested))
	seen := map[string]bool{}
	for _, language := range requested {
		language = strings.TrimSpace(language)
		if language == "" || seen[language] {
			continue
		}
		if !known[language] {
			return nil, fmt.Errorf("unknown LSP language %q (manifest %s supports: %v)", language, lspManifestServerName, all)
		}
		seen[language] = true
		out = append(out, language)
	}
	return out, nil
}

func loadLSPRouterRegistryEntries() ([]WorkspaceEntry, error) {
	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return nil, err
	}
	return reg.LSPEntries(), nil
}

func lspRegistryPortsByLanguage(entries []WorkspaceEntry) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, entry := range entries {
		if entry.Language == "" || entry.Port <= 0 {
			continue
		}
		if out[entry.Language] == nil {
			out[entry.Language] = map[int]bool{}
		}
		out[entry.Language][entry.Port] = true
	}
	return out
}

func lspLegacyCandidateEntryNames(entries []WorkspaceEntry, language, clientName string) []string {
	var names []string
	base := LSPRouterEntryName(language)
	for _, entry := range entries {
		if entry.Language != language {
			continue
		}
		if entry.ClientEntries != nil {
			if name := strings.TrimSpace(entry.ClientEntries[clientName]); name != "" {
				names = append(names, name)
			}
		}
		if entry.WorkspaceKey != "" {
			short := entry.WorkspaceKey
			if len(short) > 4 {
				short = short[:4]
			}
			names = append(names, base+"-"+short, base+"-"+entry.WorkspaceKey)
		}
	}
	return uniqueSortedStrings(names)
}

func lspRouterMCPEntryForClient(opts LSPClientRouterOpts, adapter clients.Client, name, targetURL string) (clients.MCPEntry, error) {
	entry := clients.MCPEntry{
		Name:     name,
		URL:      targetURL,
		RelayURL: targetURL,
	}
	// Relay-stdio adapters (antigravity, zed) need RelayExePath so AddEntry
	// can emit the `command`+`args` stdio-bridge entry. Both forward to
	// RelayURL (already set above to targetURL), so the relay takes its
	// --url branch and needs no RelayServer/RelayDaemon here. URL-native
	// HTTP adapters consume the URL directly and need no relay context.
	if !adapter.IsRelayStdio() {
		return entry, nil
	}
	relayExe := opts.McphubExePath
	if relayExe == "" {
		var err error
		relayExe, err = canonicalMcphubPath()
		if err != nil {
			return clients.MCPEntry{}, err
		}
	}
	entry.RelayExePath = relayExe
	return entry, nil
}

func lspLegacyMCPEntryForClient(opts LSPClientRouterOpts, adapter clients.Client, name, targetURL string) (clients.MCPEntry, error) {
	entry := clients.MCPEntry{
		Name: name,
		URL:  targetURL,
	}
	// Relay-stdio adapters (antigravity, zed) need RelayExePath + RelayURL
	// so AddEntry emits the stdio-bridge `command`+`args` entry forwarding
	// to targetURL via the relay --url branch (no RelayServer/RelayDaemon
	// needed). URL-native HTTP adapters consume URL directly.
	if !adapter.IsRelayStdio() {
		return entry, nil
	}
	relayExe := opts.McphubExePath
	if relayExe == "" {
		var err error
		relayExe, err = canonicalMcphubPath()
		if err != nil {
			return clients.MCPEntry{}, err
		}
	}
	entry.RelayURL = targetURL
	entry.RelayExePath = relayExe
	return entry, nil
}

func entryMatchesLSPRouter(entry *clients.MCPEntry, targetURL string) bool {
	return entryMatchesURL(entry, targetURL)
}

func entryMatchesURL(entry *clients.MCPEntry, targetURL string) bool {
	if entry == nil {
		return false
	}
	return entry.URL == targetURL || entry.RelayURL == targetURL
}

func entryIsHubOwnedLSPClientEntry(entry *clients.MCPEntry, language string, ports map[int]bool) bool {
	return entryIsLSPRouterForLanguage(entry, language) || entryPointsAtLegacyLSPPort(entry, ports)
}

func entryIsLSPRouterForLanguage(entry *clients.MCPEntry, language string) bool {
	if entry == nil {
		return false
	}
	for _, raw := range []string{entry.URL, entry.RelayURL} {
		parsedLanguage, ok := lspRouterURLLanguage(raw)
		if ok && parsedLanguage == language {
			return true
		}
	}
	return false
}

func lspRouterURLLanguage(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "lsp" || parts[2] != "mcp" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func entryPointsAtLegacyLSPPort(entry *clients.MCPEntry, ports map[int]bool) bool {
	if entry == nil || len(ports) == 0 {
		return false
	}
	for _, raw := range []string{entry.URL, entry.RelayURL} {
		port, ok := lspLegacyURLPort(raw)
		if ok && ports[port] {
			return true
		}
	}
	return false
}

func lspLegacyURLPort(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Path != "/mcp" {
		return 0, false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return 0, false
	}
	portText := parsed.Port()
	if portText == "" {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, false
	}
	return port, true
}

func lspRollbackRestoreOpFromBackup(
	adapter clients.Client,
	clientName, language, entryName, targetURL string,
	report *LSPClientRouterReport,
) (lspClientRouterOp, bool, bool) {
	backupPath, ok, err := adapter.LatestBackupPath()
	if err != nil {
		report.Failed = append(report.Failed, lspFailure(clientName, language, entryName, "backup-read", err))
		return lspClientRouterOp{}, false, true
	}
	if !ok {
		return lspClientRouterOp{}, false, false
	}
	has, err := adapter.BackupContainsEntry(backupPath, entryName)
	if err != nil {
		report.Failed = append(report.Failed, lspFailure(clientName, language, entryName, "backup-read", err))
		return lspClientRouterOp{}, false, true
	}
	if !has {
		return lspClientRouterOp{}, false, false
	}
	return lspClientRouterOp{
		kind:      "restore",
		language:  language,
		entryName: entryName,
		backup:    backupPath,
		entry:     clients.MCPEntry{Name: entryName, URL: targetURL},
	}, true, false
}

func applyLSPRouterOps(opts LSPClientRouterOpts, adapter clients.Client, clientName string, keepN int, ops []lspClientRouterOp, report *LSPClientRouterReport) {
	if len(ops) == 0 {
		return
	}
	backupPath, err := adapter.BackupKeep(keepN)
	if err != nil {
		report.Failed = append(report.Failed, lspFailure(clientName, "", "", "backup", err))
		return
	}
	report.Backups = append(report.Backups, LSPClientRouterBackup{Client: clientName, Path: backupPath})
	addFailedByLanguage := map[string]bool{}
	for _, op := range ops {
		switch op.kind {
		case "add":
			if err := adapter.AddEntry(op.entry); err != nil {
				addFailedByLanguage[op.language] = true
				report.Failed = append(report.Failed, lspFailure(clientName, op.language, op.entryName, "add", err))
				continue
			}
			report.Applied = append(report.Applied, LSPClientRouterChange{
				Client: clientName, Language: op.language, EntryName: op.entryName, URL: op.entry.URL,
			})
		case "remove":
			if addFailedByLanguage[op.language] {
				continue
			}
			if err := adapter.RemoveEntry(op.entryName); err != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, op.language, op.entryName, "remove", err))
				continue
			}
			report.Removed = append(report.Removed, LSPClientRouterChange{
				Client: clientName, Language: op.language, EntryName: op.entryName,
			})
		case "restore":
			if err := adapter.RestoreEntryFromBackupForRollback(op.backup, op.entryName); err != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, op.language, op.entryName, "restore", err))
				continue
			}
			restored, err := adapter.GetEntry(op.entryName)
			if err != nil {
				report.Failed = append(report.Failed, lspFailure(clientName, op.language, op.entryName, "read", err))
				continue
			}
			if entryIsLSPRouterForLanguage(restored, op.language) {
				fallback, prepErr := lspLegacyMCPEntryForClient(opts, adapter, op.entryName, op.entry.URL)
				if prepErr != nil {
					report.Failed = append(report.Failed, lspFailure(clientName, op.language, op.entryName, "prepare", prepErr))
					continue
				}
				if err := adapter.AddEntry(fallback); err != nil {
					report.Failed = append(report.Failed, lspFailure(clientName, op.language, op.entryName, "add", err))
					continue
				}
				report.Applied = append(report.Applied, LSPClientRouterChange{
					Client: clientName, Language: op.language, EntryName: op.entryName, URL: fallback.URL,
				})
				continue
			}
			report.Restored = append(report.Restored, LSPClientRouterChange{
				Client: clientName, Language: op.language, EntryName: op.entryName,
			})
		}
	}
}

func lspFailure(client, language, entryName, op string, err error) LSPClientRouterFailure {
	return LSPClientRouterFailure{
		Client:    client,
		Language:  language,
		EntryName: entryName,
		Op:        op,
		Err:       err.Error(),
	}
}

func lspRouterReportError(report *LSPClientRouterReport, label string) error {
	if report == nil || len(report.Failed) == 0 {
		return nil
	}
	return fmt.Errorf("%s failed for %d operation(s)", label, len(report.Failed))
}

func sortedLSPClientNames(clientMap map[string]clients.Client) []string {
	names := make([]string, 0, len(clientMap))
	for name := range clientMap {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
