//go:build windows

package api

func livenessPrincipalSID() (string, error) { return currentUserSIDString() }
