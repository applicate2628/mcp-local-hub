//go:build windows

package reversedepgraph

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileCaseSensitiveInfo struct {
	Flags uint32
}

const caseSensitiveRootEnv = "MCPHUB_TEST_CASE_SENSITIVE_ROOT"

func TestBindTrustedRootRefusesDistinctCaseSensitiveWindowsRoots(t *testing.T) {
	base := t.TempDir()
	if err := enableCaseSensitiveDirectory(base); err == nil {
		assertDistinctCaseSensitiveRootsAreRejected(t, base, "local TempDir")
		return
	} else if !caseSensitiveEnableUnavailable(err) {
		t.Fatalf("enable per-directory case sensitivity on local TempDir: %v", err)
	} else {
		t.Logf("local TempDir cannot enable per-directory case sensitivity: %v", err)
	}

	configuredRoot, supplied := os.LookupEnv(caseSensitiveRootEnv)
	if !supplied || configuredRoot == "" {
		t.Skipf("per-directory case sensitivity unavailable locally; set %s to an existing absolute Windows-visible case-sensitive directory", caseSensitiveRootEnv)
	}
	if !filepath.IsAbs(configuredRoot) {
		t.Fatalf("%s must be an absolute Windows-visible directory, got %q", caseSensitiveRootEnv, configuredRoot)
	}
	info, err := os.Stat(configuredRoot)
	if err != nil || !info.IsDir() {
		t.Fatalf("%s=%q is unreadable or not a directory: %v", caseSensitiveRootEnv, configuredRoot, err)
	}
	base, err = os.MkdirTemp(configuredRoot, "mcphub-case-sensitive-")
	if err != nil {
		t.Fatalf("create disposable case-sensitive child under %s=%q: %v", caseSensitiveRootEnv, configuredRoot, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("remove disposable case-sensitive child %q: %v", base, err)
		}
	})
	t.Logf("using disposable case-sensitive child under %s", caseSensitiveRootEnv)
	assertDistinctCaseSensitiveRootsAreRejected(t, base, caseSensitiveRootEnv)
}

func assertDistinctCaseSensitiveRootsAreRejected(t *testing.T, base, source string) {
	t.Helper()
	trusted := filepath.Join(base, "Trusted")
	requested := filepath.Join(base, "trusted")
	if err := os.Mkdir(trusted, 0o755); err != nil {
		t.Fatalf("%s: create Trusted directory: %v", source, err)
	}
	if err := os.Mkdir(requested, 0o755); err != nil {
		t.Fatalf("%s: create distinct trusted directory: %v", source, err)
	}
	trustedInfo, err := os.Stat(trusted)
	if err != nil {
		t.Fatalf("%s: stat Trusted directory: %v", source, err)
	}
	requestedInfo, err := os.Stat(requested)
	if err != nil {
		t.Fatalf("%s: stat trusted directory: %v", source, err)
	}
	if os.SameFile(trustedInfo, requestedInfo) {
		t.Fatalf("%s: case-sensitive fixture did not create distinct Trusted and trusted roots", source)
	}
	if _, err := BindTrustedRoot(requested, trusted); err == nil {
		t.Fatalf("%s: distinct case-sensitive root was accepted", source)
	}
}

func caseSensitiveEnableUnavailable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}

func enableCaseSensitiveDirectory(path string) error {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(pathPointer, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	info := fileCaseSensitiveInfo{Flags: windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR}
	return windows.SetFileInformationByHandle(handle, windows.FileCaseSensitiveInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}
