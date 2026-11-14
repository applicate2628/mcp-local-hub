//go:build windows

package process

import "fmt"

// ForegroundCloseObservation is evidence only: it exports no native handle or
// close operation. A false ClosedKnown is not successful closure.
type ForegroundCloseObservation struct {
	AcquiredKnown, Acquired, CloseAttempted, ClosedKnown, Closed bool
	Error                                                        error
}

// ForegroundInheritedCloseObservation preserves actual and reported outcomes
// separately. Its index identifies the original acquired slot, never a handle.
type ForegroundInheritedCloseObservation struct {
	CloseAttempted             bool
	NativeError, ReportedError error
	ClosedKnown, OwnedKnown    bool
}

// ForegroundOutcome is a Windows-only snapshot of the retained original owners.
// It cannot construct a settlement receipt or transfer their capabilities.
type ForegroundOutcome struct {
	AdmissionPresent, AdmissionCloseAttempted            bool
	AdmissionCloseError                                  error
	AcquisitionEntered                                   bool
	JobAcquired, OriginalAcquired, ChildObservationKnown bool
	Identity                                             ContainedRootIdentity
	IdentityError                                        error
	JobClose, ProcessClose, ThreadClose, WrapperClose    ForegroundCloseObservation
	Inherited                                            []ForegroundInheritedCloseObservation
	AttributesPresent                                    bool
	Attributes                                           ProcThreadAttributeListCloseObservation
	MemberObservationPresent                             bool
	Members                                              CreatorJobObservation
	MemberPolicyError                                    error
	Pending                                              []string
}

// Cached original member policy only; this starts no observation or native action.
func (j *Job) retainedMemberPolicy() (CreatorJobObservation, bool, error) {
	if j == nil || j.memberCustody == nil {
		return CreatorJobObservation{}, false, nil
	}
	j.memberCustody.mu.Lock()
	defer j.memberCustody.mu.Unlock()
	if j.memberPolicy == nil {
		return CreatorJobObservation{}, false, nil
	}
	return copyCreatorJobObservation(j.memberPolicy.observation.native), true, j.memberPolicy.err
}

func (j *Job) foregroundCloseObservation() ForegroundCloseObservation {
	if j == nil {
		return ForegroundCloseObservation{}
	}
	j.closeMu.Lock()
	defer j.closeMu.Unlock()
	return ForegroundCloseObservation{AcquiredKnown: true, Acquired: true,
		CloseAttempted: j.closeAttempted, ClosedKnown: j.closeAttempted && j.closeError == nil,
		Closed: j.closeAttempted && j.closeError == nil, Error: j.closeError}
}

// projectForegroundOutcome runs synchronously after the original operation has
// returned. It reads cached owner facts only; no native query, wait or close.
func projectForegroundOutcome(result ForegroundResult) ForegroundOutcome {
	out := ForegroundOutcome{AcquisitionEntered: result.Outcome.AcquisitionEntered,
		JobAcquired: result.Job != nil, OriginalAcquired: result.Original != nil}
	out.AdmissionPresent, out.AdmissionCloseAttempted, out.AdmissionCloseError = result.Admission != nil, result.AdmissionCloseAttempted, result.AdmissionCloseError
	if out.AdmissionPresent && (!out.AdmissionCloseAttempted || out.AdmissionCloseError != nil) {
		out.Pending = append(out.Pending, "creation-admission")
	}
	out.JobClose = result.Job.foregroundCloseObservation()
	out.Members, out.MemberObservationPresent, out.MemberPolicyError = result.Job.retainedMemberPolicy()
	pendingClose := func(name string, view ForegroundCloseObservation) {
		if view.Acquired && (!view.ClosedKnown || !view.Closed) {
			out.Pending = append(out.Pending, name)
		}
	}
	pendingClose("job-handle", out.JobClose)
	if result.Original == nil {
		return out
	}
	child, ok := result.Original.child.(*windowsContainedChild)
	if !ok || child == nil {
		out.Pending = append(out.Pending, "original-resource-observation-unknown")
	} else {
		out.ChildObservationKnown = true
		out.Identity, out.IdentityError = child.containedRootIdentity()
		closeView := func(acquired, attempted bool, err error) ForegroundCloseObservation {
			return ForegroundCloseObservation{AcquiredKnown: true, Acquired: acquired,
				CloseAttempted: attempted, ClosedKnown: acquired && attempted && err == nil,
				Closed: acquired && attempted && err == nil, Error: err}
		}
		out.ProcessClose = closeView(child.original.handle != 0 || child.process != 0, child.processCloseAttempted, child.processCloseError)
		out.ThreadClose = closeView(child.thread != 0 || child.threadCloseAttempted, child.threadCloseAttempted, child.threadCloseError)
		out.WrapperClose = closeView(child.wrapperOwned && child.cmd != nil && child.cmd.Process != nil, child.wrapperCloseAttempted, child.wrapperCloseError)
		pendingClose("original-process-handle", out.ProcessClose)
		pendingClose("original-thread-handle", out.ThreadClose)
		pendingClose("process-wrapper", out.WrapperClose)
		for i, slot := range child.inheritedHandles {
			out.Inherited = append(out.Inherited, ForegroundInheritedCloseObservation{CloseAttempted: slot.closeAttempted,
				NativeError: slot.actual.NativeError, ReportedError: slot.reported,
				ClosedKnown: slot.actual.ClosedKnown, OwnedKnown: slot.actual.OwnedKnown})
			if !slot.closeAttempted || !slot.actual.ClosedKnown {
				out.Pending = append(out.Pending, fmt.Sprintf("inherited-standard-handle[%d]", i))
			}
		}
		out.AttributesPresent = child.attributeOwner != nil || child.attributeClose.Returned
		out.Attributes = child.attributeClose
		if out.AttributesPresent && !out.Attributes.Complete() {
			out.Pending = append(out.Pending, "process-thread-attributes")
		}
	}
	if !result.Native.NoChildKnown {
		if !result.Root.Known {
			out.Pending = append(out.Pending, "original-root-wait")
		}
		if !result.Native.MembersExited {
			out.Pending = append(out.Pending, "original-job-members")
		}
		if !result.Native.AccountingKnown {
			out.Pending = append(out.Pending, "original-job-accounting")
		}
	}
	if out.MemberObservationPresent {
		for _, member := range out.Members.Members {
			if !member.HandleClosed {
				out.Pending = append(out.Pending, fmt.Sprintf("member-handle[pid=%d]", member.PID))
			}
		}
	}
	if !result.Settlement.Complete() {
		out.Pending = append(out.Pending, "original-settlement")
	}
	return out
}
