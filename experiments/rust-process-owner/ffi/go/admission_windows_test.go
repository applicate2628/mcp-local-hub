//go:build windows && amd64 && cgo

package caller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
)

func TestCheckedAdmissionObservesOriginalBeforeChildRuns(t *testing.T) {
	lib, child, dir := setup(t)
	bytes, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	child = filepath.Join(dir, "fixture-λ-🦀.exe")
	if err := os.WriteFile(child, bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately distinct input spelling: evidence must be the OS image name,
	// not an echo of the program string received by New.
	spelled := filepath.Dir(child) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(child)
	marker := filepath.Join(dir, "checked-λ")
	owner, initial, err := lib.New(spelled, dir, []string{"exit", marker, "7", "checked"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	var got ProcessObservation
	calls := 0
	out, err := owner.AdmitChecked(func(info ProcessObservation) error {
		calls++
		got = info
		if info.PID != initial.PID || info.PID == 0 {
			return errors.New("not the original process")
		}
		observed := string(utf16.Decode(info.ImagePathUTF16))
		if !strings.EqualFold(filepath.Clean(observed), filepath.Clean(child)) {
			return errors.New("wrong original image")
		}
		if strings.Contains(observed, string(os.PathSeparator)+"."+string(os.PathSeparator)) {
			return errors.New("echoed create argument instead of observing process")
		}
		if _, e := os.Stat(marker); !os.IsNotExist(e) {
			return errors.New("child executed before policy completed")
		}
		runtime.GC() // callback may safely run Go allocation/GC while Rust waits
		return nil
	})
	if err != nil || calls != 1 || out.PID != initial.PID {
		t.Fatalf("checked admission: calls=%d out=%+v err=%v", calls, out, err)
	}
	out, err = owner.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 7 {
		t.Fatalf("finish: %+v %v", out, err)
	}
	runtime.GC()
	if !strings.EqualFold(filepath.Clean(string(utf16.Decode(got.ImagePathUTF16))), filepath.Clean(child)) {
		t.Fatal("observation did not outlive the native callback")
	}
	// The retained slice is Go-owned, not a still-borrowed native buffer.
	got.ImagePathUTF16[0] = 'X'
}

func TestCheckedAdmissionDenialPreservesPolicyErrorAndOwner(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "denied-checked")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "must-not-run"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	reason := errors.New("test policy rejected original image")
	calls := 0
	_, err = owner.AdmitChecked(func(ProcessObservation) error { calls++; return reason })
	if !errors.Is(err, reason) {
		t.Fatalf("policy cause lost: %v", err)
	}
	requireStatus(t, err, denied)
	absent(t, marker)
	_, err = owner.AdmitChecked(func(ProcessObservation) error { calls++; return nil })
	requireStatus(t, err, invalidState)
	if calls != 1 {
		t.Fatalf("one-shot policy replayed %d times", calls)
	}
	_, err = owner.Admit(true)
	requireStatus(t, err, invalidState)
	if _, err = owner.Stop(82); err != nil {
		t.Fatal(err)
	}
	out, err := owner.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 82 {
		t.Fatalf("denied cleanup: %+v %v", out, err)
	}
	absent(t, marker)
}

func TestCheckedAdmissionPolicyPanicDeniesWithoutCrossingFFI(t *testing.T) {
	for _, value := range []any{"fake-sensitive-panic", nil} {
		t.Run(map[bool]string{true: "nil", false: "value"}[value == nil], func(t *testing.T) {
			lib, child, dir := setup(t)
			marker := filepath.Join(dir, "panic")
			owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "no"})
			if owner != nil {
				own(t, owner)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = owner.AdmitChecked(func(ProcessObservation) error { panic(value) })
			if !errors.Is(err, ErrAdmissionPanic) {
				t.Fatalf("panic was not denied: %v", err)
			}
			requireStatus(t, err, denied)
			if strings.Contains(err.Error(), "fake-sensitive-panic") {
				t.Fatal("panic payload disclosed")
			}
			absent(t, marker)
			_, err = owner.Admit(true)
			requireStatus(t, err, invalidState)
		})
	}
}

func TestCheckedAdmissionNilPolicyDoesNotConsumeAdmission(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "nil-policy")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "ok"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.AdmitChecked(nil)
	requireStatus(t, err, argument)
	absent(t, marker)
	calls := 0
	if _, err = owner.AdmitChecked(func(ProcessObservation) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("missing actual policy invocation")
	}
}

func TestCheckedAdmissionCannotObserveStoppedOrReleasedOwner(t *testing.T) {
	lib, child, dir := setup(t)
	owner, _, err := lib.New(child, dir, []string{"exit", filepath.Join(dir, "stop-before-check"), "0", "no"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Stop(83); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = owner.AdmitChecked(func(ProcessObservation) error { called = true; return nil })
	requireStatus(t, err, invalidState)
	if called {
		t.Fatal("policy invoked after stop")
	}
	out, err := owner.Finish(3000)
	if err != nil || out.State != complete {
		t.Fatalf("stop: %+v %v", out, err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	_, err = owner.AdmitChecked(func(ProcessObservation) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("released pointer used for policy")
	}
}

func TestCheckedAdmissionSerializesPolicyWithOtherOwnerCalls(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "policy-barrier")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "ok"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var donePolicy atomic.Bool
	result := make(chan error, 1)
	go func() {
		_, e := owner.AdmitChecked(func(ProcessObservation) error {
			close(entered)
			<-release
			donePolicy.Store(true)
			return nil
		})
		result <- e
	}()
	select {
	case <-entered:
	case e := <-result:
		t.Fatalf("policy never started: %v", e)
	case <-time.After(3 * time.Second):
		t.Fatal("policy entry timeout")
	}
	absent(t, marker)
	if e := lib.Close(); e == nil {
		t.Fatal("library unloaded during checked callback")
	}
	finished := make(chan error, 1)
	go func() {
		_, e := owner.Finish(0)
		if e == nil && !donePolicy.Load() {
			e = errors.New("owner call overlapped active policy")
		}
		finished <- e
	}()
	once.Do(func() { close(release) })
	select {
	case e := <-result:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not return")
	}
	select {
	case e := <-finished:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("joined caller did not return")
	}
}

func TestCheckedAdmissionCancellationRejectsBeforeResume(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "cancel-policy")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "no"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = owner.AdmitChecked(func(ProcessObservation) error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	requireStatus(t, err, denied)
	absent(t, marker)
	_, err = owner.Admit(true)
	requireStatus(t, err, invalidState)
}
