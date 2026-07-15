package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"

	"gopkg.in/yaml.v3"
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

// ExecuteDeAdoptOpts carries request-scoped destructive-operation consent.
// AcceptConflictClients is validated again at E3 under each client's config
// lock; naming a client here never by itself makes that client terminal.
type ExecuteDeAdoptOpts struct {
	AcceptConflictClients []string
}

// DeAdoptClientFailure is a redaction-safe per-client failure. Reason is always
// a fixed class/message assembled by this file; raw adapter, path, snapshot, or
// config errors are never copied into the report.
type DeAdoptClientFailure struct {
	Client string `json:"client"`
	Reason string `json:"reason"`
}

// DeAdoptReport carries the all-clients E3 outcomes. Restored includes clients
// already proven RESTORE-DONE during a roll-forward retry. Accepted contains
// only conflicts re-proven at the E3 mutation point.
type DeAdoptReport struct {
	Restored []string               `json:"restored"`
	Failed   []DeAdoptClientFailure `json:"failed"`
	Accepted []string               `json:"accepted"`
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

	probe := ProbeHubGate()
	plan := &DeAdoptPlan{
		ManifestName: server,
		Routing:      DeAdoptRoutingRefuse,
		Eligibility: DeAdoptEligibility{
			GateOn:        len(probe.GatedOn) != 0,
			GateOnClients: append([]string(nil), probe.GatedOn...),
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
	plan.Eligibility.Eligible = found && len(probe.GatedOn) == 0 && len(probe.Unreadable) == 0

	if len(probe.GatedOn) != 0 || len(probe.Unreadable) != 0 {
		var blockers []string
		if len(probe.GatedOn) != 0 {
			blockers = append(blockers, fmt.Sprintf("gate is ON for %d client(s) (%s); gate OFF first, then de-adopt", len(probe.GatedOn), strings.Join(probe.GatedOn, ", ")))
		}
		if len(probe.Unreadable) != 0 {
			blockers = append(blockers, fmt.Sprintf("cannot prove gate-OFF: %d client config(s) unreadable (%s); de-adopt refuses until every client's hub-gate state is readable", len(probe.Unreadable), strings.Join(probe.Unreadable, ", ")))
		}
		reason := strings.Join(blockers, "; ")
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
		if classifyDeadAdoptingRow(*rec) != adoptRowCommittedKeep {
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

// ExecuteDeAdopt applies the atomic all-clients de-adopt operation using no
// conflict-acceptance overrides.
func (a *API) ExecuteDeAdopt(server string, w io.Writer) (*DeAdoptReport, error) {
	return a.ExecuteDeAdoptWithOpts(server, w, ExecuteDeAdoptOpts{})
}

// ExecuteDeAdoptWithOpts executes the E1..E7 roll-forward sequence from the
// accepted de-adopt design. The per-manifest lease is outermost and remains held
// through E6. Every inner lock is acquired by an existing owner for one bounded
// operation and released before the next inner lock is taken.
func (a *API) ExecuteDeAdoptWithOpts(server string, w io.Writer, opts ExecuteDeAdoptOpts) (*DeAdoptReport, error) {
	if w == nil {
		w = io.Discard
	}

	// The read-only planner is the single owner of gate-OFF, provenance routing,
	// manifest readiness, and plan-time client classification.
	plan, err := a.BuildDeAdoptPlan(server)
	if err != nil {
		return nil, fmt.Errorf("de-adopt plan for manifest %q could not be built", strings.TrimSpace(server))
	}
	return a.executeDeAdoptPlanWithOpts(plan, w, opts)
}

// executeDeAdoptPlanWithOpts owns the lease-held E1..E7 executor body. Keeping
// the immutable advisory plan explicit also lets tests deterministically prove
// that E3 revalidates every client after a plan-to-lease state change.
func (a *API) executeDeAdoptPlanWithOpts(plan *DeAdoptPlan, w io.Writer, opts ExecuteDeAdoptOpts) (*DeAdoptReport, error) {
	if w == nil {
		w = io.Discard
	}
	if plan.Routing == DeAdoptRoutingRefuse {
		return nil, fmt.Errorf("de-adopt refused: %s", plan.RefusalReason)
	}
	if plan.provenance == nil {
		return nil, fmt.Errorf("de-adopt refused: manifest %q has no executable provenance", plan.ManifestName)
	}

	rec := plan.provenance
	acceptSet, err := deAdoptAcceptConflictSet(opts.AcceptConflictClients, rec.AdoptClients)
	if err != nil {
		return nil, err
	}

	// E1 — acquire the outermost per-manifest lease. No IPC, process kill, or
	// wait is invoked anywhere before the deferred unlock.
	lease, leased, leaseErr := tryAcquireAdoptManifestLease(plan.ManifestName)
	if leaseErr != nil {
		return nil, fmt.Errorf("de-adopt: acquire per-manifest lease for %q failed", plan.ManifestName)
	}
	if !leased {
		return nil, fmt.Errorf("de-adopt: concurrent operation for manifest %q; retry after it completes", plan.ManifestName)
	}
	callerWriter := w
	var narration bytes.Buffer
	pendingEvents := make([]func(), 0, 2)
	// Register the potentially blocking flush before Unlock. Defers run in LIFO
	// order, so the lease is always released before caller I/O or event-log I/O.
	defer func() {
		if narration.Len() != 0 {
			_, _ = callerWriter.Write(narration.Bytes())
		}
		for _, emit := range pendingEvents {
			emit()
		}
	}()
	defer func() { _ = lease.Unlock() }()
	w = &narration

	// E2 — idempotently enter de_adopting. Mark performs the B4 committed-row
	// re-verification while this caller retains the lease.
	if err := MarkAdoptProvenanceDeAdopting(plan.ManifestName); err != nil {
		return nil, fmt.Errorf("de-adopt: mark provenance for manifest %q de-adopting failed", plan.ManifestName)
	}

	report := &DeAdoptReport{}
	resolvedClients := 0
	allClients := clients.AllClients()

	// E3 — restore/remove every target before any topology mutation.
	for _, clientName := range rec.AdoptClients {
		clientRec, ok := deAdoptClientRecord(rec, clientName)
		if !ok {
			deAdoptFailClient(report, clientName, "client provenance is missing or duplicated", w)
			continue
		}
		adapter, ok := allClients[clientName]
		if !ok {
			deAdoptFailClient(report, clientName, "client adapter is unavailable", w)
			continue
		}
		mutator, ok := clients.AsCASEntryMutator(adapter)
		if !ok {
			deAdoptFailClient(report, clientName, "client does not support atomic de-adopt mutation", w)
			continue
		}

		snapshotState, snapshot, snapshotSubtree, snapshotReason := readDeAdoptSnapshot(rec, clientRec, mutator)
		if acceptSet[clientName] {
			if clientRec.OriginalState == AdoptOriginalStatePresent && snapshotState != deAdoptSnapshotAvailable {
				reason := "--accept-conflict passed but the pinned snapshot could not be verified; repair it and retry"
				if snapshotReason != "" {
					reason = "--accept-conflict rejected: " + snapshotReason
				}
				deAdoptFailClient(report, clientName, reason, w)
				continue
			}
			if clientRec.OriginalState != AdoptOriginalStatePresent &&
				clientRec.OriginalState != AdoptOriginalStateAbsent &&
				clientRec.OriginalState != AdoptOriginalStatePresentMergedLower {
				deAdoptFailClient(report, clientName, "--accept-conflict rejected: unsupported original client state", w)
				continue
			}

			verdict, classifyErr := mutator.ClassifyEntryUnderLock(
				rec.SourceEntryName,
				deAdoptLiveBindingMatcher(rec, clientName),
				snapshotSubtree,
			)
			if classifyErr != nil {
				verdict = clients.ClassifyUnreadable
			}
			switch verdict {
			case clients.ClassifyGenuineConflict:
				report.Accepted = append(report.Accepted, clientName)
				resolvedClients++
				manifestName, acceptedClient := plan.ManifestName, clientName
				pendingEvents = append(pendingEvents, func() {
					emitDeAdoptClientAccepted(manifestName, acceptedClient)
				})
				fmt.Fprintf(w, "WARNING: --accept-conflict %q honored: %s\n", clientName, deAdoptAcceptConflictWarning)
			case clients.ClassifyRestoreDone:
				// A harmless no-op when the client became restored after planning.
				report.Restored = append(report.Restored, clientName)
				resolvedClients++
				fmt.Fprintf(w, "De-adopt client %q is already restored; --accept-conflict was not needed.\n", clientName)
			case clients.ClassifyStillHub:
				deAdoptFailClient(report, clientName, fmt.Sprintf("--accept-conflict passed but %q is still the hub entry; omit the flag to restore it", clientName), w)
			case clients.ClassifyUnreadable:
				deAdoptFailClient(report, clientName, "--accept-conflict rejected: live client config could not be read or parsed", w)
			default:
				deAdoptFailClient(report, clientName, "--accept-conflict rejected: unsupported client classification", w)
			}
			continue
		}

		match := deAdoptLiveBindingMatcher(rec, clientName)
		verdict, classifyErr := mutator.ClassifyEntryUnderLock(
			rec.SourceEntryName,
			match,
			snapshotSubtree,
		)
		if classifyErr != nil {
			verdict = clients.ClassifyUnreadable
		}
		disposition, _, reason := mapDeAdoptClientDisposition(
			plan.Routing,
			clientRec.OriginalState,
			snapshotState,
			plan.Manifest.AlreadyAbsent,
			verdict,
		)

		var mutationErr error
		switch disposition {
		case DeAdoptClientRestoreDone:
			report.Restored = append(report.Restored, clientName)
			resolvedClients++
			fmt.Fprintf(w, "De-adopt client %q is already restored; skipping.\n", clientName)
			continue
		case DeAdoptClientRestorePending:
			mutationErr = mutator.CASRestoreEntryFromBytes(rec.SourceEntryName, match, snapshot)
		case DeAdoptClientRemovePending:
			mutationErr = mutator.CASGuardedRemoveEntry(rec.SourceEntryName, match)
		case DeAdoptClientFailed:
			deAdoptFailClient(report, clientName, reason, w)
			continue
		default:
			deAdoptFailClient(report, clientName, "client classification returned an unsupported disposition", w)
			continue
		}

		if mutationErr != nil {
			if errors.Is(mutationErr, clients.ErrCASConflict) {
				deAdoptFailClient(report, clientName, "live client entry changed; atomic de-adopt mutation refused", w)
			} else {
				deAdoptFailClient(report, clientName, "client config mutation failed", w)
			}
			continue
		}
		report.Restored = append(report.Restored, clientName)
		resolvedClients++
		fmt.Fprintf(w, "De-adopt restored client %q.\n", clientName)
	}

	// CLOSE-READY is the one terminal-state predicate shared by E4, E5, and E6.
	// Returning here is the only unresolved-client branch; the complete topology,
	// routed-secret set, provenance row, and every snapshot remain intact.
	closeReady := resolvedClients == len(rec.AdoptClients)
	if !closeReady {
		pendingEvents = append(pendingEvents, func() {
			emitDeAdoptCloseReadyBlocked(plan.ManifestName, rec.ExpectedManifestHash, report)
		})
		fmt.Fprintf(w, "De-adopt for manifest %q is not close-ready: %d client(s) failed; topology and recovery state were preserved.\n", plan.ManifestName, len(report.Failed))
		return report, nil
	}

	// E4 — CLOSE-READY only. Build the exact adopt-created one-daemon ownership
	// scope before deleting the manifest. captureLivePIDs=false keeps the existing
	// cleanup core free of IPC, process probes, kills, and waits under the lease.
	expectedManifest := &config.ServerManifest{
		Name:    rec.ManifestName,
		Daemons: []config.DaemonSpec{{Name: adoptDefaultDaemonName, Port: rec.Port}},
	}
	intentScope := supervisorIntentOwnershipScopeForManifest(expectedManifest, nil, "")
	if !plan.Manifest.AlreadyAbsent {
		if err := a.ManifestDeleteInWithHash(adoptCommittedManifestDir(), rec.ManifestName, rec.ExpectedManifestHash); err != nil {
			pendingEvents = append(pendingEvents, func() {
				emitDeAdoptCloseFailed(plan.ManifestName, rec.ExpectedManifestHash, "manifest-delete", report)
			})
			switch {
			case errors.Is(err, ErrManifestHashRequired):
				return report, fmt.Errorf("de-adopt: manifest %q delete refused because its expected hash is missing", plan.ManifestName)
			case errors.Is(err, ErrManifestHashMismatch):
				return report, fmt.Errorf("de-adopt: manifest %q delete refused because its content hash changed", plan.ManifestName)
			default:
				return report, fmt.Errorf("de-adopt: hash-gated manifest delete for %q failed", plan.ManifestName)
			}
		}
	}
	if _, _, _, err := a.removeServerFromSupervisorIntentCore(context.Background(), rec.ManifestName, intentScope, false); err != nil {
		pendingEvents = append(pendingEvents, func() {
			emitDeAdoptCloseFailed(plan.ManifestName, rec.ExpectedManifestHash, "supervisor-intent", report)
		})
		return report, fmt.Errorf("de-adopt: supervisor-intent cleanup for manifest %q failed", plan.ManifestName)
	}

	// E5 — CLOSE-READY only. The prefilter and delete each take the vault lock
	// through their existing owner and release it before the next inner operation.
	toDelete, sharedSkipped, prefilterErr := a.prepareDeAdoptRoutedSecretCleanup(rec.ManifestName, rec.RoutedSecretKeys)
	if prefilterErr != nil {
		pendingEvents = append(pendingEvents, func() {
			emitDeAdoptCloseFailed(plan.ManifestName, rec.ExpectedManifestHash, "routed-secret-prefilter", report)
		})
		return report, fmt.Errorf("de-adopt: routed-secret prefilter for manifest %q failed", plan.ManifestName)
	}
	if err := deleteAdoptRoutedSecrets(toDelete); err != nil {
		pendingEvents = append(pendingEvents, func() {
			emitDeAdoptCloseFailed(plan.ManifestName, rec.ExpectedManifestHash, "routed-secret-delete", report)
		})
		return report, fmt.Errorf("de-adopt: routed-secret cleanup for manifest %q failed for %d key(s)", plan.ManifestName, len(toDelete))
	}
	for _, key := range sharedSkipped {
		fmt.Fprintf(w, "WARNING: routed secret key %q is referenced by another live manifest and was preserved.\n", key)
	}

	// E6 — CLOSE-READY and every routed key deleted, already absent, or
	// deliberately skipped-as-shared. Close deletes snapshots first, then the row.
	if err := CloseAdoptProvenance(rec.ManifestName); err != nil {
		pendingEvents = append(pendingEvents, func() {
			emitDeAdoptCloseFailed(plan.ManifestName, rec.ExpectedManifestHash, "provenance-close", report)
		})
		return report, fmt.Errorf("de-adopt: provenance close for manifest %q failed", plan.ManifestName)
	}

	// E7 — best-effort redaction-safe audit plus the G4 report.
	pendingEvents = append(pendingEvents, func() {
		emitDeAdoptExecuted(plan.ManifestName, rec.ExpectedManifestHash, report, sharedSkipped)
	})
	fmt.Fprintf(w, "De-adopted manifest %q: restored=%d accepted=%d failed=%d.\n", plan.ManifestName, len(report.Restored), len(report.Accepted), len(report.Failed))
	return report, nil
}

func deAdoptAcceptConflictSet(requested, targets []string) (map[string]bool, error) {
	targetSet := make(map[string]bool, len(targets))
	for _, target := range targets {
		targetSet[target] = true
	}
	accepted := make(map[string]bool, len(requested))
	for _, raw := range requested {
		clientName := strings.TrimSpace(raw)
		if clientName == "" {
			return nil, fmt.Errorf("de-adopt: --accept-conflict requires a client name")
		}
		if !targetSet[clientName] {
			return nil, fmt.Errorf("de-adopt: --accept-conflict client %q is not an adopt target", clientName)
		}
		accepted[clientName] = true
	}
	return accepted, nil
}

func deAdoptFailClient(report *DeAdoptReport, clientName, reason string, w io.Writer) {
	report.Failed = append(report.Failed, DeAdoptClientFailure{Client: clientName, Reason: reason})
	fmt.Fprintf(w, "De-adopt client %q failed: %s.\n", clientName, reason)
}

// prepareDeAdoptRoutedSecretCleanup returns only still-present, non-shared keys
// for deleteAdoptRoutedSecrets. A key already absent is complete; a present key
// referenced by another live on-disk manifest is deliberately skipped and also
// complete for E6. Manifest scan failures fail closed before the vault is read.
func (a *API) prepareDeAdoptRoutedSecretCleanup(manifestName string, routedKeys []string) (toDelete, sharedSkipped []string, err error) {
	keys := dedupeSortedDeAdoptStrings(routedKeys)
	if len(keys) == 0 {
		return nil, nil, nil
	}
	shared, err := a.deAdoptSharedRoutedSecretKeys(manifestName, keys)
	if err != nil {
		return nil, nil, err
	}

	vaultMutex.Lock()
	defer vaultMutex.Unlock()
	err = secrets.WithVaultLock(secrets.DefaultVaultPath(), func() error {
		vault, openErr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
		if openErr != nil {
			return fmt.Errorf("routed-secret vault is unavailable")
		}
		present := make(map[string]bool)
		for _, key := range vault.List() {
			present[key] = true
		}
		for _, key := range keys {
			if !present[key] {
				continue
			}
			if shared[key] {
				sharedSkipped = append(sharedSkipped, key)
				continue
			}
			toDelete = append(toDelete, key)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return toDelete, sharedSkipped, nil
}

func (a *API) deAdoptSharedRoutedSecretKeys(manifestName string, routedKeys []string) (map[string]bool, error) {
	candidates := make(map[string]bool, len(routedKeys))
	for _, key := range routedKeys {
		candidates[key] = true
	}
	shared := make(map[string]bool)
	dir := adoptCommittedManifestDir()
	names, err := a.ManifestListIn(dir)
	if err != nil {
		return nil, fmt.Errorf("list live manifests for routed-secret scan failed")
	}
	for _, name := range names {
		if name == manifestName {
			continue
		}
		raw, readErr := a.ManifestGetIn(dir, name)
		if readErr != nil {
			return nil, fmt.Errorf("read live manifest %q for routed-secret scan failed", name)
		}
		var live config.ServerManifest
		if parseErr := yaml.Unmarshal([]byte(raw), &live); parseErr != nil {
			return nil, fmt.Errorf("parse live manifest %q for routed-secret scan failed", name)
		}
		for key := range deAdoptManifestVaultReferenceKeys(&live) {
			if candidates[key] {
				shared[key] = true
			}
		}
	}
	return shared, nil
}

// deAdoptManifestVaultReferenceKeys returns every vault key a live manifest can
// resolve: exact secret: references in Env plus canonical ${secret:KEY}
// placeholders in the remote-http URL and every header value.
func deAdoptManifestVaultReferenceKeys(live *config.ServerManifest) map[string]bool {
	references := make(map[string]bool)
	for _, value := range live.Env {
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "secret:") {
			if key := strings.TrimPrefix(value, "secret:"); key != "" {
				references[key] = true
			}
		}
	}
	addPlaceholders := func(value string) {
		for _, match := range secrets.SecretPlaceholderRE.FindAllStringSubmatch(value, -1) {
			if len(match) > 1 {
				references[match[1]] = true
			}
		}
	}
	addPlaceholders(live.URL)
	for _, value := range live.Headers {
		addPlaceholders(value)
	}
	return references
}

func dedupeSortedDeAdoptStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
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
		case clients.ClassifyRestoreDone:
			return DeAdoptClientRestoreDone, false, "client is already in its de-adopted target state"
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
