package api

import (
	"bytes"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// This wrapper deliberately omits the optional create-only recovery capability.
type weeklySchedulerWithoutRecovery struct{ weeklyRefreshTaskScheduler }

func TestRegisterSchedulerPreservesOptionalRecoveryThroughTestHooks(t *testing.T) {
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
	t.Cleanup(InstallTestHooks(
		func() (TestSchedulerIface, error) { return sch, nil },
		func() map[string]TestClientIface { return nil },
		"",
	))
	backend, err := schedulerNewForRegister()
	if err != nil {
		t.Fatal(err)
	}
	creator, ok := backend.(scheduler.XMLCreator)
	if !ok {
		t.Fatal("scheduler composition hid the original create-only recovery capability")
	}
	prior := []byte("<Task>retained prior</Task>")
	if err := creator.CreateXMLIfAbsent(WeeklyRefreshTaskName, prior); err != nil {
		t.Fatal(err)
	}
	if err := creator.CreateXMLIfAbsent(WeeklyRefreshTaskName, []byte("<Task>replacement</Task>")); !errors.Is(err, scheduler.ErrTaskAlreadyExists) {
		t.Fatalf("forwarded capability did not preserve conflict refusal: %v", err)
	}
	if !bytes.Equal(sch.xml[WeeklyRefreshTaskName], prior) || sch.restoreCalls != 2 || sch.importCalls != 0 {
		t.Fatal("forwarded recovery did not act on the original scheduler with create-only semantics")
	}
}

func TestWeeklyRefreshReplacementAdmission(t *testing.T) {
	prior := []byte("<Task>retained prior</Task>")
	for _, tc := range []struct {
		name         string
		xml          []byte
		exportErr    error
		noRecovery   bool
		wantSnapshot bool
		wantCause    error
	}{
		{name: "empty-success", xml: []byte{}, wantCause: ErrWeeklyRefreshSnapshotUnavailable},
		{name: "contradictory-absence", xml: prior, exportErr: scheduler.ErrTaskNotFound, wantCause: ErrWeeklyRefreshSnapshotUnavailable},
		{name: "missing-recovery-capability", xml: prior, noRecovery: true, wantSnapshot: true, wantCause: ErrSchedulerReplacementUnowned},
		{name: "snapshot-cannot-fit", xml: bytes.Repeat([]byte{'x'}, maxStateFileBytes), wantCause: ErrSchedulerReplacementUnowned},
		{name: "existing-definition", xml: prior, wantSnapshot: true, wantCause: ErrSchedulerReplacementUnowned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sch := &weeklyAtomicScheduler{
				xml:                  map[string][]byte{WeeklyRefreshTaskName: bytes.Clone(tc.xml)},
				failExportOnCall:     map[int]error{1: tc.exportErr},
				exportOverrideOnCall: map[int][]byte{1: tc.xml},
			}
			root := filepath.Dir(installWeeklyAtomicHarness(t, sch))
			var backend weeklyRefreshTaskScheduler = sch
			if tc.noRecovery {
				backend = weeklySchedulerWithoutRecovery{sch}
			}
			result, err := runWeeklyRefreshTaskTransaction(backend, weeklyRefreshMutation{
				desired: func([]byte, bool) (scheduler.TaskSpec, error) {
					return weeklyRefreshTaskSpec(filepath.Join(root, "mcphub.exe"), nil), nil
				},
			})
			if err == nil || (tc.wantCause != nil && !errors.Is(err, tc.wantCause)) || result.state != "" || result.restoreStatus != "n/a" {
				t.Fatalf("admission result=%+v err=%v, want unmutated refusal with cause %v", result, err, tc.wantCause)
			}
			if sch.deleteCalls != 0 || sch.createCalls != 0 || sch.restoreCalls != 0 || sch.importCalls != 0 || !bytes.Equal(sch.xml[WeeklyRefreshTaskName], tc.xml) {
				t.Fatalf("admission refusal mutated the task: deletes=%d creates=%d restores=%d imports=%d", sch.deleteCalls, sch.createCalls, sch.restoreCalls, sch.importCalls)
			}
			if tc.wantSnapshot {
				path := readInstallRecoverySnapshot(t, root, WeeklyRefreshTaskName, tc.xml)
				if !strings.Contains(err.Error(), strconv.Quote(path)) {
					t.Fatalf("refusal lost retained snapshot path: %v", err)
				}
			} else if paths, globErr := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json")); globErr != nil || len(paths) != 0 {
				t.Fatalf("unexpected retained snapshots=%v err=%v", paths, globErr)
			}
		})
	}
}

func TestWeeklyRefreshFreshCreateFailureObservation(t *testing.T) {
	foreign := []byte("<Task>external occupant</Task>")
	createCause := errors.New("fixture create reply failed")
	observeCause := errors.New("fixture observation failed")
	for _, tc := range []struct {
		name          string
		foreignOnFail []byte
		createApplied bool
		observeCall   int
		wantStatus    string
		wantCurrent   []byte
		wantCause     error
	}{
		{name: "absence-confirmed", wantStatus: "n/a"},
		{name: "matching-desired-is-not-create-success", createApplied: true, wantStatus: "degraded", wantCause: ErrWeeklyRefreshConflict},
		{name: "foreign-current", foreignOnFail: foreign, wantStatus: "degraded", wantCurrent: foreign, wantCause: ErrWeeklyRefreshConflict},
		{name: "current-observation-failed", observeCall: 2, wantStatus: "degraded", wantCause: observeCause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sch := &weeklyAtomicScheduler{
				xml:                   map[string][]byte{},
				failCreateCount:       1,
				createFailure:         createCause,
				foreignOnFail:         tc.foreignOnFail,
				createErrorAfterWrite: tc.createApplied,
				failExportOnCall:      map[int]error{tc.observeCall: observeCause},
			}
			root := filepath.Dir(installWeeklyAtomicHarness(t, sch))
			spec := weeklyRefreshTaskSpec(filepath.Join(root, "mcphub.exe"), nil)
			result, err := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
				desired: func([]byte, bool) (scheduler.TaskSpec, error) { return spec, nil },
			})
			if !errors.Is(err, createCause) || (tc.wantCause != nil && !errors.Is(err, tc.wantCause)) || result.restoreStatus != tc.wantStatus || len(result.finalXML) != 0 {
				t.Fatalf("recovery result=%+v err=%v; want status=%s and both original/cause=%v", result, err, tc.wantStatus, tc.wantCause)
			}
			if result.state != "" {
				t.Fatalf("failed creation reported a committed state: %q", result.state)
			}
			wantCurrent := tc.wantCurrent
			if tc.createApplied {
				wantCurrent = weeklyTaskXMLForSpec(spec)
			}
			if !bytes.Equal(sch.xml[WeeklyRefreshTaskName], wantCurrent) || sch.deleteCalls != 0 || sch.createCalls != 1 || sch.restoreCalls != 0 || sch.importCalls != 0 {
				t.Fatalf("observation changed an occupant or retried: current=%q want=%q deletes=%d creates=%d restores=%d imports=%d", sch.xml[WeeklyRefreshTaskName], wantCurrent, sch.deleteCalls, sch.createCalls, sch.restoreCalls, sch.importCalls)
			}
			if paths, globErr := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json")); globErr != nil || len(paths) != 0 {
				t.Fatalf("fresh creation unexpectedly retained prior snapshots=%v err=%v", paths, globErr)
			}
		})
	}
}

func TestWeeklyRefreshAbsentTaskNeverDeletesNewOccupant(t *testing.T) {
	foreign := []byte("<Task>external occupant</Task>")
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
	sch.operationHook = func(operation string) {
		if operation == "Create" {
			sch.mu.Lock()
			sch.xml[WeeklyRefreshTaskName] = bytes.Clone(foreign)
			sch.mu.Unlock()
		}
	}
	root := filepath.Dir(installWeeklyAtomicHarness(t, sch))
	result, err := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
		desired: func([]byte, bool) (scheduler.TaskSpec, error) {
			return weeklyRefreshTaskSpec(filepath.Join(root, "mcphub.exe"), nil), nil
		},
	})
	if !errors.Is(err, scheduler.ErrTaskAlreadyExists) || result.restoreStatus != "degraded" || result.state != "" {
		t.Fatalf("new occupant result=%+v err=%v, want preserved conflict", result, err)
	}
	if sch.deleteCalls != 0 || sch.restoreCalls != 0 || sch.importCalls != 0 || !bytes.Equal(sch.xml[WeeklyRefreshTaskName], foreign) {
		t.Fatalf("new occupant was mutated: deletes=%d restores=%d imports=%d current=%q", sch.deleteCalls, sch.restoreCalls, sch.importCalls, sch.xml[WeeklyRefreshTaskName])
	}
}

func TestWeeklyRefreshPostCreateForeignTaskIsPreserved(t *testing.T) {
	foreign := []byte("<Task>external post-create occupant</Task>")
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
	sch.operationHook = func(operation string) {
		if operation == "ExportXML" {
			sch.mu.Lock()
			if sch.createCalls > 0 {
				sch.xml[WeeklyRefreshTaskName] = bytes.Clone(foreign)
			}
			sch.mu.Unlock()
		}
	}
	root := filepath.Dir(installWeeklyAtomicHarness(t, sch))
	result, err := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
		desired: func([]byte, bool) (scheduler.TaskSpec, error) {
			return weeklyRefreshTaskSpec(filepath.Join(root, "mcphub.exe"), nil), nil
		},
	})
	if !errors.Is(err, ErrWeeklyRefreshConflict) || result.state != "" || result.restoreStatus != "degraded" || len(result.finalXML) != 0 {
		t.Fatalf("post-create result=%+v err=%v, want an unverified result", result, err)
	}
	if sch.deleteCalls != 0 || sch.createCalls != 1 || sch.restoreCalls != 0 || sch.importCalls != 0 || !bytes.Equal(sch.xml[WeeklyRefreshTaskName], foreign) {
		t.Fatalf("post-create occupant was mutated: deletes=%d creates=%d restores=%d imports=%d current=%q", sch.deleteCalls, sch.createCalls, sch.restoreCalls, sch.importCalls, sch.xml[WeeklyRefreshTaskName])
	}
}
