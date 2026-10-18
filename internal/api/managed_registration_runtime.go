package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
)

type managedRegistrationFrozenInstallInput struct {
	Server                     string
	ReleaseIdentity            managedRegistrationReleaseIdentity
	CapturedAtUTC              time.Time
	StateRoot                  string
	JournalRoot                string
	BackupIO                   clients.ManagedRegistrationBackupIO
	ClientPopulation           map[string]clients.Client
	ClientConstructionFailures []string
	Targets                    []managedRegistrationFrozenInstallTarget
}

type managedRegistrationReleaseIdentity struct{ Version, Commit string }

type managedRegistrationFrozenInstallTarget struct {
	ProjectAttachment                             bool
	ClientID, LogicalEntryName, PhysicalEntryName string
	Action                                        ClientUpdateAction
	DaemonName, TaskName                          string
	Port                                          int
	URLPath                                       string
	Desired                                       clients.MCPEntry
	Codex                                         *managedRegistrationFrozenCodexRelocation
}

type managedRegistrationFrozenCodexRelocation struct {
	SourceEntryName, TargetEntryName string
	ExpectedSource                   clients.CodexTransport
	AlreadyConfigured                bool
}

type managedRegistrationInstallCore interface {
	ReconcileInstall(context.Context, managedRegistrationFrozenInstallInput) (*ManagedRegistrationReconcileReport, error)
}

func newManagedRegistrationRuntime(input managedRegistrationFrozenInstallInput) (*ManagedRegistrationRuntime, error) {
	if managedRegistrationNilBackupIO(input.BackupIO) || !managedRegistrationSafeIdentifier(input.Server) || input.ReleaseIdentity.Version == "" || input.ReleaseIdentity.Commit == "" || input.CapturedAtUTC.IsZero() || input.CapturedAtUTC.Location() != time.UTC || !managedRegistrationCleanAbsolutePath(input.StateRoot) || !managedRegistrationCleanAbsolutePath(input.JournalRoot) || input.JournalRoot != input.StateRoot || len(input.Targets) == 0 {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	frozen := managedRegistrationFrozenInstallInput{Server: input.Server, ReleaseIdentity: input.ReleaseIdentity, CapturedAtUTC: input.CapturedAtUTC, StateRoot: input.StateRoot, JournalRoot: input.JournalRoot, BackupIO: input.BackupIO, ClientPopulation: make(map[string]clients.Client, len(input.ClientPopulation)), ClientConstructionFailures: append([]string(nil), input.ClientConstructionFailures...), Targets: make([]managedRegistrationFrozenInstallTarget, len(input.Targets))}
	for clientID, client := range input.ClientPopulation {
		if clientID == "" || client == nil || client.Name() != clientID {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
		frozen.ClientPopulation[clientID] = client
	}
	selected, seen := make(map[string]struct{}, len(input.Targets)), make(map[string]struct{}, len(input.Targets))
	for i, target := range input.Targets {
		if err := validateManagedRegistrationFrozenTarget(input.Server, target); err != nil {
			return nil, err
		}
		key := strings.Join([]string{target.ClientID, target.LogicalEntryName, target.PhysicalEntryName, target.DaemonName, target.TaskName}, "\x00")
		if _, duplicate := seen[key]; duplicate {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
		}
		seen[key], selected[target.ClientID] = struct{}{}, struct{}{}
		frozen.Targets[i] = cloneManagedRegistrationFrozenTarget(target)
	}
	failures := make(map[string]struct{}, len(frozen.ClientConstructionFailures))
	for _, clientID := range frozen.ClientConstructionFailures {
		if !managedRegistrationSafeIdentifier(clientID) {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
		if _, duplicate := failures[clientID]; duplicate {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
		if _, present := frozen.ClientPopulation[clientID]; present {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
		failures[clientID] = struct{}{}
	}
	if len(selected) != len(frozen.ClientPopulation)+len(failures) {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
	}
	managedClients := make(map[string]clients.ManagedRegistrationClient, len(frozen.ClientPopulation))
	for clientID, client := range frozen.ClientPopulation {
		if _, ok := selected[clientID]; !ok {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
		managed, ok := clients.AsManagedRegistrationClient(client)
		if !ok {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
		managedClients[clientID] = managed
	}
	for clientID := range failures {
		if _, ok := selected[clientID]; !ok {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
	}
	return &ManagedRegistrationRuntime{frozen: frozen, receipt: managedRegistrationFrozenSourceReceipt(frozen), stateRoot: frozen.StateRoot, capturedAtUTC: frozen.CapturedAtUTC, releaseIdentity: ManagedRegistrationReleaseIdentity{Version: frozen.ReleaseIdentity.Version, Commit: frozen.ReleaseIdentity.Commit}, clients: managedClients, clientConstructionFailures: failures}, nil
}

func managedRegistrationNilBackupIO(value clients.ManagedRegistrationBackupIO) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Ptr && reflected.IsNil()
}

func managedRegistrationCleanAbsolutePath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func cloneManagedRegistrationFrozenTarget(target managedRegistrationFrozenInstallTarget) managedRegistrationFrozenInstallTarget {
	target.Desired.Headers = cloneManagedRegistrationStringMap(target.Desired.Headers)
	target.Desired.Env = cloneManagedRegistrationStringMap(target.Desired.Env)
	if target.Codex != nil {
		codex := *target.Codex
		target.Codex = &codex
	}
	return target
}

func cloneManagedRegistrationStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func validateManagedRegistrationFrozenTarget(server string, target managedRegistrationFrozenInstallTarget) error {
	if target.Action != ClientUpdateAddReplace || !managedRegistrationSafeIdentifier(target.ClientID) || (!target.ProjectAttachment && target.LogicalEntryName != server) || !managedRegistrationSafeIdentifier(target.LogicalEntryName) || !managedRegistrationSafeIdentifier(target.PhysicalEntryName) || !managedRegistrationSafeIdentifier(target.DaemonName) || !managedRegistrationSafeIdentifier(target.TaskName) || target.Port < 1 || target.Port > 65535 || target.Desired.Name != target.PhysicalEntryName || target.Desired.Raw != nil || target.Desired.ToolTimeoutSec < 0 || !managedRegistrationFrozenURLPathValid(target.URLPath) {
		return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	expectedURL := fmt.Sprintf("http://127.0.0.1:%d%s", target.Port, target.URLPath)
	relay := target.Desired.RelayURL != ""
	if relay {
		if target.Desired.URL != "" || target.Desired.RelayURL != expectedURL || target.Desired.RelayServer != server || target.Desired.RelayDaemon != target.DaemonName || !managedRegistrationCleanAbsolutePath(target.Desired.RelayExePath) {
			return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
		}
	} else if target.Desired.URL != expectedURL {
		return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	alias := target.PhysicalEntryName != target.LogicalEntryName
	if target.ProjectAttachment {
		if target.ClientID != "codex-cli" || target.PhysicalEntryName != target.LogicalEntryName+"-mcphub" || target.Codex != nil {
			return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
		}
		return nil
	}
	if target.Codex == nil {
		if target.ClientID == "codex-cli" && alias {
			return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
		}
		return nil
	}
	if target.ClientID != "codex-cli" || !alias || target.Codex.SourceEntryName != target.LogicalEntryName || target.Codex.TargetEntryName != target.PhysicalEntryName {
		return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	if target.Codex.AlreadyConfigured {
		if target.Codex.ExpectedSource != clients.CodexTransportNone {
			return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
		}
		return nil
	}
	if target.Codex.ExpectedSource != clients.CodexTransportHTTP && target.Codex.ExpectedSource != clients.CodexTransportStdio {
		return newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	return nil
}

func managedRegistrationFrozenURLPathValid(value string) bool {
	return strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "?#")
}

func managedRegistrationFrozenSourceReceipt(input managedRegistrationFrozenInstallInput) string {
	parts := []string{"managed-registration-frozen-install/v1", input.Server, input.ReleaseIdentity.Version, input.ReleaseIdentity.Commit, input.CapturedAtUTC.Format(time.RFC3339Nano), input.StateRoot, input.JournalRoot}
	targets := append([]managedRegistrationFrozenInstallTarget(nil), input.Targets...)
	sort.Slice(targets, func(i, j int) bool {
		return strings.Join([]string{targets[i].ClientID, targets[i].LogicalEntryName, targets[i].PhysicalEntryName, targets[i].DaemonName, targets[i].TaskName}, "\x00") < strings.Join([]string{targets[j].ClientID, targets[j].LogicalEntryName, targets[j].PhysicalEntryName, targets[j].DaemonName, targets[j].TaskName}, "\x00")
	})
	for _, target := range targets {
		parts = append(parts, strconv.FormatBool(target.ProjectAttachment))
		parts = append(parts, target.ClientID, target.LogicalEntryName, target.PhysicalEntryName, string(target.Action), target.DaemonName, target.TaskName, strconv.Itoa(target.Port), target.URLPath, target.Desired.Name, target.Desired.URL, target.Desired.RelayServer, target.Desired.RelayDaemon, target.Desired.RelayExePath, target.Desired.RelayURL, strconv.Itoa(target.Desired.ToolTimeoutSec), strconv.FormatBool(target.Desired.Disabled))
		parts = append(parts, managedRegistrationFrozenMapParts(target.Desired.Headers)...)
		parts = append(parts, managedRegistrationFrozenMapParts(target.Desired.Env)...)
		if target.Codex == nil {
			parts = append(parts, "codex:none")
		} else {
			parts = append(parts, "codex", target.Codex.SourceEntryName, target.Codex.TargetEntryName, string(target.Codex.ExpectedSource), strconv.FormatBool(target.Codex.AlreadyConfigured))
		}
	}
	return managedRegistrationRuntimeHash(parts...)
}

func managedRegistrationFrozenMapParts(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := []string{strconv.Itoa(len(keys))}
	for _, key := range keys {
		parts = append(parts, key, values[key])
	}
	return parts
}

// ReconcileManagedRegistrations is the API-owned composition boundary. The
// current runtime is a pre-write admission proof; the existing reconciler
// remains the sole durable writer.
func (apiHandle *API) ReconcileManagedRegistrations(ctx context.Context, options ManagedRegistrationReconcileOptions) (*ManagedRegistrationReconcileReport, error) {
	if apiHandle == nil || ctx == nil {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	request := options.Request
	if !managedRegistrationOperationTriggerAllowed(request.Operation, request.Trigger) {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	if request.NoClientConfig {
		if request.Operation != ManagedRegistrationForward || len(request.Servers) == 0 || len(request.Clients) != 0 || len(request.Targets) != 0 || len(request.Entries) != 0 {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
		}
		if _, err := managedRegistrationCanonicalSelectors(request.Servers); err != nil {
			return nil, err
		}
		return &ManagedRegistrationReconcileReport{}, nil
	}
	if options.Runtime == nil {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	if len(request.Targets) != 0 || len(request.Entries) != 0 {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
	}
	if request.ReleaseIdentity.Version == "" && request.ReleaseIdentity.Commit == "" {
		request.ReleaseIdentity = options.Runtime.releaseIdentity
	} else if request.ReleaseIdentity.Version != options.Runtime.releaseIdentity.Version ||
		request.ReleaseIdentity.Commit != options.Runtime.releaseIdentity.Commit {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailureReleaseIdentityMismatch, nil)
	}
	request.sourceReceipt = options.Runtime.receipt
	releaseFingerprint, err := managedRegistrationReleaseFingerprint(request.ReleaseIdentity, request.sourceReceipt)
	if err != nil {
		return nil, err
	}
	var servers, clientIDs []string
	var targets []ManagedRegistrationTarget
	var entries []ManagedRegistrationEntryIntent
	var selectedServers, selectedClients map[string]struct{}
	var rollbackAbsenceProof *ManagedRegistrationHubOwnerAbsentProof
	switch request.Operation {
	case ManagedRegistrationForward:
		servers, err = managedRegistrationCanonicalSelectors(request.Servers)
		if err != nil || len(servers) == 0 {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, err)
		}
		clientIDs, err = managedRegistrationCanonicalSelectors(request.Clients)
		if err != nil || len(clientIDs) == 0 {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, err)
		}
		selectedServers, selectedClients = managedRegistrationSelectorSet(servers), managedRegistrationSelectorSet(clientIDs)
		targets, entries, err = options.Runtime.resolveForward(selectedServers, selectedClients)
		if err != nil {
			return nil, err
		}
	case ManagedRegistrationRollback:
		servers, err = managedRegistrationCanonicalSelectors(request.Servers)
		if err != nil || len(servers) != 1 || options.Runtime.uniqueDaemonForServer(servers[0]) == "" {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, err)
		}
		clientIDs, err = managedRegistrationCanonicalSelectors(request.Clients)
		if err != nil || len(clientIDs) == 0 {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, err)
		}
		selectedServers, selectedClients = managedRegistrationSelectorSet(servers), managedRegistrationSelectorSet(clientIDs)
		for _, clientID := range clientIDs {
			if options.Runtime.clients[clientID] == nil {
				return nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
			}
		}
		rollbackAbsenceProof, err = options.Runtime.ResolveRollbackAbsence(servers[0], options.Runtime.uniqueDaemonForServer(servers[0]))
		if err != nil {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureRollbackAbsenceUnproven, err)
		}
	case ManagedRegistrationRecover:
		// Caller narrowing is ignored by contract. Recovery resolves only the
		// identities carried by the current durable journal.
	}
	relationshipState, err := options.Runtime.relationshipsFor(selectedServers, selectedClients)
	if err != nil {
		return nil, err
	}
	if request.RequireAlreadyDesired {
		if request.Operation != ManagedRegistrationForward || request.Trigger != ManagedRegistrationTriggerInstall {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
		}
		// The frozen install plan is the transient relationship owner for the
		// just-committed PR-core write. This does not persist or authorize a write;
		// planEntry must still prove the physical entry is already exact.
		for _, target := range options.Runtime.frozen.Targets {
			if _, selected := selectedServers[options.Runtime.frozen.Server]; !selected {
				continue
			}
			if _, selected := selectedClients[target.ClientID]; !selected {
				continue
			}
			if target.PhysicalEntryName == "" || target.ClientID == "" {
				return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
			}
			if relationshipState.current[target.ClientID] == nil {
				relationshipState.current[target.ClientID] = make(map[string]string)
			}
			if existing, found := relationshipState.current[target.ClientID][target.PhysicalEntryName]; found && existing != options.Runtime.frozen.Server {
				return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
			}
			relationshipState.current[target.ClientID][target.PhysicalEntryName] = options.Runtime.frozen.Server
		}
	}
	operationRuntime := *options.Runtime
	operationRuntime.relationships = relationshipState.current

	reconciler, err := NewManagedRegistrationReconciler(ManagedRegistrationReconcilerOptions{
		StateDir:                      operationRuntime.stateRoot,
		Clients:                       operationRuntime.clients,
		Now:                           func() time.Time { return operationRuntime.capturedAtUTC },
		BackupIO:                      operationRuntime.frozen.BackupIO,
		ExistingManagedRelationships:  relationshipState.current,
		LegacyManagedRelationships:    relationshipState.legacy,
		CanonicalAliases:              operationRuntime.CanonicalAliases(),
		ExpectedReleaseIdentitySHA256: releaseFingerprint,
		ResolveRecoveryEntryName:      operationRuntime.resolveRecoveryEntryName,
		ResolveRollbackTargetGroup:    operationRuntime.resolveRollbackTargetGroup,
		resolvePreparedAbsentCreate:   operationRuntime.resolvePreparedAbsentCreate,
	})
	if err != nil {
		return nil, err
	}
	// A nonterminal generation is normally recovered by the existing journal
	// owner before a new forward request. Install verification is deliberately
	// no-write: Reconcile's own journal gate reports any nonterminal generation
	// as write-uncertain without invoking recovery mutation.
	var recovered ManagedRegistrationReconcileReport
	if !request.RequireAlreadyDesired {
		recovered, err = reconciler.Recover(ctx)
		if err != nil {
			return nil, err
		}
	}
	if request.Operation == ManagedRegistrationRecover {
		return &recovered, nil
	}
	if request.Operation == ManagedRegistrationRollback {
		if rollbackAbsenceProof == nil || recovered.GenerationID == "" {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailureRollbackAbsenceUnproven, nil)
		}
		report, err := reconciler.RollbackTargetGroup(ctx, ManagedRegistrationRollbackRequest{GenerationID: recovered.GenerationID, Server: rollbackAbsenceProof.server, Daemon: rollbackAbsenceProof.daemon, absenceProof: rollbackAbsenceProof, clientIDs: selectedClients})
		return &report, err
	}
	request.Servers = servers
	request.Clients = clientIDs
	request.Targets = targets
	request.Entries = entries
	if request.GenerationID == "" {
		request.GenerationID = "managed-" + managedRegistrationRuntimeHash(string(request.Trigger), options.Runtime.receipt, strings.Join(servers, ","), strings.Join(clientIDs, ","))[:32]
	}
	report, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		return &report, err
	}
	return &report, nil
}

func (runtime *ManagedRegistrationRuntime) SourceReceipt() string {
	if runtime == nil {
		return ""
	}
	return runtime.receipt
}

// CanonicalAliases derives only planner-frozen logical-to-physical Codex
// mappings and performs no runtime re-resolution.
func (runtime *ManagedRegistrationRuntime) CanonicalAliases() map[string]string {
	aliases := make(map[string]string)
	if runtime == nil {
		return aliases
	}
	for _, target := range runtime.frozen.Targets {
		if target.Codex != nil && target.PhysicalEntryName != target.LogicalEntryName {
			aliases[target.PhysicalEntryName] = target.LogicalEntryName
		}
	}
	return aliases
}

func managedRegistrationOperationTriggerAllowed(operation ManagedRegistrationOperation, trigger ManagedRegistrationTrigger) bool {
	switch operation {
	case ManagedRegistrationForward:
		return trigger == ManagedRegistrationTriggerTargetedMigrate || trigger == ManagedRegistrationTriggerInstall || trigger == ManagedRegistrationTriggerLegacyUpgrade || trigger == ManagedRegistrationTriggerV5Upgrade
	case ManagedRegistrationRecover:
		return trigger == ManagedRegistrationTriggerRecovery
	case ManagedRegistrationRollback:
		return trigger == ManagedRegistrationTriggerRollback
	default:
		return false
	}
}

func managedRegistrationCanonicalSelectors(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != value || !managedRegistrationSafeIdentifier(value) {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, newManagedRegistrationFailure(ManagedRegistrationFailurePreconditionConflict, nil)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func managedRegistrationSelectorSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func (runtime *ManagedRegistrationRuntime) resolveForward(servers, selectedClients map[string]struct{}) ([]ManagedRegistrationTarget, []ManagedRegistrationEntryIntent, error) {
	if runtime == nil {
		return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	if len(servers) != 1 {
		return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeTargetConflict, nil)
	}
	if _, selected := servers[runtime.frozen.Server]; !selected {
		return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeTargetConflict, nil)
	}
	for clientID := range selectedClients {
		if _, failed := runtime.clientConstructionFailures[clientID]; failed {
			return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
		if runtime.clients[clientID] == nil {
			return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
	}
	serverHits := make(map[string]bool, len(servers))
	clientHits := make(map[string]bool, len(selectedClients))
	targetKeys := make(map[string]struct{})
	var targets []ManagedRegistrationTarget
	var entries []ManagedRegistrationEntryIntent
	for _, frozen := range runtime.frozen.Targets {
		if _, selected := selectedClients[frozen.ClientID]; !selected {
			continue
		}
		transport := clients.ManagedRegistrationTransportHTTP
		if frozen.Desired.RelayURL != "" {
			transport = clients.ManagedRegistrationTransportStdioRelay
		}
		desired := cloneManagedRegistrationFrozenTarget(frozen).Desired
		targetIdentity, err := clients.ManagedRegistrationTargetFingerprint(desired, transport == clients.ManagedRegistrationTransportStdioRelay)
		if err != nil {
			return nil, nil, err
		}
		targetKey := strings.Join([]string{frozen.TaskName, runtime.frozen.Server, frozen.DaemonName, strconv.Itoa(frozen.Port), frozen.URLPath}, "\x00")
		if _, duplicate := targetKeys[targetKey]; !duplicate {
			targetKeys[targetKey] = struct{}{}
			targets = append(targets, ManagedRegistrationTarget{TaskKey: frozen.TaskName, Server: runtime.frozen.Server, Daemon: frozen.DaemonName, Port: frozen.Port, Path: frozen.URLPath, TargetIdentitySHA256: targetIdentity, Desired: true, Ready: true, Live: true})
		}
		enabled := clients.ManagedRegistrationEnabled
		if desired.Disabled {
			enabled = clients.ManagedRegistrationDisabled
		}
		var codex *managedRegistrationFrozenCodexRelocation
		if frozen.Codex != nil {
			copy := *frozen.Codex
			codex = &copy
		}
		entries = append(entries, ManagedRegistrationEntryIntent{ClientID: frozen.ClientID, EntryName: frozen.PhysicalEntryName, Server: runtime.frozen.Server, EnabledState: enabled, Transport: transport, RelayExecutable: desired.RelayExePath, Desired: desired, Codex: codex})
		serverHits[runtime.frozen.Server] = true
		clientHits[frozen.ClientID] = true
	}
	for server := range servers {
		if !serverHits[server] {
			return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeTargetConflict, nil)
		}
	}
	for clientID := range selectedClients {
		if !clientHits[clientID] {
			return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		return managedRegistrationTargetKey(targets[i]) < managedRegistrationTargetKey(targets[j])
	})
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ClientID+"\x00"+entries[i].Server < entries[j].ClientID+"\x00"+entries[j].Server
	})
	return targets, entries, nil
}

func managedRegistrationFingerprintFromHex(value string) (clients.ManagedRegistrationFingerprint, error) {
	var fingerprint clients.ManagedRegistrationFingerprint
	if err := json.Unmarshal([]byte(strconv.Quote(value)), &fingerprint); err != nil {
		return clients.ManagedRegistrationFingerprint{}, newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, err)
	}
	return fingerprint, nil
}

type managedRegistrationRelationshipState struct {
	current map[string]map[string]string
	legacy  map[string]map[string]string
}

func loadManagedRegistrationRelationships(stateRoot, frozenServer string, frozenTargets []managedRegistrationFrozenInstallTarget, managedClients map[string]clients.ManagedRegistrationClient, selectedServers, selectedClients map[string]struct{}) (managedRegistrationRelationshipState, error) {
	result := managedRegistrationRelationshipState{current: make(map[string]map[string]string), legacy: make(map[string]map[string]string)}
	raw, err := ReadStateFileInodeAnchored(filepath.Join(stateRoot, managedEntriesFileLeaf))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, err)
	}
	var state ManagedEntries
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != managedEntriesSchemaVersion {
		return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, err)
	}
	journalStore, err := newManagedRegistrationJournalStore(stateRoot)
	if err != nil {
		return managedRegistrationRelationshipState{}, err
	}
	journal, err := journalStore.read(context.Background())
	if err != nil {
		return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, err)
	}
	seen := make(map[string]struct{}, len(state.Entries))
	for _, entry := range state.Entries {
		key := entry.Client + "\x00" + entry.Server
		if _, duplicate := seen[key]; duplicate {
			return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
		}
		seen[key] = struct{}{}
		if len(selectedServers) != 0 {
			if _, selected := selectedServers[entry.Server]; !selected {
				continue
			}
		}
		if len(selectedClients) != 0 {
			if _, selected := selectedClients[entry.Client]; !selected {
				continue
			}
		}
		client := managedClients[entry.Client]
		if client == nil {
			return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
		}
		physicalEntryName, targetErr := managedRegistrationFrozenPhysicalEntryName(frozenServer, frozenTargets, entry)
		if managedRegistrationEntryIsLegacy(entry) {
			if targetErr != nil {
				return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureLegacyOwnershipUnproven, targetErr)
			}
			if result.legacy[entry.Client] == nil {
				result.legacy[entry.Client] = make(map[string]string)
			}
			result.legacy[entry.Client][physicalEntryName] = entry.Server
			continue
		}
		if targetErr != nil {
			return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, targetErr)
		}
		var expected clients.ManagedRegistrationEntrySnapshot
		matches := 0
		for _, row := range journal.Rows {
			if row.ClientID != entry.Client || row.After.ClientID == "" || row.After.TargetIdentitySHA256 != entry.TargetIdentitySHA256 {
				continue
			}
			expected = row.After
			matches++
		}
		if matches == 0 && len(selectedServers) == 0 && len(selectedClients) == 0 {
			continue
		}
		if matches != 1 || !managedRegistrationFingerprintValid(expected.DesiredProjectionSHA256) || !managedRegistrationFingerprintValid(expected.ConfigPathSHA256) || !managedRegistrationFingerprintValid(expected.EntryKeySHA256) {
			return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
		}
		if !entry.Exists || expected.ClientID != entry.Client || !expected.Exists || expected.Transport != entry.Transport || expected.EnabledState != entry.EnabledState || expected.IdentityState != entry.IdentityState || expected.ConfigSHA256 != entry.ConfigSHA256 || expected.EntrySHA256 != entry.EntrySHA256 || expected.DesiredProjectionSHA256 != entry.DesiredProjectionSHA256 || expected.ConfigPathSHA256 != entry.ConfigPathSHA256 || expected.EntryKeySHA256 != entry.EntryKeySHA256 || expected.TargetIdentitySHA256 != entry.TargetIdentitySHA256 {
			return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
		}
		name, err := client.ResolveManagedRegistrationEntryName(context.Background(), []string{physicalEntryName}, expected)
		if err != nil || name != physicalEntryName {
			return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, err)
		}
		if result.current[entry.Client] == nil {
			result.current[entry.Client] = make(map[string]string)
		}
		result.current[entry.Client][physicalEntryName] = entry.Server
	}
	return result, nil
}

func managedRegistrationFrozenPhysicalEntryName(server string, targets []managedRegistrationFrozenInstallTarget, entry ManagedEntry) (string, error) {
	name := ""
	matches := 0
	legacy := managedRegistrationEntryIsLegacy(entry)
	for _, target := range targets {
		if server != entry.Server || target.LogicalEntryName != entry.Server || target.ClientID != entry.Client {
			continue
		}
		if !legacy {
			relay := target.Desired.RelayURL != ""
			identity, err := clients.ManagedRegistrationTargetFingerprint(target.Desired, relay)
			if err != nil || identity != entry.TargetIdentitySHA256 {
				continue
			}
		}
		matches++
		name = target.PhysicalEntryName
	}
	if matches != 1 || name == "" {
		return "", newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
	}
	return name, nil
}

func (runtime *ManagedRegistrationRuntime) relationshipsFor(selectedServers, selectedClients map[string]struct{}) (managedRegistrationRelationshipState, error) {
	if runtime == nil {
		return managedRegistrationRelationshipState{}, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	return loadManagedRegistrationRelationships(runtime.stateRoot, runtime.frozen.Server, runtime.frozen.Targets, runtime.clients, selectedServers, selectedClients)
}

// ManagedRegistrationSelectors returns the deterministic persisted managed
// population for post-upgrade reconciliation. An absent projection is an empty
// population; malformed or duplicate rows fail closed.
func ManagedRegistrationSelectors(stateRoot string) (servers, clientIDs []string, err error) {
	raw, readErr := ReadStateFileInodeAnchored(filepath.Join(stateRoot, managedEntriesFileLeaf))
	if errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, nil
	}
	if readErr != nil {
		return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, readErr)
	}
	var state ManagedEntries
	if decodeErr := json.Unmarshal(raw, &state); decodeErr != nil || state.Version != managedEntriesSchemaVersion {
		return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, decodeErr)
	}
	serverSet := make(map[string]struct{})
	clientSet := make(map[string]struct{})
	rowSet := make(map[string]struct{})
	for _, entry := range state.Entries {
		if !managedRegistrationSafeIdentifier(entry.Server) || !managedRegistrationSafeIdentifier(entry.Client) {
			return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
		}
		key := entry.Server + "\x00" + entry.Client
		if _, duplicate := rowSet[key]; duplicate {
			return nil, nil, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
		}
		rowSet[key] = struct{}{}
		serverSet[entry.Server] = struct{}{}
		clientSet[entry.Client] = struct{}{}
	}
	for server := range serverSet {
		servers = append(servers, server)
	}
	for clientID := range clientSet {
		clientIDs = append(clientIDs, clientID)
	}
	sort.Strings(servers)
	sort.Strings(clientIDs)
	return servers, clientIDs, nil
}

func (runtime *ManagedRegistrationRuntime) resolveRecoveryEntryName(_ context.Context, clientID string, expected clients.ManagedRegistrationEntrySnapshot) (string, error) {
	client := runtime.clients[clientID]
	if client == nil {
		return "", newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
	}
	byEntry := runtime.relationships[clientID]
	candidates := make([]string, 0, len(byEntry))
	for entryName := range byEntry {
		candidates = append(candidates, entryName)
	}
	name, err := client.ResolveManagedRegistrationEntryName(context.Background(), candidates, expected)
	if err != nil {
		return "", newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, err)
	}
	return name, nil
}

func (runtime *ManagedRegistrationRuntime) resolvePreparedAbsentCreate(_ context.Context, clientID string, targetGroup clients.ManagedRegistrationFingerprint) (managedRegistrationPreparedCreateResolution, error) {
	if runtime == nil || runtime.clients[clientID] == nil || !managedRegistrationFingerprintValid(targetGroup) {
		return managedRegistrationPreparedCreateResolution{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
	}
	var resolution managedRegistrationPreparedCreateResolution
	matches := 0
	for _, frozen := range runtime.frozen.Targets {
		if frozen.ClientID != clientID {
			continue
		}
		relay := frozen.Desired.RelayURL != ""
		identity, err := clients.ManagedRegistrationTargetFingerprint(frozen.Desired, relay)
		if err != nil {
			return managedRegistrationPreparedCreateResolution{}, err
		}
		target := ManagedRegistrationTarget{TaskKey: frozen.TaskName, Server: runtime.frozen.Server, Daemon: frozen.DaemonName, Port: frozen.Port, Path: frozen.URLPath, TargetIdentitySHA256: identity, Desired: true, Ready: true, Live: true}
		group, err := managedRegistrationTargetGroupFingerprint(target)
		if err != nil || group != targetGroup {
			continue
		}
		matches++
		resolution = managedRegistrationPreparedCreateResolution{EntryName: frozen.PhysicalEntryName, Desired: cloneManagedRegistrationFrozenTarget(frozen).Desired, Target: target}
	}
	if matches != 1 {
		return managedRegistrationPreparedCreateResolution{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelationshipConflict, nil)
	}
	return resolution, nil
}

func (runtime *ManagedRegistrationRuntime) ResolveRollbackAbsence(server, daemon string) (*ManagedRegistrationHubOwnerAbsentProof, error) {
	return nil, newManagedRegistrationFailure(ManagedRegistrationFailureRollbackAbsenceUnproven, nil)
}

func (runtime *ManagedRegistrationRuntime) uniqueDaemonForServer(server string) string {
	daemon := ""
	for _, target := range runtime.frozen.Targets {
		if runtime.frozen.Server != server {
			continue
		}
		if daemon != "" && daemon != target.DaemonName {
			return ""
		}
		daemon = target.DaemonName
	}
	return daemon
}

func (runtime *ManagedRegistrationRuntime) resolveRollbackTargetGroup(_ context.Context, server, daemon string) (ManagedRegistrationTargetGroupResolution, error) {
	proof, err := runtime.ResolveRollbackAbsence(server, daemon)
	if err != nil {
		return ManagedRegistrationTargetGroupResolution{}, err
	}
	return ManagedRegistrationTargetGroupResolution{TargetGroupIdentitySHA256: proof.targetGroupSHA256, AbsenceProofTargetGroupSHA256: proof.targetGroupSHA256, SupervisorSnapshotSHA256: proof.supervisorSnapshotSHA256}, nil
}

func managedRegistrationRuntimeHash(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(hash, "%d:", len(part))
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
