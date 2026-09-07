//go:build !windows

package api

import "fmt"

func livenessPrincipalSID() (string, error) {
	return "", fmt.Errorf("liveness principal SID is unsupported on this platform")
}
