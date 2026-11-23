//go:build windows

package process

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

func (c *provisionalWindowsChild) observeCreatorIdentity(ctx context.Context) (CreatorIdentitySnapshot, error) {
	return c.observeCreatorIdentityWithReceipt(ctx, nil)
}

func (c *provisionalWindowsChild) observeCreatorIdentityWithReceipt(ctx context.Context, evidence *CreatorIdentityObservation) (CreatorIdentitySnapshot, error) {
	var empty CreatorIdentitySnapshot
	if c != nil && c.memberSource != nil {
		exit, err := c.memberSource.gate.enterQuery(nil)
		if err != nil {
			return empty, err
		}
		defer exit()
	}
	if err := creatorClosureContext(ctx); err != nil {
		return empty, err
	}
	binding, err := c.creatorBinding()
	if err != nil {
		return empty, err
	}
	actual, err := identityFromHandle(binding.pid, c.original.handle)
	if err != nil {
		return empty, err
	}
	birth := actual.CreationTime.UTC().Format(time.RFC3339Nano)
	if birth != binding.startedAt {
		return empty, creatorClosureUnavailable(errors.New("original held birth changed"))
	}
	if evidence != nil {
		evidence.partial = CreatorIdentitySnapshot{Version: 1, OriginalPID: binding.pid,
			OriginalStartedAt: birth, ExecutablePath: actual.ExecutablePath, OriginalJobBound: true}
	}
	if err := creatorClosureContext(ctx); err != nil {
		return empty, err
	}
	var ownerSID string
	if evidence != nil {
		ownerSID, evidence.principal, err = observeProcessHandleOwnerSID(c.original.handle)
		if err != nil {
			return empty, err
		}
		evidence.partial.OwnerSID = ownerSID
	} else {
		sid, err := processHandleOwnerUserSID(c.original.handle)
		if err != nil {
			return empty, err
		}
		if sid == nil {
			return empty, creatorClosureUnavailable(errors.New("original held owner SID unavailable"))
		}
		value, formatting, formatErr := FormatSIDObserved(sid)
		c.ownerSIDFormatting = formatting
		if formatErr != nil {
			return empty, formatErr
		}
		if err := formatting.ValidateInput(sid, value); err != nil {
			return empty, err
		}
		ownerSID = value
	}
	if err := creatorClosureContext(ctx); err != nil {
		return empty, err
	}
	operations := c.job.terminationOperations()
	member, err := operations.isMember(c.original.handle, c.job)
	if err != nil {
		return empty, err
	}
	if evidence != nil {
		evidence.partial.CurrentJobMembershipKnown, evidence.partial.CurrentJobMember = true, member
	}
	status, err := operations.wait(c.original.handle)
	if err != nil {
		return empty, err
	}
	if status != uint32(windows.WAIT_OBJECT_0) && status != uint32(windows.WAIT_TIMEOUT) {
		return empty, creatorClosureUnavailable(errors.New("original held wait result undefined"))
	}
	if evidence != nil {
		evidence.partial.RootWaitKnown, evidence.partial.RootWaitResult = true, status
		evidence.partial.RootAlive = status == uint32(windows.WAIT_TIMEOUT)
	}
	if status == uint32(windows.WAIT_TIMEOUT) && !member {
		return empty, creatorClosureUnavailable(errors.New("live original root is outside its retained Job"))
	}
	if err := creatorClosureContext(ctx); err != nil {
		return empty, err
	}
	return CreatorIdentitySnapshot{Version: 1, OriginalPID: binding.pid, OriginalStartedAt: birth, ExecutablePath: actual.ExecutablePath, OwnerSID: ownerSID, OriginalJobBound: true, CurrentJobMembershipKnown: true, CurrentJobMember: member, RootWaitKnown: true, RootWaitResult: status, RootAlive: status == uint32(windows.WAIT_TIMEOUT)}, nil
}

func (c *provisionalWindowsChild) creatorSIDFormatObservation() *SIDFormatObservation {
	return c.ownerSIDFormatting
}
