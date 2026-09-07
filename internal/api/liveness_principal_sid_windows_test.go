//go:build windows

package api

import "testing"

func TestLivenessPrincipalSIDMatchesCurrentUserSIDString(t *testing.T) {
	want, err := currentUserSIDString()
	if err != nil {
		t.Fatalf("currentUserSIDString: %v", err)
	}
	got, err := livenessPrincipalSID()
	if err != nil {
		t.Fatalf("livenessPrincipalSID: %v", err)
	}
	if got != want {
		t.Fatalf("liveness principal SID = %q, want current user SID %q", got, want)
	}
}
