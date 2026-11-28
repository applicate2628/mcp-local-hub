//go:build windows && amd64 && cgo

package caller

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

func TestIdentityQueryPreservesAdmissionAndOwnsCopies(t *testing.T) {
	for _, mode := range []string{"identity", "token-user"} {
		t.Run(mode, func(t *testing.T) {
			lib, child, _ := setup(t)
			dir := identityFixtureDir(t)
			marker := filepath.Join(dir, "private-query-child-observation")
			owner, initial, err := lib.New(child, dir, []string{mode, marker})
			if owner != nil {
				own(t, owner)
			}
			if err != nil {
				t.Fatal(err)
			}
			first, err := owner.QueryIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if first.PID != initial.PID || first.CreationTime100ns == 0 ||
				first.OriginalJobMembership != JobMember || first.WaitState != IdentityNotSignaled ||
				!strings.EqualFold(filepath.Clean(string(utf16.Decode(first.ImagePathUTF16))), filepath.Clean(child)) {
				t.Fatal("query did not describe original suspended process")
			}
			got, err := owner.QueryIdentity()
			if err != nil || !reflect.DeepEqual(got, first) {
				t.Fatalf("repeated query changed original facts: %v", err)
			}
			first.ImagePathUTF16[0] = 'X'
			first.TokenUserSID[0] = 0
			if got.ImagePathUTF16[0] == 'X' || got.TokenUserSID[0] != 1 {
				t.Fatal("query observations share mutable copies")
			}
			runtime.GC()
			absent(t, marker)
			// Success also checks the original suspend count is still exactly 1.
			// A missing marker alone would not prove that query never resumed.
			if _, err := owner.AdmitIdentity(func(v IdentityObservation) error {
				if !reflect.DeepEqual(v, got) {
					return errors.New("admission no longer observes the queried original owner")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			out, err := owner.Finish(3000)
			if err != nil || out.State != complete || out.RootExit != 0 {
				t.Fatalf("queried child did not settle: %+v %v", out, err)
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
					t.Fatal("query differs from child's independent birth report")
				}
			} else if !bytes.Equal(data, got.TokenUserSID) {
				t.Fatal("query differs from child's independent TokenUser SID")
			}
			// Go copies remain owned after native storage and DLL have gone away.
			got.ImagePathUTF16[0] = 'Y'
			got.TokenUserSID[0] = 0
		})
	}
}

func TestIdentityQueryAfterAdmissionAndDenial(t *testing.T) {
	for _, allow := range []bool{true, false} {
		name := "denied"
		if allow {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			lib, child, dir := setup(t)
			marker := filepath.Join(dir, "query-"+name)
			owner, _, err := lib.New(child, dir, []string{"block", marker})
			if owner != nil {
				own(t, owner)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := owner.QueryIdentity()
			if err != nil {
				t.Fatal(err)
			}
			_, err = owner.Admit(allow)
			if allow && err != nil {
				t.Fatal(err)
			}
			if !allow {
				requireStatus(t, err, denied)
			} else {
				deadline := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(marker); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("admitted query child never became ready")
					}
					time.Sleep(time.Millisecond)
				}
			}
			for i := 0; i < 2; i++ {
				got, err := owner.QueryIdentity()
				if err != nil || !reflect.DeepEqual(got, before) {
					t.Fatalf("query after %s: %v", name, err)
				}
			}
			_, err = owner.Admit(true)
			requireStatus(t, err, invalidState)
			if !allow {
				absent(t, marker)
			}
			if _, err := owner.Stop(88); err != nil {
				t.Fatal(err)
			}
			got, err := owner.QueryIdentity()
			requireStatus(t, err, invalidState)
			if !reflect.DeepEqual(got, IdentityObservation{}) {
				t.Fatal("stopped query exposed a partial observation")
			}
			out, err := owner.Finish(3000)
			if err != nil || out.State != complete || out.RootExit != 88 {
				t.Fatalf("query owner did not settle: %+v %v", out, err)
			}
			got, err = owner.QueryIdentity()
			requireStatus(t, err, invalidState)
			if !reflect.DeepEqual(got, IdentityObservation{}) {
				t.Fatal("terminal query exposed a partial observation")
			}
			if err := owner.Release(); err != nil {
				t.Fatal(err)
			}
			got, err = owner.QueryIdentity()
			if err == nil || !reflect.DeepEqual(got, IdentityObservation{}) {
				t.Fatal("released query was accepted")
			}
		})
	}
}

func TestIdentityQueryConcurrentWithAdmission(t *testing.T) {
	lib, child, dir := setup(t)
	owner, initial, err := lib.New(child, dir, []string{"block", filepath.Join(dir, "concurrent-query")})
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	const readers = 12
	start := make(chan struct{})
	results := make(chan error, readers+1)
	var joined sync.WaitGroup
	for i := 0; i < readers; i++ {
		joined.Add(1)
		go func() {
			defer joined.Done()
			<-start
			value, err := owner.QueryIdentity()
			if err == nil && (value.PID != initial.PID || value.WaitState != IdentityNotSignaled) {
				err = errors.New("concurrent query returned different original process")
			}
			results <- err
		}()
	}
	joined.Add(1)
	go func() {
		defer joined.Done()
		<-start
		_, err := owner.Admit(true)
		results <- err
	}()
	close(start)
	joined.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
}
