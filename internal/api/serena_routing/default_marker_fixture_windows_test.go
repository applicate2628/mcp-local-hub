//go:build windows

package serena_routing

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows file modes do not define DACL read permissions. Install a protected
// test-only DACL explicitly, without relying on inherited TEMP permissions.
func writeReadRelaxedMarker(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write marker fixture: %v", err)
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("get fixture owner SID: %v", err)
	}
	// Current user, LocalSystem and Administrators retain full access;
	// Authenticated Users receive read access, never write or DACL control.
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;AU)", user.User.Sid.String()))
	if err != nil {
		t.Fatalf("build marker fixture DACL: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("get marker fixture DACL: %v", err)
	}
	if dacl == nil {
		t.Fatal("fixture descriptor returned a nil DACL")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatalf("set marker fixture DACL: %v", err)
	}
}
