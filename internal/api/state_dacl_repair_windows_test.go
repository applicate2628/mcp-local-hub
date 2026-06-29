//go:build windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRepairStateFileDACL_WindowsRepairsBroadCurrentUserFileAndHardenedReadPasses(t *testing.T) {
	stateDir := isolateStateDir(t)
	t.Setenv(RequireSingleUserHomeEnv, "1")

	target := filepath.Join(stateDir, "workspaces.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
		t.Fatalf("write stale registry: %v", err)
	}
	applyFileDACLWithAuthUsersWriteACE(t, target)

	if _, err := readStateFileInodeAnchored(target); err == nil {
		t.Fatalf("strict hardened read must reject write-broadened stale file before repair")
	} else if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Fatalf("pre-repair read err = %v, want ErrDaclOutsideAllowlist", err)
	}

	report, err := RepairStateFileDACL(target)
	if err != nil {
		t.Fatalf("RepairStateFileDACL: %v", err)
	}
	if report.Status != StateFileDACLRepairStatusRepaired {
		t.Fatalf("repair status = %q, want %q (report=%+v)", report.Status, StateFileDACLRepairStatusRepaired, report)
	}
	if report.WriterExclusionGuarantee != StateFileDACLWriterExclusionEnforced {
		t.Fatalf("writer-exclusion guarantee = %q, want %q (report=%+v)", report.WriterExclusionGuarantee, StateFileDACLWriterExclusionEnforced, report)
	}
	if report.RepairOpenTier != StateFileDACLRepairOpenTierStrong {
		t.Fatalf("repair open tier = %q, want %q (report=%+v)", report.RepairOpenTier, StateFileDACLRepairOpenTierStrong, report)
	}
	if !containsRepairSID(report.RemovedSIDs, "S-1-5-11") {
		t.Fatalf("removed SIDs = %v, want Authenticated Users SID S-1-5-11", report.RemovedSIDs)
	}
	assertWindowsFileDACLAllowlist(t, target)

	data, err := readStateFileInodeAnchored(target)
	if err != nil {
		t.Fatalf("hardened read after repair: %v", err)
	}
	if string(data) != "version: 1\nworkspaces: []\n" {
		t.Fatalf("repaired file content = %q", data)
	}

	event, line := findSupervisorEventByName(t, filepath.Join(stateDir, SupervisorEventLogFileLeaf), "state-file-dacl-operator-repaired")
	if event == nil {
		t.Fatalf("missing state-file-dacl-operator-repaired audit event")
	}
	if got := event["severity"]; got != SupervisorEventSeverityInfo {
		t.Fatalf("audit severity = %v, want %q (line=%s)", got, SupervisorEventSeverityInfo, line)
	}
	body, ok := event["body"].(map[string]any)
	if !ok {
		t.Fatalf("audit body missing or wrong type: %#v", event["body"])
	}
	if got := body["path"]; got != target {
		t.Fatalf("audit path = %v, want %q", got, target)
	}
	if got := body["writer_exclusion_guarantee"]; got != string(StateFileDACLWriterExclusionEnforced) {
		t.Fatalf("audit writer_exclusion_guarantee = %v, want %q (line=%s)", got, StateFileDACLWriterExclusionEnforced, line)
	}
	if got := body["repair_open_tier"]; got != string(StateFileDACLRepairOpenTierStrong) {
		t.Fatalf("audit repair_open_tier = %v, want %q (line=%s)", got, StateFileDACLRepairOpenTierStrong, line)
	}
	if _, ok := body["fallback_path"]; ok {
		t.Fatalf("audit body must not include fallback_path for strong tier: %v", body)
	}
	if !strings.Contains(line, "S-1-5-11") {
		t.Fatalf("audit line %q does not name removed SID S-1-5-11", line)
	}
}

func TestRepairStateFileDACL_WindowsHeldOpenFileRefusesAndLeavesDACLUnchanged(t *testing.T) {
	stateDir := isolateStateDir(t)
	target := filepath.Join(stateDir, "workspaces.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
		t.Fatalf("write stale registry: %v", err)
	}
	applyFileDACLWithAuthUsersWriteACE(t, target)

	held := openWindowsFileForShareConflictTest(t, target)
	defer windows.CloseHandle(held)

	report, err := RepairStateFileDACL(target)
	if err == nil {
		t.Fatalf("RepairStateFileDACL unexpectedly succeeded while file was held open (report=%+v)", report)
	}
	if !errors.Is(err, ErrStateFileDACLSharingViolation) {
		t.Fatalf("err = %v, want ErrStateFileDACLSharingViolation", err)
	}
	if !strings.Contains(err.Error(), "a process currently holds") {
		t.Fatalf("sharing refusal message = %q, want operator guidance", err.Error())
	}
	if report.Status != StateFileDACLRepairStatusRefused {
		t.Fatalf("repair status = %q, want refused (report=%+v)", report.Status, report)
	}
	if verifyErr := verifyWriteBroadenedDACLStillPresent(target); verifyErr != nil {
		t.Fatalf("held-open refusal changed the stale DACL: %v", verifyErr)
	}
}

func TestRepairStateFileDACL_WindowsRepairsCurrentUserWriteDACOnlyStaleDACL(t *testing.T) {
	stateDir := isolateStateDir(t)
	t.Setenv(RequireSingleUserHomeEnv, "1")

	target := filepath.Join(stateDir, "workspaces.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
		t.Fatalf("write stale registry: %v", err)
	}
	applyRepairableCurrentUserWriteDACOnlyDACL(t, target)

	parentHandle, err := openDirHandleNoReparse(filepath.Dir(target))
	if err != nil {
		t.Fatalf("open parent: %v", err)
	}
	strongAccess := uint32(windows.FILE_WRITE_DATA | windows.DELETE | windows.WRITE_DAC | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	oldHandle, oldErr := ntOpenRelativeWithShareAccess(parentHandle, filepath.Base(target), strongAccess, 0)
	_ = windows.CloseHandle(parentHandle)
	if oldErr == nil {
		_ = windows.CloseHandle(oldHandle)
		t.Fatalf("fixture unexpectedly allows the strong repair access mask with FILE_WRITE_DATA and DELETE")
	}

	report, err := RepairStateFileDACL(target)
	if err != nil {
		t.Fatalf("RepairStateFileDACL: %v", err)
	}
	if report.Status != StateFileDACLRepairStatusRepaired {
		t.Fatalf("repair status = %q, want %q (report=%+v)", report.Status, StateFileDACLRepairStatusRepaired, report)
	}
	if report.WriterExclusionGuarantee != StateFileDACLWriterExclusionBestEffort {
		t.Fatalf("writer-exclusion guarantee = %q, want %q (report=%+v)", report.WriterExclusionGuarantee, StateFileDACLWriterExclusionBestEffort, report)
	}
	if report.RepairOpenTier != StateFileDACLRepairOpenTierMetadataOnlyFallback {
		t.Fatalf("repair open tier = %q, want %q (report=%+v)", report.RepairOpenTier, StateFileDACLRepairOpenTierMetadataOnlyFallback, report)
	}
	if !containsRepairSID(report.RemovedSIDs, "S-1-5-11") {
		t.Fatalf("removed SIDs = %v, want Authenticated Users SID S-1-5-11", report.RemovedSIDs)
	}
	assertWindowsFileDACLAllowlist(t, target)

	data, err := readStateFileInodeAnchored(target)
	if err != nil {
		t.Fatalf("hardened read after repair: %v", err)
	}
	if string(data) != "version: 1\nworkspaces: []\n" {
		t.Fatalf("repaired file content = %q", data)
	}

	event, line := findSupervisorEventByName(t, filepath.Join(stateDir, SupervisorEventLogFileLeaf), "state-file-dacl-operator-repaired")
	if event == nil {
		t.Fatalf("missing state-file-dacl-operator-repaired audit event")
	}
	body, ok := event["body"].(map[string]any)
	if !ok {
		t.Fatalf("audit body missing or wrong type: %#v", event["body"])
	}
	if got := body["writer_exclusion_guarantee"]; got != string(StateFileDACLWriterExclusionBestEffort) {
		t.Fatalf("audit writer_exclusion_guarantee = %v, want %q (line=%s)", got, StateFileDACLWriterExclusionBestEffort, line)
	}
	if got := body["repair_open_tier"]; got != string(StateFileDACLRepairOpenTierMetadataOnlyFallback) {
		t.Fatalf("audit repair_open_tier = %v, want %q (line=%s)", got, StateFileDACLRepairOpenTierMetadataOnlyFallback, line)
	}
	if got := body["fallback_path"]; got != "tier1-access-denied-metadata-only" {
		t.Fatalf("audit fallback_path = %v, want tier1-access-denied-metadata-only (line=%s)", got, line)
	}
}

func TestRepairStateFileDACL_WindowsReportsAllRemovedSIDs(t *testing.T) {
	stateDir := isolateStateDir(t)
	t.Setenv(RequireSingleUserHomeEnv, "1")

	target := filepath.Join(stateDir, "workspaces.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
		t.Fatalf("write stale registry: %v", err)
	}
	applyFileDACLWithAuthUsersAndEveryoneWriteACEs(t, target)

	report, err := RepairStateFileDACL(target)
	if err != nil {
		t.Fatalf("RepairStateFileDACL: %v", err)
	}
	if report.Status != StateFileDACLRepairStatusRepaired {
		t.Fatalf("repair status = %q, want %q (report=%+v)", report.Status, StateFileDACLRepairStatusRepaired, report)
	}
	for _, want := range []string{"S-1-5-11", "S-1-1-0"} {
		if !containsRepairSID(report.RemovedSIDs, want) {
			t.Fatalf("removed SIDs = %v, want %s", report.RemovedSIDs, want)
		}
	}

	event, line := findSupervisorEventByName(t, filepath.Join(stateDir, SupervisorEventLogFileLeaf), "state-file-dacl-operator-repaired")
	if event == nil {
		t.Fatalf("missing state-file-dacl-operator-repaired audit event")
	}
	for _, want := range []string{"S-1-5-11", "S-1-1-0"} {
		if !strings.Contains(line, want) {
			t.Fatalf("audit line %q does not name removed SID %s", line, want)
		}
	}
	assertWindowsFileDACLAllowlist(t, target)
}

func TestRepairStateFileDACL_WindowsForeignOwnerPredicateAndIntegration(t *testing.T) {
	stateDir := isolateStateDir(t)
	target := filepath.Join(stateDir, "foreign-owner.yaml")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyAllowlistOnlyDACL(t, target)

	h := openWindowsFileForDACLTest(t, target, windows.READ_CONTROL|windows.WRITE_OWNER)
	defer func() {
		if h != windows.InvalidHandle {
			_ = windows.CloseHandle(h)
		}
	}()
	owned, err := stateFileOwnerIsCurrentUser(h)
	if err != nil {
		t.Fatalf("stateFileOwnerIsCurrentUser current-owned file: %v", err)
	}
	if !owned {
		t.Fatalf("stateFileOwnerIsCurrentUser returned false for current-user-owned test file")
	}

	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}
	if err := windows.SetSecurityInfo(
		h,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
		authUsersSID,
		nil,
		nil,
		nil,
	); err != nil {
		t.Skipf("setting a foreign owner requires privileges unavailable on this host: %v", err)
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatalf("close setup handle before repair: %v", err)
	}
	h = windows.InvalidHandle

	report, err := RepairStateFileDACL(target)
	if err == nil {
		t.Fatalf("RepairStateFileDACL unexpectedly repaired a foreign-owned file (report=%+v)", report)
	}
	if !errors.Is(err, ErrWrongOwner) {
		t.Fatalf("err = %v, want ErrWrongOwner", err)
	}
	if report.Status != StateFileDACLRepairStatusRefused {
		t.Fatalf("status = %q, want refused", report.Status)
	}
}

func TestFindStateFileDACLRepairCandidates_WindowsListsOnlyUnsafeStateFiles(t *testing.T) {
	stateDir := isolateStateDir(t)
	unsafe := filepath.Join(stateDir, "workspaces.yaml")
	safe := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(unsafe, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
		t.Fatalf("write unsafe: %v", err)
	}
	if err := os.WriteFile(safe, []byte(`{"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write safe: %v", err)
	}
	applyFileDACLWithAuthUsersWriteACE(t, unsafe)
	applyAllowlistOnlyDACL(t, safe)

	candidates, err := FindStateFileDACLRepairCandidates(stateDir)
	if err != nil {
		t.Fatalf("FindStateFileDACLRepairCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1 (%+v)", len(candidates), candidates)
	}
	if candidates[0].Path != unsafe {
		t.Fatalf("candidate path = %q, want %q", candidates[0].Path, unsafe)
	}
}

func assertWindowsFileDACLAllowlist(t *testing.T, target string) {
	t.Helper()
	h := openWindowsFileForDACLTest(t, target, windows.READ_CONTROL)
	defer windows.CloseHandle(h)
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		t.Fatalf("verifyWindowsDACLFromHandle(%s): %v", target, err)
	}
}

func verifyWriteBroadenedDACLStillPresent(target string) error {
	pathW, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(
		pathW,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := verifyWindowsDACLFromHandleWriteOrAdmin(h); !errors.Is(err, ErrDaclOutsideAllowlist) {
		return err
	}
	return nil
}

func openWindowsFileForDACLTest(t *testing.T, target string, access uint32) windows.Handle {
	t.Helper()
	pathW, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	h, err := windows.CreateFile(
		pathW,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(%s): %v", target, err)
	}
	return h
}

func openWindowsFileForShareConflictTest(t *testing.T, target string) windows.Handle {
	t.Helper()
	pathW, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile share-conflict fixture: %v", err)
	}
	return h
}

func applyFileDACLWithAuthUsersAndEveryoneWriteACEs(t *testing.T, target string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}
	everyoneSID, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatalf("Everyone sid: %v", err)
	}

	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_WRITE),
		explicitAccessAllow(everyoneSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_READ),
	}
	applyProtectedDACLFromEntries(t, target, entries)
}

func applyRepairableCurrentUserWriteDACOnlyDACL(t *testing.T, target string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}

	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.WRITE_DAC|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_READ),
	}
	applyProtectedDACLFromEntries(t, target, entries)
}

func containsRepairSID(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
