//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/scheduler"
)

func receiveAggregateRollbackJournal(leaf *api.WindowsProductRollbackContinuation, stateDir, target string) (windowsProductRecoveryJournalV3, error) {
	var zero windowsProductRecoveryJournalV3
	if leaf == nil {
		return zero, ErrWindowsProductRecoveryIdentity
	}
	j, err := decodeWindowsProductRecoveryJournalV3(leaf.Bytes())
	if err != nil || j.Phase != WindowsProductRecoverySettledRollback || j.PriorServingAggregate == nil ||
		j.EntryBundle == nil || j.EntryBundle.CleanArchiveSHA256 != "" || j.EntryBundle.Bundle.Disposition.State != "settled" ||
		j.EntryBundle.Bundle.Disposition.OriginalOwnerPending || j.EntryBundle.Bundle.Disposition.DispositionPending ||
		j.EntryBundle.Bundle.Disposition.CloseError != nil || !samePath(j.EntryBundle.CanonicalEntry.Target, target) ||
		!samePath(j.LegacyWindowless.Target, scheduler.WindowsLegacyEntrypointPath(target)) ||
		!samePath(j.Receipts.V3.Path, filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json")) ||
		!samePath(j.Receipts.V2.Path, filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json")) {
		return zero, errors.Join(ErrWindowsProductRecoveryIdentity, err)
	}
	return j, nil
}

// The former V3 is now input for read-only receiving of restored originals.
// No task, file inverse, publication or clean-archive callback is invoked.
func restoreProductPriorAggregate(parent context.Context, stateDir, target string, allowance WindowsProductJournalAllowance, leaf *api.WindowsProductRollbackContinuation) (retErr error) {
	ctx, cancel := context.WithTimeout(parent, 2*defaultSupervisorLockReleaseTimeout+2*api.SupervisorAggregateStatusTimeout)
	defer cancel()
	j, err := receiveAggregateRollbackJournal(leaf, stateDir, target)
	if err != nil {
		return err
	}
	ns, err := api.OpenWindowsProductStateNamespace(ctx, stateDir)
	if ns != nil {
		defer api.ReleaseAndJoin(&retErr, ns.Close, "close rollback aggregate namespace")
	}
	if err != nil {
		return err
	}
	fence, err := api.AcquireUpgradeFenceBound(ctx, stateDir, ns)
	if err != nil {
		return err
	}
	defer func() {
		if fence != nil {
			api.ReleaseAndJoin(&retErr, fence.Release, "release rollback aggregate fence")
		}
	}()
	if err := requireWindowsProductWithoutBackendHold(ctx, stateDir); err != nil {
		return err
	}
	path := filepath.Join(stateDir, api.WindowsProductAggregateRollbackLeaf)
	req := windowsProductRecoveryRequest{Context: ctx, StateDir: stateDir, JournalAllowance: allowance}
	read := func(name string) ([]byte, error) {
		if name != path {
			return nil, ErrWindowsProductRecoveryIdentity
		}
		if err := ns.VerifyAggregateRollback(ctx, leaf); err != nil {
			return nil, err
		}
		return leaf.Bytes(), nil
	}
	s, err := prepareCurrentCanonicalRecoverySource(req, windowsProductRecoveryDeps{ReadJournal: read}, path, j, canonicalRecoveryRollbackCleanup)
	if err != nil {
		return err
	}
	prior, err := s.acquireCurrentSide(ctx, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	var priorCloseErr error
	closePrior := func() error {
		if !s.active {
			return priorCloseErr
		}
		priorCloseErr = errors.Join(s.closeCurrentMembers(prior), s.closeCurrentDirectories())
		s.active = false
		return priorCloseErr
	}
	defer api.ReleaseAndJoin(&retErr, closePrior, "close restored prior originals")
	if err != nil {
		return err
	}
	view, err := prior.RuntimeProduct(ctx)
	if err != nil || view == nil {
		return errors.Join(ErrWindowsProductRecoveryIdentity, err)
	}
	artifact := view.Artifact()
	if !samePath(artifact.Path, j.EntryBundle.PriorRuntime.Path) || artifact.SHA256 != j.EntryBundle.PriorRuntime.SHA256 {
		return ErrWindowsProductRecoveryIdentity
	}
	a := j.PriorServingAggregate
	checkReceipt := func() error {
		if err := ns.VerifyAggregateRollback(ctx, leaf); err != nil {
			return err
		}
		return verifyServingRollbackReceipts(j.Receipts, func(path string) ([]byte, error) {
			return ns.ReadBoundFile(ctx, path, filepath.Base(path))
		})
	}
	if err := checkReceipt(); err != nil {
		return err
	}
	receipt, err := DecodeUpgradeReceipt(j.Receipts.V3.PriorBytes)
	if err != nil {
		return err
	}
	r := receipt.V3
	successor, scope, err := aggregateProductQualificationDigests(stateDir, artifact.SHA256, r.EntryBundle)
	if err != nil {
		return err
	}

	var selection api.RouteAggregateApply
	var owner api.SupervisorLockOwner
	var peer api.SupervisorProcessIdentityV1
	var restored *priorAggregateProcesses
	var publication api.HubRouteEndpointSelection
	var reload api.HubRouteReloadSelection
	defer func() { api.ReleaseAndJoin(&retErr, restored.close, "close restored aggregate generation") }()
	checkEndpoint := func(applied *api.RouteAggregateStatus) error {
		if err := checkReceipt(); err != nil {
			return err
		}
		return ns.WithHubRouteState(ctx, func(h *api.HubRouteState) error {
			return ns.WithSavedHubEndpointGateIn(ctx, api.SettingsPath(), func(enabled bool) error {
				if !enabled {
					return errors.New("saved prior aggregate gate changed")
				}
				ep, err := h.ReadEndpoint()
				if err != nil {
					return err
				}
				row, found, err := h.ReadQualification(a.PriorTransactionID)
				if err != nil || !found || row.TransactionID != a.PriorTransactionID || row.SuccessorSHA256 != successor || row.ScopeSHA256 != scope ||
					row.EndpointAuthoritySHA256 != a.EndpointAuthoritySHA256 || row.EndpointProvenanceSHA256 != a.QualificationProvenanceSHA256 ||
					ep.AuthoritySHA256() != a.EndpointAuthoritySHA256 || ep.Witness() != a.PriorTransactionID {
					return errors.Join(ErrWindowsProductRecoveryIdentity, err)
				}
				selection = api.RouteAggregateApply{ReceiptSHA256: a.PriorReceiptSHA256, EndpointSHA256: hubAggregateEndpointSHA(ep.Endpoint())}
				_, occupied, err := api.LoopbackPortOwnerPIDContext(ctx, ep.Endpoint().Port)
				if err != nil {
					return err
				}
				if !occupied {
					if applied != nil || ep.ProvenanceSHA256() != a.EndpointProvenanceSHA256 || !a.matchesOriginalEndpoint(ep) {
						return ErrWindowsProductRecoveryIdentity
					}
					return h.CheckReloadTokenAbsent()
				}
				current, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
				if err != nil || applied != nil && current != owner {
					return errors.Join(ErrWindowsProductRecoveryIdentity, err)
				}
				var status api.RouteAggregateStatus
				if applied == nil {
					status, err = observeRestoredServingAggregate(ctx, stateDir, view, current, ep)
				} else {
					token, tokenErr := h.ObserveReloadToken()
					if tokenErr != nil {
						return tokenErr
					}
					if restored == nil {
						restored, err = holdPriorAggregateProcesses(ctx, stateDir, view, ep.Endpoint().PID)
						if err != nil {
							return err
						}
						publication, reload = ep, token
					}
					if restored.owner != owner || restored.peer != peer || ep.Endpoint().PID != restored.routeOwner.PID ||
						!sameOriginalAggregateEndpoint(ep, publication) || !reload.Matches(token) {
						return ErrWindowsProductRecoveryIdentity
					}
					status, err = restored.servingStatus(ctx, stateDir, ep.Endpoint().Port)
				}
				if err != nil || status.Ownership != api.RouteAggregateOwned || status.Effective != "ready" ||
					applied != nil && status.Generation != applied.Generation {
					return errors.Join(ErrWindowsProductRecoveryIdentity, err)
				}
				if current == a.Supervisor && status.Generation == a.RouteGeneration && ep.Endpoint().PID == a.Route.PID &&
					(!a.matchesOriginalEndpoint(ep) || ep.ProvenanceSHA256() != a.EndpointProvenanceSHA256) {
					return ErrWindowsProductRecoveryIdentity
				}
				return nil
			})
		})
	}
	if err := checkEndpoint(nil); err != nil {
		return err
	}
	if err := requireWindowsProductWithoutBackendHold(ctx, stateDir); err != nil {
		return err
	}
	// Receive the restored prior through the current original-runtime owner.
	// A fresh start keeps the P06 creator and readiness binding; an already
	// restarted generation receives query-only verification, never stop authority.
	if err := recoverWindowsCanonicalEntryPriorRuntime(ctx, stateDir, j, view); err != nil {
		return err
	}
	owner, err = api.ReadOccupiedSupervisorLockOwner(stateDir)
	if err != nil {
		return err
	}
	var status api.RouteAggregateStatus
	err = withOriginalSupervisorImage(ctx, artifact.Path, artifact.SHA256, owner, view, func(held process.HeldPIDObservation) error {
		peer, err = observeOriginalSupervisorRuntime(ctx, artifact.Path, artifact.SHA256, owner, view)
		if err != nil {
			return err
		}
		check := func(ctx context.Context) error {
			actual, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
			if err != nil || actual != owner {
				return errors.Join(errSupervisorControlGenerationChanged, err)
			}
			return errors.Join(ctx.Err(), requireRunningCommittedSupervisor(held))
		}
		if err := errors.Join(check(ctx), view.ValidateOriginal(ctx), checkReceipt()); err != nil {
			return err
		}
		if err := requireRestoredFleetReady(ctx, stateDir, owner, peer, check); err != nil {
			return err
		}
		// The 0.4.66 Route independently acquires this same Product fence and
		// validates the restored Product's own embedded binding and prior receipt.
		released := fence
		fence = nil
		if err := released.Release(); err != nil {
			return err
		}
		status, err = api.ApplySupervisorAggregate(ctx, stateDir, owner, peer, check, selection)
		if err != nil || status.Ownership != api.RouteAggregateOwned || status.Effective != "ready" {
			return errors.Join(ErrWindowsProductRecoveryPending, err)
		}
		fence, err = api.AcquireUpgradeFenceBound(ctx, stateDir, ns)
		if err != nil {
			return err
		}
		if err := requireWindowsProductWithoutBackendHold(ctx, stateDir); err != nil {
			return err
		}
		if err := errors.Join(check(ctx), view.ValidateOriginal(ctx)); err != nil {
			return err
		}
		// Bind both live images while the original file bank is still available.
		if err := checkEndpoint(&status); err != nil {
			return err
		}
		if err := closePrior(); err != nil {
			return err
		}
		// Many native file handles may have closed. Re-receive the live endpoint,
		// token, serving generation and readiness on the same two process handles.
		if err := requireWindowsProductWithoutBackendHold(ctx, stateDir); err != nil {
			return err
		}
		if err := checkEndpoint(&status); err != nil {
			return err
		}
		checkRoute := func(ctx context.Context) error {
			return restored.checkRoute(ctx, stateDir, publication.Endpoint().Port)
		}
		if err := requireRestoredFleetReady(ctx, stateDir, owner, peer, checkRoute); err != nil {
			return err
		}
		return ns.RetireAggregateRollback(ctx, fence, leaf)
	})
	if err != nil {
		return fmt.Errorf("restore original serving aggregate: %w", err)
	}
	return nil
}

func requireRestoredFleetReady(ctx context.Context, stateDir string, owner api.SupervisorLockOwner, peer api.SupervisorProcessIdentityV1, check func(context.Context) error) error {
	if err := check(ctx); err != nil {
		return err
	}
	ready, err := api.ReadSupervisorControlReadinessForPeerFromStateDir(ctx, stateDir, owner, peer)
	if err != nil || !ready.ReconcileReady {
		return errors.Join(ErrWindowsProductRecoveryPending, errors.New("restored Supervisor reconciliation not ready"), err)
	}
	return check(ctx)
}
