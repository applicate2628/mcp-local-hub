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

type StateFileDACLWriterExclusionGuarantee string

const (
	StateFileDACLWriterExclusionEnforced   StateFileDACLWriterExclusionGuarantee = "enforced"
	StateFileDACLWriterExclusionBestEffort StateFileDACLWriterExclusionGuarantee = "best-effort"
)

type StateFileDACLRepairOpenTier string

const (
	StateFileDACLRepairOpenTierStrong               StateFileDACLRepairOpenTier = "strong"
	StateFileDACLRepairOpenTierMetadataOnlyFallback StateFileDACLRepairOpenTier = "metadata-only-fallback"
)

var ErrStateFileDACLSharingViolation = errors.New("hub-mcp state file DACL repair refused because a process currently holds the file open")

type StateFileDACLRepairReport struct {
	Path                     string
	Status                   StateFileDACLRepairStatus
	Reason                   string
	RemovedSIDs              []string
	WriterExclusionGuarantee StateFileDACLWriterExclusionGuarantee
	RepairOpenTier           StateFileDACLRepairOpenTier
	FallbackPath             string
}

func RepairStateFileDACL(path string) (StateFileDACLRepairReport, error) {
	report, err := repairStateFileDACL(path)
	if err == nil && report.Status == StateFileDACLRepairStatusRepaired {
		emitStateFileDACLRepairAudit(report)
	}
	return report, err
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

func emitStateFileDACLRepairAudit(report StateFileDACLRepairReport) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return
	}
	body := map[string]any{
		"path":         report.Path,
		"removed_sids": report.RemovedSIDs,
	}
	if report.WriterExclusionGuarantee != "" {
		body["writer_exclusion_guarantee"] = string(report.WriterExclusionGuarantee)
	}
	if report.RepairOpenTier != "" {
		body["repair_open_tier"] = string(report.RepairOpenTier)
	}
	if report.FallbackPath != "" {
		body["fallback_path"] = report.FallbackPath
	}

	raw, err := marshalSupervisorEventLine(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		Event:         "state-file-dacl-operator-repaired",
		Severity:      SupervisorEventSeverityInfo,
		Source:        SupervisorEventSourceLifecycle,
		Body:          body,
	})
	if err != nil {
		return
	}

	eventsPath := filepath.Join(stateDir, SupervisorEventLogFileLeaf)
	existing, err := os.ReadFile(eventsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	payload := append(existing, raw...)

	// Repair is the code path used when the state dir may already be
	// broadened. Do not use SupervisorEventLog.Emit here: its append path can
	// create supervisor-events.log and the flock sidecar through ordinary
	// OS defaults. The skip-parent-gate secure writer still applies owner-only
	// file permissions at create/replace time and creates no lock sidecar.
	_ = secureWriteClientConfigSkipParentGate(eventsPath, payload)
}
