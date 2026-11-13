//go:build windows && amd64 && cgo && ffi_faults

package caller

import (
	"os"
	"path/filepath"
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
