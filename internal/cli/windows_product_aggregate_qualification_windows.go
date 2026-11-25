package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

// One original ordinary upgrade owns this selection from the GUI reservation
// until the native Product decision has returned. It is never an RPC input.
type productAggregateQualification struct {
	stateDir       string
	guiPath        string
	priorPath      string
	namespace      *api.WindowsProductStateNamespace
	endpoint       api.HubRouteEndpointSelection
	restart        *gui.HandoffMarkerRecord
	live           *priorAggregateProcesses
	serving        *windowsProductPriorServingAggregateV1
	reload         api.HubRouteReloadSelection
	prepared       *windowsProductPreparedTxn
	priorReceipt   productPairFileSnapshot
	priorSelection api.HubRouteQualification
	next           api.HubRouteQualification
}

func sameOriginalAggregateEndpoint(a, b api.HubRouteEndpointSelection) bool {
	return a.OriginalIdentity() == b.OriginalIdentity() && a.OriginalSHA256() == b.OriginalSHA256()
}

func reserveAbsentPriorAggregate(ctx context.Context, stateDir, priorPath string, prior *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1, namespace *api.WindowsProductStateNamespace) (gui.OwnedSingleInstanceLease, *productAggregateQualification, error) {
	if prior == nil || namespace == nil {
		return nil, nil, errors.New("original prior Product unavailable for aggregate transfer")
	}
	path, err := resolveInstalledUpgradeGUIPidportPath()
	if err != nil {
		return nil, nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
	defer cancel()
	if err := namespace.Validate(bounded); err != nil {
		return nil, nil, err
	}
	q := &productAggregateQualification{stateDir: stateDir, guiPath: path, priorPath: priorPath, namespace: namespace}
	var observed bool
	lease, err := acquireGUIOwnedPriorReservation(bounded, nil, guiOwnedPriorReservationOps{
		ProveNoGUIOrTray: func(ctx context.Context) error {
			if err := prior.ValidateOriginal(ctx); err != nil {
				return err
			}
			if err := q.proveGUIAbsent(ctx); err != nil {
				return err
			}
			current, err := readQuiescentAggregateGUIRestartBound(ctx, stateDir, namespace)
			if err != nil {
				return err
			}
			if observed && !sameAggregateGUIRestart(q.restart, current) {
				return errors.New("GUI restart marker changed during Product reservation")
			}
			q.restart = current
			return namespace.WithHubRouteState(ctx, func(h *api.HubRouteState) error {
				endpoint, err := h.ReadEndpoint()
				if err != nil {
					return err
				}
				if observed && !sameOriginalAggregateEndpoint(q.endpoint, endpoint) {
					return errors.New("protected Hub endpoint changed during Product reservation")
				}
				pid, bound, err := api.LoopbackPortOwnerPIDContext(ctx, endpoint.Endpoint().Port)
				if err != nil {
					return err
				}
				if observed && bound != (q.serving != nil) {
					return errors.New("prior aggregate occupancy changed during GUI reservation")
				}
				if bound {
					if err := q.observeServing(ctx, h, endpoint, prior, pid); err != nil {
						return err
					}
				} else if err := h.CheckReloadTokenAbsent(); err != nil {
					return err
				}
				// Retain exact native identity and raw bytes from the post-acquisition
				// observation. The H owner compares them again at decision time.
				q.endpoint = endpoint
				observed = true
				return nil
			})
		},
		AcquireExact: func() (gui.OwnedSingleInstanceLease, error) {
			return gui.AcquireSingleInstanceLockOnlyBoundAt(bounded, namespace, path)
		},
	})
	if err != nil || lease == nil {
		var reservation *upgradeGUIPriorSettlementError
		if errors.As(err, &reservation) {
			namespace.RetainAfterUnconfirmedRelease(reservation.reservationRelease)
		}
		return lease, nil, errors.Join(errors.New("original aggregate GUI reservation unavailable"), err, q.live.close())
	}
	return lease, q, nil
}

func (q *productAggregateQualification) proveGUIAbsent(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	found, err := findRunningGUIsOnTargetContext(ctx, api.NewAPI(), q.priorPath)
	if err != nil || len(found) != 0 {
		return errors.Join(errors.New("original prior GUI absence unconfirmed"), err)
	}
	all, err := api.NewAPI().ListMatchingProcessesContext(ctx, []string{"mcphub.exe"})
	if err != nil {
		return err
	}
	for _, candidate := range all {
		if supervisorCommandLineSubcommand(candidate.Cmdline) != "tray" {
			continue
		}
		identity, err := process.LookupPIDHandleIdentity(candidate.PID)
		if err != nil || samePath(identity.ExecutablePath, q.priorPath) {
			return errors.Join(errors.New("original prior tray absence unconfirmed"), err)
		}
	}
	record, err := gui.ReadGUIOwnerRecordBound(ctx, q.namespace, q.guiPath)
	if err == nil {
		return proveUpgradeGUIOwnerEndpointAbsent(ctx, record, api.LoopbackPortOwnerPIDContext)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return ctx.Err()
}

func readQuiescentAggregateGUIRestartBound(ctx context.Context, stateDir string, namespace *api.WindowsProductStateNamespace) (*gui.HandoffMarkerRecord, error) {
	return expectQuiescentAggregateGUIRestart(gui.NewHandoffMarkerStore(stateDir, gui.DefaultRestartDeadlines()).ReadBound(ctx, namespace))
}

func expectQuiescentAggregateGUIRestart(record *gui.HandoffMarkerRecord, err error) (*gui.HandoffMarkerRecord, error) {
	if err != nil {
		return nil, err
	}
	if record != nil && record.Phase != gui.HandoffPhaseCommitted && record.Phase != gui.HandoffPhaseInterrupted {
		return nil, errors.New("original GUI restart has no terminal observation")
	}
	return record, nil
}

func sameAggregateGUIRestart(before, after *gui.HandoffMarkerRecord) bool {
	if before == nil || after == nil {
		return before == after
	}
	return *before == *after
}

func proveProductAggregateVacancy(ctx context.Context, h *api.HubRouteState, endpoint api.HubEndpoint) error {
	if _, bound, err := api.LoopbackPortOwnerPIDContext(ctx, endpoint.Port); err != nil || bound {
		return errors.Join(errors.New("original saved Hub socket vacancy unconfirmed"), err)
	}
	return h.CheckReloadTokenAbsent()
}

// Check the original selection after the prior fleet has settled. The Product
// owner calls this after persisting quiescence and before moving any file.
func (q *productAggregateQualification) beforePromotion(ctx context.Context) error {
	if q.prepared == nil || q.prepared.journal.Quiescence == nil {
		return errors.New("prior aggregate has no durable Product quiescence")
	}
	if err := q.live.receiveExit(ctx); err != nil {
		return err
	}
	if err := q.proveGUIAbsent(ctx); err != nil {
		return err
	}
	marker, err := readQuiescentAggregateGUIRestartBound(ctx, q.stateDir, q.namespace)
	if err != nil || !sameAggregateGUIRestart(q.restart, marker) {
		return errors.Join(errors.New("prior GUI restart changed before Product promotion"), err)
	}
	return q.namespace.WithHubRouteState(ctx, func(h *api.HubRouteState) error {
		actual, err := h.ReadEndpoint()
		if err != nil || !sameOriginalAggregateEndpoint(actual, q.endpoint) {
			return errors.Join(errors.New("original Hub endpoint changed before Product promotion"), err)
		}
		return proveProductAggregateVacancy(ctx, h, actual.Endpoint())
	})
}

// Called only by the existing post-Prepare callback. All inputs have already
// been retained/read back by the real Product owner; no H or settings I/O runs.
func (q *productAggregateQualification) capturePrepared(txn *WindowsProductTxn) error {
	if q == nil || q.prepared != nil || txn == nil || txn.prepared == nil {
		return errors.New("original prepared aggregate Product unavailable")
	}
	p := txn.prepared
	j := p.journal
	if p.entryBundle == nil || j.EntryBundle == nil || j.Phase != WindowsProductRecoveryPrepared || j.Mode != WindowsProductPairModeUpgrade ||
		j.TransactionID == "" || len(p.lastJournalBytes) == 0 || !sameWindowsProductPath(txn.Opts.StateDir, q.stateDir) ||
		j.EntryBundle.PriorRuntime == nil || !j.EntryBundle.PriorRuntime.Present || !sameWindowsProductPath(j.EntryBundle.PriorRuntime.Path, q.priorPath) ||
		p.receiptV3Prior.present != j.Receipts.V3.PriorPresent || !bytes.Equal(p.receiptV3Prior.body, j.Receipts.V3.PriorBytes) ||
		!sameWindowsProductPath(p.receiptV3Prior.path, j.Receipts.V3.Path) {
		return errors.New("native Product Prepare does not match the original aggregate reservation")
	}
	entry := currentEntrySetReceipt(j)
	successor, scope, err := aggregateProductQualificationDigests(q.stateDir, p.staged.SHA256, entry)
	if err != nil {
		return err
	}
	if p.staged.SHA256 != j.Product.NewSHA256 || p.staged.SHA256 != p.entryBundle.snapshot.Product.SHA256 {
		return errors.New("prepared native successor Product digest differs")
	}
	var old api.HubRouteQualification
	if p.receiptV3Prior.present {
		receipt, err := DecodeUpgradeReceipt(p.receiptV3Prior.body)
		if err != nil || receipt.V3 == nil {
			return errors.Join(errors.New("native prior receipt is not a valid V3 preimage"), err)
		}
		r := receipt.V3
		if r.Admission == UpgradeAdmissionCanonicalEntryProductV2 && r.Mode == WindowsProductPairModeUpgrade && r.EntryBundle != nil {
			e := r.EntryBundle
			if !sameWindowsProductPath(e.CanonicalEntryPath, entry.CanonicalEntryPath) || !sameWindowsProductPath(e.ProductPath, q.priorPath) ||
				r.Artifact.SHA256 != j.EntryBundle.PriorRuntime.SHA256 || !matchesAggregatePriorMember(p.entryBundle, e.CanonicalEntryPath, e.CanonicalEntrySHA256) ||
				!matchesAggregatePriorMember(p.entryBundle, e.IdentityPath, e.IdentitySHA256) {
				return errors.New("native V3 prior receipt does not match its actual original entry bundle")
			}
			oldSucc, oldScope, err := aggregateProductQualificationDigests(q.stateDir, r.Artifact.SHA256, e)
			if err != nil {
				return err
			}
			old = api.HubRouteQualification{TransactionID: r.TransactionID, SuccessorSHA256: oldSucc, ScopeSHA256: oldScope}
		}
	}
	if q.serving != nil {
		if old.TransactionID != q.priorSelection.TransactionID || old.ScopeSHA256 != q.priorSelection.ScopeSHA256 ||
			old.SuccessorSHA256 != q.priorSelection.SuccessorSHA256 || old.TransactionID != q.serving.PriorTransactionID || fmt.Sprintf("%x", sha256.Sum256(p.receiptV3Prior.body)) != q.serving.PriorReceiptSHA256 ||
			j.PriorServingAggregate == nil || *j.PriorServingAggregate != *q.serving || j.PreSettlement == nil || j.PreSettlement.Supervisor.Generation == nil {
			return errors.New("prepared Product does not own the selected prior serving aggregate")
		}
		old = q.priorSelection
		generation := j.PreSettlement.Supervisor.Generation
		if generation.PID != q.serving.Supervisor.PID || generation.StartedAt != q.serving.Supervisor.StartedAt {
			return errors.New("prepared prior Supervisor differs from serving aggregate owner")
		}
	}
	q.prepared = p
	q.priorReceipt = p.receiptV3Prior
	q.priorReceipt.body = bytes.Clone(p.receiptV3Prior.body)
	q.priorSelection = old
	q.next = api.HubRouteQualification{TransactionID: j.TransactionID, SuccessorSHA256: successor, ScopeSHA256: scope,
		EndpointAuthoritySHA256: q.endpoint.AuthoritySHA256(), EndpointProvenanceSHA256: q.endpoint.ProvenanceSHA256()}
	return nil
}

func matchesAggregatePriorMember(p *windowsCanonicalEntryBundlePreparedV2, path, digest string) bool {
	root := filepath.Dir(p.targets.CanonicalEntryPath)
	for _, m := range p.observation.Members {
		if m.File.SHA256 == digest && sameWindowsProductPath(filepath.Join(root, filepath.FromSlash(m.File.Path)), path) {
			return true
		}
	}
	return false
}

// The future Route receiver derives the same two projections from its locally
// verified terminal V3 receipt, using these same Product-owned encodings.
func aggregateProductQualificationDigests(stateDir, productSHA string, entry *windowsCanonicalEntrySetReceiptV2) (successor, scope string, err error) {
	if entry == nil || entry.Schema != binaryadmission.WindowsCanonicalEntrySetSchemaV2 || entry.BuildID == "" ||
		!validLowerHexSHA256(entry.RegistryProjectionSHA256) || !validLowerHexSHA256(entry.CanonicalEntrySHA256) || !validLowerHexSHA256(productSHA) || !validLowerHexSHA256(entry.IdentitySHA256) ||
		!cleanAbsolutePath(stateDir) || !cleanAbsolutePath(entry.CanonicalEntryPath) || !cleanAbsolutePath(entry.ProductPath) {
		return "", "", errors.New("original aggregate Product qualification projection is incomplete")
	}
	// JSON arrays encode exact field order and boundaries, with separate domains.
	staged, err := json.Marshal([6]string{"hub-route-product-successor-v1", entry.BuildID, entry.RegistryProjectionSHA256, entry.CanonicalEntrySHA256, productSHA, entry.IdentitySHA256})
	if err != nil {
		return "", "", err
	}
	namespace, err := json.Marshal([4]string{"hub-route-product-scope-v1", normalizeWindowsProductPath(stateDir), normalizeWindowsProductPath(entry.CanonicalEntryPath), normalizeWindowsProductPath(entry.ProductPath)})
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(staged)), fmt.Sprintf("%x", sha256.Sum256(namespace)), nil
}

func normalizeWindowsProductPath(path string) string { return strings.ToLower(filepath.Clean(path)) }

// The original transaction calls this guard only after successor control and
// Q readiness. Hold the H and settings scopes through the original writer's
// real return. Failures before that invocation remain reversible.
func (q *productAggregateQualification) beforeDecision(ctx context.Context, write func() error) error {
	if q == nil || q.namespace == nil || q.prepared == nil || write == nil {
		return errors.New("aggregate Product qualification lacks its original prepared decision")
	}
	p := q.prepared
	if p.journal.TransactionID != q.next.TransactionID || p.journal.Phase != WindowsProductRecoverySuccessorStarting ||
		p.successorLaunch == nil || p.successorBinding.proof == nil || p.deps.WaitSupervisorReady == nil {
		return errors.New("original aggregate Product successor control and readiness are unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
	defer cancel()
	if err := q.proveGUIAbsent(bounded); err != nil {
		return err
	}
	marker, err := readQuiescentAggregateGUIRestartBound(bounded, q.stateDir, q.namespace)
	if err != nil || !sameAggregateGUIRestart(q.restart, marker) {
		return errors.Join(errors.New("original GUI restart marker changed before Product decision"), err)
	}
	path := api.SettingsPath()
	return q.namespace.WithHubRouteState(bounded, func(h *api.HubRouteState) error {
		return api.NewAPI().WithSavedHubEndpointGateBoundIn(bounded, path, q.namespace, func(enabled bool) error {
			if !enabled {
				return errors.New("saved aggregate gate changed before Product decision")
			}
			// Read the same captured V3 leaf again while still holding the Product
			// fence. A missing receipt is valid only when Prepare captured absence.
			raw, err := p.deps.ReadReceipt(q.priorReceipt.path)
			if (q.priorReceipt.present && (err != nil || !bytes.Equal(raw, q.priorReceipt.body))) ||
				(!q.priorReceipt.present && !errors.Is(err, os.ErrNotExist)) {
				return errors.Join(errors.New("native prior Product receipt changed before aggregate decision"), err)
			}
			current, err := h.ReadEndpoint()
			if err != nil || !sameOriginalAggregateEndpoint(current, q.endpoint) {
				return errors.Join(errors.New("protected Hub endpoint changed before Product decision"), err)
			}
			if err := proveProductAggregateVacancy(bounded, h, current.Endpoint()); err != nil {
				return err
			}
			previous, witness := q.priorSelection, current.Witness()
			if witness != "" && witness != previous.TransactionID {
				return errors.New("protected Hub endpoint witness does not match the actual terminal prior Product")
			}
			if previous.TransactionID != "" {
				held, present, err := h.ReadQualification(previous.TransactionID)
				if err != nil {
					return err
				}
				if q.serving != nil && (!present || held != previous) {
					return errors.New("original serving aggregate qualification changed before Product decision")
				}
				if witness != "" && !present {
					return errors.New("protected Hub endpoint witness has no applicable prior qualification")
				}
				if present && (held.SuccessorSHA256 != previous.SuccessorSHA256 || held.ScopeSHA256 != previous.ScopeSHA256 ||
					held.EndpointAuthoritySHA256 != current.AuthoritySHA256() || (held.EndpointProvenanceSHA256 != current.ProvenanceSHA256() && witness != previous.TransactionID)) {
					return errors.New("prior qualification differs from the actual terminal Product or endpoint")
				}
			}
			proof := p.successorBinding.proof
			if proof.issuer != p.successorLaunch || proof.validateCreator == nil || proof.originalOutcome == nil ||
				!sameWindowsProductPath(proof.expected.StateDir, q.stateDir) || !sameWindowsProductPath(proof.expected.ProductPath, p.journal.Product.Target) ||
				proof.expected.ProductSHA256 != p.staged.SHA256 {
				return errSupervisorControlBinding
			}
			if err := proof.validateCreator(); err != nil {
				return errors.Join(errSupervisorControlBinding, err)
			}
			if exited, err := proof.originalOutcome(); exited || err != nil {
				return errors.Join(errSupervisorControlBinding, err)
			}
			if current, err := api.ReadOccupiedSupervisorLockOwner(q.stateDir); err != nil || current != proof.owner {
				return errors.Join(errSupervisorControlBinding, err)
			}
			if err := q.namespace.Validate(bounded); err != nil {
				return err
			}
			if err := h.StageQualification(q.priorSelection.TransactionID, q.endpoint, q.next); err != nil {
				return err
			}
			if err := q.namespace.Validate(bounded); err != nil {
				return err
			}
			return write()
		})
	})
}

func applySettledProductAggregate(ctx context.Context, stateDir, target, transactionID string,
	input windowsCanonicalEntryCommandInput, binding SupervisorControlBinding, endpoint api.HubEndpoint,
) error {
	bounded, cancel := context.WithTimeout(ctx, api.SupervisorAggregateStatusTimeout)
	defer cancel()
	proof := binding.proof
	if proof == nil || !samePath(proof.expected.StateDir, stateDir) || proof.owner.PID <= 0 || transactionID == "" {
		return errSupervisorControlBinding
	}
	return withCurrentAggregateReceipt(bounded, stateDir, target, input,
		func(_ *binaryadmission.WindowsCanonicalEntryDispatchSetV2, receipt *UpgradeReceiptV3, digest string) error {
			if receipt.TransactionID != transactionID || receipt.Artifact.SHA256 != proof.expected.ProductSHA256 ||
				!samePath(receipt.EntryBundle.ProductPath, proof.expected.ProductPath) {
				return errors.New("settled Product receipt does not belong to the original successor")
			}
			check := func(ctx context.Context) error {
				owner, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
				if err != nil || proof.owner != owner {
					return errors.Join(errSupervisorControlGenerationChanged, err)
				}
				return ctx.Err()
			}
			if err := check(bounded); err != nil {
				return err
			}
			result, err := api.ApplySupervisorAggregate(bounded, stateDir, proof.owner, proof.peer, check,
				api.RouteAggregateApply{ReceiptSHA256: digest, EndpointSHA256: hubAggregateEndpointSHA(endpoint)})
			if err != nil {
				return err
			}
			if result.Ownership != api.RouteAggregateOwned || result.Effective != "ready" {
				return errors.New("original Route did not attest ready aggregate serving")
			}
			return nil
		})
}
