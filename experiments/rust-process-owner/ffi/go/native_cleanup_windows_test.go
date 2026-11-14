//go:build windows && amd64 && cgo && ffi_native_faults

package caller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Qualification only. The normal DLL has no export revealing owner slots.
// The supplied runner appends a read-only accessor in a disposable source copy;
// the actual close/finish algorithm and normal DLL are not replaced or mocked.
func loadSlotProbe(t *testing.T) *syscall.Proc {
	t.Helper()
	if os.Getenv("MCPHUB_FFI_NATIVE_FAULT") != "protected-close" {
		t.Fatal("explicit owned native-close qualification DLL required")
	}
	dll, err := syscall.LoadDLL(os.Getenv("MCPHUB_RX02_FFI_DLL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := dll.Release(); e != nil {
			t.Error(e)
		}
	})
	proc, err := dll.FindProc("mh_qualification_slots1")
	if err != nil {
		t.Fatal(err)
	}
	return proc
}
func originalSlots(t *testing.T, o *Owner, proc *syscall.Proc) [3]syscall.Handle {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.raw == nil {
		t.Fatal("fixture requested slots from a released owner")
	}
	var slots [3]syscall.Handle
	var pin runtime.Pinner
	pin.Pin(&slots)
	defer pin.Unpin()
	// Exclusive retained Rust allocation; initialized pinned Go output contains
	// no Go pointers. Native accessor copies three integers synchronously only.
	code, _, _ := proc.Call(uintptr(unsafe.Pointer(o.raw)), uintptr(unsafe.Pointer(&slots[0])), 3)
	runtime.KeepAlive(o)
	if code != 0 {
		t.Fatalf("test slot accessor failed: %d", code)
	}
	return slots
}
func nativeFlags(h syscall.Handle) (uint32, error) {
	var flags uint32
	ok, _, e := testKernel.NewProc("GetHandleInformation").Call(uintptr(h), uintptr(unsafe.Pointer(&flags)))
	if ok == 0 {
		return 0, e
	}
	return flags, nil
}
func setCloseProtection(h syscall.Handle, enabled bool) error {
	value := uintptr(0)
	if enabled {
		value = 2
	}
	ok, _, err := testKernel.NewProc("SetHandleInformation").Call(uintptr(h), 2, value)
	if ok == 0 {
		return err
	}
	return nil
}

type protectedSlots struct {
	owner     *Owner
	probe     *syscall.Proc
	handles   [3]syscall.Handle
	protected [3]bool
	expected  [3]int32
	detached  bool // confirmed by direct native slots, never by decoded outcome alone
}

func protectSlots(t *testing.T, o *Owner, p *syscall.Proc, mask uint32) *protectedSlots {
	t.Helper()
	g := &protectedSlots{owner: o, probe: p, handles: originalSlots(t, o, p)}
	// Registered after own(): restore original ownership or reclaim only known
	// detached handles BEFORE ordinary owner cleanup. No PID or handle census.
	t.Cleanup(func() { g.recover(t) })
	for i, h := range g.handles {
		if h == 0 {
			t.Fatal("expected all three original live owner slots")
		}
		if mask&(1<<i) == 0 {
			continue
		}
		flags, e := nativeFlags(h)
		if e != nil || flags&2 != 0 {
			t.Fatalf("unexpected original flags %d %v", flags, e)
		}
		if e = setCloseProtection(h, true); e != nil {
			t.Fatal(e)
		}
		g.protected[i] = true
		// Real Windows failure on the protected original, not a guessed error
		// number. Protection means this attempt cannot close/recycle the handle.
		e = syscall.CloseHandle(h)
		var native syscall.Errno
		if e == nil || !errors.As(e, &native) || native == 0 {
			t.Fatalf("close protection did not induce a native failure: %v", e)
		}
		g.expected[i] = int32(native)
		flags, e = nativeFlags(h)
		if e != nil || flags&2 == 0 {
			t.Fatalf("protection did not retain original handle: %d %v", flags, e)
		}
	}
	return g
}
func (g *protectedSlots) assertDetached(t *testing.T) {
	t.Helper()
	if got := originalSlots(t, g.owner, g.probe); got != ([3]syscall.Handle{}) {
		t.Fatalf("native finish left retained slots: %v", got)
	}
	g.detached = true
}
func (g *protectedSlots) assertProtected(t *testing.T) {
	t.Helper()
	for i, h := range g.handles {
		if !g.protected[i] {
			continue
		}
		f, e := nativeFlags(h)
		if e != nil || f&2 == 0 {
			t.Fatalf("original protected slot %d was lost/retried: %d %v", i, f, e)
		}
	}
}
func (g *protectedSlots) recover(t *testing.T) {
	t.Helper()
	var retained [3]syscall.Handle
	if !g.detached {
		retained = originalSlots(t, g.owner, g.probe)
	}
	for i, h := range g.handles {
		if !g.protected[i] {
			continue
		}
		flags, e := nativeFlags(h)
		if e != nil || flags&2 == 0 {
			t.Errorf("refusing ambiguous numeric-handle cleanup for slot%d: %d %v", i, flags, e)
			continue
		}
		if !g.detached && retained[i] != 0 && retained[i] != h {
			t.Errorf("native owner changed slot%d unexpectedly; no stale close", i)
			continue
		}
		if e = setCloseProtection(h, false); e != nil {
			t.Errorf("restore owned protection%d: %v", i, e)
			continue
		}
		g.protected[i] = false
		if g.detached || retained[i] == 0 {
			// Finish transferred this exact protected live resource out of its
			// slot. Test recovery, not runtime retry, now performs its one close.
			if e = syscall.CloseHandle(h); e != nil {
				t.Errorf("fixture recovery close%d: %v", i, e)
			}
		}
	}
}
func assertCleanupOutcome(t *testing.T, got Outcome, mask uint32, codes [3]int32) {
	t.Helper()
	if got.State != cleanupFailed || got.Flags&1 == 0 || got.RootExit != 259 || got.CleanupMask != mask || got.CleanupCodes != codes || got.NativeError != 0 {
		t.Fatalf("real native cleanup outcome altered across FFI: %+v; mask=%d codes=%v", got, mask, codes)
	}
}

func TestNativeCleanupRealFailuresCrossBothFinishPaths(t *testing.T) {
	for _, contextWait := range []bool{false, true} {
		for mask := uint32(1); mask < 8; mask++ {
			t.Run(fmt.Sprintf("context-%t/mask-%d", contextWait, mask), func(t *testing.T) {
				lib, child, dir := setup(t)
				probe := loadSlotProbe(t)
				o, _, err := lib.New(child, dir, []string{"exit", filepath.Join(dir, "native-close"), "259", "done"})
				if o != nil {
					own(t, o)
				}
				if err != nil {
					t.Fatal(err)
				}
				guard := protectSlots(t, o, probe, mask)
				if _, err = o.AdmitChecked(func(ProcessObservation) error { return nil }); err != nil {
					t.Fatal(err)
				}
				var out Outcome
				if contextWait {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					out, err = o.FinishContext(ctx)
				} else {
					out, err = o.Finish(3000)
				}
				if err != nil {
					t.Fatal(err)
				}
				assertCleanupOutcome(t, out, mask, guard.expected)
				t.Logf("native result: state=%d root=%d mask=%d OS-codes=%v", out.State, out.RootExit, out.CleanupMask, out.CleanupCodes)
				guard.assertDetached(t)
				guard.assertProtected(t)
				again, err := o.Finish(0)
				if err != nil || again != out {
					t.Fatalf("cached failed cleanup was changed/retried: %+v %v", again, err)
				}
				if err = lib.Close(); err == nil {
					t.Fatal("terminal but unreleased owner allowed unload")
				}
				if err = o.Release(); err != nil {
					t.Fatal(err)
				}
				guard.assertProtected(t) // Release is allocation release, not OS retry
				if err = o.Release(); err != nil {
					t.Fatal(err)
				}
				guard.recover(t) // exact test-owned live resources reclaimed explicitly
			})
		}
	}
}

func TestNativeCleanupFailureRemainsCachedAfterAuthorizedRecovery(t *testing.T) {
	lib, child, dir := setup(t)
	probe := loadSlotProbe(t)
	o, _, err := lib.New(child, dir, []string{"exit", filepath.Join(dir, "cache"), "259", "done"})
	if o != nil {
		own(t, o)
	}
	if err != nil {
		t.Fatal(err)
	}
	guard := protectSlots(t, o, probe, 7)
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	first, err := o.Finish(3000)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanupOutcome(t, first, 7, guard.expected)
	guard.assertDetached(t)
	guard.recover(t)
	// No native pointers are dereferenced after fixture recovery. Cached failure
	// records must remain failures even though the TEST resolved known resources.
	again, err := o.FinishContext(context.Background())
	if err != nil || again != first {
		t.Fatalf("historical cleanup failure became success: %+v %v", again, err)
	}
	if err = o.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCleanupQualificationExportAbsentFromOrdinaryDLL(t *testing.T) {
	if os.Getenv("MCPHUB_FFI_NATIVE_FAULT") != "normal" {
		t.Fatal("explicit normal-artifact control required")
	}
	path := os.Getenv("MCPHUB_RX02_FFI_DLL")
	if !filepath.IsAbs(path) {
		t.Fatal("explicit normal DLL path required")
	}
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := dll.Release(); e != nil {
			t.Error(e)
		}
	})
	_, err = dll.FindProc("mh_qualification_slots1")
	if !errors.Is(err, syscall.Errno(127)) {
		t.Fatalf("normal DLL unexpectedly exposes native-owner slots: %v", err)
	}
}
