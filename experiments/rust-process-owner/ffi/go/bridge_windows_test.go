//go:build windows && amd64 && cgo

package caller

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

func setup(t *testing.T) (*Library, string, string) {
	t.Helper()
	dll := os.Getenv("MCPHUB_RX02_FFI_DLL")
	child := os.Getenv("MCPHUB_RX02_FFI_CHILD")
	if dll == "" || child == "" {
		t.Fatal("explicit owned DLL and fixture paths are required; this test never downloads/builds a dependency")
	}
	lib, err := Open(dll)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := lib.Close(); e != nil {
			t.Error(e)
		}
	})
	return lib, child, t.TempDir()
}
func own(t *testing.T, o *Owner) {
	t.Helper()
	t.Cleanup(func() {
		o.mu.Lock()
		released := o.raw == nil
		o.mu.Unlock()
		if released {
			return
		}
		out, err := o.Finish(0)
		if err != nil || out.State == pending {
			_, _ = o.Stop(123)
			out, err = o.Finish(3000)
		}
		if err != nil || (out.State != complete && out.State != cleanupFailed) {
			t.Errorf("own fixture failed to settle: %+v %v", out, err)
			return
		}
		if e := o.Release(); e != nil {
			t.Error(e)
		}
	})
}
func requireStatus(t *testing.T, err error, want uint32) {
	t.Helper()
	var e *CallError
	if !errors.As(err, &e) || e.Status != want {
		t.Fatalf("got %v; want status %d", err, want)
	}
}
func absent(t *testing.T, path string) {
	t.Helper()
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatalf("child effect before admission: %v", e)
	}
}

func TestForeignLifecycleRetainsOwnerAndCopiesInput(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "proof-λ.txt")
	payload := "space quote\" slash\\ unicode-🦀"
	args := []string{"exit", marker, "7", payload}
	o, initial, err := lib.New(child, dir, args)
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	if initial.PID == 0 || initial.Flags != 0 {
		t.Fatalf("bad original observation: %+v", initial)
	}
	absent(t, marker)
	args[3] = "replaced after Create"
	args = nil
	runtime.GC() // C input has already been freed
	for i := 0; i < 4; i++ {
		b := make([]byte, 1<<20)
		b[0] = 1
		runtime.KeepAlive(b)
	}
	out, err := o.Finish(0)
	if err != nil || out.State != pending || out.Flags&1 != 0 {
		t.Fatalf("suspended owner not pending: %+v %v", out, err)
	}
	requireStatus(t, o.Release(), notSettled)
	if err := lib.Close(); err == nil {
		t.Fatal("live Rust owner allowed DLL unload")
	}
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Admit(true); err == nil {
		t.Fatal("admission replay accepted")
	}
	out, err = o.Finish(3000)
	if err != nil || out.State != complete || out.Flags&1 == 0 || out.RootExit != 7 {
		t.Fatalf("terminal result lost: %+v %v", out, err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != payload {
		t.Fatalf("foreign input lifetime/encoding: %q %v", content, err)
	}
	again, err := o.Finish(0)
	if err != nil || again != out {
		t.Fatalf("terminal outcome changed: %+v %v", again, err)
	}
}
func TestForeignDeniedAdmissionHasNoEffect(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "denied")
	o, _, err := lib.New(child, dir, []string{"exit", marker, "0", "unexpected"})
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	_, err = o.Admit(false)
	requireStatus(t, err, denied)
	absent(t, marker)
	_, err = o.Admit(true)
	requireStatus(t, err, invalidState)
	if _, err = o.Stop(73); err != nil {
		t.Fatal(err)
	}
	out, err := o.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 73 {
		t.Fatalf("denied cleanup: %+v %v", out, err)
	}
	absent(t, marker)
}
func TestForeignErrorsAreTypedAndNoOwnerEscapes(t *testing.T) {
	lib, child, dir := setup(t)
	for _, v := range []uint32{0, 2, ^uint32(0)} {
		o, _, err := lib.newVersion(child, dir, nil, v)
		if o != nil {
			t.Fatal("bad ABI produced owner")
		}
		requireStatus(t, err, abiMismatch)
	}
	o, out, err := lib.New(filepath.Join(dir, "does-not-exist.exe"), dir, nil)
	if o != nil {
		t.Fatal("missing image produced owner")
	}
	requireStatus(t, err, nativeFailure)
	if out.NativeError == 0 {
		t.Fatal("OS error code discarded")
	}
	o, _, err = lib.New("relative.exe", dir, nil)
	if o != nil {
		t.Fatal("relative image accepted")
	}
	requireStatus(t, err, argument)
	o, _, err = lib.New(child, "relative", nil)
	if o != nil {
		t.Fatal("relative cwd accepted")
	}
	requireStatus(t, err, argument)
}
func TestForeignReleaseClearsHandleAndDisallowsUse(t *testing.T) {
	lib, child, dir := setup(t)
	o, _, err := lib.New(child, dir, []string{"exit", filepath.Join(dir, "release"), "0", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Finish(3000); err != nil {
		t.Fatal(err)
	}
	if e := lib.Close(); e == nil {
		t.Fatal("terminal but unreleased owner allowed DLL unload")
	}
	if err = o.Release(); err != nil {
		t.Fatal(err)
	}

	if err = o.Release(); err != nil {
		t.Fatal("nil release not idempotent")
	}
	if _, err = o.Finish(0); err == nil {
		t.Fatal("released pointer used")
	}
	if err = lib.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = lib.New(child, dir, nil); err == nil {
		t.Fatal("unloaded library used")
	}
}
func TestForeignCallerSerializesConcurrentUse(t *testing.T) {
	lib, child, dir := setup(t)
	o, _, err := lib.New(child, dir, []string{"block", filepath.Join(dir, "block")})
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := o.Finish(0); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Error(e)
		}
	}
	if _, err = o.Stop(74); err != nil {
		t.Fatal(err)
	}
	out, err := o.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 74 {
		t.Fatalf("stop: %+v %v", out, err)
	}
}

func TestForeignRootExitDoesNotReleaseLiveDescendant(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "root")
	o, _, err := lib.New(child, dir, []string{"detach", marker})
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	var out Outcome
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, err = o.Finish(25)
		if err != nil || out.Flags&1 != 0 || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || out.State != pending || out.Flags&1 == 0 || out.RootExit != 7 || out.Flags&2 == 0 || out.Active == 0 {
		t.Fatalf("root exit collapsed into terminal result: %+v %v", out, err)
	}
	requireStatus(t, o.Release(), notSettled)
	if _, err = o.Stop(81); err != nil {
		t.Fatal(err)
	}
	out, err = o.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 7 {
		t.Fatalf("descendant stop erased root result: %+v %v", out, err)
	}
}
func TestForeignExplicitContextDoesNotInheritOrMutateParent(t *testing.T) {
	const key = "MCPHUB_FFI_FAKE_SENTINEL"
	t.Setenv(key, "test-only-parent-value")
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "context.bin")
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := lib.New(child, dir, []string{"context-relative", marker, key})
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	runtime.GC()
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	out, err := o.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 0 {
		t.Fatalf("context fixture: %+v %v", out, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 8 {
		t.Fatal("missing context result")
	}
	n := int(binary.LittleEndian.Uint32(data[:4]))
	if n > 32768 || len(data) != 8+2*n {
		t.Fatal("bad bounded context result")
	}
	units := make([]uint16, n)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[4+2*i:])
	}
	got := string(utf16.Decode(units))
	if !strings.EqualFold(filepath.Clean(got), filepath.Clean(dir)) {
		t.Fatalf("child cwd %q expected %q", got, dir)
	}
	if binary.LittleEndian.Uint32(data[4+2*n:]) != ^uint32(0) {
		t.Fatal("explicit empty environment inherited parent sentinel")
	}
	proof, err := os.ReadFile(filepath.Join(dir, "relative-proof.bin"))
	if err != nil || string(proof) != "owned-context" {
		t.Fatalf("child relative effect: %q %v", proof, err)
	}
	after, _ := os.Getwd()
	if before != after || os.Getenv(key) != "test-only-parent-value" {
		t.Fatal("parent global state mutated")
	}
}
func TestForeignBudgetsDoNotConsumeAdmission(t *testing.T) {
	lib, child, dir := setup(t)
	o, _, err := lib.New(child, dir, []string{"exit", filepath.Join(dir, "bounded"), "0", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	own(t, o)
	_, err = o.Finish(60001)
	requireStatus(t, err, argument)
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	out, err := o.Finish(3000)
	if err != nil || out.State != complete {
		t.Fatalf("bad budget damaged owner: %+v %v", out, err)
	}
	tooMany := make([]string, 65)
	bad, _, err := lib.New(child, dir, tooMany)
	if bad != nil {
		t.Fatal("oversized argv produced owner")
	}
	requireStatus(t, err, argument)
}
