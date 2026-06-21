//go:build !windows

package api

import "errors"

// CurrentUserICACLSSidLiteral is available only on Windows, where icacls and
// token SIDs exist. Non-Windows callers use their existing fallback path.
func CurrentUserICACLSSidLiteral() (string, error) {
	return "", errors.New("current user icacls SID literal is Windows-only")
}
