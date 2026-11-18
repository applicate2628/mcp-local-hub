//go:build windows && amd64 && cgo && identity_faults

package caller

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Each run uses a separately built, recorded producer mutation. These synthetic
// foreign responses test mapping/rejection, not spontaneous Windows failures.
func TestIdentityForeignFault(t *testing.T) {
	mode := os.Getenv("MCPHUB_IDENTITY_FAULT")
	if mode == "" {
		t.Fatal("explicit identity fault scenario required")
	}
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "identity-fault-effect")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "identity-effect"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = owner.AdmitIdentity(func(IdentityObservation) error { calls++; return nil })
	switch mode {
	case "query-close", "close-only":
		var receipt *IdentityError
		if !errors.As(err, &receipt) || receipt.Stage != IdentityStageToken || receipt.TokenCloseError != 6 ||
			!errors.Is(err, syscall.Errno(6)) {
			t.Fatal("token close receipt was lost or changed")
		}
		if mode == "query-close" {
			if receipt.Kind != IdentityNativeError || receipt.NativeError != 5 || !errors.Is(err, syscall.Errno(5)) {
				t.Fatal("query error was lost beside token close failure")
			}
		} else if receipt.Kind != IdentityNoError || receipt.NativeError != 0 {
			t.Fatal("close-only failure invented a query error")
		}
		requireStatus(t, err, invalidState)
		if calls != 0 {
			t.Fatal("failed token observation reached policy")
		}
		absent(t, marker)
	case "late-report":
		if !errors.Is(err, ErrAdmissionObservation) || calls != 1 {
			t.Fatal("malformed identity report was accepted or callback was lost")
		}
		// A malformed result is not evidence of no effect. This producer already
		// resumed the real child; retain the same owner and observe its outcome.
		out, finishErr := owner.Finish(3000)
		if finishErr != nil || out.State != complete || out.RootExit != 0 {
			t.Fatalf("late report lost actual outcome: %+v %v", out, finishErr)
		}
		data, readErr := os.ReadFile(marker)
		if readErr != nil || string(data) != "identity-effect" {
			t.Fatal("late report incorrectly treated actual execution as no effect")
		}
		return
	case "invalid-observation":
		if !errors.Is(err, ErrAdmissionObservation) || calls != 0 {
			t.Fatal("malformed identity observation reached policy")
		}
		requireStatus(t, err, denied)
		absent(t, marker)
	default:
		t.Fatal("unknown identity fault scenario")
	}
	_, err = owner.Admit(true)
	requireStatus(t, err, invalidState)
	if _, err := owner.Stop(86); err != nil {
		t.Fatal(err)
	}
	out, err := owner.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 86 {
		t.Fatalf("identity failure lost original owner: %+v %v", out, err)
	}
	absent(t, marker)
}
