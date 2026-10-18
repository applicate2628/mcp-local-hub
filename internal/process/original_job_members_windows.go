//go:build windows

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"golang.org/x/sys/windows"
)

type originalMemberRootBinding struct {
	job       *Job
	original  containedJobOriginal
	startedAt string
}
type originalMemberHeld struct {
	handle   windows.Handle
	row      OriginalJobMemberRowV1
	borrowed bool
}

func installOriginalMemberSource(job *Job, binding func() (originalMemberRootBinding, error)) (*originalMemberSource, error) {
	if job == nil || binding == nil {
		return nil, errors.New("original creator/Job binding required")
	}
	if job.memberCustody != nil {
		return nil, errors.New("original Job custody already registered")
	}
	g := newOriginalMemberCustody()
	source := &originalMemberSource{gate: g}
	g.source = source
	job.memberCustody = g
	source.available = func() error {
		root, err := binding()
		if err != nil {
			return err
		}
		if root.job != job || job.handle == 0 || !root.original.atCreate || root.original.job != job || root.original.handle == 0 || root.original.handle == windows.InvalidHandle || root.original.pid == 0 || root.startedAt == "" {
			return errors.New("original member source is partial, released or unbound")
		}
		return nil
	}
	source.acquire = func(ctx context.Context, o *OriginalJobMemberObservation) error {
		return acquireOriginalJobMemberHandles(ctx, binding, o)
	}
	source.revalidate = func(ctx context.Context, o *OriginalJobMemberObservation, pid uint32) error {
		return revalidateOriginalJobMemberHandles(ctx, binding, o, pid)
	}
	return source, nil
}

func originalSourceAvailable(ctx context.Context, source *originalMemberSource) (OriginalJobMemberSource, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if source == nil || source.gate == nil {
		return nil, errors.New("original source unbound")
	}
	exit, err := source.gate.enterQuery(nil)
	if err != nil {
		return nil, err
	}
	defer exit()
	if source.available == nil {
		return nil, errors.New("original source availability binding missing")
	}
	if err := source.available(); err != nil {
		return nil, err
	}
	return source, ctx.Err()
}

func originalMemberAccountingAndList(ctx context.Context, root originalMemberRootBinding) (jobObjectBasicAccountingInformation, jobProcessIDList, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return jobObjectBasicAccountingInformation{}, jobProcessIDList{}, err
	}
	if root.job == nil || root.job.handle == 0 || !root.original.atCreate || root.original.job != root.job || root.original.handle == 0 || root.original.pid == 0 || root.startedAt == "" {
		return jobObjectBasicAccountingInformation{}, jobProcessIDList{}, errors.New("original member creation binding unavailable")
	}
	ops := root.job.terminationOperations()
	accounting, err := ops.accounting(root.job)
	if err != nil {
		return accounting, jobProcessIDList{}, err
	}
	list, err := ops.listMembers(root.job)
	if err != nil {
		return accounting, list, err
	}
	if accounting.ActiveProcesses == 0 || list.Assigned != accounting.ActiveProcesses || uint32(len(list.PIDs)) != accounting.ActiveProcesses {
		return accounting, list, errors.New("original live Job membership is incomplete or empty")
	}
	sort.Slice(list.PIDs, func(i, j int) bool { return list.PIDs[i] < list.PIDs[j] })
	for i, pid := range list.PIDs {
		if pid == 0 || (i > 0 && list.PIDs[i-1] == pid) {
			return accounting, list, errors.New("original Job membership has zero/duplicate PID")
		}
	}
	return accounting, list, ctx.Err()
}

func readOriginalJobMember(ctx context.Context, root originalMemberRootBinding, pid uint32, handle windows.Handle) (OriginalJobMemberRowV1, error) {
	row := OriginalJobMemberRowV1{PID: pid}
	ops := root.job.terminationOperations()
	held := &heldPIDObservation{heldPIDJobObservation: &heldPIDJobObservation{pid: int(pid), handle: handle, identity: identityFromHandle, verify: verifyHandleIdentity}, ownerSID: processHandleOwnerUserSID}
	// Adapt the existing one-argument Job wait operation to the SAME held read.
	held.wait = func(h windows.Handle, _ uint32) (uint32, error) { return ops.wait(h) }
	native := &PublisherIncarnation{native: PublisherNativeIncarnationV1{PID: pid}}
	wait, err := readHeldPublisherIncarnation(ctx, native, held, publisherCaptureNativeOperations(), false)
	if err != nil {
		return row, err
	}
	member, err := ops.isMember(handle, root.job)
	if err != nil {
		return row, err
	}
	row.NativeBirthFILETIME = native.native.NativeBirthFILETIME
	row.ImagePathSHA256 = native.native.NativeImagePathSHA256
	row.PrincipalSIDSHA256 = native.native.PrincipalSIDSHA256
	row.MembershipDefined = true
	row.IsMember = member
	row.LiveDefined = wait != nil
	row.Live = wait != nil && *wait == uint32(windows.WAIT_TIMEOUT)
	if !row.IsMember || !row.Live {
		return row, errors.New("held original member is not live in its original Job")
	}
	if pid == root.original.pid {
		identity, err := held.Identity()
		if err != nil {
			return row, err
		}
		if identity.StartedAt.UTC().Format(time.RFC3339Nano) != root.startedAt {
			return row, errors.New("borrowed original creator birth differs")
		}
	}
	return row, ctx.Err()
}

func acquireOriginalJobMemberHandles(ctx context.Context, binding func() (originalMemberRootBinding, error), o *OriginalJobMemberObservation) error {
	root, err := binding()
	if err != nil {
		return err
	}
	pre, list, err := originalMemberAccountingAndList(ctx, root)
	if err != nil {
		return err
	}
	o.view.PreTotal = pre.TotalProcesses
	o.view.PreActive = pre.ActiveProcesses
	ops := root.job.terminationOperations()
	for _, pid := range list.PIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		borrowed := pid == root.original.pid
		handle := root.original.handle
		if !borrowed {
			handle, err = ops.openProcess(pid)
			if err != nil {
				return err
			}
			if handle == 0 || handle == windows.InvalidHandle {
				return errors.New("successful member query has no original handle")
			}
		}
		r := &originalMemberResource{view: OriginalJobMemberResourceV1{PID: pid, BorrowedOriginal: borrowed, Acquired: true}, native: &originalMemberHeld{handle: handle, borrowed: borrowed}}
		if !borrowed {
			owned := handle
			r.close = func() error { return ops.closeProcess(owned) }
		}
		o.resources = append(o.resources, r)
		row, err := readOriginalJobMember(ctx, root, pid, handle)
		if err != nil {
			return err
		}
		r.native.(*originalMemberHeld).row = row
		o.view.Rows = append(o.view.Rows, row)
		o.view.Resources = append(o.view.Resources, r.view)
	}
	post, after, err := originalMemberAccountingAndList(ctx, root)
	if err != nil {
		return err
	}
	if pre.TotalProcesses != post.TotalProcesses || pre.ActiveProcesses != post.ActiveProcesses || !sameOriginalMemberPIDs(list.PIDs, after.PIDs) {
		return errors.New("original Job membership changed during acquisition")
	}
	o.view.PostTotal = post.TotalProcesses
	o.view.PostActive = post.ActiveProcesses
	o.view.OriginalCreationSHA256 = sidFormatObservationDigest([]byte(fmt.Sprintf("original-creation-v1|%d|%s", root.original.pid, root.startedAt)))
	o.view.OriginalJobSHA256 = sidFormatObservationDigest([]byte(fmt.Sprintf("original-job-custody-v1|%s|%x", o.view.OriginalCreationSHA256, root.job.handle)))
	raw, _ := json.Marshal(o.view.Rows)
	o.view.MembershipSHA256 = sidFormatObservationDigest(raw)
	return ctx.Err()
}

func sameOriginalMemberPIDs(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func revalidateOriginalJobMemberHandles(ctx context.Context, binding func() (originalMemberRootBinding, error), o *OriginalJobMemberObservation, pid uint32) error {
	root, err := binding()
	if err != nil {
		return err
	}
	before, list, err := originalMemberAccountingAndList(ctx, root)
	if err != nil {
		return err
	}
	if before.TotalProcesses != o.view.PreTotal || before.ActiveProcesses != o.view.PreActive || len(list.PIDs) != len(o.resources) {
		return errors.New("original Job accounting changed")
	}
	matched := pid == 0
	for i, r := range o.resources {
		if list.PIDs[i] != r.view.PID {
			return errors.New("original Job member set changed")
		}
		held, ok := r.native.(*originalMemberHeld)
		if !ok || held.handle == 0 {
			return errors.New("original held member binding missing")
		}
		row, err := readOriginalJobMember(ctx, root, r.view.PID, held.handle)
		if err != nil {
			return err
		}
		if row != held.row {
			return errors.New("same held original member identity changed")
		}
		if row.PID == pid {
			matched = true
		}
	}
	post, after, err := originalMemberAccountingAndList(ctx, root)
	if err != nil {
		return err
	}
	if before.TotalProcesses != post.TotalProcesses || before.ActiveProcesses != post.ActiveProcesses || !sameOriginalMemberPIDs(list.PIDs, after.PIDs) {
		return errors.New("original membership changed during revalidation")
	}
	if !matched {
		return errors.New("TCP owner is not a retained live original member")
	}
	return ctx.Err()
}
