//go:build windows

// secure_write_windows.go — Windows leg of SecureWriteClientConfig.
//
// Sequence (spec §"SecureWriteClientConfig sequence", Windows leg):
//
//  1. Open parent dir via CreateFile with FILE_LIST_DIRECTORY +
//     FILE_FLAG_BACKUP_SEMANTICS + FILE_FLAG_OPEN_REPARSE_POINT.
//     REPARSE_POINT here means "open the metadata of the parent
//     itself WITHOUT following any reparse link present on that final
//     component"; combined with step 2's allowlist DACL verify on the
//     resulting handle, any reparse-point parent dir whose underlying
//     DACL falls outside the allowlist is rejected. Ancestor chain is
//     covered by the per-user trust boundary documented in the spec.
//  2. Handle-bound DACL verify on dirHandle via
//     verifyWindowsDACLFromHandle — owner == current-user AND every
//     read-capable ALLOW ACE names a SID in {current-user, LocalSystem,
//     BuiltinAdministrators}. Spec lines 323-326 + 422-432 ("per-target
//     parent-dir DACL check is THE confidentiality boundary"). Without
//     this gate, an enterprise GPO that broadens %USERPROFILE% to
//     Domain Users would silently leak tokens to every domain user.
//     SKIPPED when skipParentGate=true (relax lane — see step 3a/4).
//  3. crypto/rand 8-byte hex tempName = ".<base>.tmp.<pid>.<hex>".
//  3a. Build restrictive PROTECTED SECURITY_DESCRIPTOR (current-user,
//     LocalSystem, BuiltinAdministrators all granted GENERIC_ALL;
//     SE_DACL_PROTECTED set so inherited parent ACEs do not apply).
//     PR #185 r4 codex-deep-sec P1 closure: this SD is passed to
//     step 4 via OBJECT_ATTRIBUTES.SecurityDescriptor so the file
//     is BORN with the restrictive DACL. Pre-r4 the DACL was
//     installed via SetSecurityInfo AFTER NtCreateFile returned,
//     leaving a small window during which the temp file briefly
//     carried the parent's inherited DACL — a co-resident principal
//     allowed by the parent could open the file in that window and
//     keep the handle past the DACL tighten. ACL changes do not
//     revoke existing handles on Windows. Creation-time SD closes
//     that window.
//  4. NtCreateFile relative to dirHandle (RootDirectory=dirHandle,
//     ObjectName=tempName, FILE_CREATE disposition, FILE_GENERIC_WRITE
//     | DELETE | SYNCHRONIZE | WRITE_DAC desired access,
//     OBJECT_ATTRIBUTES.SecurityDescriptor=step-3a SD). DELETE is
//     mandatory because the rename in step 7 requires it; WRITE_DAC
//     enables step 5's defense-in-depth SetSecurityInfo.
//  5. SetSecurityInfo(fileHandle, DACL_SECURITY_INFORMATION |
//     PROTECTED_DACL_SECURITY_INFORMATION, ..., restrictiveDACL) —
//     defense-in-depth re-apply of the restrictive DACL on the open
//     handle. Primary protection is step 3a's create-time SD; this
//     step re-asserts the PROTECTED flag and would catch a future
//     regression that drops the create-time SD parameter.
//  6. WriteFile + FlushFileBuffers.
//  7. NtSetInformationFile(fileHandle, FileRenameInformationEx,
//     { Flags: REPLACE_IF_EXISTS|POSIX_SEMANTICS,
//       RootDirectory: dirHandle, FileName: base }).
//     fileHandle stays open across the rename (codex r5 MED).
//  8. Retain fileHandle through post-rename verification and owned cleanup.
//  9. Re-open destination via dirHandle (FILE_OPEN with
//     FILE_OPEN_REPARSE_POINT so the open does NOT silently follow a
//     reparse point swapped in between rename and re-open) and
//     re-verify the file DACL using verifyWindowsDACLFromHandle
//     (defined in hub_mcp_state_dacl_windows.go). Combined with the
//     atomic rename's FILE_RENAME_POSIX_SEMANTICS this guarantees the
//     post-rename handle refers to the file we wrote.
// 10. Close verifyHandle; close fileHandle once; close dirHandle.
//
// On any error after step 4 the temp file is marked DELETE-on-close
// via NtSetInformationFile(FileDispositionInformation). The fileHandle
// close in the defer then drops the inode.

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"mcp-local-hub/internal/clients"

	"golang.org/x/sys/windows"
)

// fileRenameInformationEx mirrors FILE_RENAME_INFORMATION_EX from
// ntifs.h (Windows 10 1709+). The Flags field is a single uint32
// bitfield holding FILE_RENAME_REPLACE_IF_EXISTS |
// FILE_RENAME_POSIX_SEMANTICS combined. RootDirectory is the parent
// dir handle the rename resolves against. FileName is a UTF-16
// trailing array; FileNameLength is in BYTES (not chars).
//
// Distinct from the legacy FILE_RENAME_INFORMATION (which exposes a
// `ReplaceIfExists` BOOLEAN at the same offset) — the EX form is
// preferred because the Flags bitfield is the only way to request
// POSIX semantics on the rename (codex r6 MED closure).
type fileRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16 // trailing UTF-16; allocated past struct end
}

// fileInformationClassRenameEx is FileRenameInformationEx in the
// FILE_INFORMATION_CLASS enum (NtSetInformationFile class parameter).
// x/sys exposes FileRenameInformation = 10 but not the EX form; the
// constant 65 is from ntifs.h (Windows SDK).
const fileInformationClassRenameEx uint32 = 65

// secureWriteClientConfigImpl is the Windows handle-relative writer.
// Symmetric with the POSIX impl in secure_write_posix.go.
//
// When skipParentGate=true the parent-dir DACL verify (step 2) is
// SKIPPED — relax lane for corp-policy hosts where the parent dir's
// inherited ACEs (CodexSandboxUsers, AppContainer SIDs, Domain Users,
// orphan AD SIDs) trip the strict gate. Everything else — symlink
// refusal, handle-relative NtCreateFile with restrictive DACL
// installed on the handle BEFORE any bytes write, atomic rename
// across the held handle, post-rename DACL verify — still applies,
// so there is NO race window between temp create and DACL apply.
// PR #185 r3 (codex deep-sec P1 closure on r2): the previous relax
// lane used os.CreateTemp + path-based SetNamedSecurityInfo, which
// left a window during which the temp file inherited parent ACEs
// before the restrictive DACL was applied. A co-resident SID allowed
// by the parent could race-open the temp file (Go's Windows runtime
// opens with FILE_SHARE_READ|FILE_SHARE_WRITE) and retain the handle
// across the DACL tighten (ACL changes do not revoke existing
// handles). Reusing the hardened pipeline closes that window.
func secureWriteClientConfigImpl(path string, contents []byte, skipParentGate bool) error {
	return secureWriteClientConfigImplObserved(path, contents, skipParentGate, nil)
}

func secureWriteClientConfigImplObserved(path string, contents []byte, skipParentGate bool, observation *stateFileIOCapture) (retErr error) {
	observeStringEntry() // AF-1 T5 observation; no-op (compiled out) in production.
	parentDir, base := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	if base == "" {
		return fmt.Errorf("secure write: empty base name in path %q", path)
	}
	// Strip trailing path separator from parent so CreateFile sees a
	// clean dir path.
	parentDir = filepath.Clean(parentDir)

	// 1. Open parent dir with FILE_LIST_DIRECTORY + FILE_FLAG_BACKUP_SEMANTICS
	//    + FILE_FLAG_OPEN_REPARSE_POINT. The reparse-point flag means
	//    "open the reparse point itself rather than its target".
	dirHandle, err := openDirHandleNoReparse(parentDir)
	if err != nil {
		return fmt.Errorf("secure write: open parent %s: %w", parentDir, err)
	}
	observation.acquired("parent")
	defer func() {
		closeErr := windows.CloseHandle(dirHandle)
		observation.closed("parent", closeErr)
		retErr = errors.Join(retErr, closeErr)
	}()

	// Steps 2-10 run against the held parent handle. The shared owner
	// (secureWriteClientConfigToResolvedParent) does NOT close dirHandle —
	// this caller owns it via the defer above. parentDirForDiag carries the
	// human-readable parent path for the ErrSecureWriteParentInsecure wrap.
	return secureWriteClientConfigToResolvedParentObserved(dirHandle, parentDir, base, contents, skipParentGate, observation, nil)
}

// secureWriteClientConfigToResolvedParent runs the hardened post-parent-open
// sequence (steps 2-10) against a CALLER-OPENED parent directory handle.
// It is the SINGLE OWNER of those steps: the path-based
// secureWriteClientConfigImpl opens the parent by path and delegates here,
// and the symlink-resolve-to-handle lane
// (secureWriteThroughResolvedParentHandle) opens the resolved target's
// parent and delegates here too — so neither path re-walks a string after
// the parent handle is frozen (AF-1 closure: no filepath.Split of a
// resolved string, no second path-based open).
//
// CONTRACT: dirHandle MUST stay open for the duration of this call and is
// NOT closed here — the caller owns the handle's lifetime. parentDirForDiag
// is the human-readable parent path used only in the
// ErrSecureWriteParentInsecure diagnostic; it is NEVER re-opened.
//
// When skipParentGate=true the parent-dir DACL verify (step 2) is SKIPPED
// (relax lane). Everything else — symlink refusal, born-with-restrictive-DACL
// temp create relative to dirHandle, atomic rename across the held handle,
// post-rename DACL re-verify — still applies, so there is NO race window
// between temp create and DACL apply and NO TOCTOU window on the parent.
func secureWriteClientConfigToResolvedParent(dirHandle windows.Handle, parentDirForDiag, base string, contents []byte, skipParentGate bool, expected ...*clients.ConfigWriteRequest) error {
	return secureWriteClientConfigToResolvedParentObserved(dirHandle, parentDirForDiag, base, contents, skipParentGate, nil, nil, expected...)
}

func secureWriteClientConfigToResolvedParentObserved(dirHandle windows.Handle, parentDirForDiag, base string, contents []byte, skipParentGate bool, observation *stateFileIOCapture, beforePublish func() error, expected ...*clients.ConfigWriteRequest) (retErr error) {
	observeResolvedParentEntry() // AF-1 T5 observation; no-op (compiled out) in production.
	guard := expectedConfigWriteRequest(expected)
	if err := verifyExpectedSnapshotWindowsObserved(dirHandle, base, guard, observation, "expected-snapshot-entry"); err != nil {
		return err
	}
	if base == "" {
		return fmt.Errorf("secure write: empty base name (parent %q)", parentDirForDiag)
	}

	// 2. Handle-bound parent-dir DACL verify (spec lines 323-326 +
	//    422-432). Wrap with parent-dir context so operators see
	//    "secure write: parent <dir> not single-user safe" rather than
	//    a file-path-only diagnostic. Without this gate, a parent dir
	//    broadened via Group Policy (Domain Users / Authenticated Users
	//    inherited ACE) would silently pass and the writer would
	//    install a token under a directory listable by every domain
	//    user. Mirror of secure_write_posix.go's verifyPosixParentDirFromFd.
	//
	// skipParentGate=true bypasses ONLY this step (relax lane). The
	// per-file DACL applied on the handle at step 5 still grants
	// access exclusively to {current-user, LocalSystem, Builtin
	// Administrators} — so the new file is NOT readable by the
	// parent's inherited principals regardless of whether the parent
	// itself is broadened.
	if !skipParentGate {
		if err := verifyWindowsDACLFromHandle(dirHandle); err != nil {
			// Wrap with ErrSecureWriteParentInsecure so the cross-package
			// wrapper in client_write_init.go can match via errors.Is
			// (issue #161 P1).
			return wrapParentGateRefusal(parentDirForDiag, err)
		}
	}

	// 2a. Refuse to overwrite a pre-existing symlink/junction at base.
	// Rename with POSIX semantics silently replaces a reparse point;
	// the caller probably meant to update the symlink target rather
	// than create a new restricted regular file at the symlink slot.
	if err := refusePreexistingReparsePoint(dirHandle, base); err != nil {
		return fmt.Errorf("secure write: target %s: %w", filepath.Join(parentDirForDiag, base), err)
	}

	// 3. Compose unpredictable temp name to defeat slot-squat races.
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("secure write: crypto/rand: %w", err)
	}
	tempName := fmt.Sprintf(".%s.tmp.%d.%s", base, os.Getpid(), hex.EncodeToString(randBytes))

	// 3a. Build restrictive PROTECTED security descriptor BEFORE temp
	//     creation so step 4 can pass it via OBJECT_ATTRIBUTES.
	//     SecurityDescriptor. This is the codex-deep-sec r3 P1 closure
	//     (PR #185 r4): the previous r2/r3 path created the temp file
	//     first and applied the DACL afterward via SetSecurityInfo —
	//     leaving a race window during which a parent-allowed
	//     principal could open the file (it briefly carried the
	//     parent's inherited DACL) and retain the handle past the
	//     DACL tighten. ACL changes do not revoke existing handles
	//     on Windows. Passing the SD at create time means the file
	//     is BORN with the restrictive DACL — zero window.
	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return fmt.Errorf("secure write: build SD: %w", err)
	}

	// 4. NtCreateFile relative to dirHandle (FILE_CREATE disposition;
	//    fails if the temp slot already exists). DELETE | WRITE_DAC |
	//    FILE_GENERIC_WRITE | SYNCHRONIZE desired access. The SD
	//    passed via OBJECT_ATTRIBUTES.SecurityDescriptor takes effect
	//    at the moment the kernel creates the object — restrictive
	//    DACL is present in the file's security descriptor before any
	//    other process can open it.
	fileHandle, err := ntCreateRelative(
		dirHandle,
		tempName,
		windows.DELETE|windows.GENERIC_WRITE|windows.SYNCHRONIZE|windows.WRITE_DAC,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		sd,
	)
	if err != nil {
		return fmt.Errorf("secure write: ntcreate temp %s: %w", tempName, err)
	}
	closed := false
	var cleanupErr error
	observation.acquired("temp")
	defer func() {
		if !closed {
			closed = true
			closeErr := windows.CloseHandle(fileHandle)
			observation.closed("temp", closeErr)
			retErr = errors.Join(retErr, cleanupErr, closeErr)
		}
	}()

	// Cleanup marks only our retained created object. Its lifetime owner closes
	// it once; cleanup failure stays visible with or without an observation.
	cleanup := func() {
		deleteErr := setFileDeleteOnClose(fileHandle)
		cleanupErr = errors.Join(cleanupErr, deleteErr)
		if observation != nil {
			observation.cleanup = errors.Join(observation.cleanup, deleteErr)
		}
	}

	// 5. SetSecurityInfo handle-bound — defense-in-depth re-apply of
	//    the restrictive DACL. Step 3a's create-time SD already covers
	//    the primary security boundary (file born with restrictive
	//    DACL, no pre-write open window). This step exists to:
	//    (a) re-assert PROTECTED_DACL_SECURITY_INFORMATION via the
	//        handle in case the create-time path took a different
	//        kernel route for the protected flag,
	//    (b) keep behavior symmetric with the old strict-only sequence
	//        so downstream tests asserting DACL state-on-handle keep
	//        passing,
	//    (c) catch a hypothetical future regression that drops the
	//        create-time SD parameter without removing this step.
	if err := setRestrictiveDACL(fileHandle); err != nil {
		cleanup()
		return fmt.Errorf("secure write: set DACL: %w", err)
	}

	// 6. WriteFile + FlushFileBuffers.
	if observation != nil {
		observation.write.entered = true
	}
	writeErr := windowsWriteAll(fileHandle, contents)
	if observation != nil {
		observation.write.returned = true
		observation.write.err = writeErr
	}
	if writeErr != nil {
		cleanup()
		return fmt.Errorf("secure write: write temp: %w", writeErr)
	}
	if observation != nil {
		observation.flush.entered = true
	}
	flushErr := windows.FlushFileBuffers(fileHandle)
	if observation != nil {
		observation.flush.returned = true
		observation.flush.err = flushErr
	}
	if flushErr != nil {
		cleanup()
		return fmt.Errorf("secure write: flush temp: %w", flushErr)
	}

	// 7. NtSetInformationFile(FileRenameInformationEx) — atomic rename
	//    relative to dirHandle. fileHandle MUST stay open across the
	//    call (codex r5 MED).
	if guard != nil && expectedSnapshotBeforePublishHook != nil {
		expectedSnapshotBeforePublishHook()
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			cleanup()
			return err
		}
	}
	if err := verifyExpectedSnapshotWindowsObserved(dirHandle, base, guard, observation, "expected-snapshot-prepublish"); err != nil {
		cleanup()
		return err
	}
	if observation != nil {
		observation.rename.entered = true
	}
	renameErr := ntRenameRelative(fileHandle, dirHandle, base)
	if observation != nil {
		observation.rename.returned = true
		observation.rename.err = renameErr
	}
	if renameErr != nil {
		cleanup()
		return fmt.Errorf("secure write: ntrename %s -> %s: %w", tempName, base, renameErr)
	}

	// Retain the original DELETE-capable created handle through verification.
	// A replacement at base is not ours; failure cleanup may mark only this
	// original object, whose single close remains owned by the defer above.

	// 9. Re-open destination via SAME dirHandle and re-verify DACL.
	//    GENERIC_READ + READ_CONTROL is needed for GetSecurityInfo to
	//    read the DACL.
	//
	//    After step 7's atomic rename a COMPLETE owner-only file is published
	//    at `base`. Re-open can fail transiently (sharing violation, momentary
	//    access-denied, scanner hiccup), so do not erase the published file
	//    on that failure. Definitive verification failure below marks only the
	//    original held object; reopening base never grants deletion authority.
	verifyHandle, err := openPostRenameVerifyHandleWindows(dirHandle, base)
	if err != nil {
		return fmt.Errorf("secure write: re-open %s: %w", base, err)
	}
	observation.acquired("verify")
	defer func() {
		closeErr := windows.CloseHandle(verifyHandle)
		observation.closed("verify", closeErr)
		retErr = errors.Join(retErr, closeErr)
	}()
	// Test-only seam (secure_write_client_config.go): force a post-rename
	// verify failure so the "no file on error" cleanup path is
	// exercisable without platform-specific DACL synthesis. nil in
	// production → the real verify below runs.
	if postRenameVerifyFailHook != nil {
		if herr := postRenameVerifyFailHook(); herr != nil {
			return postRenameFailureWindowsObserved(fileHandle,
				fmt.Errorf("secure write: post-rename verify %s: %w", base, herr), observation)
		}
	}
	if err := verifyWindowsDACLFromHandle(verifyHandle); err != nil {
		return postRenameFailureWindowsObserved(fileHandle,
			fmt.Errorf("secure write: post-rename DACL verify %s: %w", base, err), observation)
	}
	return nil
}

func secureWriteExpectedSnapshot(req clients.ConfigWriteRequest, skip bool) error {
	if req.ExpectedSnapshot == nil {
		return clients.ErrConfigWriteSnapshotConflict
	}
	if err := req.ExpectedSnapshot.ValidateBinding(req.Context, req.Path); err != nil {
		return err
	}
	return secureWriteClientConfigToResolvedParent(windows.Handle(req.ExpectedSnapshot.BorrowedParentHandle()), filepath.Dir(req.Path), filepath.Base(req.Path), req.Contents, skip, &req)
}

// The existing paired client coordinator invokes this scope while its global
// and project locks are held. The callback is synchronous and receives only the
// genuine global snapshot and the request-shaped per-operation writer. The API
// owns every capture and closes them before returning to the getter owner.
// This forward publication is Windows-specific, like its original publisher;
// it adds no route, target codec or other-platform fallback.
func withClientConfigConditionalWriterObserved(ctx context.Context, sources []*clients.CodexConfiguredProjectProfile, projects []clients.Client, evidence clients.CodexSharedStdioReplayEvidenceV1, consent *ResolvedSymlinkConsent, apply func(*clients.ConfigWriteSnapshot, func(clients.ConfigWriteRequest) error) (clients.CodexSharedStdioMutationResult, error)) (observation *clientConfigConditionalWriteObservation, result clients.CodexSharedStdioMutationResult, retErr error) {
	observation = &clientConfigConditionalWriteObservation{ctx: ctx, sources: append([]*clients.CodexConfiguredProjectProfile(nil), sources...), projects: append([]clients.Client(nil), projects...), evidence: clients.CloneCodexSharedStdioReplayEvidence(evidence)}
	defer func() {
		observation.active = false
		observation.result = result
		if observation.primaryError == nil {
			observation.primaryError = retErr
		}
		retErr = errors.Join(retErr, observation.closeObserved())
	}()
	if ctx == nil || apply == nil || len(sources) == 0 || len(sources) != len(projects) || len(sources) != len(evidence.Sources) {
		return observation, result, clients.ErrConfigWriteSnapshotConflict
	}
	if err := ctx.Err(); err != nil {
		return observation, result, err
	}
	if _, finite := ctx.Deadline(); !finite {
		return observation, result, errors.New("secure write: original finite invocation context required")
	}
	if consent != nil {
		copy := *consent
		observation.consent = &copy
	}
	for i, source := range observation.sources {
		if source == nil || observation.projects[i] == nil {
			return observation, result, clients.ErrConfigWriteSnapshotConflict
		}
		global, project, err := source.ConfiguredSourcePreimages()
		if err != nil {
			return observation, result, err
		}
		logical, referent, links, err := source.ConfiguredGlobalSourcePath()
		if err != nil {
			return observation, result, err
		}
		if i == 0 {
			observation.globalSource = global
			observation.logical = logical
			observation.referent = referent
			observation.links = links
		} else if logical != observation.logical || referent != observation.referent || !bytes.Equal(global, observation.globalSource) {
			return observation, result, clients.ErrConfigWriteSnapshotConflict
		}
		observation.projectSources = append(observation.projectSources, project)
	}
	observation.global = captureConditionalClientConfigInput(ctx, observation.referent, observation.globalSource)
	if observation.global.captureError != nil {
		return observation, result, observation.global.captureError
	}
	if observation.global.snapshot == nil {
		return observation, result, clients.ErrConfigWriteSnapshotConflict
	}
	for i, project := range observation.projects {
		input := captureConditionalClientConfigInput(ctx, project.ConfigPath(), observation.projectSources[i])
		observation.projectInputs = append(observation.projectInputs, input)
		if input.captureError != nil {
			return observation, result, input.captureError
		}
		if input.snapshot == nil {
			return observation, result, clients.ErrConfigWriteSnapshotConflict
		}
	}
	observation.active = true
	observation.clientEntered = true
	result, observation.clientError = apply(observation.global.snapshot, observation.publish)
	observation.clientReturned = true
	observation.active = false
	if observation.clientError != nil && observation.primaryError == nil {
		observation.primaryError = observation.clientError
	}
	if !observation.publicationEntered && observation.clientError == nil {
		if result.State != clients.CodexSharedStdioNoChange || (result.DesiredToken != clients.CodexSharedStdioBefore && result.DesiredToken != clients.CodexSharedStdioApplied) || result.ExpectedConfigSHA256 != preparedSerenaHash(observation.globalSource) {
			observation.publicationError = errors.New("secure write: conditional callback did not qualify publication or no-change")
		} else if err := observation.validateInputs(); err != nil {
			observation.publicationError = err
		} else {
			observation.readback = captureConditionalClientConfigInput(ctx, observation.referent, observation.globalSource)
			observation.readbackError = observation.readback.captureError
		}
	}
	return observation, result, errors.Join(observation.primaryError, observation.clientError, observation.publicationError, observation.readbackError)
}

// One semantic check is invoked at the original commit boundary, or once for a
// qualified no-write arm. It never substitutes replay location for current inode.
func (o *clientConfigConditionalWriteObservation) validateInputs() error {
	if err := o.ctx.Err(); err != nil {
		return err
	}
	for i, source := range o.sources {
		if err := source.ValidateConfiguredGlobalBinding(o.ctx); err != nil {
			return err
		}
		if err := source.ValidateConfiguredProjectSourceSnapshot(o.ctx, o.projects[i], o.projectInputs[i].snapshot); err != nil {
			return err
		}
		if err := source.ValidateSharedStdioReplaySource(o.ctx, o.evidence.Sources[i], o.evidence.GlobalLocation); err != nil {
			return err
		}
	}
	return validateConditionalClientConfigConsent(o)
}
func (o *clientConfigConditionalWriteObservation) publish(req clients.ConfigWriteRequest) (retErr error) {
	if o == nil || !o.active {
		return clients.ErrConfigWriteSnapshotConflict
	}
	defer func() {
		o.publicationError = errors.Join(o.publicationError, retErr)
		if retErr != nil && o.primaryError == nil {
			o.primaryError = retErr
		}
	}()
	if o.publicationEntered || req.Context != o.ctx || req.Path != o.referent || req.ExpectedSnapshot == nil || req.ExpectedSnapshot != o.global.snapshot || req.FreshCreationEvidence != nil {
		return clients.ErrConfigWriteSnapshotConflict
	}
	if err := req.ExpectedSnapshot.ValidateObservedCapture(o.ctx, o.referent); err != nil {
		return err
	}
	o.request = req
	o.request.Contents = append([]byte(nil), req.Contents...)
	o.publicationEntered = true
	// One semantic guard at the original publication boundary. The project
	// validator proves its exact constructor/profile/capture/current-file
	// relationship; a matching ConfigPath or hash alone cannot qualify it.
	beforePublish := o.validateInputs
	writeErr := secureWriteWithParentPolicy(o.referent, func(skip bool) error {
		attempt := clientConfigConditionalNativeWrite{capture: newStateFileIOCapture(), entered: true}
		o.native = append(o.native, attempt)
		index := len(o.native) - 1
		err := secureWriteClientConfigToResolvedParentObserved(windows.Handle(o.global.snapshot.BorrowedParentHandle()), filepath.Dir(o.referent), filepath.Base(o.referent), o.request.Contents, skip, o.native[index].capture, beforePublish, &o.request)
		o.native[index].returned = true
		o.native[index].err = err
		return err
	})
	o.publicationReturned = true
	if writeErr != nil && o.primaryError == nil {
		o.primaryError = writeErr
	}
	// Successful rename is retained as an event, including when later native
	// verification or close fails. It does not imply that the file still exists.
	for _, attempt := range o.native {
		rename := attempt.capture.rename
		if rename.entered && rename.returned && rename.err == nil {
			o.readback = captureConditionalClientConfigInput(o.ctx, o.referent, o.request.Contents)
			o.readbackError = o.readback.captureError
			break
		}
	}
	return errors.Join(writeErr, o.readbackError)
}

func verifyExpectedSnapshotWindows(parent windows.Handle, base string, req *clients.ConfigWriteRequest) error {
	return verifyExpectedSnapshotWindowsObserved(parent, base, req, nil, "")
}
func verifyExpectedSnapshotWindowsObserved(parent windows.Handle, base string, req *clients.ConfigWriteRequest, capture *stateFileIOCapture, slot string) (retErr error) {
	if req == nil {
		return nil
	}
	if req.ExpectedSnapshot == nil {
		return clients.ErrConfigWriteSnapshotConflict
	}
	if err := req.ExpectedSnapshot.ValidateBinding(req.Context, req.Path); err != nil {
		return err
	}
	h, err := ntOpenRelative(parent, base, windows.FILE_GENERIC_READ)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), req.Path)
	if f == nil {
		return errors.Join(errors.New("STATE_EXPECTED_SNAPSHOT_FILE_UNAVAILABLE"), windows.CloseHandle(h))
	}
	capture.acquired(slot)
	defer func() {
		closeErr := f.Close()
		capture.closed(slot, closeErr)
		retErr = errors.Join(retErr, closeErr)
	}()
	return req.ExpectedSnapshot.VerifyCurrentFile(req.Context, f)
}

func openPostRenameVerifyHandleWindows(dirHandle windows.Handle, base string) (windows.Handle, error) {
	var lastErr error
	for attempt := 1; attempt <= postRenameOpenMaxAttempts; attempt++ {
		if postRenameOpenFailHook != nil {
			if herr := postRenameOpenFailHook(); herr != nil {
				lastErr = herr
				if !isRetryablePostRenameOpenErrWindows(herr) || attempt == postRenameOpenMaxAttempts {
					return windows.InvalidHandle, postRenameOpenErrAfterAttempts(attempt, herr)
				}
				time.Sleep(postRenameOpenRetryDelay)
				continue
			}
		}
		h, err := ntOpenRelative(dirHandle, base, windows.GENERIC_READ|windows.READ_CONTROL)
		if err == nil {
			return h, nil
		}
		lastErr = err
		if !isRetryablePostRenameOpenErrWindows(err) || attempt == postRenameOpenMaxAttempts {
			return windows.InvalidHandle, postRenameOpenErrAfterAttempts(attempt, err)
		}
		time.Sleep(postRenameOpenRetryDelay)
	}
	return windows.InvalidHandle, lastErr
}

func postRenameOpenErrAfterAttempts(attempts int, err error) error {
	if attempts <= 1 {
		return err
	}
	return fmt.Errorf("still failing after %d attempts: %w", attempts, err)
}

func isRetryablePostRenameOpenErrWindows(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_BUSY) ||
		ntStatusIs(err, windows.STATUS_ACCESS_DENIED) ||
		ntStatusIs(err, windows.STATUS_SHARING_VIOLATION) ||
		ntStatusIs(err, windows.STATUS_FILE_LOCK_CONFLICT) ||
		ntStatusIs(err, windows.STATUS_LOCK_NOT_GRANTED) ||
		ntStatusIs(err, windows.STATUS_PENDING) ||
		ntStatusIs(err, windows.STATUS_OPLOCK_BREAK_IN_PROGRESS) ||
		ntStatusIs(err, windows.STATUS_DELETE_PENDING) ||
		ntStatusIs(err, windows.STATUS_DEVICE_BUSY) ||
		ntStatusIs(err, windows.STATUS_TIMEOUT) ||
		ntStatusIs(err, windows.STATUS_RETRY)
}

// Definitive failure marks only the operation-created object whose original
// DELETE handle is still held. The caller owns its one close attempt. A current
// basename, including a foreign replacement, supplies no deletion authority.
func postRenameFailureWindowsObserved(ownedFile windows.Handle, origErr error, observation *stateFileIOCapture) error {
	deleteErr := setFileDeleteOnClose(ownedFile)
	if observation != nil {
		observation.cleanup = errors.Join(observation.cleanup, deleteErr)
	}
	if deleteErr != nil {
		return errors.Join(origErr, fmt.Errorf("secure write: mark original published object delete-on-close (owned cleanup remains unsettled): %w", deleteErr))
	}
	return origErr
}

// openDirHandleNoReparse opens a directory with FILE_LIST_DIRECTORY +
// READ_CONTROL + FILE_FLAG_BACKUP_SEMANTICS + FILE_FLAG_OPEN_REPARSE_POINT.
// The reparse-point flag here means "open the reparse point itself rather
// than the target", which is the right behavior for our purposes —
// the caller's downstream operations (NtCreateFile relative to this
// handle) inherit the same anchor and can't be re-walked from root.
// READ_CONTROL is required so the step-2 parent-DACL verify (which
// calls GetSecurityInfo on this handle) can read the DACL; without it
// GetSecurityInfo fails with ACCESS_DENIED on dirs whose DACL doesn't
// happen to grant the current process token read-control via some
// broader right.
//
// FILE_FLAG_OPEN_REPARSE_POINT alone doesn't fail on symlinks — but
// any subsequent DACL allowlist check rejects mis-owned reparse points.
func openDirHandleNoReparse(path string) (windows.Handle, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("utf16 %q: %w", path, err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.FILE_LIST_DIRECTORY|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return h, nil
}

// openTraverseOnlyDirHandleNoReparse opens a directory TRAVERSE-ONLY
// (FILE_TRAVERSE | FILE_READ_ATTRIBUTES) with FILE_FLAG_BACKUP_SEMANTICS +
// FILE_FLAG_OPEN_REPARSE_POINT. It is the Finding 1 volume-root-anchor analog
// of openDirHandleNoReparse for the resolved-symlink component descent: the
// root anchor (a drive root "C:\" or a UNC share root \\server\share) only
// needs to be TRAVERSED to descend O_NOFOLLOW-relative into its first
// intermediate — ordinary path traversal and the old full-parent open never
// needed FILE_LIST_DIRECTORY on the root.
//
// It is SEPARATE from openDirHandleNoReparse on purpose: that shared helper
// returns a LIST/READ_CONTROL handle because its OTHER callers
// (secureWriteImpl's production parent open, G17's home-anchor open,
// secure_create_client_config_windows.go) run a parent-DACL verify via
// GetSecurityInfo, which needs READ_CONTROL. Leaving openDirHandleNoReparse
// UNCHANGED keeps G17's read-fd DACL-verify path intact; only the
// resolved-symlink anchor drops to traverse-only.
//
// FILE_FLAG_OPEN_REPARSE_POINT is kept (so even a reparse-point root would be
// opened as the link itself rather than followed — though a drive/share root
// is never itself a reparse point); the per-component reparse refusal still
// fires on every intermediate.
func openTraverseOnlyDirHandleNoReparse(path string) (windows.Handle, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("utf16 %q: %w", path, err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return h, nil
}

// ntCreateRelative wraps NtCreateFile with RootDirectory=dirHandle.
// ObjectName is the relative path; ShareAccess is full r/w/d so the
// post-rename re-open at step 8 succeeds even if AV scanners hold
// transient handles.
//
// securityDescriptor is optional. When non-nil it is attached via
// OBJECT_ATTRIBUTES.SecurityDescriptor so the file is BORN with that
// security descriptor (no pre-DACL race window). Callers opening
// existing files (FILE_OPEN disposition via ntOpenRelative) pass
// nil — the existing file's SD is preserved. Callers creating new
// files for token-bearing writes pass a restrictive SD (see
// buildRestrictiveSecurityDescriptor) so the temp file's DACL is
// in place at byte zero rather than installed afterward via
// SetSecurityInfo. PR #185 r4 codex-deep-sec P1 closure.
func ntCreateRelative(
	dirHandle windows.Handle,
	name string,
	desiredAccess uint32,
	disposition uint32,
	createOptions uint32,
	securityDescriptor *windows.SECURITY_DESCRIPTOR,
) (windows.Handle, error) {
	return ntCreateRelativeWithShareAccess(
		dirHandle,
		name,
		desiredAccess,
		disposition,
		createOptions,
		securityDescriptor,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
}

func ntCreateRelativeWithShareAccess(
	dirHandle windows.Handle,
	name string,
	desiredAccess uint32,
	disposition uint32,
	createOptions uint32,
	securityDescriptor *windows.SECURITY_DESCRIPTOR,
	shareAccess uint32,
) (windows.Handle, error) {
	return ntCreateRelativeWithShareAccessObserved(dirHandle, name, desiredAccess, disposition, createOptions, securityDescriptor, shareAccess, nil, "")
}

func ntCreateRelativeObserved(dirHandle windows.Handle, name string, desiredAccess, disposition, createOptions uint32, securityDescriptor *windows.SECURITY_DESCRIPTOR, b *stateFileReadBuilder, slot string) (windows.Handle, error) {
	return ntCreateRelativeWithShareAccessObserved(dirHandle, name, desiredAccess, disposition, createOptions, securityDescriptor, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, b, slot)
}

func ntCreateRelativeWithShareAccessObserved(dirHandle windows.Handle, name string, desiredAccess, disposition, createOptions uint32, securityDescriptor *windows.SECURITY_DESCRIPTOR, shareAccess uint32, b *stateFileReadBuilder, slot string) (windows.Handle, error) {
	if b != nil {
		if err := b.checkContext(); err != nil {
			return windows.InvalidHandle, err
		}
		if b.resource(slot) == nil {
			b.prearmResource(slot)
		}
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("nt unicode %q: %w", name, err)
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		ObjectName:         objectName,
		RootDirectory:      dirHandle,
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: securityDescriptor,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var allocSize int64 = 0
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		desiredAccess,
		oa,
		&iosb,
		&allocSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		shareAccess,
		disposition,
		createOptions,
		0,
		0,
	); err != nil {
		if b != nil && handle != 0 && handle != windows.InvalidHandle {
			b.resource(slot).view.Ownership = "borrowed"
			b.borrowedErr = errors.Join(b.borrowedErr, errors.New("failed directory native call returned undefined handle; no close authority"))
		}
		return windows.InvalidHandle, err
	}
	if b != nil {
		if handle == 0 || handle == windows.InvalidHandle {
			return windows.InvalidHandle, errors.New("successful directory native call has no original handle")
		}
		registerParentDirectory(b, slot, handle)
		if err := b.checkContext(); err != nil {
			return windows.InvalidHandle, errors.Join(err, closeParentDirectory(b, slot, handle))
		}
	}
	return handle, nil
}

// ntOpenRelative wraps NtCreateFile with FILE_OPEN disposition for an
// existing file. Used by the post-rename re-verify step.
//
// FILE_OPEN_REPARSE_POINT in createOptions does NOT make the open
// fail on a reparse point — it instructs NtCreateFile to open the
// reparse point's metadata WITHOUT following the link. Callers that
// must reject reparse points still need to inspect FileAttributes
// after open (see refusePreexistingReparsePoint earlier in this file).
//
// The actual reparse-point reject for the destination filename comes
// from the explicit refusePreexistingReparsePoint call in step 2a
// (run BEFORE the temp write). The atomic rename in step 7 uses
// FILE_RENAME_POSIX_SEMANTICS, which renames the file we just wrote
// over the destination basename in one transactional step; the
// post-rename re-open via this helper therefore lands on the file we
// wrote, even if a hostile process swapped a reparse point into the
// slot between rename and re-open (the rename was atomic on the
// dirHandle-relative basename, not a path re-walk).
func ntOpenRelative(
	dirHandle windows.Handle,
	name string,
	desiredAccess uint32,
) (windows.Handle, error) {
	return ntOpenRelativeWithShareAccess(
		dirHandle,
		name,
		desiredAccess,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
}

func ntOpenRelativeWithShareAccess(
	dirHandle windows.Handle,
	name string,
	desiredAccess uint32,
	shareAccess uint32,
) (windows.Handle, error) {
	// Opening an existing file — pass nil SD so the existing security
	// descriptor on disk is preserved.
	return ntCreateRelativeWithShareAccess(
		dirHandle,
		name,
		desiredAccess|windows.SYNCHRONIZE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		nil,
		shareAccess,
	)
}

// ntRenameRelative wraps NtSetInformationFile with
// FileRenameInformationEx (class 65). Renames `fileHandle` to
// `newBase` under `dirHandle`. Uses REPLACE_IF_EXISTS | POSIX_SEMANTICS
// so the rename is atomic and replaces any existing destination.
//
// IMPORTANT: fileHandle MUST remain open across this call (codex r5
// MED). After success the caller should close fileHandle and re-open
// the new path via dirHandle.
func ntRenameRelative(
	fileHandle windows.Handle,
	dirHandle windows.Handle,
	newBase string,
) error {
	newBaseUTF16, err := windows.UTF16FromString(newBase)
	if err != nil {
		return fmt.Errorf("utf16 %q: %w", newBase, err)
	}
	// UTF16FromString includes a trailing NUL — strip it from the
	// length field per FILE_RENAME_INFORMATION_EX contract (length is
	// in bytes, excludes trailing NUL).
	nameLenChars := len(newBaseUTF16) - 1
	if nameLenChars < 0 {
		nameLenChars = 0
	}
	nameLenBytes := nameLenChars * 2

	// Allocate a single buffer big enough for the struct header plus
	// the trailing UTF-16 array (without trailing NUL).
	headerSize := int(unsafe.Offsetof(fileRenameInformationEx{}.FileName))
	bufferSize := headerSize + nameLenBytes
	if bufferSize < int(unsafe.Sizeof(fileRenameInformationEx{})) {
		bufferSize = int(unsafe.Sizeof(fileRenameInformationEx{}))
	}
	buffer := make([]byte, bufferSize)
	typedBuf := (*fileRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	typedBuf.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	typedBuf.RootDirectory = dirHandle
	typedBuf.FileNameLength = uint32(nameLenBytes)
	if nameLenChars > 0 {
		dst := unsafe.Slice((*uint16)(unsafe.Pointer(&typedBuf.FileName[0])), nameLenChars)
		copy(dst, newBaseUTF16[:nameLenChars])
	}

	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		fileHandle,
		&iosb,
		&buffer[0],
		uint32(bufferSize),
		fileInformationClassRenameEx,
	)
}

// setRestrictiveDACL applies a DACL to fileHandle that grants
// GENERIC_ALL to {current-user-SID, LocalSystem, BuiltinAdministrators}
// only. PROTECTED_DACL_SECURITY_INFORMATION prevents inherited ACEs
// from re-broadening access between rename and the post-rename DACL
// re-verify.
//
// On any error the file is in an inconsistent state — the caller MUST
// unlink it via cleanup().
func setRestrictiveDACL(fileHandle windows.Handle) error {
	dacl, err := buildRestrictiveDACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		fileHandle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

// buildRestrictiveSecurityDescriptor wraps the restrictive DACL into
// a SECURITY_DESCRIPTOR with PROTECTED_DACL set, suitable for passing
// via OBJECT_ATTRIBUTES.SecurityDescriptor at file-create time. PR
// #185 r4 codex-deep-sec P1 closure: the previous r3 path installed
// the DACL after NtCreateFile returned, leaving a window during
// which a parent-allowed principal could open the file and retain
// the handle past the DACL tighten. Building the SD here and passing
// it at create time means the file is born with the restrictive
// DACL — no window.
//
// The PROTECTED bit (SE_DACL_PROTECTED) blocks inheritance from the
// parent dir's ACEs, so even if the parent has Authenticated Users
// or similar inheritable read ACEs, they do NOT propagate to this
// file.
//
// v0.5.0 phase 1 task 1.2: body now delegates to BuildAllowlistSD
// (dacl_shared_windows.go) which renders the same allowlist
// ({current-user, LocalSystem, BuiltinAdministrators} with GENERIC_ALL)
// as a PROTECTED DACL via SecurityDescriptorFromString. The two paths
// produce SDs with identical on-disk ACE binary form (same control
// bits, same ACE type, same SID set, same masks); the EXPLICIT_ACCESS
// trustee-type hints from the prior ACLFromEntries path do not survive
// into the persisted ACE format, and verifyWindowsDACLFromHandleMasked
// matches by SID equality. The shared primitive will be reused by the
// v0.5.0 supervisor named-pipe SDDL form (AllowlistMaskPipe), which is
// why the helper supports both output shapes.
func buildRestrictiveSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	return BuildAllowlistSD()
}

// buildRestrictiveDACL constructs the {current-user, LocalSystem,
// BuiltinAdministrators}-only DACL used by setRestrictiveDACL and
// buildRestrictiveSecurityDescriptor.
func buildRestrictiveDACL() (*windows.ACL, error) {
	entries, err := allowlistExplicitAccess()
	if err != nil {
		return nil, err
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("acl from entries: %w", err)
	}
	return dacl, nil
}

// explicitAccessAllow builds an EXPLICIT_ACCESS entry that ALLOWs the
// given SID the given access mask. NO_INHERITANCE means the ACE does
// not propagate to children — the temp file has no children, so this
// is purely correctness hygiene.
func explicitAccessAllow(sid *windows.SID, trusteeType uint32, mask uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(mask),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_TYPE(trusteeType),
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

// windowsWriteAll writes the full payload to fileHandle, retrying on
// partial writes. WriteFile may return short on huge buffers; we loop
// until done or error.
func windowsWriteAll(handle windows.Handle, p []byte) error {
	for len(p) > 0 {
		var written uint32
		if err := windows.WriteFile(handle, p, &written, nil); err != nil {
			return err
		}
		if written == 0 {
			return fmt.Errorf("WriteFile returned 0 bytes")
		}
		p = p[written:]
	}
	return nil
}

// refusePreexistingReparsePoint stats the destination `name` under
// `dirHandle` (relative-open) and refuses if the existing file is a
// reparse point (symlink/junction). If the file doesn't exist, returns
// nil; if it's a regular file, returns nil so the atomic rename can
// replace it.
//
// Implementation: open `name` via dirHandle with READ_ATTRIBUTES +
// FILE_OPEN_REPARSE_POINT. If the open fails with ERROR_FILE_NOT_FOUND
// or ERROR_PATH_NOT_FOUND, the slot is empty (success). If the open
// succeeds, query attributes via GetFileInformationByHandle and check
// FILE_ATTRIBUTE_REPARSE_POINT.
func refusePreexistingReparsePoint(dirHandle windows.Handle, name string) error {
	h, err := ntOpenRelative(dirHandle, name, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		// ERROR_FILE_NOT_FOUND / ERROR_PATH_NOT_FOUND / NT
		// STATUS_OBJECT_NAME_NOT_FOUND all map through x/sys to the
		// same NTStatus error wrapping. Treat any "not found" as
		// "slot empty, fine".
		if isNotFoundErr(err) {
			return nil
		}
		// Cannot tell — surface the open error so the caller doesn't
		// proceed with a write that might collide with an existing
		// reparse point.
		return fmt.Errorf("probe %s: %w", name, err)
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fmt.Errorf("get file info %s: %w", name, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrSecureWriteReparsePointRefused
	}
	return nil
}

// isNotFoundErr returns true if err is one of the Windows "object not
// found" sentinels. NtCreateFile maps several NTSTATUS values into
// this group; we match NTSTATUS values STATUS_OBJECT_NAME_NOT_FOUND
// and STATUS_OBJECT_PATH_NOT_FOUND via direct typed comparison against
// the x/sys/windows-exported constants.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	// x/sys returns NTStatus directly for NtCreateFile failures.
	if ns, ok := err.(windows.NTStatus); ok {
		if ns == windows.STATUS_OBJECT_NAME_NOT_FOUND || ns == windows.STATUS_OBJECT_PATH_NOT_FOUND {
			return true
		}
	}
	return false
}

// setFileDeleteOnClose marks the file for delete-on-close via
// FILE_DISPOSITION_INFORMATION (legacy class — single-byte BOOLEAN).
// Best-effort cleanup when a step after NtCreateFile fails.
func setFileDeleteOnClose(handle windows.Handle) error {
	// Use the raw 1-byte BOOLEAN per FILE_DISPOSITION_INFORMATION
	// layout. The legacy class is broadly supported; the EX form has
	// extra fields we don't need.
	delete := uint8(1)
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		handle,
		&iosb,
		&delete,
		1,
		windows.FileDispositionInformation,
	)
}
