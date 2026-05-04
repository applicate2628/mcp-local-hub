package api

import (
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

type exportErrScheduler struct {
	exportErr error
	deleted   bool
}

func (s *exportErrScheduler) Create(spec scheduler.TaskSpec) error                 { return nil }
func (s *exportErrScheduler) Delete(name string) error                             { s.deleted = true; return nil }
func (s *exportErrScheduler) Run(name string) error                                { return nil }
func (s *exportErrScheduler) Stop(name string) error                               { return nil }
func (s *exportErrScheduler) Status(name string) (scheduler.TaskStatus, error)     { return scheduler.TaskStatus{}, nil }
func (s *exportErrScheduler) List(prefix string) ([]scheduler.TaskStatus, error)   { return nil, nil }
func (s *exportErrScheduler) ExportXML(name string) ([]byte, error)                { return nil, s.exportErr }
func (s *exportErrScheduler) ImportXML(name string, xml []byte) error              { return nil }

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
