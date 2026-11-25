package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

// The two query-only handles remain with the upgrade through the exact prior
// stop. A persisted observation cannot reconstruct either handle.
type priorAggregateProcesses struct {
	supervisor process.HeldPIDObservation
	route      process.HeldPIDObservation
	owner      api.SupervisorLockOwner
	routeOwner api.SupervisorLockOwner
	peer       api.SupervisorProcessIdentityV1
	closed     bool
	closeErr   error
}

func (p *priorAggregateProcesses) close() error {
	if p == nil {
		return nil
	}
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	if p.route != nil {
		api.ReleaseAndJoin(&p.closeErr, p.route.Close, "close original prior Route")
	}
	if p.supervisor != nil {
		api.ReleaseAndJoin(&p.closeErr, p.supervisor.Close, "close original prior Supervisor")
	}
	return p.closeErr
}

// Wait only on the two original handles. A vacated port or lock can precede
// process exit. Receive both close outcomes before touching the old image.
func (p *priorAggregateProcesses) receiveExit(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if p.closed {
		return errors.Join(ErrWindowsProductRecoveryPending, errors.New("original aggregate process holds already closed"), p.closeErr)
	}
	bounded, cancel := context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
	defer cancel()
	for _, held := range []process.HeldPIDObservation{p.route, p.supervisor} {
		for {
			if err := bounded.Err(); err != nil {
				return errors.Join(ErrWindowsProductRecoveryPending, err)
			}
			wait, err := held.Wait()
			if err != nil {
				return errors.Join(ErrWindowsProductRecoveryPending, err)
			}
			if wait.Result == uint32(windows.WAIT_OBJECT_0) {
				break
			}
			if wait.Result != uint32(windows.WAIT_TIMEOUT) {
				return errors.Join(ErrWindowsProductRecoveryPending, fmt.Errorf("unknown original process wait result %d", wait.Result))
			}
			select {
			case <-bounded.Done():
				return errors.Join(ErrWindowsProductRecoveryPending, bounded.Err())
			case <-time.After(supervisorReadyPollInterval):
			}
		}
	}
	if err := p.close(); err != nil {
		return errors.Join(ErrWindowsProductRecoveryPending, err)
	}
	return nil
}

func holdPriorAggregateProcesses(ctx context.Context, stateDir string, prior *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1, routePID int) (result *priorAggregateProcesses, retErr error) {
	owner, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
	if err != nil || owner.PID <= 0 || routePID <= 0 || owner.PID == routePID {
		return nil, errors.Join(errors.New("distinct original Supervisor and Route required"), err)
	}
	p := &priorAggregateProcesses{owner: owner}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, p.close())
		}
	}()
	p.supervisor, err = process.HoldPIDForObservation(owner.PID)
	if err != nil {
		return nil, err
	}
	p.route, err = process.HoldPIDForObservation(routePID)
	if err != nil {
		return nil, err
	}
	path := prior.Artifact().Path
	p.peer, err = observeOriginalSupervisorRuntime(ctx, path, prior.Artifact().SHA256, owner, prior)
	if err != nil {
		return nil, err
	}
	route, err := p.route.Identity()
	if err != nil || route.PID != routePID || route.StartedAt.IsZero() || !samePath(route.ExecutablePath, path) {
		return nil, errors.Join(errors.New("saved Hub listener has no original Product Route identity"), err)
	}
	p.routeOwner = api.SupervisorLockOwner{PID: routePID, StartedAt: route.StartedAt.UTC().Format(time.RFC3339Nano)}
	if err := p.route.VerifyIdentity(process.PIDIdentityProof{PID: routePID, StartedAt: p.routeOwner.StartedAt, ExecutablePath: path, StartTolerance: time.Nanosecond}); err != nil {
		return nil, err
	}
	caller, err := api.ObserveSupervisorProcessIdentityV1(os.Getpid())
	if err != nil || caller.SessionID == 0 || !strings.EqualFold(caller.UserSID, p.peer.UserSID) || caller.SessionID != p.peer.SessionID {
		return nil, errors.Join(api.ErrSupervisorControlPeerIdentity, err)
	}
	routePeer, err := api.ObserveSupervisorProcessIdentityV1(routePID)
	if err != nil || api.ValidateSupervisorStatusServerIdentityV1(p.routeOwner, routePeer, caller.UserSID, caller.SessionID, path) != nil {
		return nil, errors.Join(api.ErrSupervisorControlPeerIdentity, err)
	}
	for _, held := range []process.HeldPIDObservation{p.supervisor, p.route} {
		if err := verifyUpgradePriorRuntimeHeld(ctx, prior, held); err != nil {
			return nil, err
		}
	}
	if err := p.checkSupervisor(ctx, stateDir); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *priorAggregateProcesses) checkSupervisor(ctx context.Context, stateDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
	if err != nil || owner != p.owner {
		return errors.Join(api.ErrSupervisorControlPeerIdentity, err)
	}
	if err := p.supervisor.VerifyIdentity(process.PIDIdentityProof{PID: owner.PID, StartedAt: owner.StartedAt, ExecutablePath: p.peer.ImagePath, StartTolerance: time.Nanosecond}); err != nil {
		return err
	}
	wait, err := p.supervisor.Wait()
	if err != nil || wait.Result != uint32(windows.WAIT_TIMEOUT) {
		return errors.Join(api.ErrSupervisorControlPeerIdentity, err)
	}
	return nil
}

func (p *priorAggregateProcesses) checkRoute(ctx context.Context, stateDir string, port int) error {
	if err := p.checkSupervisor(ctx, stateDir); err != nil {
		return err
	}
	wait, err := p.route.Wait()
	if err != nil || wait.Result != uint32(windows.WAIT_TIMEOUT) {
		return errors.Join(errors.New("original prior Route is no longer running"), err)
	}
	parent, err := retainedGenerationParentPID(p.routeOwner.PID)
	if err != nil || parent != p.owner.PID {
		return errors.Join(errors.New("original prior Route has a different parent"), err)
	}
	pid, bound, err := api.LoopbackPortOwnerPIDContext(ctx, port)
	if err != nil || !bound || pid != p.routeOwner.PID {
		return errors.Join(errors.New("saved Hub listener owner changed"), err)
	}
	return nil
}

func matchServingRoute(row api.DaemonStatus, path string, original api.SupervisorLockOwner, commandLine string) error {
	d := api.BuildBuiltinRouteDaemon(path, row.Port)
	_, argv := ResolveWindowsConsolePolicy(process.TokenizeWindowsCommandLine(commandLine))
	if row.TaskName != d.TaskName || row.Server != d.Server || row.Daemon != d.Daemon || row.State != "Running" ||
		row.IsMaintenance || row.Workspace != "" || row.PID != original.PID || row.StartedAtInternal != original.StartedAt ||
		row.Port < 1 || row.Port > 65535 || !samePath(row.CommandInternal, path) || !slices.Equal(row.ArgsInternal, d.Args) ||
		len(argv) < 2 || !samePath(argv[0], path) || !slices.Equal(argv[1:], d.Args) {
		return errors.New("saved Hub listener is not the authenticated Supervisor's original built-in Route")
	}
	return nil
}

// The original Route can attest its ownership before any Product lifecycle
// mutation. H is released before the Supervisor receives any stop request.
func (q *productAggregateQualification) observeServing(ctx context.Context, h *api.HubRouteState, endpoint api.HubRouteEndpointSelection, prior *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1, pid int) error {
	if pid != endpoint.Endpoint().PID || endpoint.Witness() == "" {
		return errors.New("saved Hub publication does not identify the original serving Route")
	}
	raw, err := withWindowsProductDefaults(WindowsProductTxnDeps{}).ReadReceipt(filepath.Join(q.stateDir, UpgradeReceiptSchemaV3+".json"))
	if err != nil {
		return err
	}
	old, err := DecodeUpgradeReceipt(raw)
	if err != nil || old.V3 == nil || old.V3.EntryBundle == nil || old.V3.Mode != WindowsProductPairModeUpgrade ||
		old.V3.Admission != UpgradeAdmissionCanonicalEntryProductV2 || old.V3.TransactionID != endpoint.Witness() ||
		old.V3.Artifact.SHA256 != prior.Artifact().SHA256 || !samePath(old.V3.EntryBundle.ProductPath, q.priorPath) {
		return errors.Join(errors.New("saved Hub does not belong to the prior canonical Product receipt"), err)
	}
	successor, scope, err := aggregateProductQualificationDigests(q.stateDir, old.V3.Artifact.SHA256, old.V3.EntryBundle)
	if err != nil {
		return err
	}
	row, present, err := h.ReadQualification(old.V3.TransactionID)
	if err != nil || !present || row.SuccessorSHA256 != successor || row.ScopeSHA256 != scope || row.EndpointAuthoritySHA256 != endpoint.AuthoritySHA256() {
		return errors.Join(errors.New("serving Route lacks its prior Product qualification"), err)
	}
	token, err := h.ObserveReloadToken()
	if err != nil {
		return err
	}
	if q.live == nil {
		q.live, err = holdPriorAggregateProcesses(ctx, q.stateDir, prior, pid)
		if err != nil {
			return err
		}
	} else if !q.reload.Matches(token) {
		return errors.New("prior Route reload object changed during reservation")
	}
	p := q.live
	status, err := p.servingStatus(ctx, q.stateDir, endpoint.Endpoint().Port)
	if err != nil {
		return err
	}
	obs := windowsProductPriorServingAggregateV1{
		Supervisor: p.owner, Route: p.routeOwner, RouteGeneration: status.Generation,
		PriorTransactionID: old.V3.TransactionID, PriorReceiptSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)),
		EndpointAuthoritySHA256: endpoint.AuthoritySHA256(), EndpointProvenanceSHA256: endpoint.ProvenanceSHA256(),
		EndpointIdentity: endpoint.OriginalIdentity(), EndpointRawSHA256: endpoint.OriginalSHA256(),
		QualificationProvenanceSHA256: row.EndpointProvenanceSHA256,
	}
	if q.serving != nil && (*q.serving != obs || q.priorSelection != row) {
		return errors.New("prior serving aggregate changed during reservation")
	}
	q.serving, q.reload, q.priorSelection = &obs, token, row
	return nil
}

func (p *priorAggregateProcesses) servingStatus(ctx context.Context, stateDir string, port int) (api.RouteAggregateStatus, error) {
	check := func(ctx context.Context) error { return p.checkRoute(ctx, stateDir, port) }
	if err := check(ctx); err != nil {
		return api.RouteAggregateStatus{}, err
	}
	route, err := api.ReadSupervisorRouteForPeer(ctx, stateDir, p.owner, p.peer)
	if err != nil {
		return api.RouteAggregateStatus{}, err
	}
	role, err := process.LookupProcessIdentityContext(ctx, p.routeOwner.PID)
	if err != nil {
		return api.RouteAggregateStatus{}, err
	}
	if err := matchServingRoute(route, p.peer.ImagePath, p.routeOwner, role.CommandLine); err != nil {
		return api.RouteAggregateStatus{}, err
	}
	status, err := api.QuerySupervisorAggregateStatus(ctx, stateDir, p.owner, p.peer, check)
	if err != nil || status.Ownership != api.RouteAggregateOwned || status.Effective != "ready" ||
		status.Activation != nil && status.Activation.Creator != p.routeOwner {
		return api.RouteAggregateStatus{}, errors.Join(errors.New("original Route did not attest to serving the prior aggregate"), err)
	}
	return status, nil
}

// Recovery has already received the restored prior bank and its fixed endpoint
// under the protected state namespace. Re-observe both live native processes.
func observeRestoredServingAggregate(ctx context.Context, stateDir string, prior *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1, expected api.SupervisorLockOwner, endpoint api.HubRouteEndpointSelection) (status api.RouteAggregateStatus, retErr error) {
	pid, bound, err := api.LoopbackPortOwnerPIDContext(ctx, endpoint.Endpoint().Port)
	if err != nil || !bound || pid != endpoint.Endpoint().PID || endpoint.Witness() == "" || prior == nil {
		return status, errors.Join(errors.New("restored fixed Hub listener unavailable"), err)
	}
	p, err := holdPriorAggregateProcesses(ctx, stateDir, prior, pid)
	if err != nil {
		return status, err
	}
	defer api.ReleaseAndJoin(&retErr, p.close, "close restored aggregate process observations")
	if p.owner != expected {
		return status, api.ErrSupervisorControlPeerIdentity
	}
	return p.servingStatus(ctx, stateDir, endpoint.Endpoint().Port)
}

// The prepared journal already carries this serving observation. Confirm its
// still-live endpoint and reload object before the first mutating request.
func (q *productAggregateQualification) checkBeforeStop(ctx context.Context) error {
	if err := q.proveGUIAbsent(ctx); err != nil {
		return err
	}
	marker, err := readQuiescentAggregateGUIRestartBound(ctx, q.stateDir, q.namespace)
	if err != nil || !sameAggregateGUIRestart(q.restart, marker) {
		return errors.Join(errors.New("GUI restart changed before original Supervisor stop"), err)
	}
	return q.namespace.WithHubRouteState(ctx, func(h *api.HubRouteState) error {
		endpoint, err := h.ReadEndpoint()
		if err != nil || !sameOriginalAggregateEndpoint(endpoint, q.endpoint) {
			return errors.Join(errors.New("original aggregate endpoint changed before stop"), err)
		}
		qualification, present, err := h.ReadQualification(q.serving.PriorTransactionID)
		if err != nil || !present || qualification != q.priorSelection {
			return errors.Join(errors.New("original aggregate Product qualification changed before stop"), err)
		}
		raw, err := withWindowsProductDefaults(WindowsProductTxnDeps{}).ReadReceipt(filepath.Join(q.stateDir, UpgradeReceiptSchemaV3+".json"))
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != q.serving.PriorReceiptSHA256 {
			return errors.Join(errors.New("prior Product receipt changed before stop"), err)
		}
		token, err := h.ObserveReloadToken()
		if err != nil || !q.reload.Matches(token) {
			return errors.Join(errors.New("original aggregate reload object changed before stop"), err)
		}
		return q.live.checkRoute(ctx, q.stateDir, endpoint.Endpoint().Port)
	})
}

// Every fresh IPC connection is checked against the admitted kernel peer.
// The ordinary reaper may request force after an IPC failure; an identity
// failure in either operation prevents even an attempted force fallback.
type servingAggregateReap struct {
	*v5UpgradeDeps
	live       *priorAggregateProcesses
	beforeStop func(context.Context) error
	stateDir   string
	blocked    error
}

func (r *servingAggregateReap) verifyOriginalPriorReap(ctx context.Context, stateDir string, prior *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1, observed *windowsProductQuiescenceObservationV1) error {
	if r.live == nil || r.live.closed || prior == nil || prior != r.originalRuntime || !samePath(r.stateDir, stateDir) ||
		observed == nil || observed.Supervisor.State != windowsProductSupervisorGracefullySettled || observed.Supervisor.Generation == nil {
		return errors.New("serving prior missing its original reap observation")
	}
	generation := observed.Supervisor.Generation
	if generation.PID != r.live.owner.PID || generation.StartedAt != r.live.owner.StartedAt {
		return api.ErrSupervisorControlPeerIdentity
	}
	if err := r.live.checkSupervisor(ctx, stateDir); err != nil {
		return err
	}
	return verifyUpgradePriorRuntimeHeld(ctx, prior, r.live.supervisor)
}

func (r *servingAggregateReap) send(ctx context.Context, pipe, command string, args map[string]any, timeoutMs int) (api.IPCResponse, error) {
	if r.blocked != nil {
		return api.IPCResponse{}, r.blocked
	}
	if err := r.live.checkSupervisor(ctx, r.stateDir); err != nil {
		r.blocked = err
		return api.IPCResponse{}, err
	}
	send, err := r.peerIPCSender()
	if err != nil || pipe != api.SupervisorIPCAddress(r.stateDir) {
		r.blocked = errors.Join(errors.New("original Supervisor IPC sender unavailable"), err)
		return api.IPCResponse{}, r.blocked
	}
	resp, err := send(ctx, pipe, r.live.owner, r.live.peer, command, args, time.Duration(timeoutMs+5000)*time.Millisecond)
	if err != nil {
		var dispatch *supervisorIPCDispatchError
		if !errors.As(err, &dispatch) || !dispatch.requestWriteEntered || errors.Is(err, api.ErrSupervisorControlPeerIdentity) {
			r.blocked = err
		}
	}
	return resp, err
}

func (r *servingAggregateReap) QuiesceTimers(ctx context.Context, pipe string, timeoutMs int) (api.IPCResponse, error) {
	if r.beforeStop != nil {
		if err := r.beforeStop(ctx); err != nil {
			r.blocked = err
			return api.IPCResponse{}, err
		}
	}
	return r.send(ctx, pipe, "quiesce-timers", map[string]any{"timeout_ms": timeoutMs}, timeoutMs)
}

func (r *servingAggregateReap) ExitGraceful(ctx context.Context, pipe string, timeoutMs int) (api.IPCResponse, error) {
	return r.send(ctx, pipe, "exit", map[string]any{"graceful": true, "timeout_ms": timeoutMs}, timeoutMs)
}

func (r *servingAggregateReap) ForceKillSupervisor(pipe string) error {
	if r.blocked != nil {
		return r.blocked
	}
	if pipe != api.SupervisorIPCAddress(r.stateDir) || r.originalContext == nil {
		return api.ErrSupervisorControlPeerIdentity
	}
	wait, err := r.live.supervisor.Wait()
	if err != nil {
		return err
	}
	if wait.Result == uint32(windows.WAIT_OBJECT_0) {
		return errUpgradeSupervisorAlreadyExited
	}
	if err := r.live.checkSupervisor(r.originalContext, r.stateDir); err != nil {
		return err
	}
	return r.forceKillSupervisorForOwner(r.live.owner)
}

func (a *windowsProductPriorServingAggregateV1) matchesOriginalEndpoint(ep api.HubRouteEndpointSelection) bool {
	return ep.OriginalIdentity() == a.EndpointIdentity && ep.OriginalSHA256() == a.EndpointRawSHA256
}

// Before reversing a prepared, unmoved cohort, cold recovery can receive the
// still-serving original under its GUI reservation. A restarted generation or
// uncertain process/socket/token remains pending.
func stopRecordedServingPrior(ctx context.Context, stateDir string, j windowsProductRecoveryJournalV3, prior *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1) (retErr error) {
	record := j.PriorServingAggregate
	if record == nil || j.Phase != WindowsProductRecoveryPrepared || prior == nil {
		return errors.New("prepared serving prior recovery unavailable")
	}
	ns, err := api.OpenWindowsProductStateNamespace(ctx, stateDir)
	if ns != nil {
		defer api.ReleaseAndJoin(&retErr, ns.Close, "close prior aggregate recovery namespace")
	}
	if err != nil {
		return err
	}
	lease, q, err := reserveAbsentPriorAggregate(ctx, stateDir, prior.Artifact().Path, prior, ns)
	if lease != nil {
		defer func() {
			release := lease.ReleaseErr()
			ns.RetainAfterUnconfirmedRelease(release)
			retErr = errors.Join(retErr, release)
		}()
	}
	if err != nil {
		return err
	}
	defer api.ReleaseAndJoin(&retErr, q.live.close, "close recovered prior aggregate processes")
	if !record.matchesOriginalEndpoint(q.endpoint) || q.endpoint.AuthoritySHA256() != record.EndpointAuthoritySHA256 ||
		q.endpoint.ProvenanceSHA256() != record.EndpointProvenanceSHA256 || q.endpoint.Witness() != record.PriorTransactionID {
		return errors.New("recorded serving prior has a different protected endpoint")
	}
	checkVacancy := func() error {
		return ns.WithHubRouteState(ctx, func(h *api.HubRouteState) error {
			actual, err := h.ReadEndpoint()
			if err != nil || !record.matchesOriginalEndpoint(actual) {
				return errors.Join(errors.New("recorded prior endpoint changed during settlement"), err)
			}
			return proveProductAggregateVacancy(ctx, h, actual.Endpoint())
		})
	}
	frozen := expandWindowsProductFrozenPorts(j.FrozenPorts)
	if q.serving == nil {
		absent, err := observeWindowsProductPairQuiescence(ctx, stateDir, prior.Artifact().Path, frozen)
		if err != nil || absent.Supervisor.State != windowsProductSupervisorKnownAbsent {
			return errors.Join(errors.New("recorded serving prior has no confirmed settlement"), err)
		}
		if err := validateWindowsProductQuiescenceObservation(absent, frozen); err != nil {
			return err
		}
		return checkVacancy()
	}
	if *q.serving != *record {
		return errors.New("a different serving generation occupies the recorded prior")
	}
	deps := buildV5UpgradeDeps(prior.Artifact().Path, stateDir)
	deps.originalRuntime, deps.originalContext = prior, ctx
	pinned := &servingAggregateReap{v5UpgradeDeps: deps, live: q.live, beforeStop: q.checkBeforeStop, stateDir: stateDir}
	ports := make([]int, 0, len(frozen))
	for _, row := range frozen {
		ports = append(ports, row.Port)
	}
	if err := api.ReapSupervisor(ctx, api.SupervisorReapOpts{PipePath: deps.pipePath, Deps: pinned,
		IsAlreadyExited: isAlreadyExitedError, ExpectedPorts: ports, VerifyPortsUnbound: verifyPortsUnboundForUpgrade}); err != nil {
		return err
	}
	if err := deps.WaitSupervisorLockReleased(ctx, defaultSupervisorLockReleaseTimeout); err != nil {
		return err
	}
	if err := q.live.receiveExit(ctx); err != nil {
		return err
	}
	return checkVacancy()
}

// Keep the query-only original handle through aggregate readiness and apply.
func withOriginalSupervisorImage(ctx context.Context, binaryPath, expectedSHA256 string, owner api.SupervisorLockOwner, original *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1, receive func(process.HeldPIDObservation) error) (retErr error) {
	if ctx == nil || original == nil {
		return errors.New("original supervisor runtime and operation context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	artifact := original.Artifact()
	if !cleanAbsolutePath(binaryPath) || !validLowerHexSHA256(expectedSHA256) || !samePath(binaryPath, artifact.Path) || expectedSHA256 != artifact.SHA256 {
		return errors.New("original supervisor runtime path/SHA-256 mismatch")
	}
	held, err := process.HoldPIDForObservation(owner.PID)
	if err != nil {
		return fmt.Errorf("hold original supervisor generation: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, held.Close(), ctx.Err()) }()
	if err := held.VerifyIdentity(process.PIDIdentityProof{
		PID: owner.PID, ExecutablePath: binaryPath, StartedAt: owner.StartedAt, StartTolerance: time.Nanosecond,
	}); err != nil {
		return fmt.Errorf("verify original supervisor generation: %w", err)
	}
	if err := verifyUpgradePriorRuntimeHeld(ctx, original, held); err != nil {
		return fmt.Errorf("validate original supervisor runtime and image: %w", err)
	}
	return receive(held)
}

func requireRunningCommittedSupervisor(held process.HeldPIDObservation) error {
	state, err := held.Wait()
	if err != nil || state.Result != uint32(windows.WAIT_TIMEOUT) {
		return errors.Join(err, errors.New("original committed supervisor is no longer running"))
	}
	return nil
}
