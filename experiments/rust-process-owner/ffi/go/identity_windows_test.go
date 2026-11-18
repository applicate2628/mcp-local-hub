//go:build windows && amd64 && cgo

package caller

import (
	"bytes"
	"encoding/binary"
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

func identityFixtureDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("MCPHUB_RX02_IDENTITY_FIXTURES")
	if root == "" {
		t.Fatal("explicit owned identity fixture root required")
	}
	dir, err := os.MkdirTemp(root, "identity-")
	if err != nil {
		t.Fatal(err)
	}
	// The runner preserves private binary SID markers here for review and
	// trash-first disposition; do not print or upload their account bytes.
	return dir
}

func TestIdentityAdmissionObservesOriginalFacts(t *testing.T) {
	for _, mode := range []string{"identity", "token-user"} {
		t.Run(mode, func(t *testing.T) {
			lib, child, _ := setup(t)
			dir := identityFixtureDir(t)
			marker := filepath.Join(dir, "private-child-observation")
			owner, initial, err := lib.New(child, dir, []string{mode, marker})
			if owner != nil {
				own(t, owner)
			}
			if err != nil {
				t.Fatal(err)
			}
			var got IdentityObservation
			calls := 0
			out, err := owner.AdmitIdentity(func(v IdentityObservation) error {
				calls++
				got = v
				if v.PID == 0 || v.PID != initial.PID ||
					v.CreationTime100ns == 0 || v.OriginalJobMembership != JobMember ||
					v.WaitState != IdentityNotSignaled {
					return errors.New("identity did not describe original suspended process")
				}
				if !strings.EqualFold(filepath.Clean(string(utf16.Decode(v.ImagePathUTF16))), filepath.Clean(child)) {
					return errors.New("identity image did not match original child")
				}
				if _, e := os.Stat(marker); !os.IsNotExist(e) {
					return errors.New("child ran before identity policy returned")
				}
				runtime.GC()
				return nil
			})
			if err != nil || calls != 1 || out.PID != initial.PID {
				t.Fatalf("identity admission failed: calls=%d err=%v", calls, err)
			}
			out, err = owner.Finish(3000)
			if err != nil || out.State != complete || out.RootExit != 0 {
				t.Fatalf("identity child did not settle: %+v %v", out, err)
			}
			if err := owner.Release(); err != nil {
				t.Fatal(err)
			}
			if err := lib.Close(); err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "identity" {
				if len(data) != 12 || binary.LittleEndian.Uint32(data[:4]) != got.PID ||
					binary.LittleEndian.Uint64(data[4:]) != got.CreationTime100ns {
					t.Fatal("identity differs from child's independent birth report")
				}
			} else if !bytes.Equal(data, got.TokenUserSID) {
				t.Fatal("identity differs from child's independent TokenUser SID")
			}
			if len(got.ImagePathUTF16) == 0 || len(got.TokenUserSID) < 8 {
				t.Fatal("identity copies did not survive release and library unload")
			}
			// Mutations after unload are safe only for independent Go-owned copies.
			got.ImagePathUTF16[0] = 'X'
			got.TokenUserSID[0] = 0
		})
	}
}

func TestIdentityAdmissionDenialAndPanicRetainOwner(t *testing.T) {
	for _, mode := range []string{"error", "panic", "panic-nil"} {
		t.Run(mode, func(t *testing.T) {
			lib, child, dir := setup(t)
			marker := filepath.Join(dir, "denied-identity")
			owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "must-not-run"})
			if owner != nil {
				own(t, owner)
			}
			if err != nil {
				t.Fatal(err)
			}
			reason := errors.New("identity policy refused")
			calls := 0
			_, err = owner.AdmitIdentity(func(IdentityObservation) error {
				calls++
				switch mode {
				case "panic":
					panic("private-identity-policy-payload")
				case "panic-nil":
					panic(nil)
				default:
					return reason
				}
			})
			if mode == "error" && !errors.Is(err, reason) {
				t.Fatal("identity policy error lost")
			}
			if mode != "error" && !errors.Is(err, ErrAdmissionPanic) {
				t.Fatal("identity panic escaped or was lost")
			}
			if err != nil && strings.Contains(err.Error(), "private-identity-policy-payload") {
				t.Fatal("identity panic payload disclosed")
			}
			requireStatus(t, err, denied)
			absent(t, marker)
			_, err = owner.AdmitIdentity(func(IdentityObservation) error { calls++; return nil })
			requireStatus(t, err, invalidState)
			if calls != 1 {
				t.Fatal("identity policy replayed")
			}
			if _, err := owner.Stop(84); err != nil {
				t.Fatal(err)
			}
			out, err := owner.Finish(3000)
			if err != nil || out.State != complete || out.RootExit != 84 {
				t.Fatalf("denied identity lost settlement: %+v %v", out, err)
			}
			absent(t, marker)
		})
	}
}

func TestIdentityAdmissionNilPolicyAndReleasedOwner(t *testing.T) {
	lib, child, dir := setup(t)
	owner, _, err := lib.New(child, dir, []string{"exit", filepath.Join(dir, "nil-identity"), "0", "ok"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.AdmitIdentity(nil)
	requireStatus(t, err, argument)
	calls := 0
	if _, err := owner.AdmitIdentity(func(IdentityObservation) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	out, err := owner.Finish(3000)
	if err != nil || out.State != complete {
		t.Fatalf("finish: %+v %v", out, err)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.AdmitIdentity(func(IdentityObservation) error { calls++; return nil }); err == nil || calls != 1 {
		t.Fatal("identity reused a released owner")
	}
}

func TestIdentityAdmissionStoppedOwnerHasNoObservation(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "stopped-identity")
	owner, _, err := lib.New(child, dir, []string{"exit", marker, "0", "must-not-run"})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Stop(85); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = owner.AdmitIdentity(func(IdentityObservation) error { called = true; return nil })
	requireStatus(t, err, invalidState)
	if called {
		t.Fatal("stopped identity owner reached policy")
	}
	absent(t, marker)
}

func TestIdentityAdmissionSerializesPolicyAndOwner(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "identity-barrier")
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
	var returned atomic.Bool
	result := make(chan error, 1)
	go func() {
		_, e := owner.AdmitIdentity(func(IdentityObservation) error {
			close(entered)
			<-release
			returned.Store(true)
			return nil
		})
		result <- e
	}()
	select {
	case <-entered:
	case e := <-result:
		t.Fatalf("identity policy never entered: %v", e)
	case <-time.After(3 * time.Second):
		t.Fatal("identity policy entry timed out")
	}
	absent(t, marker)
	// The callback is held at its barrier and no competing owner call has
	// started yet. TryLock directly proves its owner mutex remains held.
	if owner.mu.TryLock() {
		owner.mu.Unlock()
		t.Fatal("identity policy did not retain the owner mutex")
	}
	if err := lib.Close(); err == nil {
		t.Fatal("library unloaded during identity policy")
	}
	finished := make(chan error, 1)
	go func() {
		_, e := owner.Finish(0)
		if e == nil && !returned.Load() {
			e = errors.New("owner call overlapped identity policy")
		}
		finished <- e
	}()
	once.Do(func() { close(release) })
	for _, done := range []<-chan error{result, finished} {
		select {
		case e := <-done:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("identity policy/owner caller did not join")
		}
	}
}
