package api

import (
	"bytes"
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
	installSwapWeeklyHarness(t)
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
	installSwapWeeklyHarness(t)
	sch := &exportErrScheduler{exportErr: scheduler.ErrTaskNotFound}
	res := upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, "/tmp/mcphub")
	if res == nil || res.Err != "" {
		t.Fatalf("expected successful upgrade when export is not found, got %#v", res)
	}
	if sch.deleted {
		t.Fatalf("delete must not follow a confirmed not-found export")
	}
}

func TestSchedulerUpgradeRefusesExistingWeeklyTask(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), "mcphub.exe")
	priorSpec := weeklyRefreshTaskSpec(filepath.Join(t.TempDir(), "old-mcphub.exe"), &ScheduleSpec{
		Kind: ScheduleWeekly, DayOfWeek: 5, Hour: 9, Minute: 45,
	})
	prior := weeklyTaskXMLForSpec(priorSpec)
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{WeeklyRefreshTaskName: bytes.Clone(prior)}}
	installWeeklyAtomicHarness(t, sch)

	result := upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, canonical)
	if result == nil || !strings.Contains(result.Err, ErrSchedulerReplacementUnowned.Error()) || result.NewCmd != "" {
		t.Fatalf("upgrade result = %+v, want existing-task refusal", result)
	}
	current, err := sch.ExportXML(WeeklyRefreshTaskName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, prior) || sch.createCalls != 0 || sch.deleteCalls != 0 || sch.restoreCalls != 0 || sch.importCalls != 0 {
		t.Fatalf("refused upgrade changed prior trigger or command: current=%q create/delete/restore/import=%d/%d/%d/%d", current, sch.createCalls, sch.deleteCalls, sch.restoreCalls, sch.importCalls)
	}
}
