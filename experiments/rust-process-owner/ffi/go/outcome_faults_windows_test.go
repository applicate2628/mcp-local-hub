//go:build windows && amd64 && cgo && ffi_outcome_faults

package caller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An owned test DLL changes ONLY serialized finish output after the real native
// owner has settled. Native creation/admission/cleanup and Release remain real.
// These tests do not claim that the normal DLL emits impossible states or that
// fabricated cleanup codes are real CloseHandle failures.
func outcomeFixture(t *testing.T) (*Library, *Owner, string) {
	t.Helper()
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "observed-effect")
	o, _, err := lib.New(child, dir, []string{"exit", marker, "259", "real-child-completed"})
	if o != nil {
		t.Cleanup(func() {
			o.mu.Lock()
			live := o.raw != nil
			o.mu.Unlock()
			if live {
				_, _ = o.Stop(98) // explicit safety for this test's finite child only
				_, _ = o.Finish(3000)
				if e := o.Release(); e != nil {
					t.Errorf("test owner release: %v", e)
				}
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, o.Release(), notSettled)
	if _, err = o.AdmitChecked(func(ProcessObservation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return lib, o, marker
}
func checkOutcomeCustody(t *testing.T, lib *Library, o *Owner, marker string) {
	t.Helper()
	bytes, err := os.ReadFile(marker)
	if err != nil || string(bytes) != "real-child-completed" {
		t.Fatalf("real child effect lost: %q %v", bytes, err)
	}
	if e := lib.Close(); e == nil {
		t.Fatal("response failure discarded the live allocation owner")
	}
	if e := o.Release(); e != nil {
		t.Fatalf("same owner could not release cached native terminal state: %v", e)
	}
	if e := o.Release(); e != nil {
		t.Fatal("cleared pointer reused")
	}
	if e := lib.Close(); e != nil {
		t.Fatal(e)
	}
}
func faultFinish(t *testing.T, o *Owner, form string) (Outcome, error) {
	t.Helper()
	if form == "finite" {
		return o.Finish(3000)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return o.FinishContext(ctx)
}

func TestOutcomeRejectsContradictoryMetadata(t *testing.T) {
	mode := os.Getenv("MCPHUB_FFI_OUTCOME_CASE")
	// The OS error code is independent diagnostic evidence only when layout is
	// recognized. Unknown layouts preserve the separately returned status, not
	// a field interpreted at a potentially incompatible offset.
	cases := map[string]struct {
		status uint32
		native int32
	}{
		"unknown-state": {}, "unknown-flags": {}, "missing-root": {},
		"unflagged-root": {}, "unflagged-active": {}, "live-terminal": {},
		"cleanup-on-complete": {}, "cleanup-empty": {}, "cleanup-no-code": {},
		"cleanup-stray-code": {}, "cleanup-unknown-bit": {}, "native-on-success": {},
		"finish-unknown": {}, "unknown-status": {99, 0},
		"native-no-code": {nativeFailure, 0}, "native-invalid-flags": {nativeFailure, 87},
		"bad-layout-native": {nativeFailure, 0},
	}
	expected, ok := cases[mode]
	if !ok {
		t.Fatal("explicit matching outcome-fault DLL required")
	}
	for _, form := range []string{"finite", "context"} {
		t.Run(form, func(t *testing.T) {
			lib, o, marker := outcomeFixture(t)
			out, err := faultFinish(t, o, form)
			if !errors.Is(err, ErrInvalidOutcome) {
				t.Errorf("contradictory outcome accepted without protocol rejection: %+v %v", out, err)
			}
			if out != (Outcome{}) {
				t.Errorf("invalid outcome exposed as trusted observation: %+v", out)
			}
			var call *CallError
			if expected.status != 0 {
				if !errors.As(err, &call) || call.Status != expected.status || call.Native != expected.native {
					t.Errorf("independent call cause lost/misread: %v", err)
				}
			}
			checkOutcomeCustody(t, lib, o, marker)
		})
	}
}

func TestOutcomePreservesValidCleanupReport(t *testing.T) {
	mode := os.Getenv("MCPHUB_FFI_OUTCOME_CASE")
	n, err := strconv.Atoi(strings.TrimPrefix(mode, "valid-cleanup-"))
	if err != nil || n < 1 || n > 7 {
		t.Fatal("explicit valid-cleanup test DLL required")
	}
	for _, form := range []string{"finite", "context"} {
		t.Run(form, func(t *testing.T) {
			lib, o, marker := outcomeFixture(t)
			out, err := faultFinish(t, o, form)
			if err != nil || out.State != cleanupFailed || out.Flags&1 == 0 || out.RootExit != 259 || out.CleanupMask != uint32(n) {
				t.Errorf("valid cleanup report was lost or collapsed: %+v %v", out, err)
			}
			for i, code := range out.CleanupCodes {
				want := int32(0)
				if n&(1<<i) != 0 {
					want = int32(5 + i)
				}
				if code != want {
					t.Errorf("close slot %d: got%d want%d", i, code, want)
				}
			}
			// Only the serialized observation is changed. Normal Release deliberately
			// reports the real kernel-success state; this is not a kernel fault test.
			checkOutcomeCustody(t, lib, o, marker)
		})
	}
}
