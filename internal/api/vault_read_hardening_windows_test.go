//go:build windows

package api

import "testing"

func broadenVaultReadForTest(t *testing.T, path string) {
	t.Helper()
	applyFileDACLWithAuthUsersReadACE(t, path)
}

func vaultReadBroadeningErrorForTest() error {
	return ErrDaclOutsideAllowlist
}
