package api

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mcp-local-hub/internal/clients"
)

const (
	lspManifestServerName    = "mcp-language-server"
	lspRouterURLPathTemplate = "/lsp/%s/mcp"
	lspRouterURLTemplate     = "http://127.0.0.1:%d" + lspRouterURLPathTemplate
)

// LSPClientRouterOpts controls the client-config reconcile that points
// eligible present MCP clients at the workspace-agnostic LSP router.
type LSPClientRouterOpts struct {
	// GUIPort is the port written into LSP router client URLs. Zero means
	// read the validated gui_server.port setting through SettingsGet.
	// Despite the field name it is a plain port value, not GUI-specific:
	// sub-increment 2a's `mcphub install --reconcile-mcp-front` command
	// passes the settings-owned mcp_front.port here explicitly (a non-zero
	// value skips the gui_server.port read entirely), pointing LSP router
	// client URLs at the supervisor-managed front daemon instead of the
	// GUI. Every existing caller keeps passing 0 or the live GUI port, so
	// this is purely an additive use of an already-generic field.
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
	// ForceClientName bypasses inferred enablement evidence for one explicitly
	// requested client. A persisted lsp_router_disabled opt-out still wins.
	ForceClientName string
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
	Skipped  []LSPClientRouterChange
	Failed   []LSPClientRouterFailure

	// Pending is written ONLY by RestoreLSPRouterClientEntriesSnapshot. It
	// carries recovery rows that are neither restored nor failed but
	// UNREACHABLE right now: a client that was present when the pre-state was
	// captured but whose adapter or config file is absent at restore time.
	//
	// It is deliberately distinct from both siblings. Skipped means "we
	// decided not to touch this, and that decision is final"; Failed means "we
	// tried and the attempt errored". Pending means "this row still needs
	// restoring and we could not even attempt it" — the caller must NOT retire
	// the recovery record while any Pending row remains, or the row is lost
	// forever the moment the client reappears. Treating an unreachable client
	// as restored is precisely how a rollback record gets deleted while the
	// client it describes is still on the new endpoint.
	Pending []LSPClientRouterChange
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

// LSPRouterPlannedOperation is one exact, immutable mutation captured before a
// front-reconcile generation writes any client configuration.
type LSPRouterPlannedOperation struct {
	Client        string                 `json:"client"`
	Language      string                 `json:"language"`
	EntryName     string                 `json:"entry_name"`
	Operation     string                 `json:"operation"`
	PreState      LSPRouterEntrySnapshot `json:"pre_state"`
	IntendedState LSPRouterEntrySnapshot `json:"intended_state"`
	entry         clients.MCPEntry
}

// LSPRouterClientPlan freezes the client population and exact entry states for
// one operation. The adapter map is deliberately private and never persisted;
// applying a plan can only use the same concrete population captured here.
type LSPRouterClientPlan struct {
	Port            int                         `json:"port"`
	Operations      []LSPRouterPlannedOperation `json:"operations"`
	CaptureFailures []LSPClientRouterFailure    `json:"capture_failures,omitempty"`
	clientMap       map[string]clients.Client
	keepN           int
	opts            LSPClientRouterOpts
}

type LSPRouterMutationObservation struct {
	Operation            LSPRouterPlannedOperation
	ObservedState        LSPRouterEntrySnapshot
	Invoked              bool
	PreconditionConflict bool
	PreparationErr       error
	AdapterErr           error
	ObservationErr       error
}

type LSPRouterApplyCallbacks struct {
	OnPrepared             func(LSPRouterPlannedOperation) error
	OnFinished             func(LSPRouterMutationObservation) error
	OnPreconditionConflict func(LSPRouterMutationObservation) error
}

var ErrLSPRouterPlanPreconditionConflict = errors.New("lsp router plan precondition no longer matches")

// PlanLSPRouterClientEntries captures one complete eligible client population,
// its exact canonical/legacy pre-state, and every operation the forward pass
// may perform. It calls clients.AllClients at most once; application never
// enumerates or re-admits clients.
func (a *API) PlanLSPRouterClientEntries(opts LSPClientRouterOpts) (*LSPRouterClientPlan, error) {
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return nil, err
	}
	port, err := a.lspRouterGUIPort(opts.GUIPort)
	if err != nil {
		return nil, err
	}
	opts.GUIPort = port
	regEntries, err := loadLSPRouterRegistryEntries()
	if err != nil {
		return nil, err
	}
	portsByLanguage := lspRegistryPortsByLanguage(regEntries)
	disabledClients, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		return nil, err
	}
	enabledClients, err := a.ClientInstallEnabledSet()
	if err != nil {
		return nil, err
	}
	keepN := opts.BackupKeepN
	if keepN == 0 {
		keepN = a.EffectiveBackupKeepN()
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}
	capturedClients := make(map[string]clients.Client, len(clientMap))
	for clientName, adapter := range clientMap {
		capturedClients[clientName] = adapter
	}
	plan := &LSPRouterClientPlan{
		Port:      port,
		clientMap: capturedClients,
		keepN:     keepN,
		opts:      opts,
	}
	forceClientName := strings.TrimSpace(opts.ForceClientName)
	for _, clientName := range sortedLSPClientNames(capturedClients) {
		adapter := capturedClients[clientName]
		if adapter == nil || !adapter.Exists() || disabledClients[clientName] {
			continue
		}
		if !enabledClients[clientName] && clientName != forceClientName {
			hasEvidence, evidenceErr := clientHasLSPRouterEnablementEvidence(
				clientName, adapter, languages, port, regEntries, portsByLanguage)
			if evidenceErr != nil {
				plan.CaptureFailures = append(plan.CaptureFailures, lspFailure(clientName, "", "", "enablement", evidenceErr))
				continue
			}
			if !hasEvidence {
				continue
			}
		}
		for _, language := range languages {
			targetName := LSPRouterEntryName(language)
			targetURL := LSPRouterURL(port, language)
			current, readErr := adapter.GetEntry(targetName)
			if readErr != nil {
				plan.CaptureFailures = append(plan.CaptureFailures, lspFailure(clientName, language, targetName, "read", readErr))
				continue
			}
			canonicalPre := lspSnapshotFromEntry(clientName, language, targetName, current)
			if !entryMatchesLSPRouter(current, targetURL) {
				if current != nil && !entryIsHubOwnedLSPClientEntry(targetName, current, language, port, portsByLanguage[language]) {
					plan.CaptureFailures = append(plan.CaptureFailures, lspFailure(clientName, language, targetName, "ownership",
						errors.New("live entry is not hub-owned; refusing to overwrite")))
					continue
				}
				entry, prepErr := lspRouterMCPEntryForClient(opts, adapter, targetName, targetURL)
				if prepErr != nil {
					plan.CaptureFailures = append(plan.CaptureFailures, lspFailure(clientName, language, targetName, "prepare", prepErr))
					continue
				}
				// IntendedState must be the expected READBACK, not a projection
				// of the write request: PreState and ObservedState are both
				// readbacks, so projecting the command object here would make
				// every successful add compare unequal to its own readback and
				// settle as `forward-ownership-unknown`. The family rule is
				// owned by intendedEntryReadbackProjection.
				intendedReadback := intendedEntryReadbackProjection(adapter, entry)
				plan.Operations = append(plan.Operations, LSPRouterPlannedOperation{
					Client: clientName, Language: language, EntryName: targetName, Operation: "add",
					PreState:      canonicalPre,
					IntendedState: lspSnapshotFromEntry(clientName, language, targetName, &intendedReadback),
					entry:         entry,
				})
			}
			legacyEntries, legacyReadErrs := collectLegacyLSPEntriesToMigrate(
				adapter, regEntries, portsByLanguage[language], language, clientName, targetName)
			if len(legacyReadErrs) > 0 {
				for _, legacyReadErr := range legacyReadErrs {
					plan.CaptureFailures = append(plan.CaptureFailures, lspFailure(clientName, language, legacyReadErr.Name, "read", legacyReadErr.Err))
				}
				continue
			}
			for _, legacy := range legacyEntries {
				plan.Operations = append(plan.Operations, LSPRouterPlannedOperation{
					Client: clientName, Language: language, EntryName: legacy.Name, Operation: "remove",
					PreState:      lspSnapshotFromEntry(clientName, language, legacy.Name, legacy.Entry),
					IntendedState: LSPRouterEntrySnapshot{Client: clientName, Language: language, EntryName: legacy.Name},
				})
			}
		}
	}
	return plan, nil
}

// ApplyLSPRouterClientPlan applies only the frozen operations and adapters in
// plan. Every write is preceded by an exact pre-state check and a durable
// OnPrepared callback; OnFinished is total over adapter success and error.
// A pre-state mismatch invokes the separate OnPreconditionConflict callback
// because no mutation attempt was prepared or made.
func (a *API) ApplyLSPRouterClientPlan(plan *LSPRouterClientPlan, callbacks LSPRouterApplyCallbacks) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	if plan == nil {
		return report, errors.New("nil lsp router client plan")
	}
	report.Failed = append(report.Failed, plan.CaptureFailures...)
	backedUp := map[string]bool{}
	canonicalReady := map[string]bool{}
	for _, op := range plan.Operations {
		group := op.Client + "\x00" + op.Language
		if _, ok := canonicalReady[group]; !ok {
			canonicalReady[group] = true
		}
		if op.Operation == "remove" && !canonicalReady[group] {
			continue
		}
		adapter := plan.clientMap[op.Client]
		if adapter == nil {
			report.Failed = append(report.Failed, lspFailure(op.Client, op.Language, op.EntryName, "unavailable",
				errors.New("planned adapter is unavailable")))
			if op.Operation == "add" {
				canonicalReady[group] = false
			}
			continue
		}
		mutator, ok := adapter.(clients.ConditionalEntryMutator)
		if !ok {
			capabilityErr := errors.New("planned adapter lacks conditional entry mutation capability")
			report.Failed = append(report.Failed, lspFailure(op.Client, op.Language, op.EntryName, "capability", capabilityErr))
			if op.Operation == "add" {
				canonicalReady[group] = false
			}
			continue
		}
		var backupKeepN *int
		if !backedUp[op.Client] {
			backupKeepN = &plan.keepN
		}
		var prepareCallbackErr error
		mutation := clients.EntryMutationOperation(op.Operation)
		observed := mutator.ConditionalEntryMutation(clients.ConditionalEntryMutationRequest{
			EntryName: op.EntryName,
			ExpectedLive: func(live *clients.MCPEntry) bool {
				return lspSnapshotStateEqual(
					lspSnapshotFromEntry(op.Client, op.Language, op.EntryName, live),
					op.PreState,
				)
			},
			BackupKeepN: backupKeepN,
			Operation:   mutation,
			Entry:       op.entry,
			BeforeMutation: func(clients.EntryMutationPreparation) error {
				if callbacks.OnPrepared != nil {
					prepareCallbackErr = callbacks.OnPrepared(op)
				}
				return prepareCallbackErr
			},
		})
		observation := LSPRouterMutationObservation{
			Operation:            op,
			Invoked:              observed.Invoked,
			PreconditionConflict: observed.PreconditionConflict,
			PreparationErr:       observed.PreparationErr,
			AdapterErr:           observed.MutationErr,
			ObservationErr:       observed.ObservationErr,
			ObservedState:        lspSnapshotFromEntry(op.Client, op.Language, op.EntryName, observed.After),
		}
		if observed.BackupPath != "" {
			report.Backups = append(report.Backups, LSPClientRouterBackup{Client: op.Client, Path: observed.BackupPath})
			backedUp[op.Client] = true
		}
		if observed.PreconditionConflict {
			observation.AdapterErr = ErrLSPRouterPlanPreconditionConflict
			observation = LSPRouterMutationObservation{
				Operation: op, ObservedState: lspSnapshotFromEntry(op.Client, op.Language, op.EntryName, observed.Before),
				Invoked: false, PreconditionConflict: true, PreparationErr: observed.PreparationErr,
				AdapterErr: ErrLSPRouterPlanPreconditionConflict,
			}
			if callbacks.OnPreconditionConflict != nil {
				if callbackErr := callbacks.OnPreconditionConflict(observation); callbackErr != nil {
					return report, callbackErr
				}
			}
			report.Failed = append(report.Failed, lspFailure(op.Client, op.Language, op.EntryName, "precondition",
				ErrLSPRouterPlanPreconditionConflict))
			if op.Operation == "add" {
				canonicalReady[group] = false
			}
			continue
		}
		if prepareCallbackErr != nil {
			return report, prepareCallbackErr
		}
		if observed.PreparationErr != nil {
			report.Failed = append(report.Failed, lspFailure(op.Client, op.Language, op.EntryName, "prepare", observed.PreparationErr))
			if op.Operation == "add" {
				canonicalReady[group] = false
			}
			continue
		}
		if callbacks.OnFinished != nil {
			if callbackErr := callbacks.OnFinished(observation); callbackErr != nil {
				return report, callbackErr
			}
		}
		if observed.ObservationErr != nil {
			report.Failed = append(report.Failed, lspFailure(op.Client, op.Language, op.EntryName, "readback", observed.ObservationErr))
			if op.Operation == "add" {
				canonicalReady[group] = false
			}
			if callbacks.OnFinished != nil {
				return report, fmt.Errorf("lsp router plan readback %s/%s/%s: %w", op.Client, op.Language, op.EntryName, observed.ObservationErr)
			}
			continue
		}
		if !lspSnapshotStateEqual(observation.ObservedState, op.IntendedState) {
			err := observed.MutationErr
			if err == nil {
				err = errors.New("post-write state differs from intended state")
			}
			report.Failed = append(report.Failed, lspFailure(op.Client, op.Language, op.EntryName, op.Operation, err))
			if op.Operation == "add" {
				canonicalReady[group] = false
			}
			if callbacks.OnFinished != nil {
				return report, fmt.Errorf("lsp router plan ownership unknown for %s/%s/%s", op.Client, op.Language, op.EntryName)
			}
			continue
		}
		if op.Operation == "add" {
			canonicalReady[group] = true
			report.Applied = append(report.Applied, LSPClientRouterChange{
				Client: op.Client, Language: op.Language, EntryName: op.EntryName, URL: snapshotPriorURL(op.IntendedState),
			})
		} else {
			report.Removed = append(report.Removed, LSPClientRouterChange{
				Client: op.Client, Language: op.Language, EntryName: op.EntryName,
			})
		}
	}
	return report, lspRouterReportError(report, "lsp client router plan")
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
	plan, err := a.PlanLSPRouterClientEntries(opts)
	if err != nil {
		return &LSPClientRouterReport{}, err
	}
	return a.ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{})
}

func (a *API) ensureLSPRouterClientEntriesWithState(
	opts LSPClientRouterOpts,
	disabledClients map[string]bool,
	enabledClients map[string]bool,
	keepN int,
) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return report, err
	}
	port, err := resolvedLSPRouterGUIPort(opts.GUIPort)
	if err != nil {
		return report, err
	}
	regEntries, err := loadLSPRouterRegistryEntries()
	if err != nil {
		return report, err
	}
	portsByLanguage := lspRegistryPortsByLanguage(regEntries)
	return a.ensureLSPRouterClientEntriesWithLoaded(opts, languages, port, regEntries, portsByLanguage, disabledClients, enabledClients, keepN)
}

func (a *API) ensureLSPRouterClientEntriesWithLoaded(
	opts LSPClientRouterOpts,
	languages []string,
	port int,
	regEntries []WorkspaceEntry,
	portsByLanguage map[string]map[int]bool,
	disabledClients map[string]bool,
	enabledClients map[string]bool,
	keepN int,
) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	forceClientName := strings.TrimSpace(opts.ForceClientName)
	if keepN == 0 {
		keepN = registryDefaultKeepN()
	}
	clientMap := opts.Clients
	if clientMap == nil {
		clientMap = clients.AllClients()
	}

	for _, clientName := range sortedLSPClientNames(clientMap) {
		adapter := clientMap[clientName]
		if adapter == nil || !adapter.Exists() || disabledClients[clientName] {
			continue
		}
		if !enabledClients[clientName] && clientName != forceClientName {
			hasEvidence, err := clientHasLSPRouterEnablementEvidence(clientName, adapter, languages, port, regEntries, portsByLanguage)
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
				if current != nil && !entryIsHubOwnedLSPClientEntry(targetName, current, language, port, portsByLanguage[language]) {
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

			legacyEntries, legacyReadErrs := collectLegacyLSPEntriesToMigrate(
				adapter, regEntries, portsByLanguage[language], language, clientName, targetName)
			for _, readErr := range legacyReadErrs {
				report.Failed = append(report.Failed, lspFailure(clientName, language, readErr.Name, "read", readErr.Err))
			}
			for _, legacy := range legacyEntries {
				// Leave registry ClientEntries on the legacy name so rollback
				// can reconstruct pre-router entries; GUI reads recognize the
				// shared router name separately for visibility.
				ops = append(ops, lspClientRouterOp{
					kind:      "remove",
					language:  language,
					entryName: legacy.Name,
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
	guiPort int,
	regEntries []WorkspaceEntry,
	portsByLanguage map[string]map[int]bool,
) (bool, error) {
	aggregate, err := adapter.GetEntry(hubReconcileAggregateEntryName)
	if err != nil {
		return false, fmt.Errorf("read %s entry %s: %w", clientName, hubReconcileAggregateEntryName, err)
	}
	if activeHubAggregateEntry(aggregate, clientName) {
		return true, nil
	}

	for _, language := range languages {
		targetName := LSPRouterEntryName(language)
		live, err := adapter.GetEntry(targetName)
		if err != nil {
			return false, fmt.Errorf("read %s entry %s: %w", clientName, targetName, err)
		}
		if activeHubOwnedLSPClientEntry(targetName, live, language, guiPort, portsByLanguage[language]) {
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
			if activeEntryPointsAtLegacyLSPPort(legacy, portsByLanguage[language]) {
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
			if live.Disabled {
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
	port, err := a.lspRouterGUIPort(opts.GUIPort)
	if err != nil {
		return report, err
	}
	opts.GUIPort = port
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
				if live != nil && !entryIsHubOwnedLSPClientEntry(entryName, live, language, port, map[int]bool{regEntry.Port: true}) {
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
			owned, lspLike := entryIsOwnedLSPRouterForLanguage(routerName, live, language, port)
			if owned && !legacyNames[routerName] {
				ops = append(ops, lspClientRouterOp{
					kind:      "remove",
					language:  language,
					entryName: routerName,
				})
			} else if lspLike && !legacyNames[routerName] {
				report.Skipped = append(report.Skipped, LSPClientRouterChange{
					Client: clientName, Language: language, EntryName: routerName, URL: entryLSPRouterURL(live),
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
	keepN := opts.BackupKeepN
	if keepN == 0 {
		keepN = a.EffectiveBackupKeepN()
	}
	port, err := a.lspRouterGUIPort(opts.GUIPort)
	if err != nil {
		return report, err
	}
	opts.GUIPort = port
	return a.rollbackLSPRouterClientEntriesForClientWithKeepN(clientName, opts, keepN)
}

func (a *API) rollbackLSPRouterClientEntriesForClientWithKeepN(clientName string, opts LSPClientRouterOpts, keepN int) (*LSPClientRouterReport, error) {
	report := &LSPClientRouterReport{}
	clientName = strings.TrimSpace(clientName)
	if clientName == "" {
		return report, fmt.Errorf("client name is required")
	}
	languages, err := loadLSPRouterLanguages(opts.Languages)
	if err != nil {
		return report, err
	}
	port, err := resolvedLSPRouterGUIPort(opts.GUIPort)
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
	if keepN == 0 {
		keepN = registryDefaultKeepN()
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
		owned, lspLike := entryIsOwnedLSPRouterForLanguage(entryName, live, language, port)
		if !owned {
			if lspLike {
				report.Skipped = append(report.Skipped, LSPClientRouterChange{
					Client: clientName, Language: language, EntryName: entryName, URL: entryLSPRouterURL(live),
				})
				continue
			}
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
	port, err := a.lspRouterGUIPort(opts.GUIPort)
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
			if owned, _ := entryIsOwnedLSPRouterForLanguage(entryName, live, language, port); owned {
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
	return resolvedLSPRouterGUIPort(port)
}

func resolvedLSPRouterGUIPort(port int) (int, error) {
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
	if entry == nil || entry.Disabled {
		return false
	}
	if entry.URL == targetURL {
		return true
	}
	return entry.RelayURL == targetURL && isCurrentMcphubRelayBinary(entry.RelayExePath)
}

// lspLegacyLiveEntry is one legacy per-workspace LSP entry that is present in
// a client's config right now AND still points at a registry-owned proxy port.
type lspLegacyLiveEntry struct {
	Name  string
	Entry *clients.MCPEntry
}

// lspLegacyEntryReadError is a per-candidate read failure, surfaced to the
// caller rather than swallowed so each call site keeps its own error polarity
// (the forward pass records a per-entry Failed row and continues; the snapshot
// fails the whole capture closed).
type lspLegacyEntryReadError struct {
	Name string
	Err  error
}

// collectLegacyLSPEntriesToMigrate is the SINGLE OWNER of the question "which
// legacy per-workspace entries does the router reconcile migrate away for this
// (client, language)".
//
// It exists because two sides must agree on that answer and used to derive it
// independently — with only one of them actually implementing it (codex bot PR
// #588). ensureLSPRouterClientEntriesWithLoaded DELETES every entry this
// returns, while SnapshotLSPRouterClientEntries captured only the canonical
// `mcp-language-server-<language>` row. A client still on registry-backed
// per-workspace entries therefore had its real pre-state silently outside the
// recovery record: the forward pass removed those entries and `--rollback`,
// iterating the snapshot, could not put back what was never captured. The host
// ends up worse than before the "reversible" cutover — the failure class the
// whole recovery-record lifecycle exists to prevent, reached through a client
// shape the record did not model.
//
// Routing both the mutation and the capture through this one function makes
// the two surfaces equal BY CONSTRUCTION: a future change to which entries are
// migrated away automatically changes what the snapshot preserves, so they
// cannot drift apart again.
//
// A candidate that equals targetName is skipped: that is the canonical entry,
// which the forward pass rewrites (never deletes) and the snapshot captures on
// its own canonical row.
func collectLegacyLSPEntriesToMigrate(
	adapter clients.Client,
	regEntries []WorkspaceEntry,
	legacyPorts map[int]bool,
	language, clientName, targetName string,
) ([]lspLegacyLiveEntry, []lspLegacyEntryReadError) {
	var found []lspLegacyLiveEntry
	var readErrs []lspLegacyEntryReadError
	for _, legacyName := range lspLegacyCandidateEntryNames(regEntries, language, clientName) {
		if legacyName == targetName {
			continue
		}
		legacy, err := adapter.GetEntry(legacyName)
		if err != nil {
			readErrs = append(readErrs, lspLegacyEntryReadError{Name: legacyName, Err: err})
			continue
		}
		if legacy == nil || !entryPointsAtLegacyLSPPort(legacy, legacyPorts) {
			continue
		}
		// A multi-layer adapter's merged read can surface an entry that is NOT
		// in the write target (mimocode: the operator's config.json below it, or
		// the ~/.claude.json import). RemoveEntry only ever deletes the write
		// target's own key, so such an entry is not this reconcile's to migrate
		// away: planning it would emit an operation that cannot take effect and
		// record a pre-state whose inverse the rollback cannot honour. Skipping
		// it here keeps this function's whole reason for existing intact — the
		// captured surface equals the MUTATED surface by construction.
		if legacy.SourceBelowWriteTarget {
			continue
		}
		found = append(found, lspLegacyLiveEntry{Name: legacyName, Entry: legacy})
	}
	return found, readErrs
}

func entryMatchesURL(entry *clients.MCPEntry, targetURL string) bool {
	if entry == nil {
		return false
	}
	return entry.URL == targetURL || entry.RelayURL == targetURL
}

func entryIsHubOwnedLSPClientEntry(entryName string, entry *clients.MCPEntry, language string, guiPort int, ports map[int]bool) bool {
	owned, _ := entryIsOwnedLSPRouterForLanguage(entryName, entry, language, guiPort)
	return owned || entryPointsAtLegacyLSPPort(entry, ports)
}

func activeHubOwnedLSPClientEntry(entryName string, entry *clients.MCPEntry, language string, guiPort int, ports map[int]bool) bool {
	return entry != nil && !entry.Disabled && entryIsHubOwnedLSPClientEntry(entryName, entry, language, guiPort, ports)
}

func entryIsLSPRouterForLanguage(entry *clients.MCPEntry, language string) bool {
	owned, _ := entryIsOwnedLSPRouterForLanguage(LSPRouterEntryName(language), entry, language, 0)
	return owned
}

func entryIsOwnedLSPRouterForLanguage(entryName string, entry *clients.MCPEntry, language string, guiPort int) (owned bool, lspLike bool) {
	if entry == nil {
		return false, false
	}
	reservedName := entryName == LSPRouterEntryName(language)
	for _, candidate := range []struct {
		raw   string
		relay bool
	}{
		{raw: entry.URL},
		{raw: entry.RelayURL, relay: true},
	} {
		parsedLanguage, parsedPort, ok := lspRouterURLLanguagePort(candidate.raw)
		if !ok || parsedLanguage != language {
			continue
		}
		lspLike = true
		if candidate.relay && !isCurrentMcphubRelayBinary(entry.RelayExePath) {
			continue
		}
		if reservedName || (guiPort > 0 && parsedPort == guiPort) {
			owned = true
		}
	}
	return owned, lspLike
}

func entryLSPRouterURL(entry *clients.MCPEntry) string {
	if entry == nil {
		return ""
	}
	if entry.URL != "" {
		return entry.URL
	}
	return entry.RelayURL
}

func isCurrentMcphubRelayBinary(cmd string) bool {
	if cmd == "" {
		return false
	}
	normalized := strings.ReplaceAll(cmd, `\`, "/")
	base := strings.ToLower(filepath.Base(normalized))
	return base == "mcphub" || base == "mcphub.exe"
}

func activeHubAggregateEntry(entry *clients.MCPEntry, clientName string) bool {
	if entry == nil || entry.Disabled {
		return false
	}
	for _, raw := range []string{entry.URL, entry.RelayURL} {
		parsedClient, ok := hubAggregateURLClient(raw)
		if ok && parsedClient == clientName {
			return true
		}
	}
	return false
}

func hubAggregateURLClient(raw string) (string, bool) {
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
	port := parsed.Port()
	n, err := strconv.Atoi(port)
	if port == "" || err != nil || n <= 0 || n > 65535 {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "clients" || parts[2] != "mcp" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func lspRouterURLLanguage(raw string) (string, bool) {
	language, _, ok := lspRouterURLLanguagePort(raw)
	return language, ok
}

func lspRouterURLLanguagePort(raw string) (string, int, bool) {
	if raw == "" {
		return "", 0, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" {
		return "", 0, false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return "", 0, false
	}
	portText := parsed.Port()
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port <= 0 || port > 65535 {
		return "", 0, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "lsp" || parts[2] != "mcp" || parts[1] == "" {
		return "", 0, false
	}
	if parsed.Path != fmt.Sprintf(lspRouterURLPathTemplate, parts[1]) {
		return "", 0, false
	}
	return parts[1], port, true
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

func activeEntryPointsAtLegacyLSPPort(entry *clients.MCPEntry, ports map[int]bool) bool {
	return entry != nil && !entry.Disabled && entryPointsAtLegacyLSPPort(entry, ports)
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
