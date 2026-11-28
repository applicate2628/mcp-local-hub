//go:build windows

package cli

import (
	"context"
	"errors"
	"strings"
	"time"

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
