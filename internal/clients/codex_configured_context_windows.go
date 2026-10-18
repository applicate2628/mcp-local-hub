//go:build windows

package clients

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func codexConfiguredKnownFolders() (string, string, error) {
	user, userErr := windows.KnownFolderPath(windows.FOLDERID_Profile, 0)
	system, systemErr := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	return user, system, errors.Join(userErr, systemErr)
}

func codexConfiguredFileIdentity(file *os.File) (string, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return "", errors.New("native source is redirected or nonregular")
	}
	return codexConfiguredWindowsIdentityValue(info), nil
}

func codexConfiguredWindowsIdentityValue(info windows.ByHandleFileInformation) string {
	return fmt.Sprintf("%08x:%08x:%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
}

// This is one ORIGINAL path observation. FileInfo and serializable identity
// come from the same held no-follow object; neither is recovered by reopening a
// path after its original observation. The returned handle/pointer is evidence,
// not liveness or a capability available to exported DATA.
func observeCodexConfiguredPathObject(path string) (out codexConfiguredPathObservation, resultErr error) {
	out.path = path
	defer func() { out.ownerError = resultErr }()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return out, err
	}
	out.openAttempted = true
	// Reuse the observed-directory owner's delete-sharing exclusion for this
	// short original-object metadata/link observation. The handle closes before
	// the configured getter; no native ownership escapes with exported DATA.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	out.openReturned, out.openError, out.nativeHandle = true, err, uintptr(handle)
	if err != nil {
		return out, err
	}
	out.reader = os.NewFile(uintptr(handle), path)
	defer func() {
		out.closeAttempted = true
		if out.reader != nil {
			out.closeError = out.reader.Close()
		} else {
			out.closeError = windows.CloseHandle(handle)
		}
		out.closeReturned = true
		resultErr = errors.Join(resultErr, out.closeError)
	}()
	if out.reader == nil {
		return out, errors.New("native path observation file unavailable")
	}
	var info windows.ByHandleFileInformation
	out.identityAttempted = true
	out.identityError = windows.GetFileInformationByHandle(handle, &info)
	out.identityReturned = true
	if out.identityError != nil {
		return out, out.identityError
	}
	out.identity = &CodexConfiguredObjectIdentityV1{Scheme: codexConfiguredIdentityScheme, Value: codexConfiguredWindowsIdentityValue(info)}
	out.reparse = info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	out.statAttempted = true
	out.info, out.statError = out.reader.Stat()
	out.statReturned = true
	if out.statError != nil {
		return out, out.statError
	}
	switch {
	case out.reparse:
		out.kind = "symlink"
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		out.kind = "directory"
	case out.info.Mode().IsRegular():
		out.kind = "regular"
	default:
		return out, errors.New("native path observation kind unsupported")
	}
	if out.kind == "symlink" {
		out.linkReadAttempted = true
		out.rawLinkTarget, out.linkReadError = os.Readlink(path)
		out.linkReadReturned = true
		if out.linkReadError != nil {
			return out, out.linkReadError
		}
	}
	return out, nil
}
