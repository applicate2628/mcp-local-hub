//go:build windows

package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/mcproute"
)

type routeAggregateOwner struct {
	launch    api.RouteAggregateLaunch
	input     windowsCanonicalEntryCommandInput
	listener  *mcproute.HubListener
	selection api.RouteAggregateApply
	revision  uint64
	status    api.RouteAggregateStatus
}

func newRouteAggregateControl(launch api.RouteAggregateLaunch, input windowsCanonicalEntryCommandInput) api.RouteAggregateControl {
	owner := &routeAggregateOwner{
		launch: launch, input: input, status: api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner},
	}
	return api.RouteAggregateControl{Status: owner.current, Apply: owner.apply, Retire: owner.retire}
}

func (o *routeAggregateOwner) current() api.RouteAggregateStatus {
	if o.listener != nil && !o.listener.Alive() && o.status.Effective == "ready" {
		o.set("error")
	}
	return o.status
}

func (o *routeAggregateOwner) set(effective string) api.RouteAggregateStatus {
	o.revision++
	o.status = api.RouteAggregateStatus{Ownership: api.RouteAggregateOwned, Revision: o.revision, Effective: effective}
	return o.status
}

func (o *routeAggregateOwner) retire(ctx context.Context) api.RouteAggregateRetirement {
	if o.listener == nil {
		return api.RouteAggregateRetirement{}
	}
	o.set("error")
	result := o.listener.RetireOriginal(ctx)
	if result.Settled() {
		o.listener = nil
		o.selection = api.RouteAggregateApply{}
		o.status = api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner}
	}
	return result
}

func (o *routeAggregateOwner) apply(operation, attached context.Context, challenge api.RouteAggregateStatusChallenge) api.RouteAggregateStatus {
	selection := *challenge.Apply
	if o.listener != nil {
		if status := o.current(); status.Effective != "ready" {
			return status
		}
		if o.selection == selection && o.listener.Alive() {
			if err := o.withCurrentBindAuthority(operation, challenge, nil,
				func(h *api.HubRouteState, endpoint api.HubRouteEndpointSelection, row api.HubRouteQualification) error {
					serving := o.listener.Endpoint()
					current := endpoint.Endpoint()
					serving.ReconcilePending = current.ReconcilePending
					if current != serving || endpoint.Witness() != row.TransactionID || !o.listener.Alive() {
						return errors.New("current aggregate endpoint changed")
					}
					if err := o.listener.VerifyOriginalReload(h); err != nil {
						return err
					}
					return api.VerifyExistingHubMcpServingInputs(operation, current)
				}); err == nil {
				return o.current()
			}
		}
		return api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner}
	}

	listener, err := mcproute.StartHubListener(operation, api.NewAPI(), mcproute.HubListenerOptions{
		Lifetime: attached,
		BindExisting: func(ctx context.Context, validate func() error) (*api.HubMcpBindResult, error) {
			var result *api.HubMcpBindResult
			err := o.withCurrentBindAuthority(ctx, challenge, validate,
				func(h *api.HubRouteState, endpoint api.HubRouteEndpointSelection, row api.HubRouteQualification) error {
					o.set("starting")
					var err error
					result, err = h.BindExistingHubMcpListener(endpoint, row)
					return err
				})
			return result, err
		},
	})
	if err != nil {
		_ = api.LogHubMcpEvent("error", "route-aggregate-apply-failed", map[string]any{"err": err.Error()})
		if listener != nil {
			o.listener = listener
		}
		if listener != nil || o.status.Effective == "starting" {
			return o.set("error")
		}
		return api.RouteAggregateStatus{Ownership: api.RouteAggregateNotOwner}
	}
	o.listener, o.selection = listener, selection
	return o.set("ready")
}

func hubAggregateEndpointSHA(endpoint api.HubEndpoint) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d|%s", endpoint.Port, endpoint.InstanceID))))
}

func withCurrentAggregateReceipt(ctx context.Context, stateDir, canonical string, input windowsCanonicalEntryCommandInput,
	use func(*binaryadmission.WindowsCanonicalEntryDispatchSetV2, *UpgradeReceiptV3, string) error,
) (retErr error) {
	if input.bindingErr != nil {
		return input.bindingErr
	}
	current, err := binaryadmission.AcquireInstalledWindowsCanonicalEntryDispatchSetV2(ctx, canonical, input.binding)
	if current != nil {
		defer api.ReleaseAndJoin(&retErr, current.Close, "close aggregate Product originals")
	}
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("current original aggregate Product unavailable")
	}
	snapshot, err := current.DispatchSnapshot()
	if err != nil {
		return err
	}
	pending, err := inspectWindowsProductRecoveryLeaves(stateDir)
	if err != nil || pending {
		return errors.Join(errors.New("aggregate role requires a settled Product"), err)
	}
	raw, err := withWindowsProductDefaults(WindowsProductTxnDeps{}).ReadReceipt(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if err != nil {
		return err
	}
	receipt, err := DecodeUpgradeReceipt(raw)
	if err != nil {
		return err
	}
	r := receipt.V3
	if r == nil || r.EntryBundle == nil || r.Admission != UpgradeAdmissionCanonicalEntryProductV2 || r.Mode != WindowsProductPairModeUpgrade ||
		r.Artifact.SHA256 != snapshot.Product.SHA256 {
		return errors.New("current Product aggregate owner receipt unavailable")
	}
	e := r.EntryBundle
	if e.BuildID != snapshot.BuildID || e.RegistryProjectionSHA256 != snapshot.RegistryProjectionSHA256 ||
		e.CanonicalEntrySHA256 != snapshot.CanonicalEntry.SHA256 || e.IdentitySHA256 != snapshot.Identity.SHA256 ||
		!samePath(e.CanonicalEntryPath, snapshot.CanonicalEntry.Path) || !samePath(e.ProductPath, snapshot.Product.Path) ||
		!samePath(e.IdentityPath, snapshot.Identity.Path) {
		return errors.New("current aggregate Product receipt differs from original files")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return use(current, r, fmt.Sprintf("%x", sha256.Sum256(raw)))
}

func readSavedAggregateGate() (bool, error) {
	gate, err := api.NewAPI().SettingsGet("gui_server.hub_endpoint_enabled")
	if err != nil {
		return false, err
	}
	return strconv.ParseBool(gate)
}

func (o *routeAggregateOwner) withCurrentBindAuthority(ctx context.Context, challenge api.RouteAggregateStatusChallenge, validateManifests func() error,
	effect func(*api.HubRouteState, api.HubRouteEndpointSelection, api.HubRouteQualification) error,
) (retErr error) {
	selection := *challenge.Apply
	stateDir, err := api.DaemonStateDirReadOnly()
	if err != nil || !samePath(stateDir, o.launch.StateDir) {
		return errors.Join(errors.New("current original aggregate namespace required"), err)
	}
	namespace, err := api.OpenWindowsProductStateNamespace(ctx, stateDir)
	if namespace != nil {
		defer api.ReleaseAndJoin(&retErr, namespace.Close, "close aggregate Product namespace")
	}
	if err != nil {
		return err
	}
	fence, err := api.AcquireUpgradeFenceBound(ctx, stateDir, namespace)
	if err != nil {
		return err
	}
	defer api.ReleaseAndJoin(&retErr, fence.Release, "release aggregate Product fence")
	target, err := setupTargetPath()
	if err != nil {
		return err
	}
	return withCurrentAggregateReceipt(ctx, stateDir, target, o.input,
		func(current *binaryadmission.WindowsCanonicalEntryDispatchSetV2, receipt *UpgradeReceiptV3, digest string) (retErr error) {
			if digest != selection.ReceiptSHA256 {
				return errors.New("aggregate apply does not match current Product receipt")
			}
			// The exact executing image is the fixed new route-owner composition.
			h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, windows.GetCurrentProcessId())
			if err != nil {
				return err
			}
			defer api.ReleaseAndJoin(&retErr, func() error { return windows.CloseHandle(h) }, "close original Route process handle")
			if err := current.VerifyCurrentProductProcessPath(ctx, uintptr(h)); err != nil {
				return err
			}
			owner, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
			if err != nil || owner != o.launch.Owner {
				return errors.Join(errors.New("original aggregate supervisor generation changed"), err)
			}
			successor, scope, err := aggregateProductQualificationDigests(stateDir, receipt.Artifact.SHA256, receipt.EntryBundle)
			if err != nil {
				return err
			}
			return namespace.WithHubRouteState(ctx, func(h *api.HubRouteState) error {
				if validateManifests != nil {
					if err := validateManifests(); err != nil {
						return fmt.Errorf("bind refused: %w", err)
					}
				}
				return api.NewAPI().WithSavedHubEndpointGateBoundIn(ctx, api.SettingsPath(), namespace, func(enabled bool) error {
					if !enabled {
						return errors.New("saved aggregate gate is not enabled")
					}
					endpoint, err := h.ReadEndpoint()
					if err != nil || hubAggregateEndpointSHA(endpoint.Endpoint()) != selection.EndpointSHA256 {
						return errors.Join(errors.New("saved aggregate endpoint changed"), err)
					}
					row, found, err := h.ReadQualification(receipt.TransactionID)
					if err != nil || !found || row.SuccessorSHA256 != successor || row.ScopeSHA256 != scope ||
						row.EndpointAuthoritySHA256 != endpoint.AuthoritySHA256() ||
						(row.EndpointProvenanceSHA256 != endpoint.ProvenanceSHA256() && endpoint.Witness() != row.TransactionID) {
						return errors.Join(errors.New("current Product lacks matching protected Hub qualification"), err)
					}
					if err := api.VerifyRouteAggregateGeneration(ctx, o.launch, challenge); err != nil {
						return err
					}
					return effect(h, endpoint, row)
				})
			})
		})
}

// The new Windows Product composition never launches a second aggregate from
// ordinary GUI startup or a retry. A restored historical Product has its own code.
func productUsesSupervisedAggregate() bool { return true }
