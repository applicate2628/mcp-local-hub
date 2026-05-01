package api

import (
	"errors"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// fakeSwapScheduler is the test double for the schedulerSwap seam used by
// swapWeeklyTriggerWith. It is intentionally distinct from the package-wide
// fakeScheduler in register_test.go: that one models a richer scheduler
// (Run, ExportXML, queued failure modes) for register/migrate test paths,
// whereas this fake exposes only the three methods the swap helper calls
// (Delete, Create, ImportXML) and just records invocation flags.
type fakeSwapScheduler struct {
	deleteErr error
	createErr error
	importErr error
	deleted   bool
	created   bool
	imported  bool
}

func (f *fakeSwapScheduler) Delete(name string) error             { f.deleted = true; return f.deleteErr }
func (f *fakeSwapScheduler) Create(spec scheduler.TaskSpec) error { f.created = true; return f.createErr }
func (f *fakeSwapScheduler) ImportXML(name string, xml []byte) error {
	f.imported = true
	return f.importErr
}

func TestSwapWeeklyTrigger_FreshInstall_Success(t *testing.T) {
	fake := &fakeSwapScheduler{}
	spec := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 1, Hour: 14, Minute: 30}
	status, err := swapWeeklyTriggerWith(fake, spec, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 fresh-install Create-success)`, status)
	}
	if !fake.deleted || !fake.created {
		t.Error("Delete + Create must both be invoked")
	}
	if fake.imported {
		t.Error("ImportXML must NOT be invoked on Create success")
	}
}

func TestSwapWeeklyTrigger_HadPriorTask_Success(t *testing.T) {
	fake := &fakeSwapScheduler{}
	spec := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}
	status, err := swapWeeklyTriggerWith(fake, spec, []byte("<Task/>"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 had-prior-task Create-success)`, status)
	}
	if fake.imported {
		t.Error("ImportXML must NOT be invoked on Create success")
	}
}

func TestSwapWeeklyTrigger_FreshInstall_CreateFails_NoRollback(t *testing.T) {
	fake := &fakeSwapScheduler{createErr: errors.New("create boom")}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, nil)
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 fresh-install Create-failed: nothing to restore)`, status)
	}
	if fake.imported {
		t.Error("ImportXML must NOT be invoked when priorXML==nil")
	}
}

func TestSwapWeeklyTrigger_HadPriorTask_CreateFails_RestoreOK(t *testing.T) {
	fake := &fakeSwapScheduler{createErr: errors.New("create boom")}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, []byte("<Task/>"))
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "ok" {
		t.Errorf(`status = %q, want "ok" (D8 had-prior-task Create-failed + ImportXML succeeded)`, status)
	}
	if !fake.imported {
		t.Error("ImportXML must be invoked when priorXML != nil and Create fails")
	}
}

func TestSwapWeeklyTrigger_HadPriorTask_CreateFails_RestoreFails_Degraded(t *testing.T) {
	fake := &fakeSwapScheduler{
		createErr: errors.New("create boom"),
		importErr: errors.New("import boom"),
	}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, []byte("<Task/>"))
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "degraded" {
		t.Errorf(`status = %q, want "degraded" (D8 had-prior-task Create-failed + ImportXML-failed)`, status)
	}
}
