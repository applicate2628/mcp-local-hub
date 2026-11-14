//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"syscall"
	"unsafe"

	"mcp-local-hub/internal/process"

	"golang.org/x/sys/windows"
)

type jobLimits struct {
	Class    uint32 `json:"class"`
	Bytes    uint32 `json:"bytes"`
	Returned uint32 `json:"returned"`
	Flags    uint32 `json:"flags"`
}

var queryInformationJobObject = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryInformationJobObject")

// The process package owns creation, original process handles, membership and
// terminal cleanup. Its Job exposes its borrowed handle but no limits query.
// Keep the two typed buffers live until direct conversion in annotated Call.
func observeJobLimits(job *process.Job) (jobLimits, error) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var returned uint32
	view := jobLimits{Class: uint32(windows.JobObjectExtendedLimitInformation), Bytes: uint32(unsafe.Sizeof(info))}
	if job == nil || job.Handle() == 0 {
		return view, errors.New("owned Job handle unavailable")
	}
	r, _, callErr := queryInformationJobObject.Call(uintptr(job.Handle()), uintptr(windows.JobObjectExtendedLimitInformation), uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))), uintptr(unsafe.Pointer(&returned)))
	if r == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = syscall.EINVAL
		}
		return view, fmt.Errorf("query owned Job limits: %w", callErr)
	}
	view.Returned = returned
	view.Flags = uint32(info.BasicLimitInformation.LimitFlags)
	if view.Returned != view.Bytes || view.Flags != windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
		return view, fmt.Errorf("outer Job requires exact native layout and KillOnClose without breakaway: %+v", view)
	}
	return view, nil
}

type grantObservation struct {
	Original    process.ContainedRootIdentity            `json:"original"`
	Members     process.OriginalJobMemberSnapshotV1      `json:"members"`
	MemberClose process.OriginalJobMemberCloseSnapshotV1 `json:"memberClose"`
	Limits      jobLimits                                `json:"limits"`
}

// READY is only a correlation request. All granted native facts are obtained
// from the same at-create owner before GRANT, while the child awaits stdin EOF.
func observeGrant(ctx context.Context, owner *process.AtCreateJobProcess, job *process.Job, ready entryReady, expected jobLimits) (native entryNative, observed grantObservation, retErr error) {
	created, err := strconv.ParseUint(ready.Created100ns, 10, 64)
	if err != nil || created == 0 || strconv.FormatUint(created, 10) != ready.Created100ns {
		return native, observed, errors.New("READY has no canonical native creation time")
	}
	start, err := owner.ObserveOriginalStart(ctx)
	if err != nil || !start.Complete() {
		return native, observed, errors.Join(err, errors.New("original at-create start incomplete"))
	}
	if err := start.Validate(ctx, ready.PID, created); err != nil {
		return native, observed, err
	}
	observed.Original = start.Identity()
	source, err := owner.OriginalMemberSource(ctx)
	if err != nil {
		return native, observed, err
	}
	members, err := owner.ObserveOriginalMembers(ctx)
	if members != nil {
		defer func() {
			closed, closeErr := members.CloseObserved()
			if closed != nil {
				observed.MemberClose = closed.Snapshot()
				closeErr = errors.Join(closeErr, closed.Validate(source))
			} else {
				closeErr = errors.Join(closeErr, errors.New("member observation close receipt missing"))
			}
			retErr = errors.Join(retErr, closeErr)
		}()
	}
	if err != nil || members == nil {
		return native, observed, errors.Join(err, errors.New("original member observation unavailable"))
	}
	if err := members.ValidateSource(ctx, source); err != nil {
		return native, observed, err
	}
	observed.Members = members.Snapshot()
	v := observed.Members
	if !v.Complete || v.PreTotal != 1 || v.PreActive != 1 || v.PostTotal != 1 || v.PostActive != 1 || len(v.Rows) != 1 || len(v.Resources) != 1 {
		return native, observed, errors.New("entry must contain exactly its original live process before GRANT")
	}
	row, resource := v.Rows[0], v.Resources[0]
	if row.PID != ready.PID || row.NativeBirthFILETIME != created || !row.MembershipDefined || !row.IsMember || !row.LiveDefined || !row.Live || resource.PID != ready.PID || !resource.BorrowedOriginal || !resource.Acquired || resource.CloseAttempted {
		return native, observed, errors.New("entry membership, generation, liveness or original-handle custody differs")
	}
	observed.Limits, err = observeJobLimits(job)
	if err != nil || observed.Limits != expected {
		return native, observed, errors.Join(err, errors.New("owned Job limits changed before GRANT"))
	}
	if err := ctx.Err(); err != nil {
		return native, observed, err
	}
	native = entryNative{observed.Limits.Class, observed.Limits.Bytes, observed.Limits.Returned, observed.Limits.Flags, row.IsMember, row.Live}
	return native, observed, nil
}
