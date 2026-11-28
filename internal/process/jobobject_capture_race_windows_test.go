//go:build windows

package process

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// All native operations are injected. The missing PID is never reopened and
// borrowed root custody is never closed by the current-member observer.
func TestJobCreatorObservationMemberDisappearsBeforeOpen(t *testing.T) {
	for _, name := range []string{
		"read-only", "after-exit", "retained-member",
		"access-denied", "invalid-handle", "zero-handle", "handle-and-error", "missing-original", "original-pid",
		"query-error", "still-listed", "partial-list", "malformed-empty-list", "query-canceled",
		"active-canceled", "expired-after-empty-query", "lifetime-drift", "final-lifetime-drift", "final-active", "final-accounting",
		"pending-original", "retained-member-pending", "retained-member-nonmember", "retained-member-close",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cause := errors.New("synthetic original Job observation failure")
			withMember := name == "retained-member" || name == "retained-member-pending" || name == "retained-member-nonmember" || name == "retained-member-close"
			pids := []uint32{11}
			if withMember {
				pids = []uint32{13, 11}
			}
			total := uint32(len(pids) + 1)
			accountingCalls, listCalls, terminated, closed, rootWaits, memberWaits := 0, 0, 0, 0, 0, 0
			opened := make(map[uint32]int)
			job := &Job{handle: windows.Handle(1)}
			original := containedJobOriginal{job: job, handle: windows.Handle(2), pid: 7, atCreate: true, required: true}
			if name == "missing-original" {
				original.handle = 0
			}
			if name == "original-pid" {
				original.pid = 11
			}
			job.terminationOps = &jobTerminationOps{
				accounting: func(got *Job) (jobObjectBasicAccountingInformation, error) {
					if got != job || got.handle != windows.Handle(1) {
						t.Fatal("accounting did not use the original Job")
					}
					accountingCalls++
					info := jobObjectBasicAccountingInformation{TotalProcesses: total}
					if accountingCalls == 1 {
						info.ActiveProcesses = uint32(len(pids))
					}
					if accountingCalls >= 2 && name == "active-canceled" {
						info.ActiveProcesses = 1
						cancel()
					}
					if accountingCalls == 2 && name == "lifetime-drift" || accountingCalls == 3 && name == "final-lifetime-drift" {
						info.TotalProcesses++
					}
					if accountingCalls == 3 && name == "final-active" {
						info.ActiveProcesses = 1
					}
					if accountingCalls == 3 && name == "final-accounting" {
						return jobObjectBasicAccountingInformation{}, cause
					}
					return info, nil
				},
				listMembers: func(got *Job) (jobProcessIDList, error) {
					if got != job || got.handle != windows.Handle(1) {
						t.Fatal("member list did not use the original Job")
					}
					listCalls++
					if listCalls == 1 || name == "still-listed" {
						return jobProcessIDList{Assigned: uint32(len(pids)), PIDs: append([]uint32(nil), pids...)}, nil
					}
					switch name {
					case "query-error":
						return jobProcessIDList{}, cause
					case "partial-list":
						return jobProcessIDList{Assigned: 1}, nil
					case "malformed-empty-list":
						return jobProcessIDList{PIDs: []uint32{11}}, nil
					case "query-canceled":
						cancel()
					}
					return jobProcessIDList{}, nil
				},
				openProcess: func(pid uint32) (windows.Handle, error) {
					opened[pid]++
					if opened[pid] != 1 {
						t.Fatal("capture retried a PID")
					}
					if pid == 13 {
						return windows.Handle(3), nil
					}
					if pid != 11 {
						t.Fatal("capture opened a PID outside the original Job list")
					}
					switch name {
					case "access-denied":
						return 0, windows.ERROR_ACCESS_DENIED
					case "invalid-handle":
						return 0, windows.ERROR_INVALID_HANDLE
					case "zero-handle":
						return 0, nil
					case "handle-and-error":
						return windows.Handle(4), windows.ERROR_INVALID_PARAMETER
					}
					return 0, windows.ERROR_INVALID_PARAMETER
				},
				isMember: func(handle windows.Handle, got *Job) (bool, error) {
					if handle != windows.Handle(3) || got != job {
						t.Fatal("member identity did not use the captured handle and original Job")
					}
					return name != "retained-member-nonmember", nil
				},
				wait: func(handle windows.Handle) (uint32, error) {
					switch handle {
					case windows.Handle(2):
						rootWaits++
						if name == "pending-original" {
							return uint32(windows.WAIT_TIMEOUT), nil
						}
					case windows.Handle(3):
						memberWaits++
						if name == "retained-member-pending" {
							cancel()
							return uint32(windows.WAIT_TIMEOUT), nil
						}
					default:
						t.Fatal("wait did not use a retained original handle")
					}
					return uint32(windows.WAIT_OBJECT_0), nil
				},
				terminate: func(got *Job) error {
					if got != job || rootWaits == 0 {
						t.Fatal("after-exit termination bypassed the original root wait")
					}
					terminated++
					return nil
				},
				closeProcess: func(handle windows.Handle) error {
					if handle != windows.Handle(3) {
						t.Fatal("observer closed the borrowed root or an uncaptured member")
					}
					closed++
					if name == "retained-member-close" {
						return cause
					}
					return nil
				},
				closeJob: func(windows.Handle) error {
					t.Fatal("member observer closed the original Job")
					return nil
				},
				now: func() time.Time {
					if name == "expired-after-empty-query" && listCalls > 1 {
						deadline, _ := ctx.Deadline()
						return deadline
					}
					return time.Now()
				},
			}
			var observed jobTerminationObservation
			var err error
			if name == "after-exit" || name == "pending-original" {
				observed, err = job.completeCreatorAfterExit(ctx, original)
			} else {
				observed, err = job.observeCreatorClosure(ctx, original)
			}
			wantClosed := name == "read-only" || name == "after-exit" || name == "retained-member"
			wantTerminated := 0
			if name == "after-exit" {
				wantTerminated = 1
			}
			if terminated != wantTerminated || job.handle != windows.Handle(1) || job.closeAttempted {
				t.Fatal("member observation changed original custody or termination policy")
			}
			if withMember && closed != 1 || !withMember && closed != 0 {
				t.Fatalf("captured handles were not closed exactly once: %d", closed)
			}
			if observed.StrictOriginalsSettled {
				t.Fatal("missing member capture invented complete lifetime coverage")
			}
			if wantClosed {
				native := observed.native
				if err != nil || !observed.ObservedCurrentMembersExited || observed.strictFailure == nil || accountingCalls != 3 || listCalls != 2 || rootWaits == 0 {
					t.Fatalf("original Job closure was not proved: observed=%+v err=%v", observed, err)
				}
				if !errors.Is(observed.strictFailure, windows.ERROR_INVALID_PARAMETER) {
					t.Fatal("original missing-capture cause was lost")
				}
				if !native.InitialAccountingKnown || native.InitialTotal != total || native.InitialActive != uint32(len(pids)) || !native.MemberListQueryKnown || !native.MemberListKnown || native.RawAssignedMembers != uint32(len(pids)) || native.AssignedMembers != uint32(len(pids)) || len(native.CapturedMemberPIDs) != len(pids) {
					t.Fatalf("original accounting/member list was replaced: %+v", native)
				}
				for i, pid := range pids {
					if native.CapturedMemberPIDs[i] != pid {
						t.Fatal("original member PID list was rewritten")
					}
				}
				if !native.OriginalRootWaitKnown || native.OriginalRootWaitResult != uint32(windows.WAIT_OBJECT_0) || !native.FinalAccountingKnown || native.FinalTotal != total || native.FinalActive != 0 || !native.MemberHandlesClosed {
					t.Fatalf("final original wait/accounting/close proof is incomplete: %+v", native)
				}
				if !withMember && len(native.Members) != 0 {
					t.Fatal("missing capture invented a waited or closed member handle")
				}
				if withMember && (memberWaits == 0 || len(native.Members) != 1 || native.Members[0].PID != 13 || !native.Members[0].OriginalJobMemberKnown || !native.Members[0].WaitKnown || native.Members[0].WaitResult != uint32(windows.WAIT_OBJECT_0) || !native.Members[0].HandleClosed) {
					t.Fatal("fresh empty list bypassed the already captured member")
				}
			} else if err == nil || name != "retained-member-close" && observed.ObservedCurrentMembersExited {
				t.Fatalf("missing capture bypassed unsettled original evidence: observed=%+v err=%v", observed, err)
			}
			if name == "retained-member-close" && observed.native.MemberHandlesClosed {
				t.Fatal("failed member close was reported as complete")
			}
			if (name == "query-canceled" || name == "active-canceled" || name == "retained-member-pending") && !errors.Is(err, context.Canceled) {
				t.Fatal("original context cancellation was lost")
			}
			if name == "expired-after-empty-query" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("original deadline bound was lost")
			}
			if (name == "final-accounting" || name == "retained-member-close") && !errors.Is(err, cause) {
				t.Fatal("original observation/close failure was lost")
			}
		})
	}
}
