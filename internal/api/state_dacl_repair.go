package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type StateFileDACLRepairStatus string

const (
	StateFileDACLRepairStatusNeedsRepair StateFileDACLRepairStatus = "needs-repair"
	StateFileDACLRepairStatusRepaired    StateFileDACLRepairStatus = "repaired"
	StateFileDACLRepairStatusRefused     StateFileDACLRepairStatus = "refused"
	StateFileDACLRepairStatusUnchanged   StateFileDACLRepairStatus = "unchanged"
)

var ErrStateFileDACLSharingViolation = errors.New("hub-mcp state file DACL repair refused because a process currently holds the file open")

type StateFileDACLRepairCandidate struct {
	Path        string
	Reason      string
	RemovedSIDs []string
}

type StateFileDACLRepairReport struct {
	Path        string
	Status      StateFileDACLRepairStatus
	Reason      string
	RemovedSIDs []string
}

func FindStateFileDACLRepairCandidates(stateDir string) ([]StateFileDACLRepairCandidate, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan state dir %s: %w", stateDir, err)
	}
	var candidates []StateFileDACLRepairCandidate
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(stateDir, entry.Name())
		candidate, needsRepair, err := inspectStateFileDACLForRepair(path)
		if err != nil {
			candidates = append(candidates, StateFileDACLRepairCandidate{
				Path:   path,
				Reason: err.Error(),
			})
			continue
		}
		if needsRepair {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Path < candidates[j].Path
	})
	return candidates, nil
}

func RepairStateFileDACL(path string) (StateFileDACLRepairReport, error) {
	report, err := repairStateFileDACL(path)
	if err == nil && report.Status == StateFileDACLRepairStatusRepaired {
		emitStateFileDACLRepairAudit(report.Path, report.RemovedSIDs)
	}
	return report, err
}

func repairCandidateFromError(path string, err error) StateFileDACLRepairCandidate {
	candidate := StateFileDACLRepairCandidate{
		Path:   path,
		Reason: err.Error(),
	}
	if sid := stateFileDACLOffendingSID(err); sid != "" {
		candidate.RemovedSIDs = []string{sid}
	}
	return candidate
}

func repairReportFromError(path string, err error) StateFileDACLRepairReport {
	report := StateFileDACLRepairReport{
		Path:   path,
		Status: StateFileDACLRepairStatusRefused,
		Reason: err.Error(),
	}
	if sid := stateFileDACLOffendingSID(err); sid != "" {
		report.RemovedSIDs = []string{sid}
	}
	return report
}

func emitStateFileDACLRepairAudit(path string, removedSIDs []string) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return
	}
	logger, err := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if err != nil {
		return
	}
	defer logger.Close()
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		Event:         "state-file-dacl-operator-repaired",
		Severity:      SupervisorEventSeverityInfo,
		Source:        SupervisorEventSourceLifecycle,
		Body: map[string]any{
			"path":         path,
			"removed_sids": removedSIDs,
		},
	})
}
