package api

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

type exportErrScheduler struct {
	exportErr error
	deleted   bool
	xml       []byte
}

func (s *exportErrScheduler) Create(spec scheduler.TaskSpec) error {
	if errors.Is(s.exportErr, scheduler.ErrTaskNotFound) {
		s.exportErr = nil
		s.xml = weeklyTaskXMLForSpec(spec)
	}
	return nil
}
func (s *exportErrScheduler) Delete(name string) error {
	s.deleted = true
	s.xml = nil
	return nil
}
func (s *exportErrScheduler) Run(name string) error  { return nil }
func (s *exportErrScheduler) Stop(name string) error { return nil }
func (s *exportErrScheduler) Status(name string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, nil
}
func (s *exportErrScheduler) List(prefix string) ([]scheduler.TaskStatus, error) { return nil, nil }
func (s *exportErrScheduler) ExportXML(name string) ([]byte, error) {
	if s.exportErr != nil {
		return nil, s.exportErr
	}
	if s.xml == nil {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), s.xml...), nil
}
func (s *exportErrScheduler) ImportXML(name string, xml []byte) error { return nil }

func TestUpgradeWorkspaceWeeklyRefreshTask_AbortOnExportError(t *testing.T) {
	sch := &exportErrScheduler{exportErr: errors.New("RPC failure")}
	res := upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, "/tmp/mcphub")
	if res == nil || !strings.Contains(res.Err, "export:") {
		t.Fatalf("expected export error result, got %#v", res)
	}
	if sch.deleted {
		t.Fatalf("expected delete not called when export fails")
	}
}

func TestUpgradeLazyProxyTask_AbortOnExportError(t *testing.T) {
	sch := &exportErrScheduler{exportErr: errors.New("access denied")}
	wsByTask := map[string]WorkspaceEntry{
		"mcp-local-hub-lsp-key-python": {WorkspacePath: "/ws", Language: "python", Port: 9001},
	}
	res := upgradeLazyProxyTask(sch, "mcp-local-hub-lsp-key-python", "mcp-local-hub-lsp-key-python", "/tmp/mcphub", wsByTask)
	if res == nil || !strings.Contains(res.Err, "export:") {
		t.Fatalf("expected export error result, got %#v", res)
	}
	if sch.deleted {
		t.Fatalf("expected delete not called when export fails")
	}
}

func TestUpgradeHelpers_AllowNotFoundExport(t *testing.T) {
	sch := &exportErrScheduler{exportErr: scheduler.ErrTaskNotFound}
	res := upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, "/tmp/mcphub")
	if res == nil || res.Err != "" {
		t.Fatalf("expected successful upgrade when export is not found, got %#v", res)
	}
	if !sch.deleted {
		t.Fatalf("expected delete called on not-found export")
	}
}

func TestSchedulerUpgradePreservesWeeklyTrigger(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), "mcphub.exe")
	priorSpec := weeklyRefreshTaskSpec(filepath.Join(t.TempDir(), "old-mcphub.exe"), &ScheduleSpec{
		Kind: ScheduleWeekly, DayOfWeek: 5, Hour: 9, Minute: 45,
	})
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{
		WeeklyRefreshTaskName: weeklyTaskXMLForSpec(priorSpec),
	}}
	installWeeklyAtomicHarness(t, sch)

	result := upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, canonical)
	if result == nil || result.Err != "" || result.NewCmd != canonical {
		t.Fatalf("upgrade result = %+v, want successful canonical-path update", result)
	}
	current, err := sch.ExportXML(WeeklyRefreshTaskName)
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := weeklyTaskTriggerFromXML(current)
	if err != nil || trigger.DayOfWeek != 5 || trigger.HourLocal != 9 || trigger.MinuteLocal != 45 {
		t.Fatalf("upgraded weekly trigger = %+v err=%v, want Friday 09:45", trigger, err)
	}
	want := weeklyRefreshTaskSpec(canonical, &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 5, Hour: 9, Minute: 45})
	matches, err := weeklyTaskXMLMatchesSpec(current, want)
	if err != nil || !matches {
		t.Fatalf("upgraded task does not preserve trigger while replacing command/workdir: matches=%t err=%v xml=%s", matches, err, current)
	}
}
