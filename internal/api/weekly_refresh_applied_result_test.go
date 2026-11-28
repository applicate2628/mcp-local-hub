package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

func installWeeklyAppliedResultHarness(t *testing.T, sch *weeklyAtomicScheduler) (root, canonical string) {
	t.Helper()
	localData := t.TempDir()
	root = apitest.HardenedDir(t, filepath.Join(localData, "mcp-local-hub"))
	t.Setenv("LOCALAPPDATA", localData)
	t.Setenv("XDG_DATA_HOME", localData)
	t.Cleanup(SetDaemonStateRootForTest(root))
	canonical = filepath.Join(root, mcphubShortName)
	t.Cleanup(SetTestCanonicalMcphubPath(canonical))
	previous := testSchedulerFactory
	testSchedulerFactory = func() (testScheduler, error) { return sch, nil }
	t.Cleanup(func() { testSchedulerFactory = previous })
	return root, canonical
}

func TestWeeklyRefreshTransaction_AppliedReleaseDisposition(t *testing.T) {
	for _, failAfterWrite := range []bool{false, true} {
		name := "acknowledged-create"
		if failAfterWrite {
			name = "create-error-after-write"
		}
		t.Run(name, func(t *testing.T) {
			createCause := errors.New("injected create result")
			sch := &weeklyAtomicScheduler{xml: map[string][]byte{}, createFailure: createCause}
			if failAfterWrite {
				sch.failCreateCount = 1
				sch.createErrorAfterWrite = true
			}
			root, canonical := installWeeklyAppliedResultHarness(t, sch)
			releaseCause := errors.New("injected weekly release result")
			SetWeeklyScheduleUnlockFailuresForTest(t, root, releaseCause, nil)
			result, err := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
				desired: func([]byte, bool) (scheduler.TaskSpec, error) {
					return weeklyRefreshTaskSpec(canonical, &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30}), nil
				},
			})
			if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
				t.Fatalf("transaction error = %v, want original release class and cause", err)
			}
			if IsAppliedLockReleaseUnconfirmed(err) != !failAfterWrite {
				t.Fatalf("transaction applied disposition = %t, want %t: result=%+v err=%v", IsAppliedLockReleaseUnconfirmed(err), !failAfterWrite, result, err)
			}
			if failAfterWrite && !errors.Is(err, createCause) {
				t.Fatalf("transaction lost original create error: %v", err)
			}
		})
	}
}

func TestApplyWeeklyRefreshSchedule_AppliedReleaseDisposition(t *testing.T) {
	for _, outcome := range []struct {
		name          string
		failCreate    bool
		writeThenFail bool
		priorTask     bool
		wantStatus    string
	}{
		{name: "committed", wantStatus: "n/a"},
		{name: "create-failed", failCreate: true, wantStatus: "n/a"},
		{name: "create-failed-after-write", failCreate: true, writeThenFail: true, wantStatus: "degraded"},
		{name: "existing-refused", priorTask: true, wantStatus: "n/a"},
	} {
		for _, release := range []struct {
			name     string
			weekly   bool
			settings bool
		}{
			{name: "weekly", weekly: true},
			{name: "settings", settings: true},
			{name: "both", weekly: true, settings: true},
		} {
			t.Run(outcome.name+"/"+release.name, func(t *testing.T) {
				wantApplied := !outcome.failCreate && !outcome.priorTask
				createCause := errors.New("injected unacknowledged weekly creation")
				sch := &weeklyAtomicScheduler{
					xml: map[string][]byte{}, createFailure: createCause,
					createErrorAfterWrite: outcome.writeThenFail,
				}
				if outcome.failCreate {
					sch.failCreateCount = 1
				}
				root, canonical := installWeeklyAppliedResultHarness(t, sch)
				original := []byte("# preserve exact rollback bytes\ndaemons.weekly_schedule: weekly Sun 03:00\nunknown-key: retained\n")
				if err := WriteStateFileBytesAtomic(SettingsPath(), original); err != nil {
					t.Fatal(err)
				}
				priorXML := weeklyTaskXMLForSpec(weeklyRefreshTaskSpec(canonical, &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3}))
				if outcome.priorTask {
					sch.xml[WeeklyRefreshTaskName] = priorXML
				}
				var weeklyCause, settingsCause error
				if release.weekly {
					weeklyCause = errors.New("injected weekly unlock failure")
				}
				if release.settings {
					settingsCause = errors.New("injected settings unlock failure")
				}
				SetWeeklyScheduleUnlockFailuresForTest(t, root, weeklyCause, settingsCause)

				desired := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30}
				status, err := NewAPI().ApplyWeeklyRefreshSchedule(desired)
				if status != outcome.wantStatus || !errors.Is(err, ErrLockReleaseUnconfirmed) {
					t.Fatalf("Apply result = %q, %v; want %q and release error", status, err, outcome.wantStatus)
				}
				for _, cause := range []error{weeklyCause, settingsCause} {
					if cause != nil && !errors.Is(err, cause) {
						t.Fatalf("Apply lost original release cause %v: %v", cause, err)
					}
				}
				if outcome.failCreate && !errors.Is(err, createCause) {
					t.Fatalf("Apply lost original create cause: %v", err)
				}
				if outcome.priorTask && (!errors.Is(err, ErrSchedulerReplacementUnowned) || errors.Is(err, createCause)) {
					t.Fatalf("Apply refusal lost its cause or included an unreached creation: %v", err)
				}
				if IsAppliedLockReleaseUnconfirmed(err) != wantApplied {
					t.Fatalf("Apply applied disposition = %t, want %t: %v", IsAppliedLockReleaseUnconfirmed(err), wantApplied, err)
				}
				settings, readErr := os.ReadFile(SettingsPath())
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !wantApplied {
					if !bytes.Equal(settings, original) {
						t.Fatalf("failed Apply did not restore exact settings bytes: %q", settings)
					}
				} else if value, getErr := NewAPI().SettingsGet("daemons.weekly_schedule"); getErr != nil || value != desired.Canonical() {
					t.Fatalf("committed settings = %q, err=%v; want %q", value, getErr, desired.Canonical())
				}
				var wantXML []byte
				if wantApplied || outcome.writeThenFail {
					wantXML = weeklyTaskXMLForSpec(weeklyRefreshTaskSpec(canonical, desired))
				} else if outcome.priorTask {
					wantXML = priorXML
				}
				if current := sch.xml[WeeklyRefreshTaskName]; !bytes.Equal(current, wantXML) {
					t.Fatalf("settled task = %s, want %s", current, wantXML)
				}
				wantCreates := 1
				if outcome.priorTask {
					wantCreates = 0
				}
				if sch.createCalls != wantCreates || sch.deleteCalls != 0 || sch.restoreCalls != 0 || sch.importCalls != 0 {
					t.Fatalf("Apply mutated beyond fresh creation: create/delete/restore/import=%d/%d/%d/%d", sch.createCalls, sch.deleteCalls, sch.restoreCalls, sch.importCalls)
				}
			})
		}
	}
}

func TestApplyWeeklyRefreshSchedule_ExistingRefusalRestoresSettings(t *testing.T) {
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
	root, canonical := installWeeklyAppliedResultHarness(t, sch)
	original := []byte("# keep exact preferences\ndaemons.weekly_schedule: weekly Sun 03:00\nunknown-key: retained\n")
	if err := WriteStateFileBytesAtomic(SettingsPath(), original); err != nil {
		t.Fatal(err)
	}
	prior := weeklyTaskXMLForSpec(weeklyRefreshTaskSpec(canonical, nil))
	sch.xml[WeeklyRefreshTaskName] = bytes.Clone(prior)
	status, err := NewAPI().ApplyWeeklyRefreshSchedule(&ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30})
	var refusal *SchedulerReplacementRefusedError
	if status != "n/a" || !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &refusal) || IsAppliedLockReleaseUnconfirmed(err) {
		t.Fatalf("Apply result = %q, %v, want unapplied refusal", status, err)
	}
	if refusal.SnapshotPath != readInstallRecoverySnapshot(t, root, WeeklyRefreshTaskName, prior) {
		t.Fatal("Apply refusal lost the observed definition")
	}
	settings, readErr := os.ReadFile(SettingsPath())
	if readErr != nil || !bytes.Equal(settings, original) {
		t.Fatalf("Apply did not restore exact settings bytes: err=%v bytes=%q", readErr, settings)
	}
	if !bytes.Equal(sch.xml[WeeklyRefreshTaskName], prior) || sch.createCalls != 0 || sch.deleteCalls != 0 || sch.restoreCalls != 0 || sch.importCalls != 0 {
		t.Fatalf("Apply refusal mutated scheduler state: create/delete/restore/import=%d/%d/%d/%d", sch.createCalls, sch.deleteCalls, sch.restoreCalls, sch.importCalls)
	}
}
