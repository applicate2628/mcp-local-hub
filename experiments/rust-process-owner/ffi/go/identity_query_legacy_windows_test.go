//go:build windows && amd64 && cgo && identity_query_legacy

package caller

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Run separately against a pinned ABI1 DLL and an admission-only identity DLL.
func TestIdentityQueryUnsupportedPreservesExplicitAdmission(t *testing.T) {
	mode := os.Getenv("MCPHUB_QUERY_LEGACY_MODE")
	if mode != "abi1" && mode != "admission-identity" {
		t.Fatal("explicit supported legacy DLL mode required")
	}
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "query-unsupported")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "explicit"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := owner.QueryIdentity()
	if !errors.Is(err, ErrIdentityQueryUnsupported) || !reflect.DeepEqual(got, IdentityObservation{}) {
		t.Fatal("missing query export was not explicitly unsupported")
	}
	absent(t, marker)
	if mode == "admission-identity" {
		_, err = owner.AdmitIdentity(func(IdentityObservation) error { return nil })
	} else {
		_, err = owner.AdmitChecked(func(ProcessObservation) error { return nil })
	}
	if err != nil {
		t.Fatalf("unsupported query consumed or replaced explicit admission: %v", err)
	}
	out, err := owner.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 0 {
		t.Fatalf("explicit legacy admission did not settle: %+v %v", out, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "explicit" {
		t.Fatal("explicit legacy admission did not run the original child")
	}
}
