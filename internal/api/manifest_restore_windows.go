//go:build windows

package api

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func (a *API) manifestRestoreExactInWithAdoptLeaseHeld(dir, name, yaml string) error {
	return a.manifestRestoreExactInWithAdoptLeaseHeldWithGate(dir, name, yaml, nil, nil)
}

// The store invokes the injected read-only admission only after the complete
// temporary file has been synced and closed, immediately before Link.
func (a *API) manifestRestoreExactInWithAdoptLeaseHeldWithGate(dir, name, yaml string, beforeGate func(), gate func() error) error {
	if err := checkManifestName(name); err != nil || isEmbeddedManifestName(name) {
		return restoreFailure("ATTACH_MANIFEST_CANDIDATE_MISMATCH", err)
	}
	if warnings := a.validateManifestForStorageName(name, yaml); len(warnings) != 0 {
		return restoreFailure("ATTACH_MANIFEST_CANDIDATE_MISMATCH", errors.New("manifest validation failed"))
	}
	if _, err := a.ManifestValidateMode(yaml, ValidateModeStrict); err != nil {
		return restoreFailure("ATTACH_MANIFEST_CANDIDATE_MISMATCH", err)
	}
	return runManifestMutationUnderVaultLock(name, func() error {
		return restoreExactManifestNoReplaceWithGateAndLink(dir, name, []byte(yaml), beforeGate, gate, os.Link)
	})
}

// restoreExactManifestNoReplace publishes only a synced, closed regular file.
// os.Link is deliberately the final-name operation: unlike Rename, it cannot
// replace an existing Windows destination.
func restoreExactManifestNoReplace(dir, name string, raw []byte) error {
	return restoreExactManifestNoReplaceWithHook(dir, name, raw, nil)
}

// beforeLink is an in-package test barrier; product calls always pass nil.
func restoreExactManifestNoReplaceWithHook(dir, name string, raw []byte, beforeLink func()) error {
	return restoreExactManifestNoReplaceWithGate(dir, name, raw, beforeLink, nil)
}

func restoreExactManifestNoReplaceWithGate(dir, name string, raw []byte, beforeLink func(), gate func() error) error {
	return restoreExactManifestNoReplaceWithGateAndLink(dir, name, raw, beforeLink, gate, os.Link)
}

// This exact operation seam is test-only; production always passes os.Link.
func restoreExactManifestNoReplaceWithLinkFn(dir, name string, raw []byte, beforeLink func(), linkFn func(string, string) error) error {
	return restoreExactManifestNoReplaceWithGateAndLink(dir, name, raw, beforeLink, nil, linkFn)
}

func restoreExactManifestNoReplaceWithGateAndLink(dir, name string, raw []byte, beforeLink func(), gate func() error, linkFn func(string, string) error) error {
	serverDir := filepath.Join(dir, name)
	final := filepath.Join(serverDir, "manifest.yaml")
	if _, err := os.Lstat(final); err == nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_CONFLICT", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_CONFLICT", err)
	}
	if err := os.MkdirAll(serverDir, 0700); err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	serverInfo, err := os.Lstat(serverDir)
	if err != nil || !serverInfo.IsDir() || serverInfo.Mode()&os.ModeSymlink != 0 {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_CONFLICT", err)
	}
	recoveryDir := filepath.Join(serverDir, ".manifest-recovery")
	if err := os.Mkdir(recoveryDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	recoveryInfo, err := os.Lstat(recoveryDir)
	if err != nil || !recoveryInfo.IsDir() || recoveryInfo.Mode()&os.ModeSymlink != 0 {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_CONFLICT", err)
	}
	temp := filepath.Join(recoveryDir, "manifest-"+ManifestHashContent(raw)+".tmp")
	entries, err := os.ReadDir(recoveryDir)
	if err != nil || len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != filepath.Base(temp)) {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	if err := settleExactRestoreTemp(temp, final, raw); err != nil {
		return err
	}
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_CONFLICT", err)
	}
	writeErr := error(nil)
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		writeErr = errors.Join(err, io.ErrShortWrite)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	writeErr = errors.Join(writeErr, f.Close())
	if writeErr != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", errors.Join(writeErr, cleanupExactRestoreTemp(temp, recoveryDir)))
	}
	tempInfo, err := os.Lstat(temp)
	if err != nil || !tempInfo.Mode().IsRegular() {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", errors.Join(err, cleanupExactRestoreTemp(temp, recoveryDir)))
	}
	if beforeLink != nil {
		beforeLink()
	}
	if gate != nil {
		if gateErr := gate(); gateErr != nil {
			if cleanupErr := cleanupExactRestoreTemp(temp, recoveryDir); cleanupErr != nil {
				return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", errors.Join(gateErr, cleanupErr))
			}
			return gateErr
		}
	}
	if err := linkFn(temp, final); err != nil {
		code := "ATTACH_MANIFEST_RESTORE_UNSUPPORTED_FS"
		if errors.Is(err, os.ErrExist) {
			code = "ATTACH_MANIFEST_RESTORE_CONFLICT"
		}
		if cleanupErr := cleanupExactRestoreTemp(temp, recoveryDir); cleanupErr != nil {
			return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", errors.Join(err, cleanupErr))
		}
		return restoreFailure(code, err)
	}
	if err := settlePublishedRestorePair(temp, final, recoveryDir, ManifestHashContent(raw), int64(len(raw))); err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	return nil
}

func cleanupExactRestoreTemp(temp, dir string) error {
	info, err := os.Lstat(temp)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("restore temporary name is not a regular file")
		}
		f, openErr := os.Open(temp)
		if openErr != nil {
			return openErr
		}
		opened, statErr := f.Stat()
		var identity windows.ByHandleFileInformation
		if statErr == nil {
			statErr = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &identity)
		}
		closeErr := f.Close()
		if statErr != nil || closeErr != nil || !os.SameFile(info, opened) || identity.NumberOfLinks != 1 {
			return errors.Join(errors.New("restore temporary ownership is unverified"), statErr, closeErr)
		}
		if err := os.Remove(temp); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return settleEmptyRestoreRecoveryDir(dir)
}

func settleEmptyRestoreRecoveryDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("restore recovery directory identity is unverified"), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return errors.Join(errors.New("restore recovery directory is not confirmed empty"), err)
	}
	if err := os.Remove(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("restore recovery directory removal unverified"), err)
	}
	return nil
}

// The opened handles, not path names alone, establish the exclusive two-link
// pair. Keeping final open permits a post-unlink count check before success.
func settlePublishedRestorePair(temp, final, recoveryDir, expectedHash string, expectedSize int64) (retErr error) {
	tempPathInfo, err := os.Lstat(temp)
	if err != nil || !tempPathInfo.Mode().IsRegular() {
		return errors.Join(errors.New("restore temporary file is unverified"), err)
	}
	finalPathInfo, err := os.Lstat(final)
	if err != nil || !finalPathInfo.Mode().IsRegular() || !os.SameFile(tempPathInfo, finalPathInfo) {
		return errors.Join(errors.New("restore canonical file identity is unverified"), err)
	}
	tempFile, err := os.Open(temp)
	if err != nil {
		return err
	}
	defer func() {
		if tempFile != nil {
			retErr = errors.Join(retErr, tempFile.Close())
		}
	}()
	finalFile, err := os.Open(final)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, finalFile.Close()) }()
	tempOpened, tempErr := tempFile.Stat()
	finalOpened, finalErr := finalFile.Stat()
	if tempErr != nil || finalErr != nil || !os.SameFile(tempPathInfo, tempOpened) || !os.SameFile(finalPathInfo, finalOpened) || !os.SameFile(tempOpened, finalOpened) {
		return errors.Join(errors.New("restore opened file identity or size is unverified"), tempErr, finalErr)
	}
	if expectedSize < 0 {
		expectedSize = finalOpened.Size()
	}
	if expectedSize <= 0 || expectedSize > attachManifestSourceMaxBytes || finalOpened.Size() != expectedSize {
		return errors.New("restore opened file size is unverified")
	}
	readback, err := io.ReadAll(io.LimitReader(finalFile, expectedSize+1))
	if err != nil || int64(len(readback)) != expectedSize || ManifestHashContent(readback) != expectedHash {
		return errors.Join(errors.New("restore canonical file hash is unverified"), err)
	}
	var tempIdentity, finalIdentity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(tempFile.Fd()), &tempIdentity); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(finalFile.Fd()), &finalIdentity); err != nil {
		return err
	}
	if tempIdentity.NumberOfLinks != 2 || finalIdentity.NumberOfLinks != 2 {
		return errors.New("restore pair has an unknown hardlink alias")
	}
	if err := tempFile.Close(); err != nil {
		tempFile = nil
		return err
	}
	tempFile = nil
	currentTemp, err := os.Lstat(temp)
	if err != nil || !os.SameFile(tempOpened, currentTemp) {
		return errors.Join(errors.New("restore temporary name changed before unlink"), err)
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(finalFile.Fd()), &finalIdentity); err != nil || finalIdentity.NumberOfLinks != 2 {
		return errors.Join(errors.New("restore pair link count changed before unlink"), err)
	}
	if err := os.Remove(temp); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(finalFile.Fd()), &finalIdentity); err != nil || finalIdentity.NumberOfLinks != 1 {
		return errors.Join(errors.New("restore canonical link count after unlink is unverified"), err)
	}
	return settleEmptyRestoreRecoveryDir(recoveryDir)
}

// A crash before Link may leave only this deterministic single-link leaf.
// A crash after Link may leave the verified same-file pair. Unknown residue is
// retained and refused; it is never treated as authority to replace final.
func settleExactRestoreTemp(temp, final string, raw []byte) error {
	info, err := os.Lstat(temp)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	f, err := os.Open(temp)
	if err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != int64(len(raw)) {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	actual, err := io.ReadAll(io.LimitReader(f, int64(len(raw))+1))
	if err != nil || ManifestHashContent(actual) != ManifestHashContent(raw) || len(actual) != len(raw) {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &identity); err != nil || identity.NumberOfLinks != 1 {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_CONFLICT", err)
	}
	if err := f.Close(); err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	current, err := os.Lstat(temp)
	if err != nil || !os.SameFile(opened, current) {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	if err := os.Remove(temp); err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	return nil
}

// settlePublishedRestoreUnderLease is the re-entry path for a crash after
// Link but before the owned temporary name was removed. It never changes the
// canonical file and refuses any unrecognised recovery-directory contents.
func (a *API) settlePublishedRestoreUnderLease(dir, name, expectedHash string) error {
	recoveryDir := filepath.Join(dir, name, ".manifest-recovery")
	dirInfo, err := os.Lstat(recoveryDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	entries, err := os.ReadDir(recoveryDir)
	if err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	final := filepath.Join(dir, name, "manifest.yaml")
	if len(entries) == 0 {
		if err := verifyExactSingleLinkRestoreFinal(final, expectedHash); err != nil {
			return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
		}
		if err := settleEmptyRestoreRecoveryDir(recoveryDir); err != nil {
			return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
		}
		return nil
	}
	if len(entries) != 1 || entries[0].Name() != "manifest-"+expectedHash+".tmp" {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", errors.New("restore recovery directory has unknown entries"))
	}
	temp := filepath.Join(recoveryDir, entries[0].Name())
	if err := settlePublishedRestorePair(temp, final, recoveryDir, expectedHash, -1); err != nil {
		return restoreFailure("ATTACH_MANIFEST_RESTORE_UNVERIFIED", err)
	}
	return nil
}

func verifyExactSingleLinkRestoreFinal(final, expectedHash string) error {
	info, err := os.Lstat(final)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > attachManifestSourceMaxBytes {
		return errors.Join(errors.New("restore canonical file is not an exact regular source"), err)
	}
	f, err := os.Open(final)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.Join(errors.New("restore canonical opened identity is unverified"), err)
	}
	actual, err := io.ReadAll(io.LimitReader(f, info.Size()+1))
	if err != nil || int64(len(actual)) != info.Size() || ManifestHashContent(actual) != expectedHash {
		return errors.Join(errors.New("restore canonical hash is unverified"), err)
	}
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &identity); err != nil || identity.NumberOfLinks != 1 {
		return errors.Join(errors.New("restore canonical file has an unknown link"), err)
	}
	return nil
}
