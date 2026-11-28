//go:build windows && amd64 && cgo && identity_query_faults

package caller

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// The runner changes only the query producer in a separate source tree.
// These synthetic responses test transport validation, not real Windows faults.
func TestIdentityQueryForeignFaultPreservesAdmission(t *testing.T) {
	mode := os.Getenv("MCPHUB_IDENTITY_QUERY_FAULT")
	if mode == "" {
		t.Fatal("explicit query fault scenario required")
	}
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "query-fault-effect")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "explicit-after-query"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := owner.QueryIdentity()
	if !reflect.DeepEqual(got, IdentityObservation{}) {
		t.Fatal("failed query exposed an observation")
	}
	switch mode {
	case "query-close", "close-only":
		var receipt *IdentityError
		if !errors.As(err, &receipt) || receipt.Stage != IdentityStageToken ||
			receipt.TokenCloseError != 6 || !errors.Is(err, syscall.Errno(6)) {
			t.Fatal("query token-close receipt was lost or changed")
		}
		if mode == "query-close" {
			if receipt.Kind != IdentityNativeError || receipt.NativeError != 5 || !errors.Is(err, syscall.Errno(5)) {
				t.Fatal("query failure was lost beside token-close failure")
			}
		} else if receipt.Kind != IdentityNoError || receipt.NativeError != 0 {
			t.Fatal("query close-only failure invented a query error")
		}
		requireStatus(t, err, invalidState)
	case "late-report", "missing-receiver", "repeated-receiver", "invalid-observation",
		"known-root-not-signaled", "pid-mismatch", "settlement", "active-observation":
		if !errors.Is(err, ErrAdmissionObservation) {
			t.Fatalf("malformed query response was accepted: %v", err)
		}
	default:
		t.Fatal("unknown query fault scenario")
	}
	absent(t, marker)
	// Even late validation failure must leave the same original admission intact.
	if _, err := owner.AdmitIdentity(func(IdentityObservation) error { return nil }); err != nil {
		t.Fatalf("failed query consumed admission: %v", err)
	}
	out, err := owner.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 0 {
		t.Fatalf("same owner did not settle after explicit admission: %+v %v", out, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "explicit-after-query" {
		t.Fatal("explicit admission did not run the original query child")
	}
}
