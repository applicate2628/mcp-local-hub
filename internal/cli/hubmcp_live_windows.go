//go:build windows

package cli

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

func queryInstalledAggregateStatus(parent context.Context, input windowsCanonicalEntryCommandInput) (status api.RouteAggregateStatus, retErr error) {
	if input.bindingErr != nil || input.binding.BuildID() == "" {
		return status, api.ErrSupervisorIPCUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, api.SupervisorAggregateStatusTimeout)
	defer cancel()
	current, launcher, err := resolveUpgradeSelfPaths()
	if err != nil {
		return status, api.ErrSupervisorIPCUnavailable
	}
	stateDir, err := api.DaemonStateDirReadOnly()
	if err != nil {
		return status, api.ErrSupervisorIPCUnavailable
	}
	installed, err := binaryadmission.AcquireInstalledWindowsCanonicalEntryDispatchSetV2(ctx, launcher, input.binding)
	defer func() {
		if err := installed.Close(); retErr == nil && err != nil {
			retErr = api.ErrSupervisorIPCUnavailable
		}
		if retErr != nil {
			status = api.RouteAggregateStatus{}
		}
	}()
	if err != nil {
		return status, aggregateStatusUnavailable(ctx)
	}
	set, err := installed.DispatchSnapshot()
	if err != nil || !samePath(current, set.Product.Path) {
		return status, api.ErrSupervisorIPCUnavailable
	}
	// The current-process pseudo-handle shares the verifier's invalid
	// sentinel. Borrow a real query-only handle for this installed Product.
	self, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, windows.GetCurrentProcessId())
	if err != nil {
		return status, aggregateStatusUnavailable(ctx)
	}
	verifyErr := installed.VerifyCurrentProductProcessPath(ctx, uintptr(self))
	if errors.Join(verifyErr, windows.CloseHandle(self)) != nil {
		return status, aggregateStatusUnavailable(ctx)
	}
	owner, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
	if err != nil {
		return status, aggregateStatusUnavailable(ctx)
	}
	held, err := process.HoldPIDForObservation(owner.PID)
	if err != nil {
		return status, api.ErrSupervisorControlPeerIdentity
	}
	defer func() {
		if err := held.Close(); retErr == nil && err != nil {
			retErr = api.ErrSupervisorIPCUnavailable
		}
		if retErr != nil {
			status = api.RouteAggregateStatus{}
		}
	}()
	// Admission has two different outcomes: current installed file objects
	// and a held live process with a matching native path, role and pipe.
	// Neither this check nor the resulting read-only status attests mapped bytes.
	borrower, ok := held.(process.HeldOriginalProcessHandleBorrower)
	if !ok {
		return status, api.ErrSupervisorControlPeerIdentity
	}
	check := func(ctx context.Context) error {
		currentOwner, err := api.ReadOccupiedSupervisorLockOwner(stateDir)
		if err != nil || currentOwner != owner {
			return api.ErrSupervisorControlPeerIdentity
		}
		if err := held.VerifyIdentity(process.PIDIdentityProof{
			PID: owner.PID, ExecutablePath: set.Product.Path, StartedAt: owner.StartedAt, StartTolerance: time.Nanosecond,
		}); err != nil {
			return err
		}
		wait, err := held.Wait()
		if err != nil || wait.Result != uint32(windows.WAIT_TIMEOUT) {
			return api.ErrSupervisorControlPeerIdentity
		}
		return borrower.BorrowOriginalProcessHandle(func(handle uintptr) error {
			return installed.VerifyCurrentProductProcessPath(ctx, handle)
		})
	}
	if err := check(ctx); err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		return status, api.ErrSupervisorControlPeerIdentity
	}
	identity, err := process.LookupProcessIdentityContext(ctx, owner.PID)
	if err != nil || !isLiveAggregateSupervisorCommand(identity.CommandLine, set.Product.Path) {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		return status, api.ErrSupervisorControlPeerIdentity
	}
	peer, err := api.ObserveSupervisorProcessIdentityV1(owner.PID)
	if err != nil {
		return status, api.ErrSupervisorControlPeerIdentity
	}
	heldSID, err := held.OwnerSID()
	if err != nil || !strings.EqualFold(peer.UserSID, heldSID) {
		return status, api.ErrSupervisorControlPeerIdentity
	}
	caller, err := api.ObserveSupervisorProcessIdentityV1(int(windows.GetCurrentProcessId()))
	if err != nil || caller.SessionID == 0 || !strings.EqualFold(heldSID, caller.UserSID) ||
		peer.SessionID != caller.SessionID ||
		api.ValidateSupervisorStatusServerIdentityV1(owner, peer, caller.UserSID, caller.SessionID, set.Product.Path) != nil {
		return status, api.ErrSupervisorControlPeerIdentity
	}
	return api.QuerySupervisorAggregateStatus(ctx, stateDir, owner, peer, check)
}

func aggregateStatusUnavailable(ctx context.Context) error {
	return errors.Join(ctx.Err(), api.ErrSupervisorIPCUnavailable)
}

// Hold the actual pipe caller and selected installed Product from open
// through admission. The durable owner, pipe identity and held handle must
// still agree each time the Supervisor receives a decision.
func observeRouteAdmissionNativePipePeer(conn net.Conn) (api.SupervisorProcessIdentityV1, routeAdmissionNativeEvidence, error) {
	return api.ObserveSupervisorIPCClientIdentityObserved(conn)
}

func qualifiesObservedRouteOperator(peer, owner api.SupervisorProcessIdentityV1, evidence routeAdmissionNativeEvidence) bool {
	observed, ok := evidence.(*process.WindowsIdentityObservation)
	if !ok || peer.UserSID == "" || peer.SessionID == 0 || peer.SessionID != owner.SessionID ||
		!strings.EqualFold(peer.UserSID, owner.UserSID) || observed.IsLocalSystemPrincipal() {
		return false
	}
	service, _, _, err := windows.LookupSID("", "NT SERVICE\\"+supervisorCstDaemonServiceName)
	if err != nil {
		return errors.Is(err, windows.ERROR_NONE_MAPPED)
	}
	return !observed.MatchesPrincipalSID(service)
}

func holdRouteAdmissionOperator(ctx context.Context, conn net.Conn, s *routeAggregateSupervisor, phases *routeAdmissionNativeObservations) (*routeAdmissionOperator, error) {
	operator := &routeAdmissionOperator{}
	if s == nil || s.entryInput.bindingErr != nil || s.entryInput.binding.BuildID() == "" {
		return operator, api.ErrSupervisorControlPeerIdentity
	}
	peer, peerEvidence, err := observeRouteAdmissionPeer(conn, phases, "operator-open-pipe")
	if err != nil || !qualifiesObservedRouteOperator(peer, s.launch.Peer, peerEvidence) || peer.PID <= 0 || peer.PID == s.launch.Owner.PID {
		return operator, errors.Join(api.ErrSupervisorControlPeerIdentity, err)
	}
	_, launcher, err := resolveUpgradeSelfPaths()
	if err != nil {
		return operator, err
	}
	installed, originalSet, err := binaryadmission.AcquireInstalledWindowsCanonicalEntryDispatchSetV2Observed(ctx, launcher, s.entryInput.binding)
	phases.add("operator-installed-product", originalSet, err)
	if installed != nil {
		operator.productClose = installed.Close
	}
	if err != nil {
		return operator, err
	}
	set, err := installed.DispatchSnapshot()
	if err != nil || !samePath(set.Product.Path, s.launch.Peer.ImagePath) {
		return operator, errors.Join(api.ErrSupervisorControlPeerIdentity, err)
	}
	caller := api.SupervisorLockOwner{PID: peer.PID, StartedAt: peer.CreationTime}
	held, originalOpen, err := process.HoldPIDForObservationObserved(caller.PID)
	phases.add("operator-held-open", originalOpen, err)
	if held != nil {
		operator.callerClose = held.Close
	}
	if err != nil {
		return operator, err
	}
	borrower, ok := held.(process.HeldOriginalProcessHandleBorrower)
	if !ok {
		return operator, api.ErrSupervisorControlPeerIdentity
	}
	operator.check = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		owner, originalLock, err := api.ObserveOccupiedSupervisorLockOwner(ctx, s.launch.StateDir)
		phases.add("operator-occupied-owner", originalLock, err)
		if err != nil || owner != s.launch.Owner {
			return errors.Join(api.ErrSupervisorControlPeerIdentity, err)
		}
		current, _, err := observeRouteAdmissionPeer(conn, phases, "operator-revalidate-pipe")
		if err != nil || api.ValidateSupervisorStatusServerIdentityV1(caller, current, s.launch.Peer.UserSID, s.launch.Peer.SessionID, set.Product.Path) != nil {
			return errors.Join(api.ErrSupervisorControlPeerIdentity, err)
		}
		if err := held.VerifyIdentity(process.PIDIdentityProof{
			PID: caller.PID, ExecutablePath: set.Product.Path, StartedAt: caller.StartedAt, StartTolerance: time.Nanosecond,
		}); err != nil {
			return err
		}
		sid, originalSID, err := process.ObserveHeldPIDOwnerSID(held)
		phases.add("operator-held-sid", originalSID, err)
		if err != nil || !strings.EqualFold(sid, current.UserSID) {
			return errors.Join(api.ErrSupervisorControlPeerIdentity, err)
		}
		wait, err := held.Wait()
		if err != nil || wait.Result != uint32(windows.WAIT_TIMEOUT) {
			return errors.Join(api.ErrSupervisorControlPeerIdentity, err)
		}
		return borrower.BorrowOriginalProcessHandle(func(handle uintptr) error {
			originalImage, err := installed.VerifyCurrentProductProcessPathObserved(ctx, handle)
			phases.add("operator-product-image", originalImage, err)
			return err
		})
	}
	return operator, operator.check(ctx)
}

func routeAdmissionPlatformClosedRead(err error) bool { return errors.Is(err, winio.ErrFileClosed) }

// The aggregate wrapper has already returned from its first legacy Close on
// this accepted pipe. The lower observer receives that original attempt.
func observeRouteAdmissionNativePipeClose(ctx context.Context, conn net.Conn) (routeAdmissionNativePipeReceipt, error) {
	original, ok := conn.(interface {
		CloseObserved(context.Context) (*winio.CloseReceipt, error)
	})
	if !ok {
		return nil, errRouteAdmissionNativePipeCloseUnavailable
	}
	return original.CloseObserved(ctx)
}

func isLiveAggregateSupervisorCommand(commandLine, productPath string) bool {
	argv := process.TokenizeWindowsCommandLine(commandLine)
	if len(argv) < 2 || argv[1] != "supervise" || !cleanAbsolutePath(argv[0]) || !samePath(argv[0], productPath) {
		return false
	}
	flags := newSuperviseCmd().Flags()
	if err := flags.Parse(argv[2:]); err != nil || flags.NArg() != 0 {
		return false
	}
	ensure, ensureErr := flags.GetBool("ensure-alive")
	noIPC, ipcErr := flags.GetBool("no-ipc")
	return ensureErr == nil && ipcErr == nil && !ensure && !noIPC
}
