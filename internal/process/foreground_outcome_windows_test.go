//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestForegroundOutcomeAcquisitionReturns(t *testing.T) {
	failure := errors.New("synthetic acquisition failure")
	closeFailure := errors.New("synthetic Job close failure")
	for _, mode := range []string{"invalid", "cancelled", "no-job", "partial-job", "no-child"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cmd := exec.Command(`C:\synthetic-never-executed.exe`)
			if mode == "invalid" {
				cmd = nil
			}
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			creates, starts, closes := 0, 0, 0
			deps := foregroundDependencies{
				newJob: func(options JobOptions) (*Job, error) {
					creates++
					if !options.KillOnClose || !options.BreakawayOK {
						t.Fatal("changed Job options")
					}
					if mode == "no-job" {
						return nil, failure
					}
					return newJobObserved(options, jobAcquisitionOps{
						create: func() (windows.Handle, error) { return 101, nil },
						configure: func(windows.Handle, *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
							if mode == "partial-job" {
								return failure
							}
							return nil
						},
						close: func(windows.Handle) error { closes++; return closeFailure },
					})
				},
				start: func(*Job, *exec.Cmd, *os.File, *os.File, *os.File) (*AtCreateJobProcess, error) {
					starts++
					return nil, failure
				},
			}
			result, err := runForeground(ctx, cmd, os.Stdin, os.Stdout, os.Stderr, deps)
			if err == nil || result.Settlement.Complete() || result.Original != nil {
				t.Fatalf("unexpected result: error=%v", err)
			}
			if mode == "invalid" || mode == "cancelled" {
				if creates != 0 || result.Outcome.AcquisitionEntered || len(result.Outcome.Pending) != 0 {
					t.Fatal("preflight acquired an owner")
				}
				if mode == "cancelled" && (!result.Cancelled || !errors.Is(err, context.Canceled)) {
					t.Fatal("pre-cancel lost")
				}
				return
			}
			if creates != 1 || !result.Outcome.AcquisitionEntered {
				t.Fatal("acquisition observation lost")
			}
			if mode == "no-job" {
				if result.Outcome.JobAcquired || closes != 0 || starts != 0 {
					t.Fatal("invented partial Job")
				}
				return
			}
			if !errors.Is(err, failure) || !errors.Is(err, closeFailure) || closes != 1 {
				t.Fatalf("original causes/close count lost: %v %d", err, closes)
			}
			if result.Outcome.JobClose.ClosedKnown || result.Outcome.JobClose.Closed || !reflect.DeepEqual(result.Outcome.Pending, []string{"job-handle"}) {
				t.Fatalf("failed close projected as success: %+v", result.Outcome.JobClose)
			}
			before := closes
			_ = projectForegroundOutcome(result)
			if closes != before {
				t.Fatal("projection retried close")
			}
			if mode == "partial-job" && starts != 0 || mode == "no-child" && starts != 1 {
				t.Fatal("start attempts changed")
			}
		})
	}
}

func TestForegroundOutcomeRetainedMemberCloseAndSnapshot(t *testing.T) {
	failure := errors.New("synthetic member close failure; no native handle exists")
	calls := map[string]int{}
	job := &Job{handle: 101}
	original := containedJobOriginal{job: job, handle: 102, pid: 21, atCreate: true, required: true}
	_, err := installOriginalMemberSource(job, func() (originalMemberRootBinding, error) {
		return originalMemberRootBinding{job: job, original: original}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job.terminationOps = &jobTerminationOps{
		accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
			calls["account"]++
			return jobObjectBasicAccountingInformation{TotalProcesses: 3}, nil
		},
		listMembers: func(*Job) (jobProcessIDList, error) {
			calls["list"]++
			return jobProcessIDList{Assigned: 2, PIDs: []uint32{22, 23}}, nil
		},
		openProcess: func(pid uint32) (windows.Handle, error) { calls["open"]++; return windows.Handle(pid), nil },
		isMember:    func(windows.Handle, *Job) (bool, error) { calls["member"]++; return true, nil },
		wait:        func(windows.Handle) (uint32, error) { calls["wait"]++; return uint32(windows.WAIT_OBJECT_0), nil },
		terminate:   func(*Job) error { t.Fatal("unexpected termination"); return nil },
		closeProcess: func(handle windows.Handle) error {
			calls["close"]++
			if handle == 23 {
				return failure
			}
			return nil
		},
		closeJob: func(windows.Handle) error { t.Fatal("unexpected Job close"); return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observed, policyError := job.completeCreatorAfterExit(ctx, original)
	if !errors.Is(policyError, failure) || calls["close"] != 2 {
		t.Fatalf("policy fixture failed: %v calls=%v", policyError, calls)
	}
	child := &windowsContainedChild{original: original, creationTime: "synthetic-birth", creationFILETIME: 7, observed: true}
	result := ForegroundResult{Job: job, Original: &AtCreateJobProcess{child: child}, Root: OriginalRootWait{Known: true, ExitCode: 37}, Native: ContainedNativeSnapshot{MembersExited: observed.ObservedCurrentMembersExited}}
	result.Outcome = projectForegroundOutcome(result)
	view := result.Outcome
	if !view.MemberObservationPresent || !errors.Is(view.MemberPolicyError, failure) || len(view.Members.Members) != 2 {
		t.Fatalf("retained policy lost: present=%t error=%v", view.MemberObservationPresent, view.MemberPolicyError)
	}
	if !view.Members.Members[0].HandleClosed || view.Members.Members[1].HandleClosed || view.Members.Members[1].ControlledFailure != "member-handle-close-failed" || view.Members.MemberHandlesClosed {
		t.Fatalf("member close facts lost: %+v", view.Members)
	}
	if !view.Members.MemberListQueryKnown || !view.Members.InitialAccountingKnown || !view.Members.FinalAccountingKnown || view.Members.InitialTotal != 3 || view.Identity.NativeBirthFILETIME != 7 {
		t.Fatal("coverage/identity lost")
	}
	baseline := make(map[string]int)
	for k, v := range calls {
		baseline[k] = v
	}
	view.Members.Members[0].HandleClosed = false
	view.Members.CapturedMemberPIDs[0] = 999
	next := projectForegroundOutcome(result)
	if !next.Members.Members[0].HandleClosed || next.Members.CapturedMemberPIDs[0] != 22 || !reflect.DeepEqual(calls, baseline) {
		t.Fatal("projection aliased owner state or entered native policy")
	}
	if result.Settlement.Complete() || result.Root.ExitCode != 37 {
		t.Fatal("observation manufactured authority or changed root")
	}
	if !strings.Contains(strings.Join(next.Pending, ","), "member-handle[pid=23]") {
		t.Fatal("failed member close missing from pending")
	}
}

func TestForegroundOutcomeAbsentAndPresentEmptyRemainDistinct(t *testing.T) {
	for _, job := range []*Job{nil, {}, {memberCustody: &originalMemberCustody{}}} {
		out := projectForegroundOutcome(ForegroundResult{Job: job})
		if out.MemberObservationPresent {
			t.Fatal("missing observation became empty success")
		}
	}
	job := &Job{memberCustody: &originalMemberCustody{}, memberPolicy: &originalMemberPolicyResult{observation: jobTerminationObservation{native: CreatorJobObservation{MemberHandlesClosed: true}}}}
	out := projectForegroundOutcome(ForegroundResult{Job: job})
	if !out.MemberObservationPresent || out.Members.MemberListKnown || out.Members.MemberListQueryKnown || out.Members.InitialAccountingKnown {
		t.Fatal("empty rows or aggregate closure invented coverage")
	}
}

func TestForegroundOutcomePartialChildCloseDistinctions(t *testing.T) {
	nativeFailure, reportedFailure := errors.New("native close"), errors.New("reported close")
	child := &windowsContainedChild{original: containedJobOriginal{handle: 101, pid: 21, atCreate: true}, creationError: errors.New("identity not acquired"), processCloseAttempted: true, processCloseError: nativeFailure,
		threadCloseAttempted: true, inheritedHandles: []originalInheritedHandle{{closeAttempted: true, actual: originalInheritedCloseOutcome{ClosedKnown: true}, reported: reportedFailure}, {closeAttempted: true, actual: originalInheritedCloseOutcome{NativeError: nativeFailure, OwnedKnown: true}}}, attributeClose: ProcThreadAttributeListCloseObservation{Returned: true, AllocationKnown: true, Allocated: true, Error: reportedFailure}}
	out := projectForegroundOutcome(ForegroundResult{Original: &AtCreateJobProcess{child: child}})
	if out.Identity.Known || !errors.Is(out.IdentityError, child.creationError) || !out.ProcessClose.Acquired || out.ProcessClose.ClosedKnown || !out.ThreadClose.ClosedKnown {
		t.Fatal("partial identity/process/thread distinctions lost")
	}
	if len(out.Inherited) != 2 || !out.Inherited[0].ClosedKnown || !errors.Is(out.Inherited[0].ReportedError, reportedFailure) || out.Inherited[1].ClosedKnown || !errors.Is(out.Inherited[1].NativeError, nativeFailure) || !out.AttributesPresent || out.Attributes.Complete() {
		t.Fatal("native/reported/attribute distinctions lost")
	}
}
