//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"mcp-local-hub/internal/process"
)

// foregroundReceiving retains the actual operation result through the terminal
// write, with transport failure distinct from execution and settlement errors.
type foregroundReceiving struct {
	Result                          process.ForegroundResult
	ExecutionError, DiagnosticError error
	ExitCode                        int
}

func receiveForeground(result process.ForegroundResult, executionError error, stderr io.Writer) foregroundReceiving {
	received := foregroundReceiving{Result: result, ExecutionError: executionError, ExitCode: foregroundExitCode(result, executionError)}
	if executionError == nil && result.AdmissionCloseError == nil && !result.Cancelled && result.Root.Known && result.Settlement.Complete() && len(result.Outcome.Pending) == 0 {
		return received
	}
	var text strings.Builder
	if executionError != nil {
		fmt.Fprintln(&text, "mcphub launcher:", executionError)
	}
	outcome := result.Outcome
	fmt.Fprintf(&text, "mcphub launcher admission: present=%t close-attempted=%t close-error=%q\n", result.Admission != nil, result.AdmissionCloseAttempted, errorText(result.AdmissionCloseError))
	fmt.Fprintf(&text, "mcphub launcher outcome: acquisition-entered=%t job-acquired=%t original-acquired=%t child-observation-known=%t root-known=%t root-exit=%d root-error=%q cancelled=%t settlement-complete=%t\n", outcome.AcquisitionEntered, outcome.JobAcquired, outcome.OriginalAcquired, outcome.ChildObservationKnown, result.Root.Known, result.Root.ExitCode, errorText(result.Root.Error), result.Cancelled, result.Settlement.Complete())
	fmt.Fprintf(&text, "mcphub launcher identity: known=%t pid=%d started-at=%q birth-filetime=%d error=%q\n", outcome.Identity.Known, outcome.Identity.PID, outcome.Identity.StartedAt, outcome.Identity.NativeBirthFILETIME, errorText(outcome.IdentityError))
	native := result.Native
	fmt.Fprintf(&text, "mcphub launcher native: identity-known=%t pid=%d started-at=%q birth-filetime=%d no-child-known=%t root-wait-known=%t root-exit=%d terminal-action=%q members-exited=%t accounting-known=%t initial-total=%d final-active=%d captured-pids=%v handles-closed=%t error=%q\n", native.RootIdentity.Known, native.RootIdentity.PID, native.RootIdentity.StartedAt, native.RootIdentity.NativeBirthFILETIME, native.NoChildKnown, native.RootWaitKnown, native.RootExitCode, native.TerminalAction, native.MembersExited, native.AccountingKnown, native.InitialTotal, native.FinalActive, native.CapturedMemberPIDs, native.HandlesClosed, errorText(native.Error))
	for _, slot := range []struct {
		name string
		view process.ForegroundCloseObservation
	}{
		{"job", outcome.JobClose}, {"process", outcome.ProcessClose}, {"thread", outcome.ThreadClose}, {"wrapper", outcome.WrapperClose},
	} {
		v := slot.view
		fmt.Fprintf(&text, "mcphub launcher close: resource=%s acquired-known=%t acquired=%t attempted=%t closed-known=%t closed=%t error=%q\n", slot.name, v.AcquiredKnown, v.Acquired, v.CloseAttempted, v.ClosedKnown, v.Closed, errorText(v.Error))
	}
	for i, slot := range outcome.Inherited {
		fmt.Fprintf(&text, "mcphub launcher inherited-close: slot=%d attempted=%t closed-known=%t owned-known=%t native-error=%q reported-error=%q\n", i, slot.CloseAttempted, slot.ClosedKnown, slot.OwnedKnown, errorText(slot.NativeError), errorText(slot.ReportedError))
	}
	a := outcome.Attributes
	fmt.Fprintf(&text, "mcphub launcher attributes: present=%t returned=%t allocation-known=%t allocated=%t initialized=%t delete-attempted=%t delete-returned=%t free-attempted=%t freed=%t error=%q\n", outcome.AttributesPresent, a.Returned, a.AllocationKnown, a.Allocated, a.Initialized, a.DeleteAttempted, a.DeleteReturned, a.FreeAttempted, a.Freed, errorText(a.Error))
	m := outcome.Members
	fmt.Fprintf(&text, "mcphub launcher member-policy: present=%t version=%d initial-accounting-known=%t initial-total=%d initial-active=%d list-known=%t assigned=%d query-known=%t raw-assigned=%d captured-pids=%v root-wait-known=%t root-wait-result=%d final-accounting-known=%t final-total=%d final-active=%d handles-closed=%t error=%q\n", outcome.MemberObservationPresent, m.Version, m.InitialAccountingKnown, m.InitialTotal, m.InitialActive, m.MemberListKnown, m.AssignedMembers, m.MemberListQueryKnown, m.RawAssignedMembers, m.CapturedMemberPIDs, m.OriginalRootWaitKnown, m.OriginalRootWaitResult, m.FinalAccountingKnown, m.FinalTotal, m.FinalActive, m.MemberHandlesClosed, errorText(outcome.MemberPolicyError))
	for _, member := range m.Members {
		fmt.Fprintf(&text, "mcphub launcher member: pid=%d membership-known=%t wait-known=%t wait-result=%d handle-closed=%t failure=%q\n", member.PID, member.OriginalJobMemberKnown, member.WaitKnown, member.WaitResult, member.HandleClosed, member.ControlledFailure)
	}
	fmt.Fprintf(&text, "mcphub launcher pending: %v\n", outcome.Pending)
	if !result.Settlement.Complete() {
		fmt.Fprintln(&text, "mcphub launcher custody: original settlement unproved; capability holder exits with this invocation; no recovery handoff")
	}
	if stderr == nil {
		received.DiagnosticError = errors.New("launcher diagnostic stderr unavailable")
		return received
	}
	n, err := io.WriteString(stderr, text.String())
	if n != text.Len() {
		err = errors.Join(err, io.ErrShortWrite)
	}
	received.DiagnosticError = err
	return received
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
