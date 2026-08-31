//go:build windows

package api

import (
	"errors"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

const windowsLeaseNamespaceAccess = uint32(
	windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES |
		windows.READ_CONTROL | windows.WRITE_DAC | windows.SYNCHRONIZE,
)

var adoptLeaseNamespaceMigrationFailureHook func(stage string) error

type windowsLeaseNamespaceEntry struct {
	handle    windows.Handle
	kind      string
	needsDACL bool
	sd        *windows.SECURITY_DESCRIPTOR
	sddl      string
	protected bool
}

func (e *windowsLeaseNamespaceEntry) close() error {
	if e == nil || e.handle == windows.InvalidHandle {
		return nil
	}
	h := e.handle
	e.handle = windows.InvalidHandle
	return windows.CloseHandle(h)
}

func inspectAdoptLeaseNamespacePlatform() (AdoptLeaseNamespaceReport, error) {
	root, report, err := openWindowsLeaseNamespaceRoot()
	if err != nil {
		return report, err
	}
	defer windows.CloseHandle(root)

	ns, missing, err := openExistingWindowsLeaseNamespace(root, windowsLeaseNamespaceAccess, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if missing {
		return AdoptLeaseNamespaceReport{State: AdoptLeaseNamespaceMissing, ReasonID: AdoptLeaseReasonNamespaceMissing, Action: AdoptLeaseActionRetryAdopt}, nil
	}
	if err != nil {
		return refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceIrregular, AdoptLeaseActionLeaveUnchanged, err)
	}
	defer windows.CloseHandle(ns)

	entries, report, err := analyzeWindowsLeaseNamespace(ns, false)
	closeWindowsLeaseNamespaceEntries(entries)
	if err != nil {
		return report, err
	}
	return report, nil
}

func migrateLegacyAdoptLeaseNamespacePlatform() (AdoptLeaseNamespaceReport, error) {
	root, report, err := openWindowsLeaseNamespaceRoot()
	if err != nil {
		return report, err
	}
	defer windows.CloseHandle(root)

	// ShareAccess=0 is the mutation fence. Any live lease/namespace user makes
	// this open fail busy; migration then changes nothing.
	ns, missing, err := openExistingWindowsLeaseNamespace(root, windowsLeaseNamespaceAccess, 0)
	if missing {
		return AdoptLeaseNamespaceReport{State: AdoptLeaseNamespaceMissing, ReasonID: AdoptLeaseReasonNamespaceMissing, Action: AdoptLeaseActionRetryAdopt}, nil
	}
	if err != nil {
		return refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceBusy, AdoptLeaseActionLeaveUnchanged, err)
	}
	defer windows.CloseHandle(ns)

	entries, report, err := analyzeWindowsLeaseNamespace(ns, true)
	defer closeWindowsLeaseNamespaceEntries(entries)
	if err != nil {
		return report, err
	}
	if report.State == AdoptLeaseNamespaceReady {
		return report, nil
	}
	if !report.MigrationEligible {
		return refusedLeaseNamespaceReport(report.ReasonID, report.Action, errors.New("namespace is not a verified legacy shape"))
	}
	namespaceNeedsDACL := verifyWindowsDACLFromHandle(ns) != nil

	nsSD, nsSDDL, nsProtected, err := captureWindowsLeaseNamespaceSD(ns)
	if err != nil {
		return refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceUnrecognized, AdoptLeaseActionLeaveUnchanged, err)
	}
	for i := range entries {
		if !entries[i].needsDACL {
			continue
		}
		entries[i].sd, entries[i].sddl, entries[i].protected, err = captureWindowsLeaseNamespaceSD(entries[i].handle)
		if err != nil {
			return refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceUnrecognized, AdoptLeaseActionLeaveUnchanged, err)
		}
	}

	changed := make([]int, 0, len(entries))
	namespaceChanged := false
	rollback := func(primary error) (AdoptLeaseNamespaceReport, error) {
		var rollbackErr error
		if namespaceChanged {
			rollbackErr = restoreWindowsLeaseNamespaceSD(ns, nsSD, nsProtected, nsSDDL)
		}
		for i := len(changed) - 1; i >= 0; i-- {
			entry := &entries[changed[i]]
			rollbackErr = errors.Join(rollbackErr, restoreWindowsLeaseNamespaceSD(entry.handle, entry.sd, entry.protected, entry.sddl))
		}
		report.RollbackPerformed = true
		report.ChangedLeafCount = 0
		report.NamespaceChanged = false
		return report, newLeaseNamespaceOperationFailure(AdoptLeaseReasonNamespaceUnrecognized, AdoptLeaseActionLeaveUnchanged, errors.Join(primary, rollbackErr))
	}

	for i := range entries {
		if !entries[i].needsDACL {
			continue
		}
		if err := setRestrictiveDACL(entries[i].handle); err != nil {
			return rollback(err)
		}
		changed = append(changed, i)
		if err := verifyWindowsDACLFromHandle(entries[i].handle); err != nil {
			return rollback(err)
		}
		if hook := adoptLeaseNamespaceMigrationFailureHook; hook != nil {
			if err := hook("leaf-tightened"); err != nil {
				return rollback(err)
			}
		}
	}
	if namespaceNeedsDACL {
		if err := setRestrictiveDACL(ns); err != nil {
			return rollback(err)
		}
		namespaceChanged = true
		if hook := adoptLeaseNamespaceMigrationFailureHook; hook != nil {
			if err := hook("namespace-tightened"); err != nil {
				return rollback(err)
			}
		}
	}
	if err := verifyWindowsDACLFromHandle(ns); err != nil {
		return rollback(err)
	}
	for i := range entries {
		if err := verifyWindowsLeaseNamespaceEntryAfterMigration(&entries[i]); err != nil {
			return rollback(err)
		}
	}

	report.State = AdoptLeaseNamespaceReady
	report.ReasonID = AdoptLeaseReasonNamespaceReady
	report.Action = AdoptLeaseActionRetryAdopt
	report.MigrationEligible = false
	report.ChangedLeafCount = len(changed)
	report.NamespaceChanged = namespaceChanged
	return report, nil
}

func openWindowsLeaseNamespaceRoot() (windows.Handle, AdoptLeaseNamespaceReport, error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return windows.InvalidHandle, AdoptLeaseNamespaceReport{}, newLeaseNamespaceOperationFailure(AdoptLeaseReasonStateRootUnavailable, AdoptLeaseActionLeaveUnchanged, err)
	}
	root, err := openDirHandleNoReparse(stateDir)
	if err != nil {
		report, publicErr := refusedLeaseNamespaceReport(AdoptLeaseReasonStateRootRefused, AdoptLeaseActionLeaveUnchanged, err)
		return windows.InvalidHandle, report, publicErr
	}
	if err := refuseReparsePointHandle(root); err != nil {
		_ = windows.CloseHandle(root)
		report, publicErr := refusedLeaseNamespaceReport(AdoptLeaseReasonStateRootRefused, AdoptLeaseActionLeaveUnchanged, err)
		return windows.InvalidHandle, report, publicErr
	}
	if err := verifyWindowsDACLFromHandle(root); err != nil {
		_ = windows.CloseHandle(root)
		report, publicErr := refusedLeaseNamespaceReport(AdoptLeaseReasonStateRootRefused, AdoptLeaseActionLeaveUnchanged, err)
		return windows.InvalidHandle, report, publicErr
	}
	return root, AdoptLeaseNamespaceReport{}, nil
}

func openExistingWindowsLeaseNamespace(root windows.Handle, access, share uint32) (windows.Handle, bool, error) {
	ns, err := ntCreateRelativeWithShareAccess(
		root,
		adoptProvenanceSnapshotSubdir,
		access,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		nil,
		share,
	)
	if err != nil {
		return windows.InvalidHandle, isNotFoundErr(err), err
	}
	if err := refuseReparsePointHandle(ns); err != nil {
		return windows.InvalidHandle, false, errors.Join(err, windows.CloseHandle(ns))
	}
	return ns, false, nil
}

func analyzeWindowsLeaseNamespace(ns windows.Handle, exclusive bool) ([]windowsLeaseNamespaceEntry, AdoptLeaseNamespaceReport, error) {
	nsReady := verifyWindowsDACLFromHandle(ns) == nil
	nsLegacy := false
	var err error
	if !nsReady {
		currentOwner, err := stateFileOwnerIsCurrentUser(ns)
		if err != nil || !currentOwner {
			report, publicErr := refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceWrongOwner, AdoptLeaseActionLeaveUnchanged, err)
			return nil, report, publicErr
		}
		nsLegacy, err = windowsDACLIsRecognizedInheritedLegacy(ns)
		if err != nil || !nsLegacy {
			report, publicErr := refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceDACLRefused, AdoptLeaseActionLeaveUnchanged, err)
			return nil, report, publicErr
		}
	}

	names, err := windowsReadDirNamesFromHandle(ns)
	if err != nil {
		report, publicErr := refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceUnrecognized, AdoptLeaseActionLeaveUnchanged, err)
		return nil, report, publicErr
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	if exclusive {
		share = 0
	}
	entries := make([]windowsLeaseNamespaceEntry, 0, len(names))
	report := AdoptLeaseNamespaceReport{}
	legacyLeaf := false
	for _, name := range names {
		entry, kind, needsDACL, openErr := openAndValidateWindowsLeaseNamespaceEntry(ns, name, share)
		if openErr != nil {
			closeWindowsLeaseNamespaceEntries(entries)
			refused, publicErr := refusedLeaseNamespaceReport(AdoptLeaseReasonNamespaceUnrecognized, AdoptLeaseActionLeaveUnchanged, openErr)
			return nil, refused, publicErr
		}
		entries = append(entries, windowsLeaseNamespaceEntry{handle: entry, kind: kind, needsDACL: needsDACL})
		legacyLeaf = legacyLeaf || needsDACL
		switch kind {
		case "lease":
			report.LeaseLeafCount++
		case "snapshot":
			report.SnapshotDirCount++
		}
	}
	if nsReady && !legacyLeaf {
		report.State = AdoptLeaseNamespaceReady
		report.ReasonID = AdoptLeaseReasonNamespaceReady
		report.Action = AdoptLeaseActionNone
		return entries, report, nil
	}
	report.State = AdoptLeaseNamespaceLegacy
	report.ReasonID = AdoptLeaseReasonNamespaceLegacyDACL
	report.Action = AdoptLeaseActionMigrateLegacy
	report.MigrationEligible = nsLegacy || legacyLeaf
	return entries, report, nil
}

func openAndValidateWindowsLeaseNamespaceEntry(ns windows.Handle, name string, share uint32) (windows.Handle, string, bool, error) {
	if !singleWindowsPathComponent(name) {
		return windows.InvalidHandle, "", false, errors.New("invalid namespace entry")
	}
	if name == adoptLeaseNamespaceLockLeaf || (strings.HasSuffix(name, adoptManifestLeaseSuffix) && validLegacyLeaseManifestName(strings.TrimSuffix(name, adoptManifestLeaseSuffix))) {
		h, err := ntCreateRelativeWithShareAccess(ns, name,
			windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC|windows.SYNCHRONIZE,
			windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, nil, share)
		if err != nil {
			return windows.InvalidHandle, "", false, err
		}
		if err := validateWindowsLegacyLeaseLeaf(h); err != nil {
			return windows.InvalidHandle, "", false, errors.Join(err, windows.CloseHandle(h))
		}
		ready := verifyWindowsDACLFromHandle(h) == nil
		if !ready {
			legacy, err := windowsDACLIsRecognizedInheritedLegacy(h)
			if err != nil || !legacy {
				if err == nil {
					err = errors.New("lease leaf DACL is not a recognized inherited legacy shape")
				}
				return windows.InvalidHandle, "", false, errors.Join(err, windows.CloseHandle(h))
			}
		}
		return h, "lease", !ready, nil
	}
	if validLegacyLeaseManifestName(name) {
		h, err := ntCreateRelativeWithShareAccess(ns, name,
			windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
			windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, nil, share)
		if err != nil {
			return windows.InvalidHandle, "", false, err
		}
		if err := refuseReparsePointHandle(h); err != nil {
			return windows.InvalidHandle, "", false, errors.Join(err, windows.CloseHandle(h))
		}
		if daclErr := verifyWindowsDACLFromHandle(h); daclErr != nil {
			return windows.InvalidHandle, "", false, errors.Join(daclErr, windows.CloseHandle(h))
		}
		return h, "snapshot", false, nil
	}
	return windows.InvalidHandle, "", false, errors.New("unrecognized namespace entry")
}

func validLegacyLeaseManifestName(name string) bool {
	return name != "" && !strings.HasSuffix(name, adoptManifestLeaseSuffix) && CheckManifestName(name) == nil
}

func validateWindowsLegacyLeaseLeaf(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 || info.FileSizeHigh != 0 || info.FileSizeLow != 0 {
		return errors.New("lease leaf identity refused")
	}
	return nil
}

func windowsReadDirNamesFromHandle(h windows.Handle) ([]string, error) {
	process := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(process, h, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(duplicate), "lease-namespace")
	if f == nil {
		_ = windows.CloseHandle(duplicate)
		return nil, errors.New("directory handle conversion failed")
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func windowsDACLIsRecognizedInheritedLegacy(h windows.Handle) (bool, error) {
	current, err := stateFileOwnerIsCurrentUser(h)
	if err != nil || !current {
		return false, err
	}
	currentSID, systemSID, adminSID, err := allowlistSIDs()
	if err != nil {
		return false, err
	}
	allowlist := []*windows.SID{currentSID, systemSID, adminSID}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false, err
	}
	foundInheritedOutsideAllowlist := false
	for i := uint32(0); i < windowsACLAceCount(dacl); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false, nil
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if sidInAllowlist(sidFromAce(ace), allowlist) {
			continue
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			return false, nil
		}
		foundInheritedOutsideAllowlist = true
	}
	return foundInheritedOutsideAllowlist, nil
}

func captureWindowsLeaseNamespaceSD(h windows.Handle) (*windows.SECURITY_DESCRIPTOR, string, bool, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, "", false, err
	}
	control, _, err := sd.Control()
	if err != nil {
		return nil, "", false, err
	}
	return sd, sd.String(), control&windows.SE_DACL_PROTECTED != 0, nil
}

func restoreWindowsLeaseNamespaceSD(h windows.Handle, sd *windows.SECURITY_DESCRIPTOR, protected bool, expected string) error {
	if sd == nil {
		return errors.New("missing rollback security descriptor")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if protected {
		info = windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		return err
	}
	current, _, _, err := captureWindowsLeaseNamespaceSD(h)
	if err != nil {
		return err
	}
	if current.String() != expected {
		return errors.New("rollback security descriptor mismatch")
	}
	return nil
}

func verifyWindowsLeaseNamespaceEntryAfterMigration(entry *windowsLeaseNamespaceEntry) error {
	if entry == nil || entry.handle == windows.InvalidHandle {
		return errors.New("missing retained namespace entry")
	}
	if entry.kind == "lease" {
		if err := validateWindowsLegacyLeaseLeaf(entry.handle); err != nil {
			return err
		}
	} else if err := refuseReparsePointHandle(entry.handle); err != nil {
		return err
	}
	return verifyWindowsDACLFromHandle(entry.handle)
}

func closeWindowsLeaseNamespaceEntries(entries []windowsLeaseNamespaceEntry) {
	for i := len(entries) - 1; i >= 0; i-- {
		_ = entries[i].close()
	}
}

func refusedLeaseNamespaceReport(reason AdoptLeaseReasonID, action AdoptLeaseAction, cause error) (AdoptLeaseNamespaceReport, error) {
	report := AdoptLeaseNamespaceReport{State: AdoptLeaseNamespaceRefused, ReasonID: reason, Action: action}
	return report, newLeaseNamespaceOperationFailure(reason, action, cause)
}

func classifyWindowsAdoptLeaseNamespaceRefusal(root windows.Handle) (AdoptLeaseReasonID, AdoptLeaseAction) {
	ns, missing, err := openExistingWindowsLeaseNamespace(root, windowsLeaseNamespaceAccess, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if missing {
		return AdoptLeaseReasonNamespaceCreateRefused, AdoptLeaseActionInspect
	}
	if err != nil {
		return AdoptLeaseReasonNamespaceIrregular, AdoptLeaseActionLeaveUnchanged
	}
	defer windows.CloseHandle(ns)
	current, ownerErr := stateFileOwnerIsCurrentUser(ns)
	if ownerErr != nil || !current {
		return AdoptLeaseReasonNamespaceWrongOwner, AdoptLeaseActionLeaveUnchanged
	}
	legacy, legacyErr := windowsDACLIsRecognizedInheritedLegacy(ns)
	if legacyErr == nil && legacy {
		return AdoptLeaseReasonNamespaceLegacyDACL, AdoptLeaseActionMigrateLegacy
	}
	return AdoptLeaseReasonNamespaceDACLRefused, AdoptLeaseActionLeaveUnchanged
}
