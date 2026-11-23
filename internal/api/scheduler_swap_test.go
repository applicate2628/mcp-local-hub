package api

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// fakeSwapScheduler is the test double for the complete weekly transaction.
type fakeSwapScheduler struct {
	deleteErr  error
	createErr  error
	restoreErr error
	xml        []byte
	deleted    bool
	created    bool
	restored   bool
}

func (f *fakeSwapScheduler) ExportXML(string) ([]byte, error) {
	if f.xml == nil {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), f.xml...), nil
}
func (f *fakeSwapScheduler) Delete(name string) error {
	f.deleted = true
	if f.deleteErr == nil {
		f.xml = nil
	}
	return f.deleteErr
}
func (f *fakeSwapScheduler) Create(spec scheduler.TaskSpec) error {
	f.created = true
	if f.xml != nil {
		return scheduler.ErrTaskAlreadyExists
	}
	if f.createErr == nil {
		f.xml = weeklyTaskXMLForSpec(spec)
	}
	return f.createErr
}
func (f *fakeSwapScheduler) CreateXMLIfAbsent(name string, xml []byte) error {
	f.restored = true
	if f.xml != nil {
		return scheduler.ErrTaskAlreadyExists
	}
	if f.restoreErr == nil {
		f.xml = append([]byte(nil), xml...)
	}
	return f.restoreErr
}

func TestSwapWeeklyTrigger_FreshInstall_Success(t *testing.T) {
	installSwapWeeklyHarness(t)
	fake := &fakeSwapScheduler{}
	spec := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 1, Hour: 14, Minute: 30}
	status, err := swapWeeklyTriggerWith(fake, spec, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 fresh-install Create-success)`, status)
	}
	if fake.deleted || !fake.created {
		t.Error("fresh creation must not delete after confirmed absence")
	}
	if fake.restored {
		t.Error("recovery must NOT be invoked on Create success")
	}
}

func TestSwapWeeklyTrigger_FreshInstall_CreateFails_NoRollback(t *testing.T) {
	installSwapWeeklyHarness(t)
	fake := &fakeSwapScheduler{createErr: errors.New("create boom")}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, nil)
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 fresh-install Create-failed: nothing to restore)`, status)
	}
	if fake.restored {
		t.Error("recovery must NOT be invoked when priorXML==nil")
	}
}

func TestSwapWeeklyTrigger_ExistingTaskRefused(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrecognized-definition", true: "matching-definition"}[matching], func(t *testing.T) {
			installSwapWeeklyHarness(t)
			spec := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}
			prior := []byte("<Task/>")
			if matching {
				canonical, err := canonicalMcphubPath()
				if err != nil {
					t.Fatal(err)
				}
				prior = weeklyTaskXMLForSpec(weeklyRefreshTaskSpec(canonical, spec))
			}
			createCause := errors.New("unreached create failure")
			fake := &fakeSwapScheduler{createErr: createCause, xml: bytes.Clone(prior)}
			status, err := swapWeeklyTriggerWith(fake, spec, prior)
			var refusal *SchedulerReplacementRefusedError
			if !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &refusal) || status != "n/a" {
				t.Fatalf("result = %q, %v, want typed refusal before task mutation", status, err)
			}
			if errors.Is(err, createCause) || fake.deleted || fake.created || fake.restored || !bytes.Equal(fake.xml, prior) {
				t.Fatalf("refused swap reached mutation or changed prior: fake=%+v err=%v", fake, err)
			}
			if refusal.SnapshotPath == "" {
				t.Fatal("refusal lost its retained definition")
			}
		})
	}
}

func TestSwapWeeklyTrigger_StaleExpectedDefinitionIsPreserved(t *testing.T) {
	installSwapWeeklyHarness(t)
	prior := []byte("<Task>current</Task>")
	fake := &fakeSwapScheduler{xml: bytes.Clone(prior)}
	status, err := swapWeeklyTriggerWith(fake, &ScheduleSpec{Kind: ScheduleWeekly}, []byte("<Task>stale</Task>"))
	if status != "n/a" || !errors.Is(err, ErrWeeklyRefreshConflict) || !errors.Is(err, ErrSchedulerReplacementUnowned) {
		t.Fatalf("stale swap result = %q, %v, want both expectation conflict and replacement refusal", status, err)
	}
	if fake.deleted || fake.created || fake.restored || !bytes.Equal(fake.xml, prior) {
		t.Fatalf("stale expected definition reached a mutation: %+v", fake)
	}
}

func installSwapWeeklyHarness(t *testing.T) {
	t.Helper()
	previousRoot := daemonStateRootOverride
	daemonStateRootOverride = hardenedTempDir(t)
	t.Cleanup(func() { daemonStateRootOverride = previousRoot })
	// The scheduler consumes a descriptor only; no Product image is executed.
	t.Cleanup(SetTestCanonicalMcphubPath(filepath.Join(daemonStateRootOverride, mcphubShortName)))
}
