package api

import (
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

func TestSwapWeeklyTrigger_HadPriorTask_Success(t *testing.T) {
	installSwapWeeklyHarness(t)
	prior := []byte("<Task/>")
	fake := &fakeSwapScheduler{xml: prior}
	spec := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}
	status, err := swapWeeklyTriggerWith(fake, spec, prior)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 had-prior-task Create-success)`, status)
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

func TestSwapWeeklyTrigger_HadPriorTask_CreateFails_RestoreOK(t *testing.T) {
	installSwapWeeklyHarness(t)
	prior := []byte("<Task/>")
	fake := &fakeSwapScheduler{createErr: errors.New("create boom"), xml: prior}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, prior)
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "ok" {
		t.Errorf(`status = %q, want "ok" (prior definition restored and verified)`, status)
	}
	if !fake.restored {
		t.Error("create-only recovery must be invoked for a missing prior definition after Create fails")
	}
}

func TestSwapWeeklyTrigger_HadPriorTask_CreateFails_RestoreFails_Degraded(t *testing.T) {
	installSwapWeeklyHarness(t)
	prior := []byte("<Task/>")
	fake := &fakeSwapScheduler{
		createErr:  errors.New("create boom"),
		restoreErr: errors.New("restore boom"),
		xml:        prior,
	}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, prior)
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "degraded" {
		t.Errorf(`status = %q, want "degraded" (Create and create-only recovery both failed)`, status)
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
