//go:build windows && amd64 && cgo && ffi_faults

package caller

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// These opt-in checks require a deliberately mutated OWNED test DLL, not the
// normal artifact. See README for the exact single-field negative controls.
// No mutation hook exists in the shipped bridge or native owner.
func TestCorruptCreateRetainsTransferredOwner(t *testing.T) {
	if os.Getenv("MCPHUB_FFI_FAULT") != "create-metadata" {
		t.Fatal("explicit create-metadata fault artifact required")
	}
	l, child, dir := setup(t)
	o, _, err := l.New(child, dir, []string{"exit", filepath.Join(dir, "never-admitted"), "0", "x"})
	if o == nil {
		t.Fatal("transferred native owner discarded after metadata validation failure")
	}
	own(t, o)
	if err == nil {
		t.Fatal("negative DLL did not corrupt metadata")
	}
	if e := l.Close(); e == nil {
		t.Fatal("unload allowed while transferred owner remains live")
	}
}
func TestCorruptReleasePreservesDeallocationFact(t *testing.T) {
	if os.Getenv("MCPHUB_FFI_FAULT") != "release-metadata" {
		t.Fatal("explicit release-metadata fault artifact required")
	}
	l, child, dir := setup(t)
	o, _, err := l.New(child, dir, []string{"exit", filepath.Join(dir, "released"), "0", "x"})
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	out, err := o.Finish(3000)
	if err != nil || out.State != complete {
		t.Fatalf("finish: %+v %v", out, err)
	}
	if err = o.Release(); err == nil {
		t.Fatal("negative DLL did not corrupt release metadata")
	}
	if err = o.Release(); err != nil {
		t.Fatal("cleared native pointer was reused")
	}
	if err = l.Close(); err != nil {
		t.Fatalf("completed deallocation lost when response failed: %v", err)
	}
}

// Run against a DLL with exactly one callback observation/decision mutation;
// no invalid non-null native memory is supplied by these controlled variants.
func TestCheckedAdmissionRejectsMalformedObservation(t *testing.T) {
	mode := os.Getenv("MCPHUB_FFI_FAULT")
	if mode != "checked-pid" && mode != "checked-null-image" && mode != "checked-length" && mode != "checked-duplicate" {
		t.Fatal("selected checked-observation fault DLL required")
	}
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "malformed-check")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "no"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	reason := errors.New("first policy rejection must survive later bad callback")
	_, err = owner.AdmitChecked(func(ProcessObservation) error {
		calls++
		if mode == "checked-duplicate" {
			return reason
		}
		return nil
	})
	if !errors.Is(err, ErrAdmissionObservation) {
		t.Fatalf("malformed observation not reported: %v", err)
	}
	requireStatus(t, err, denied)
	if mode == "checked-duplicate" && !errors.Is(err, reason) {
		t.Fatalf("first policy cause lost: %v", err)
	}
	want := 0
	if mode == "checked-duplicate" {
		want = 1
	}
	if calls != want {
		t.Fatalf("policy called with invalid/repeated evidence: got %d want %d", calls, want)
	}
	absent(t, marker)
	_, err = owner.Admit(true)
	requireStatus(t, err, invalidState)
}

func TestCheckedAdmissionRejectsUnknownDecision(t *testing.T) {
	if os.Getenv("MCPHUB_FFI_FAULT") != "checked-decision" {
		t.Fatal("unknown-decision DLL required")
	}
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "bad-decision")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "no"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = owner.AdmitChecked(func(ProcessObservation) error { calls++; return nil })
	requireStatus(t, err, argument)
	if calls != 1 {
		t.Fatalf("decision test never consulted policy: %d", calls)
	}
	absent(t, marker)
	_, err = owner.Admit(true)
	requireStatus(t, err, invalidState)
}

func TestCheckedAdmissionMissingExportFailsClosed(t *testing.T) {
	if os.Getenv("MCPHUB_FFI_FAULT") != "checked-no-symbol" {
		t.Fatal("older owned DLL without checked export required")
	}
	dll := os.Getenv("MCPHUB_RX02_FFI_DLL")
	if !filepath.IsAbs(dll) {
		t.Fatal("explicit old artifact path required")
	}
	lib, err := Open(dll)
	if lib != nil {
		_ = lib.Close()
		t.Fatal("caller accepted a DLL without checked admission")
	}
	if !errors.Is(err, syscall.Errno(127)) {
		t.Fatalf("expected missing export, not unrelated load failure: %v", err)
	}
}
