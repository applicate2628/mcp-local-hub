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

func TestWeeklyRefreshRecoveryAdmission(t *testing.T) {
	prior := []byte("<Task>retained prior</Task>")
	for _, tc := range []struct {
		name         string
		xml          []byte
		exportErr    error
		recheck      []byte
		noRecovery   bool
		wantSnapshot bool
		wantCause    error
	}{
		{name: "empty-success", xml: []byte{}, wantCause: ErrWeeklyRefreshSnapshotUnavailable},
		{name: "contradictory-absence", xml: prior, exportErr: scheduler.ErrTaskNotFound, wantCause: ErrWeeklyRefreshSnapshotUnavailable},
		{name: "missing-recovery-capability", xml: prior, noRecovery: true, wantCause: scheduler.ErrNotImplemented},
		{name: "snapshot-cannot-fit", xml: bytes.Repeat([]byte{'x'}, maxStateFileBytes)},
		{name: "changed-before-delete", xml: prior, recheck: []byte("<Task>external</Task>"), wantSnapshot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sch := &weeklyAtomicScheduler{
				xml:                  map[string][]byte{WeeklyRefreshTaskName: bytes.Clone(tc.xml)},
				failExportOnCall:     map[int]error{1: tc.exportErr},
				exportOverrideOnCall: map[int][]byte{1: tc.xml, 2: tc.recheck},
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

func TestWeeklyRefreshCreateFailureRecovery(t *testing.T) {
	prior := []byte("<Task>retained prior</Task>")
	foreign := []byte("<Task>external occupant</Task>")
	createCause := errors.New("fixture create reply failed")
	restoreCause := errors.New("fixture restore reply failed")
	observeCause := errors.New("fixture observation failed")
	for _, tc := range []struct {
		name           string
		foreignOnFail  []byte
		createApplied  bool
		restoreErr     error
		restoreApplied bool
		raceRestore    bool
		observeCall    int
		wantStatus     string
		wantRestores   int
		wantCurrent    []byte
		wantCause      error
	}{
		{name: "restored", wantStatus: "ok", wantRestores: 1, wantCurrent: prior},
		{name: "prior-definition-already-observed", foreignOnFail: prior, wantStatus: "ok", wantCurrent: prior},
		{name: "restore-refused", restoreErr: restoreCause, wantStatus: "degraded", wantRestores: 1, wantCause: restoreCause},
		{name: "restore-applied-with-error", restoreErr: restoreCause, restoreApplied: true, wantStatus: "degraded", wantRestores: 1, wantCurrent: prior, wantCause: restoreCause},
		{name: "competitor-after-absence-observation", raceRestore: true, wantStatus: "degraded", wantRestores: 1, wantCurrent: foreign, wantCause: scheduler.ErrTaskAlreadyExists},
		{name: "matching-desired-is-not-create-success", createApplied: true, wantStatus: "degraded", wantCause: ErrWeeklyRefreshConflict},
		{name: "foreign-current", foreignOnFail: foreign, wantStatus: "degraded", wantCurrent: foreign, wantCause: ErrWeeklyRefreshConflict},
		{name: "current-observation-failed", observeCall: 3, wantStatus: "degraded", wantCause: observeCause},
		{name: "restoration-readback-failed", observeCall: 4, wantStatus: "degraded", wantRestores: 1, wantCurrent: prior, wantCause: observeCause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sch := &weeklyAtomicScheduler{
				xml:                    map[string][]byte{WeeklyRefreshTaskName: bytes.Clone(prior)},
				failCreateCount:        1,
				createFailure:          createCause,
				foreignOnFail:          tc.foreignOnFail,
				createErrorAfterWrite:  tc.createApplied,
				restoreFailure:         tc.restoreErr,
				restoreErrorAfterWrite: tc.restoreApplied,
				failExportOnCall:       map[int]error{tc.observeCall: observeCause},
			}
			if tc.raceRestore {
				sch.operationHook = func(operation string) {
					if operation == "CreateXMLIfAbsent" {
						sch.mu.Lock()
						sch.xml[WeeklyRefreshTaskName] = bytes.Clone(foreign)
						sch.mu.Unlock()
					}
				}
			}
			root := filepath.Dir(installWeeklyAtomicHarness(t, sch))
			spec := weeklyRefreshTaskSpec(filepath.Join(root, "mcphub.exe"), nil)
			result, err := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
				desired: func([]byte, bool) (scheduler.TaskSpec, error) { return spec, nil },
			})
			if !errors.Is(err, createCause) || (tc.wantCause != nil && !errors.Is(err, tc.wantCause)) || result.restoreStatus != tc.wantStatus || len(result.finalXML) != 0 {
				t.Fatalf("recovery result=%+v err=%v; want status=%s and both original/cause=%v", result, err, tc.wantStatus, tc.wantCause)
			}
			wantState := weeklyRefreshTransactionState("")
			if tc.wantStatus == "ok" {
				wantState = weeklyRefreshTaskRestored
			}
			if result.state != wantState {
				t.Fatalf("recovery state=%q, want %q", result.state, wantState)
			}
			wantCurrent := tc.wantCurrent
			if tc.createApplied {
				wantCurrent = weeklyTaskXMLForSpec(spec)
			}
			if !bytes.Equal(sch.xml[WeeklyRefreshTaskName], wantCurrent) || sch.deleteCalls != 1 || sch.createCalls != 1 || sch.restoreCalls != tc.wantRestores || sch.importCalls != 0 {
				t.Fatalf("recovery changed an occupant or retried: current=%q want=%q deletes=%d creates=%d restores=%d imports=%d", sch.xml[WeeklyRefreshTaskName], wantCurrent, sch.deleteCalls, sch.createCalls, sch.restoreCalls, sch.importCalls)
			}
			path := readInstallRecoverySnapshot(t, root, WeeklyRefreshTaskName, prior)
			if !strings.Contains(err.Error(), strconv.Quote(path)) {
				t.Fatalf("recovery result lost retained snapshot path: %v", err)
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
	for _, hadPrior := range []bool{false, true} {
		name := "fresh"
		if hadPrior {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			prior := []byte("<Task>retained prior</Task>")
			foreign := []byte("<Task>external post-create occupant</Task>")
			sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
			if hadPrior {
				sch.xml[WeeklyRefreshTaskName] = bytes.Clone(prior)
			}
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
				t.Fatalf("post-create result=%+v err=%v, want incomplete recovery", result, err)
			}
			wantDeletes := boolToInt(hadPrior)
			if sch.deleteCalls != wantDeletes || sch.restoreCalls != 0 || sch.importCalls != 0 || !bytes.Equal(sch.xml[WeeklyRefreshTaskName], foreign) {
				t.Fatalf("post-create occupant was mutated: deletes=%d restores=%d imports=%d current=%q", sch.deleteCalls, sch.restoreCalls, sch.importCalls, sch.xml[WeeklyRefreshTaskName])
			}
			if hadPrior {
				readInstallRecoverySnapshot(t, root, WeeklyRefreshTaskName, prior)
			}
		})
	}
}
