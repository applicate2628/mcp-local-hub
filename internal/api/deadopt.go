package api

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// DeAdoptRoutingVerdict selects the fresh or roll-forward-resume execution path.
type DeAdoptRoutingVerdict string

const (
	DeAdoptRoutingFresh  DeAdoptRoutingVerdict = "FRESH"
	DeAdoptRoutingResume DeAdoptRoutingVerdict = "RESUME"
	DeAdoptRoutingRefuse DeAdoptRoutingVerdict = "REFUSE"
)

// DeAdoptClientDisposition is the plan-time state of one adopt-owned client.
type DeAdoptClientDisposition string

const (
	DeAdoptClientRestorePending DeAdoptClientDisposition = "restore-pending"
	DeAdoptClientRemovePending  DeAdoptClientDisposition = "remove-pending"
	DeAdoptClientRestoreDone    DeAdoptClientDisposition = "restore-done"
	DeAdoptClientFailed         DeAdoptClientDisposition = "failed"
)

// DeAdoptClientPlan is the redaction-safe plan view for one target client.
// AcceptEligible is true only for a classifier-proven genuine conflict. The
// disposition remains Failed until Phase 8 re-proves and accepts that conflict
// at the mutation point.
type DeAdoptClientPlan struct {
	Client         string
	OriginalState  AdoptOriginalState
	Disposition    DeAdoptClientDisposition
	AcceptEligible bool
	Reason         string
}

// DeAdoptManifestReadiness describes the last-binding manifest hash gate.
// HashReady is also true for a RESUME whose manifest is already absent, because
// that means the delete step is already complete and must be skipped.
type DeAdoptManifestReadiness struct {
	Present       bool
	AlreadyAbsent bool
	HashReady     bool
	ExpectedHash  string
	ActualHash    string
	Reason        string
}

// DeAdoptEligibility is the server-scoped G3 read surface. Eligible is derived
// only from adopt ownership plus gate-OFF; state-specific routing remains in
// DeAdoptPlan.Routing.
type DeAdoptEligibility struct {
	AdoptOwned    bool
	GateOn        bool
	Eligible      bool
	GateOnClients []string
	BlockedReason string
}

// DeAdoptPlan is the side-effect-free preview returned by BuildDeAdoptPlan.
// Operator-facing fields contain names, state labels, reasons, and hashes only.
type DeAdoptPlan struct {
	ManifestName    string
	SourceEntryName string
	AdoptClients    []string
	Routing         DeAdoptRoutingVerdict
	RefusalReason   string
	Manifest        DeAdoptManifestReadiness
	Eligibility     DeAdoptEligibility
	Clients         []DeAdoptClientPlan

	// provenance includes execution-only fields such as routed secret-key names
	// and snapshot references. It is unexported so embedding DeAdoptPlan in a GUI
	// response can never serialize that wire-unsafe state.
	provenance *AdoptProvenanceRecord
	// snapshotBytes can contain literal secret values from pre-adopt client
	// configs. It is structurally un-serializable for the same reason as
	// AdoptPlan.secretValues.
	snapshotBytes map[string][]byte
}

type deAdoptSnapshotState int

const (
	deAdoptSnapshotNotApplicable deAdoptSnapshotState = iota
	deAdoptSnapshotAvailable
	deAdoptSnapshotMissing
	deAdoptSnapshotUnreadable
)

// BuildDeAdoptPlan computes the gate-OFF, all-clients de-adopt plan. It never
// mutates a manifest, provenance row, snapshot, or client config.
func (a *API) BuildDeAdoptPlan(server string) (*DeAdoptPlan, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return nil, fmt.Errorf("de-adopt server name is required")
	}
	if err := CheckManifestName(server); err != nil {
		return nil, err
	}

	gated := GatedOnClients()
	plan := &DeAdoptPlan{
		ManifestName: server,
		Routing:      DeAdoptRoutingRefuse,
		Eligibility: DeAdoptEligibility{
			GateOn:        len(gated) != 0,
			GateOnClients: append([]string(nil), gated...),
		},
		snapshotBytes: make(map[string][]byte),
	}

	// Read the row even after the P0 probe so the G3 ownership surface stays
	// truthful on a gate-ON refusal. State-specific routing still stops at P0.
	rec, found, err := ReadAdoptProvenance(server)
	if err != nil {
		return nil, fmt.Errorf("de-adopt plan: read provenance for %q: %w", server, err)
	}
	plan.Eligibility.AdoptOwned = found
	plan.Eligibility.Eligible = found && len(gated) == 0

	if len(gated) != 0 {
		reason := fmt.Sprintf("gate is ON for %d client(s) (%s); gate OFF first, then de-adopt", len(gated), strings.Join(gated, ", "))
		plan.RefusalReason = reason
		plan.Eligibility.BlockedReason = reason
		return plan, nil
	}
	if !found {
		reason := fmt.Sprintf("manifest %q is not adopt-owned or is already de-adopted", server)
		plan.RefusalReason = reason
		plan.Eligibility.BlockedReason = reason
		return plan, nil
	}

	plan.SourceEntryName = rec.SourceEntryName
	plan.AdoptClients = append([]string(nil), rec.AdoptClients...)
	plan.provenance = rec

	allClients := clients.AllClients()
	switch rec.OperationState {
	case AdoptOperationStateAdopted:
		plan.Routing = DeAdoptRoutingFresh
	case AdoptOperationStateAdopting:
		if !deAdoptHasLiveBinding(rec, allClients) {
			plan.RefusalReason = fmt.Sprintf("manifest %q has an adopting row without a live hub binding; adopt orphan GC owns it", server)
			return plan, nil
		}
		plan.Routing = DeAdoptRoutingFresh
	case AdoptOperationStateDeAdopting:
		plan.Routing = DeAdoptRoutingResume
	case AdoptOperationStateClosed:
		plan.RefusalReason = fmt.Sprintf("manifest %q is already de-adopted", server)
		return plan, nil
	default:
		plan.RefusalReason = fmt.Sprintf("manifest %q has unsupported adopt provenance state %q", server, rec.OperationState)
		return plan, nil
	}

	plan.Manifest = a.buildDeAdoptManifestReadiness(rec, plan.Routing)
	plan.Clients = make([]DeAdoptClientPlan, 0, len(rec.AdoptClients))
	for _, clientName := range rec.AdoptClients {
		clientPlan := DeAdoptClientPlan{Client: clientName}
		clientRec, ok := deAdoptClientRecord(rec, clientName)
		if !ok {
			clientPlan.Disposition = DeAdoptClientFailed
			clientPlan.Reason = "client provenance is missing or duplicated"
			plan.Clients = append(plan.Clients, clientPlan)
			continue
		}
		clientPlan.OriginalState = clientRec.OriginalState

		adapter, ok := allClients[clientName]
		if !ok {
			clientPlan.Disposition = DeAdoptClientFailed
			clientPlan.Reason = "client adapter is unavailable"
			plan.Clients = append(plan.Clients, clientPlan)
			continue
		}
		mutator, ok := clients.AsCASEntryMutator(adapter)
		if !ok {
			clientPlan.Disposition = DeAdoptClientFailed
			clientPlan.Reason = "client does not support atomic de-adopt classification"
			plan.Clients = append(plan.Clients, clientPlan)
			continue
		}

		snapshotState, snapshot, snapshotSubtree, snapshotReason := readDeAdoptSnapshot(rec, clientRec, mutator)
		verdict, classifyErr := mutator.ClassifyEntryUnderLock(
			rec.SourceEntryName,
			deAdoptLiveBindingMatcher(rec, clientName),
			snapshotSubtree,
		)
		if classifyErr != nil {
			// The classifier's error can contain an absolute config path. Collapse it
			// to the fixed Unreadable class before it reaches the plan/wire.
			verdict = clients.ClassifyUnreadable
		}

		clientPlan.Disposition, clientPlan.AcceptEligible, clientPlan.Reason = mapDeAdoptClientDisposition(
			plan.Routing,
			clientRec.OriginalState,
			snapshotState,
			plan.Manifest.AlreadyAbsent,
			verdict,
		)
		if clientPlan.Disposition == DeAdoptClientFailed && snapshotReason != "" &&
			verdict != clients.ClassifyUnreadable &&
			(snapshotState == deAdoptSnapshotMissing || snapshotState == deAdoptSnapshotUnreadable) {
			clientPlan.Reason = snapshotReason
		}
		if snapshotState == deAdoptSnapshotAvailable {
			plan.snapshotBytes[clientName] = snapshot
		}
		plan.Clients = append(plan.Clients, clientPlan)
	}

	return plan, nil
}

func (a *API) buildDeAdoptManifestReadiness(rec *AdoptProvenanceRecord, routing DeAdoptRoutingVerdict) DeAdoptManifestReadiness {
	readiness := DeAdoptManifestReadiness{ExpectedHash: rec.ExpectedManifestHash}
	_, actualHash, err := a.ManifestGetInWithHash(adoptCommittedManifestDir(), rec.ManifestName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			readiness.AlreadyAbsent = true
			if routing == DeAdoptRoutingResume {
				readiness.HashReady = true
				readiness.Reason = "manifest is already absent; delete step is complete"
			} else {
				readiness.Reason = "manifest is absent"
			}
			return readiness
		}
		readiness.Reason = "manifest could not be read for the hash gate"
		return readiness
	}

	readiness.Present = true
	readiness.ActualHash = actualHash
	if rec.ExpectedManifestHash == "" {
		readiness.Reason = "expected manifest hash is missing"
		return readiness
	}
	if actualHash != rec.ExpectedManifestHash {
		readiness.Reason = "manifest hash does not match adopt provenance"
		return readiness
	}
	readiness.HashReady = true
	return readiness
}

func deAdoptHasLiveBinding(rec *AdoptProvenanceRecord, allClients map[string]clients.Client) bool {
	for _, clientName := range rec.AdoptClients {
		adapter, ok := allClients[clientName]
		if !ok {
			continue
		}
		mutator, ok := clients.AsCASEntryMutator(adapter)
		if !ok {
			continue
		}
		verdict, err := mutator.ClassifyEntryUnderLock(rec.SourceEntryName, deAdoptLiveBindingMatcher(rec, clientName), nil)
		if err == nil && verdict == clients.ClassifyStillHub {
			return true
		}
	}
	return false
}

func deAdoptLiveBindingMatcher(rec *AdoptProvenanceRecord, clientName string) func(*clients.MCPEntry) bool {
	expected := &config.ServerManifest{
		Name:    rec.ManifestName,
		Daemons: []config.DaemonSpec{{Name: adoptDefaultDaemonName, Port: rec.Port}},
	}
	binding := config.ClientBinding{
		Client:  clientName,
		Daemon:  adoptDefaultDaemonName,
		URLPath: adoptDefaultURLPath,
	}
	return func(live *clients.MCPEntry) bool {
		matched, _ := liveEntryMatchesManifestBinding(live, rec.SourceEntryName, binding, expected)
		return matched
	}
}

func deAdoptClientRecord(rec *AdoptProvenanceRecord, clientName string) (AdoptClientProvenance, bool) {
	var result AdoptClientProvenance
	found := false
	for _, clientRec := range rec.Clients {
		if clientRec.Client != clientName {
			continue
		}
		if found {
			return AdoptClientProvenance{}, false
		}
		result = clientRec
		found = true
	}
	return result, found
}

func readDeAdoptSnapshot(
	rec *AdoptProvenanceRecord,
	clientRec AdoptClientProvenance,
	mutator clients.CASEntryMutator,
) (state deAdoptSnapshotState, snapshot []byte, subtree any, reason string) {
	if clientRec.OriginalState != AdoptOriginalStatePresent {
		return deAdoptSnapshotNotApplicable, nil, nil, ""
	}
	if clientRec.SnapshotSHA256 == "" {
		return deAdoptSnapshotUnreadable, nil, nil, "snapshot integrity hash is missing"
	}
	if err := validateAdoptSnapshotClientName(clientRec.Client); err != nil {
		return deAdoptSnapshotUnreadable, nil, nil, "snapshot client name is invalid"
	}
	dir, err := adoptSnapshotDir(rec.ManifestName)
	if err != nil {
		return deAdoptSnapshotUnreadable, nil, nil, "snapshot path could not be recomputed"
	}

	// Recompute from the validated manifest/client identity. SnapshotRef is
	// intentionally never consulted: it is persisted metadata, not path authority.
	snapshotPath := filepath.Join(dir, clientRec.Client+adoptSnapshotFileSuffix)
	snapshot, err = ReadStateFileInodeAnchored(snapshotPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return deAdoptSnapshotMissing, nil, nil, "snapshot is missing"
		}
		return deAdoptSnapshotUnreadable, nil, nil, "snapshot could not be read"
	}
	if ManifestHashContent(snapshot) != clientRec.SnapshotSHA256 {
		return deAdoptSnapshotUnreadable, nil, nil, "snapshot hash does not match adopt provenance"
	}

	subtree, present, err := mutator.EntryRawSubtree(snapshot, rec.SourceEntryName)
	if err != nil || !present {
		return deAdoptSnapshotUnreadable, nil, nil, "snapshot does not contain the recorded entry"
	}
	return deAdoptSnapshotAvailable, snapshot, subtree, ""
}

func mapDeAdoptClientDisposition(
	routing DeAdoptRoutingVerdict,
	originalState AdoptOriginalState,
	snapshotState deAdoptSnapshotState,
	manifestAlreadyAbsent bool,
	verdict clients.EntryClassification,
) (DeAdoptClientDisposition, bool, string) {
	if verdict == clients.ClassifyUnreadable {
		return DeAdoptClientFailed, false, "live client config could not be read or parsed"
	}

	switch originalState {
	case AdoptOriginalStatePresent:
		switch snapshotState {
		case deAdoptSnapshotUnreadable:
			return DeAdoptClientFailed, false, "snapshot could not be verified"
		case deAdoptSnapshotMissing:
			if routing == DeAdoptRoutingResume && manifestAlreadyAbsent && verdict != clients.ClassifyStillHub {
				return DeAdoptClientRestoreDone, false, "client restore was completed before the manifest delete"
			}
			return DeAdoptClientFailed, false, "snapshot is missing"
		case deAdoptSnapshotAvailable:
			// Continue to the route/verdict mapping below.
		default:
			return DeAdoptClientFailed, false, "present client has no verified snapshot"
		}
	case AdoptOriginalStateAbsent, AdoptOriginalStatePresentMergedLower:
		if snapshotState != deAdoptSnapshotNotApplicable {
			return DeAdoptClientFailed, false, "entryless client has inconsistent snapshot provenance"
		}
	default:
		return DeAdoptClientFailed, false, "client provenance has an unsupported original state"
	}

	if routing == DeAdoptRoutingFresh {
		switch verdict {
		case clients.ClassifyStillHub:
			if originalState == AdoptOriginalStatePresent {
				return DeAdoptClientRestorePending, false, "live hub entry is ready to restore"
			}
			return DeAdoptClientRemovePending, false, "live hub entry is ready to remove"
		case clients.ClassifyGenuineConflict:
			return DeAdoptClientFailed, true, "live entry is a genuine conflict; conflict acceptance is available"
		default:
			return DeAdoptClientFailed, false, "fresh de-adopt requires the live entry to remain the hub binding"
		}
	}

	if routing != DeAdoptRoutingResume {
		return DeAdoptClientFailed, false, "de-adopt routing does not permit client classification"
	}
	switch verdict {
	case clients.ClassifyStillHub:
		if originalState == AdoptOriginalStatePresent {
			return DeAdoptClientRestorePending, false, "live hub entry is ready to restore"
		}
		return DeAdoptClientRemovePending, false, "live hub entry is ready to remove"
	case clients.ClassifyRestoreDone:
		return DeAdoptClientRestoreDone, false, "client restore is already done"
	case clients.ClassifyGenuineConflict:
		return DeAdoptClientFailed, true, "live entry is a genuine conflict; conflict acceptance is available"
	default:
		return DeAdoptClientFailed, false, "client classification returned an unsupported verdict"
	}
}
