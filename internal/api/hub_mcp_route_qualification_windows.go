package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const hubRouteGuardShare = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE

type hubRouteNative struct {
	guards         []windows.Handle
	parent         windows.Handle
	lock           windows.Handle
	borrowedParent bool
	// Test fixtures hold real native file handles at these boundaries.
	beforeRename  func() error
	afterRename   func() error
	afterReadback func() error
}

func observeHubRouteStateDirectory(ctx context.Context, stateDir string) (identity ConfigParentDirectoryIdentityV1, retErr error) {
	var native hubRouteNative
	defer func() { retErr = errors.Join(retErr, native.close()) }()
	return native.openParent(ctx, stateDir)
}

func withHubRouteState(ctx context.Context, stateDir string, expected ConfigParentDirectoryIdentityV1, run func(*HubRouteState) error) error {
	c := &hubRouteStateCore{ctx: ctx, parent: expected, stateDir: stateDir}
	actual, err := c.native.openParent(ctx, stateDir)
	if err != nil {
		return errors.Join(err, c.native.close())
	}
	if actual != expected {
		return errors.Join(errHubRouteStateChanged, c.native.close())
	}
	return runHubRouteState(c, run, nil)
}

func runHubRouteState(c *hubRouteStateCore, run func(*HubRouteState) error, retain func(error)) (retErr error) {
	ctx := c.ctx
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.active = false
		closeErr := c.native.close()
		if closeErr != nil && retain != nil {
			retain(closeErr)
		}
		retErr = errors.Join(retErr, closeErr)
		if c.acknowledged && retErr != nil {
			retErr = &HubRoutePublicationError{Outcome: HubRoutePublicationApplied, Err: retErr}
		}
	}()
	if err := c.native.lockHub(ctx); err != nil {
		return err
	}
	if err := c.native.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.active = true
	return run(&HubRouteState{core: c})
}

func (s *hubRouteNative) openParent(ctx context.Context, path string) (ConfigParentDirectoryIdentityV1, error) {
	var zero ConfigParentDirectoryIdentityV1
	if ctx == nil {
		return zero, errHubRouteStateUnavailable
	}
	path = filepath.FromSlash(path)
	clean := filepath.Clean(path)
	if path != clean {
		return zero, errHubRouteStateUnavailable
	}
	volume := filepath.VolumeName(clean)
	// Local drive roots have a kernel-native stable handle anchor. Do not
	// follow UNC, device or relative namespaces through implicit resolution.
	if !filepath.IsAbs(clean) || len(volume) != 2 || volume[1] != ':' || clean == volume+`\` {
		return zero, errHubRouteStateUnavailable
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return zero, err
	}
	if drive := windows.GetDriveType(root); drive != windows.DRIVE_FIXED && drive != windows.DRIVE_RAMDISK {
		return zero, errHubRouteStateUnavailable
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	anchor, err := windows.CreateFile(root, traverseOnlyDirAccess|windows.SYNCHRONIZE, hubRouteGuardShare, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return zero, err
	}
	s.guards = append(s.guards, anchor)
	if err := refuseReparsePointHandle(anchor); err != nil {
		return zero, err
	}
	held := anchor
	parts := strings.Split(strings.TrimPrefix(clean[len(volume):], `\`), `\`)
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if !singleWindowsPathComponent(part) || strings.Contains(part, ":") || strings.TrimRight(part, " .") != part {
			return zero, errHubRouteStateUnavailable
		}
		access := uint32(traverseOnlyDirAccess | windows.SYNCHRONIZE)
		if i == len(parts)-1 {
			access |= windows.FILE_LIST_DIRECTORY | windows.READ_CONTROL
		}
		child, err := ntCreateRelativeWithShareAccess(held, part, access, windows.FILE_OPEN,
			windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, nil, hubRouteGuardShare)
		if err != nil {
			return zero, fmt.Errorf("open protected Hub directory component %q: %w", part, err)
		}
		s.guards = append(s.guards, child)
		if err := refuseReparsePointHandle(child); err != nil {
			return zero, err
		}
		held = child
	}
	s.parent = held
	if err := verifyWindowsDACLFromHandle(held); err != nil {
		return zero, wrapParentGateRefusal(path, err)
	}
	return queryConfigParentDirectoryIdentity(held)
}

func (s *hubRouteNative) lockHub(ctx context.Context) error {
	if s.lock == 0 {
		h, err := ntOpenRelativeWithShareAccess(s.parent, hubMcpLockFileLeaf,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, hubRouteGuardShare)
		if err != nil {
			return fmt.Errorf("open original Hub lock: %w", err)
		}
		s.lock = h
	}
	if err := verifyHubRouteLock(s.lock); err != nil {
		return fmt.Errorf("inspect original Hub lock: %w", err)
	}
	// The vendored Windows flock holds the same exclusive byte [0,1), at
	// offset zero. This relative open proves the file without rewalking a path.
	ticker := time.NewTicker(hubMcpLockRetryDelay)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := windows.LockFileEx(s.lock, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *hubRouteNative) close() (retErr error) {
	if s.lock != 0 {
		// Closing the retained file handle releases the existing kernel byte lock.
		retErr = windows.CloseHandle(s.lock)
		s.lock = 0
	}
	if !s.borrowedParent {
		for i := len(s.guards) - 1; i >= 0; i-- {
			retErr = errors.Join(retErr, windows.CloseHandle(s.guards[i]))
		}
	}
	s.guards = nil
	s.parent = 0
	return retErr
}

func (s *hubRouteNative) validate() error {
	if s.parent == 0 || s.lock == 0 {
		return errHubRouteStateUnavailable
	}
	if err := s.validateParent(); err != nil {
		return err
	}
	return verifyHubRouteLock(s.lock)
}

func (s *hubRouteNative) validateParent() error {
	// Every ancestor keeps its no-delete handle until after the lock release.
	// Refuse any reparse status even if a kernel permits in-place alteration.
	for _, guard := range s.guards {
		if err := refuseReparsePointHandle(guard); err != nil {
			return err
		}
	}
	return verifyWindowsDACLFromHandle(s.parent)
}

func hubRouteFileInformation(h windows.Handle) (info windows.ByHandleFileInformation, err error) {
	kind, err := windows.GetFileType(h)
	if err != nil {
		return info, err
	}
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return info, err
	}
	if kind != windows.FILE_TYPE_DISK || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || info.NumberOfLinks != 1 {
		return info, errHubRouteStateUnavailable
	}
	return info, nil
}

func verifyHubRouteLock(h windows.Handle) error {
	info, err := hubRouteFileInformation(h)
	if err != nil {
		return err
	}
	if info.FileSizeHigh != 0 || info.FileSizeLow != 0 {
		return errHubRouteStateUnavailable
	}
	// The marker has no payload. Only the actual current token's logon SID
	// may carry the legacy read/execute ACE outside the usual allowlist.
	current, system, admin, err := allowlistSIDs()
	if err != nil {
		return err
	}
	logon, err := hubRouteCurrentLogonSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read original Hub lock owner and DACL: %w", err)
	}
	violations, err := windowsDACLAllowlistViolationsWithReadOnlySID(sd, ^uint32(0), []*windows.SID{current, system, admin}, sidString, logon)
	if err != nil {
		return err
	}
	if len(violations) != 0 {
		return &violations[0]
	}
	return nil
}

func hubRouteCurrentLogonSID() (*windows.SID, error) {
	groups, err := windows.GetCurrentProcessToken().GetTokenGroups()
	if err != nil {
		return nil, fmt.Errorf("read current token logon groups: %w", err)
	}
	var logon *windows.SID
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_LOGON_ID != windows.SE_GROUP_LOGON_ID {
			continue
		}
		if logon != nil || group.Sid == nil || !group.Sid.IsValid() {
			return nil, fmt.Errorf("ambiguous current token logon SID: %w", errHubRouteStateUnavailable)
		}
		logon = group.Sid
	}
	return logon, nil
}

func verifyHubRouteFile(h windows.Handle) error {
	if _, err := hubRouteFileInformation(h); err != nil {
		return err
	}
	return verifyWindowsDACLFromHandle(h)
}

func (s *hubRouteNative) read(name string, limit int) (leaf hubRouteLeaf, retErr error) {
	h, err := ntOpenRelative(s.parent, name, windows.GENERIC_READ|windows.READ_CONTROL)
	if isNotFoundErr(err) {
		return leaf, nil
	}
	if err != nil {
		return leaf, err
	}
	file := os.NewFile(uintptr(h), name)
	if file == nil {
		return leaf, errors.Join(errHubRouteStateUnavailable, windows.CloseHandle(h))
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	return readHubRouteLeafHandle(file, name, limit)
}

func readHubRouteLeafHandle(file *os.File, name string, limit int) (hubRouteLeaf, error) {
	h := windows.Handle(file.Fd())
	var leaf hubRouteLeaf
	if err := verifyHubRouteFile(h); err != nil {
		return leaf, err
	}
	id, err := queryConfigParentDirectoryIdentity(h)
	if err != nil {
		return leaf, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return leaf, err
	}
	if len(data) > limit {
		return leaf, fmt.Errorf("protected Hub %s exceeds its %d byte schema", name, limit)
	}
	return hubRouteLeaf{identity: id, data: data, present: true}, nil
}

func (s *hubRouteNative) checkAbsent(name string) error {
	if err := s.validate(); err != nil {
		return err
	}
	h, err := ntOpenRelative(s.parent, name, windows.FILE_READ_ATTRIBUTES)
	if isNotFoundErr(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(fmt.Errorf("protected Hub %s is already present: %w", name, errHubRouteStateChanged), windows.CloseHandle(h))
}

func (s *hubRouteNative) publicationReadback(name string, limit int, preimage, postimage hubRouteLeaf, primary error) error {
	// Parent/lock binding is rechecked before classifying either observation.
	if err := s.validate(); err != nil {
		return &HubRoutePublicationError{Outcome: HubRoutePublicationUnknown, Err: errors.Join(primary, err)}
	}
	actual, err := s.read(name, limit)
	combined := errors.Join(primary, err)
	if actual.present && postimage.present && actual.matches(postimage) {
		if s.afterReadback != nil {
			combined = errors.Join(combined, s.afterReadback())
		}
		if combined == nil {
			return nil
		}
		return &HubRoutePublicationError{Outcome: HubRoutePublicationApplied, Err: combined}
	}
	if actual.present && preimage.present && actual.matches(preimage) {
		return &HubRoutePublicationError{Outcome: HubRoutePublicationPreimageConfirmed, Err: errors.Join(errHubRouteStateChanged, combined)}
	}
	return &HubRoutePublicationError{Outcome: HubRoutePublicationUnknown, Err: errors.Join(errHubRouteStateChanged, combined)}
}

func (s *hubRouteNative) publish(ctx context.Context, name string, preimage hubRouteLeaf, payload []byte, limit int) error {
	return s.publishObserved(ctx, name, preimage, payload, limit, nil)
}

func (s *hubRouteNative) lockIdentity() (ConfigParentDirectoryIdentityV1, error) {
	return queryConfigParentDirectoryIdentity(s.lock)
}

func (s *hubRouteNative) removeOwnedReload(expected hubRouteLeaf) error {
	if !expected.present {
		return errHubRouteStateUnavailable
	}
	if err := s.validate(); err != nil {
		return err
	}
	const name = hubMcpControlTokenFileLeaf
	h, err := ntOpenRelativeWithShareAccess(s.parent, name, windows.DELETE|windows.GENERIC_READ|windows.READ_CONTROL, hubRouteGuardShare)
	if isNotFoundErr(err) {
		return nil
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(h), name)
	if file == nil {
		return errors.Join(errHubRouteStateUnavailable, windows.CloseHandle(h))
	}
	actual, err := readHubRouteLeafHandle(file, name, hubRouteReloadLimit)
	if err == nil {
		if actual.matches(expected) {
			err = setFileDeleteOnClose(h)
		} else {
			err = errHubRouteStateChanged
		}
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return s.checkAbsent(name)
}

func (s *hubRouteNative) publishObserved(ctx context.Context, name string, preimage hubRouteLeaf, payload []byte, limit int, owned *hubRouteLeaf) (retErr error) {
	if err := s.validate(); err != nil {
		return &HubRoutePublicationError{Outcome: HubRoutePublicationUnknown, Err: err}
	}
	current, err := s.read(name, limit)
	if err != nil || !current.matches(preimage) {
		return s.publicationReadback(name, limit, preimage, hubRouteLeaf{}, errors.Join(errHubRouteStateChanged, err))
	}
	if err := ctx.Err(); err != nil {
		return s.publicationReadback(name, limit, preimage, hubRouteLeaf{}, err)
	}
	tempID, err := generateHexToken()
	if err != nil {
		return s.publicationReadback(name, limit, preimage, hubRouteLeaf{}, err)
	}
	stage := "." + name + ".tmp." + tempID
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return s.publicationReadback(name, limit, preimage, hubRouteLeaf{}, err)
	}
	h, err := ntCreateRelative(s.parent, stage, windows.DELETE|windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, sd)
	if err != nil {
		return s.publicationReadback(name, limit, preimage, hubRouteLeaf{}, err)
	}
	var renamed bool
	var attempted bool
	var id ConfigParentDirectoryIdentityV1
	defer func() {
		var cleanupErr error
		if !attempted {
			cleanupErr = setFileDeleteOnClose(h)
		} else if !renamed {
			// Uncertain failed rename: only delete the retained candidate after a
			// same-parent proof that its original staging name still names it.
			probe, err := ntOpenRelative(s.parent, stage, windows.FILE_READ_ATTRIBUTES)
			if err == nil {
				observed, readErr := queryConfigParentDirectoryIdentity(probe)
				cleanupErr = errors.Join(readErr, windows.CloseHandle(probe))
				if readErr == nil && observed == id {
					cleanupErr = errors.Join(cleanupErr, setFileDeleteOnClose(h))
				}
			} else if !isNotFoundErr(err) {
				cleanupErr = err
			}
		}
		closeErr := errors.Join(cleanupErr, windows.CloseHandle(h))
		if retErr == nil && closeErr != nil {
			retErr = &HubRoutePublicationError{Outcome: HubRoutePublicationApplied, Err: closeErr}
		} else {
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	writeFailure := func(err error) error { return s.publicationReadback(name, limit, preimage, hubRouteLeaf{}, err) }
	if err := verifyHubRouteFile(h); err != nil {
		return writeFailure(err)
	}
	id, err = queryConfigParentDirectoryIdentity(h)
	if err != nil {
		return writeFailure(err)
	}
	if err := windowsWriteAll(h, payload); err != nil {
		return writeFailure(err)
	}
	if err := windows.FlushFileBuffers(h); err != nil {
		return writeFailure(err)
	}
	if err := s.validate(); err != nil {
		return writeFailure(err)
	}
	current, err = s.read(name, limit)
	if err != nil || !current.matches(preimage) {
		return writeFailure(errors.Join(errHubRouteStateChanged, err))
	}
	if err := ctx.Err(); err != nil {
		return writeFailure(err)
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(); err != nil {
			return writeFailure(err)
		}
	}
	attempted = true
	if preimage.present {
		err = ntRenameRelative(h, s.parent, name)
	} else {
		err = ntRenameRelativeNoReplace(h, s.parent, name)
	}
	renamed = err == nil
	expected := hubRouteLeaf{identity: id, data: payload, present: true}
	// A definite no-replace collision never grants custody. For any other
	// rename outcome, later cleanup may act only on the original object.
	if owned != nil && (renamed || !isAlreadyExistsErr(err)) {
		*owned = expected
	}
	if renamed && s.afterRename != nil {
		err = s.afterRename()
	}
	return s.publicationReadback(name, limit, preimage, expected, err)
}
