package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type StateFileDACLRepairStatus string

const (
	StateFileDACLRepairStatusRepaired  StateFileDACLRepairStatus = "repaired"
	StateFileDACLRepairStatusRefused   StateFileDACLRepairStatus = "refused"
	StateFileDACLRepairStatusUnchanged StateFileDACLRepairStatus = "unchanged"
)

var ErrStateFileDACLSharingViolation = errors.New("hub-mcp state file DACL repair refused because a process currently holds the file open")

type StateFileDACLRepairReport struct {
	Path        string
	Status      StateFileDACLRepairStatus
	Reason      string
	RemovedSIDs []string
}

func RepairStateFileDACL(path string) (StateFileDACLRepairReport, error) {
	return repairStateFileDACL(path)
}

func stateFileDACLRepairPathUnderStateDir(path string) (stateDirAbs, targetAbs, rel string, err error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return "", "", "", err
	}
	stateDirAbs, err = filepath.Abs(stateDir)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve state dir %s: %w", stateDir, err)
	}
	targetAbs, err = filepath.Abs(path)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve repair path %s: %w", path, err)
	}
	rel, err = filepath.Rel(stateDirAbs, targetAbs)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve repair path %s relative to state dir %s: %w", targetAbs, stateDirAbs, err)
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", "", fmt.Errorf("repair path %s is outside state dir %s", targetAbs, stateDirAbs)
	}
	return filepath.Clean(stateDirAbs), filepath.Clean(targetAbs), rel, nil
}

func splitStateFileDACLRepairRel(rel string) (dirs []string, base string, err error) {
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return nil, "", fmt.Errorf("invalid repair relative path %q", rel)
	}
	parts := strings.Split(clean, string(os.PathSeparator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, "", fmt.Errorf("invalid repair path component %q in %q", part, rel)
		}
	}
	return parts[:len(parts)-1], parts[len(parts)-1], nil
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
