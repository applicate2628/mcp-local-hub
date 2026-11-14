//go:build windows && amd64 && cgo && identity_legacy

package caller

import (
	"errors"
	"path/filepath"
	"testing"
)

// Run explicitly against the frozen old DLL. Ordinary tests require the new
// export; this compatibility case is neither silently skipped nor counted there.
func TestIdentityUnsupportedPreservesLegacyAdmission(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "legacy-after-unsupported")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "old-abi"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = owner.AdmitIdentity(func(IdentityObservation) error { called = true; return nil })
	if !errors.Is(err, ErrIdentityUnsupported) || called {
		t.Fatal("unsupported identity silently downgraded or invoked policy")
	}
	absent(t, marker)
	// This is an explicit caller choice after a proven no-effect unsupported
	// response, not an automatic fallback inside AdmitIdentity.
	_, err = owner.AdmitChecked(func(ProcessObservation) error { called = true; return nil })
	if err != nil || !called {
		t.Fatalf("unsupported export consumed old admission: %v", err)
	}
	out, err := owner.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 0 {
		t.Fatalf("legacy lifecycle failed: %+v %v", out, err)
	}
}
