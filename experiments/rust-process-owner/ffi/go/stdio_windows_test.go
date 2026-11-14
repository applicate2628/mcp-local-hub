//go:build windows && amd64 && cgo

package caller

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var testKernel = syscall.NewLazyDLL("kernel32.dll")
var testGetFlags = testKernel.NewProc("GetHandleInformation")
var testSetEvent = testKernel.NewProc("CreateEventW")
var testHandleCount = testKernel.NewProc("GetProcessHandleCount")
var streamBytes = []byte{'a', 0, 0xff, '\r', '\n', 0xc3, 0xa9}
var streamError = []byte("native-stderr\x00\xff")

func flagsOf(t *testing.T, f *os.File) uint32 {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uint32
	var native error
	err = rc.Control(func(h uintptr) {
		r, _, e := testGetFlags.Call(h, uintptr(unsafe.Pointer(&flags)))
		if r == 0 {
			native = e
		}
	})
	if err != nil || native != nil {
		t.Fatalf("handle flags: %v %v", err, native)
	}
	return flags
}
func streamFiles(t *testing.T, dir string) Stdio {
	t.Helper()
	input := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(input, streamBytes, 0600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })
	out, err := os.Create(filepath.Join(dir, "output.bin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	stderr, err := os.Create(filepath.Join(dir, "error.bin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close() })
	return Stdio{in, out, stderr}
}
func assertStreamFinish(t *testing.T, o *Owner) {
	t.Helper()
	v, e := o.Finish(3000)
	if e != nil || v.State != complete || v.RootExit != 0 {
		t.Fatalf("stdio fixture not complete: %+v %v", v, e)
	}
}
func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(got, want) {
		t.Fatalf("stream bytes: got=%x want=%x err=%v", got, want, e)
	}
}

func TestStdioCopiesNonInheritableSourcesAndPreservesOwnership(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	before := [3]uint32{flagsOf(t, s.Stdin), flagsOf(t, s.Stdout), flagsOf(t, s.Stderr)}
	for _, flags := range before {
		if flags&1 != 0 {
			t.Fatal("fixture must begin non-inheritable")
		}
	}
	marker := filepath.Join(dir, "proof")
	o, _, e := lib.NewWithStdio(child, dir, []string{"stdio", marker}, nil, s)
	if o != nil {
		own(t, o)
	}
	if e != nil {
		t.Fatal(e)
	}
	absent(t, marker)
	runtime.GC()
	after := [3]uint32{flagsOf(t, s.Stdin), flagsOf(t, s.Stdout), flagsOf(t, s.Stderr)}
	if after != before {
		t.Fatal("caller handle flags mutated")
	}
	if _, e = o.AdmitChecked(func(ProcessObservation) error { return nil }); e != nil {
		t.Fatal(e)
	}
	assertStreamFinish(t, o)
	assertBytes(t, filepath.Join(dir, "output.bin"), streamBytes)
	assertBytes(t, filepath.Join(dir, "error.bin"), streamError)
	if _, e = s.Stdout.Write([]byte("parent-still-owns")); e != nil {
		t.Fatalf("parent stdout closed: %v", e)
	}
	if _, e = s.Stdin.Seek(0, io.SeekStart); e != nil {
		t.Fatal("parent stdin closed")
	}
}
func TestStdioCallerMayCloseSourcesAfterCreate(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	o, _, e := lib.NewWithStdio(child, dir, []string{"stdio", filepath.Join(dir, "proof")}, nil, s)
	if o != nil {
		own(t, o)
	}
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range []*os.File{s.Stdin, s.Stdout, s.Stderr} {
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
	}
	runtime.GC()
	if _, e = o.Admit(true); e != nil {
		t.Fatal(e)
	}
	assertStreamFinish(t, o)
	assertBytes(t, filepath.Join(dir, "output.bin"), streamBytes)
	assertBytes(t, filepath.Join(dir, "error.bin"), streamError)
}
func TestStdioSharedOutputUsesOneUnderlyingFilePosition(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	s.Stderr = s.Stdout
	o, _, e := lib.NewWithStdio(child, dir, []string{"stdio", filepath.Join(dir, "proof")}, nil, s)
	if o != nil {
		own(t, o)
	}
	if e != nil {
		t.Fatal(e)
	}
	if _, e = o.Admit(true); e != nil {
		t.Fatal(e)
	}
	assertStreamFinish(t, o)
	want := append(append([]byte{}, streamBytes...), streamError...)
	assertBytes(t, filepath.Join(dir, "output.bin"), want)
}

// Native synchronous anonymous pipe. The ordinary Go os.Pipe async profile is a
// different qualification; this test does not infer it from file-type equality.
func synchronousPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	var r, w syscall.Handle
	if e := syscall.CreatePipe(&r, &w, nil, 0); e != nil {
		t.Fatal(e)
	}
	reader, writer := os.NewFile(uintptr(r), "owned-pipe-reader"), os.NewFile(uintptr(w), "owned-pipe-writer")
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	return reader, writer
}
func TestStdioPipeEOFHasNoHiddenParentWriter(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	r, w := synchronousPipe(t)
	s.Stdout = w
	o, _, e := lib.NewWithStdio(child, dir, []string{"stdio", filepath.Join(dir, "proof")}, nil, s)
	if o != nil {
		own(t, o)
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = o.Admit(true); e != nil {
		t.Fatal(e)
	}
	assertStreamFinish(t, o)
	done := make(chan error, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, 4097))
		if err == nil && !bytes.Equal(b, streamBytes) {
			err = errors.New("pipe payload mismatch")
		}
		done <- err
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-timer.C:
		_ = r.Close()
		e = <-done
		t.Fatalf("EOF held open by a parent handle: %v", e)
	}
}
func TestStdioDenialAndNativeFailureKeepSourceFiles(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	marker := filepath.Join(dir, "must-not-run")
	for i := 0; i < 8; i++ {
		o, _, e := lib.NewWithStdio(filepath.Join(dir, "missing.exe"), dir, nil, nil, s)
		if o != nil {
			own(t, o)
			t.Fatal("missing image returned owner")
		}
		requireStatus(t, e, nativeFailure)
		for _, f := range []*os.File{s.Stdin, s.Stdout, s.Stderr} {
			_ = flagsOf(t, f)
		}
	}
	o, _, e := lib.NewWithStdio(child, dir, []string{"stdio", marker}, nil, s)
	if o != nil {
		own(t, o)
	}
	if e != nil {
		t.Fatal(e)
	}
	_, e = o.AdmitChecked(func(ProcessObservation) error { return errors.New("deny fixture") })
	requireStatus(t, e, denied)
	if _, e = o.Stop(85); e != nil {
		t.Fatal(e)
	}
	v, e := o.Finish(3000)
	if e != nil || v.RootExit != 85 {
		t.Fatalf("deny cleanup: %+v %v", v, e)
	}
	absent(t, marker)
	assertBytes(t, filepath.Join(dir, "output.bin"), nil)
}
func TestStdioMissingOrClosedSourceNeverStartsChild(t *testing.T) {
	lib, child, dir := setup(t)
	for _, mode := range []string{"stdin-nil", "stdout-nil", "stderr-nil", "stdin-closed", "stdout-closed", "stderr-closed"} {
		t.Run(mode, func(t *testing.T) {
			sub := filepath.Join(dir, mode)
			if e := os.Mkdir(sub, 0700); e != nil {
				t.Fatal(e)
			}
			s := streamFiles(t, sub)
			slots := []**os.File{&s.Stdin, &s.Stdout, &s.Stderr}
			index := 0
			if mode[:6] == "stdout" {
				index = 1
			}
			if mode[:6] == "stderr" {
				index = 2
			}
			if mode[len(mode)-3:] == "nil" {
				*slots[index] = nil
			} else {
				_ = (*slots[index]).Close()
			}
			marker := filepath.Join(sub, "proof")
			o, _, e := lib.NewWithStdio(child, sub, []string{"stdio", marker}, nil, s)
			if o != nil {
				own(t, o)
				t.Fatal("invalid stream returned owner")
			}
			if e == nil {
				t.Fatal("invalid stream accepted")
			}
			absent(t, marker)
		})
	}
}
func TestStdioInheritanceExcludesUnrelatedParentEvent(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), InheritHandle: 1}
	raw, _, err := testSetEvent.Call(uintptr(unsafe.Pointer(&sa)), 1, 0, 0)
	if raw == 0 {
		t.Fatal(err)
	}
	event := syscall.Handle(raw)
	defer syscall.CloseHandle(event)
	marker := filepath.Join(dir, "probe")
	o, _, e := lib.NewWithStdio(child, dir, []string{"stdio-probe", marker, strconv.FormatUint(uint64(raw), 10)}, nil, s)
	if o != nil {
		own(t, o)
	}
	if e != nil {
		t.Fatal(e)
	}
	if _, e = o.Admit(true); e != nil {
		t.Fatal(e)
	}
	assertStreamFinish(t, o)
	state, e := syscall.WaitForSingleObject(event, 0)
	if e != nil || state != syscall.WAIT_TIMEOUT {
		t.Fatalf("unrelated parent capability leaked: wait=%d err=%v", state, e)
	}
}
func TestStdioComposesWithEnvironmentAndCheckedAdmission(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	marker := filepath.Join(dir, "context.bin")
	o, _, e := lib.NewWithStdio(child, dir, []string{"context", marker, "MCPHUB_STDIO"}, []EnvironmentEntry{{"MCPHUB_STDIO", "explicit"}}, s)
	if o != nil {
		own(t, o)
	}
	if e != nil {
		t.Fatal(e)
	}
	absent(t, marker)
	calls := 0
	if _, e = o.AdmitChecked(func(ProcessObservation) error { calls++; return nil }); e != nil {
		t.Fatal(e)
	}
	assertStreamFinish(t, o)
	if calls != 1 {
		t.Fatal("admission bypassed")
	}
	fields := readEnvironmentReport(t, marker, 2)
	if fields[1] == nil || *fields[1] != "explicit" {
		t.Fatal("environment lost on stdio creation")
	}
}

func ownedHandleCount(t *testing.T) uint32 {
	t.Helper()
	p, e := syscall.GetCurrentProcess()
	if e != nil {
		t.Fatal(e)
	}
	var n uint32
	r, _, e := testHandleCount.Call(uintptr(p), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		t.Fatal(e)
	}
	return n
}
func TestStdioPartialDuplicationReleasesEarlierCopies(t *testing.T) {
	dir := t.TempDir()
	s := streamFiles(t, dir)
	if e := s.Stderr.Close(); e != nil {
		t.Fatal(e)
	}
	// Warm the exact local-error path before count comparison; this does not
	// claim that process-wide first-use runtime initialization has zero cost.
	copies, e := duplicateStdio(s)
	if e == nil {
		_ = copies.close()
		t.Fatal("closed third input accepted")
	}
	baseline := ownedHandleCount(t)
	for i := 0; i < 32; i++ {
		copies, e = duplicateStdio(s)
		if e == nil {
			_ = copies.close()
			t.Fatal("closed input accepted")
		}
		if copies != (stdioCopies{}) {
			t.Fatal("failed acquisition retained temporary slots")
		}
	}
	after := ownedHandleCount(t)
	if after != baseline {
		t.Fatalf("partial duplication leaked handles: before=%d after=%d", baseline, after)
	}
	_ = flagsOf(t, s.Stdin)
	_ = flagsOf(t, s.Stdout)
}
func TestStdioTemporaryCloseFailuresRemainSeparateAndAreNotRetried(t *testing.T) {
	setFlags := testKernel.NewProc("SetHandleInformation")
	for mask := 1; mask < 8; mask++ {
		t.Run(strconv.Itoa(mask), func(t *testing.T) {
			s := streamFiles(t, t.TempDir())
			copies, e := duplicateStdio(s)
			if e != nil {
				t.Fatal(e)
			}
			originals := copies
			protected := [3]bool{}
			// Only the test's original duplicated handles are protected. Recovery
			// owns these exact still-live objects; no process-global/name sweep.
			defer func() {
				for i, h := range originals {
					if protected[i] {
						_, _, _ = setFlags.Call(uintptr(h), 2, 0)
						if e := syscall.CloseHandle(h); e != nil {
							t.Errorf("fixture cleanup: %v", e)
						}
					}
				}
			}()
			for i, h := range originals {
				if mask&(1<<i) != 0 {
					r, _, e := setFlags.Call(uintptr(h), 2, 2)
					if r == 0 {
						t.Fatal(e)
					}
					protected[i] = true
				}
			}
			e = copies.close()
			if e == nil {
				t.Fatal("protected close failure hidden")
			}
			var seen int
			var walk func(error)
			walk = func(err error) {
				switch v := err.(type) {
				case *StdioCopyError:
					if v.Operation != "close" {
						t.Errorf("wrong operation %q", v.Operation)
					}
					var native syscall.Errno
					if !errors.As(v.Err, &native) || native == 0 {
						t.Error("native close code lost")
					}
					seen |= 1 << v.Stream
				case interface{ Unwrap() []error }:
					for _, child := range v.Unwrap() {
						walk(child)
					}
				}
			}
			walk(e)
			if seen != mask {
				t.Fatalf("lost close failures: got=%d want=%d", seen, mask)
			}
			if copies != (stdioCopies{}) {
				t.Fatal("consumed handle values retained for retry")
			}
			if e = copies.close(); e != nil {
				t.Fatal("implicit stale-handle retry")
			}
			for i, h := range originals {
				var flags uint32
				r, _, _ := testGetFlags.Call(uintptr(h), uintptr(unsafe.Pointer(&flags)))
				if protected[i] {
					if r == 0 || flags&2 == 0 {
						t.Fatal("failed-close state misreported")
					}
				} else if r != 0 {
					t.Fatal("unprotected temporary handle was not closed")
				}
			}
		})
	}
}

func TestStdioGoPipesCarryBinaryInputAndReachEOF(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	inReader, inWriter, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = inReader.Close(); _ = inWriter.Close() })
	outReader, outWriter, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = outReader.Close(); _ = outWriter.Close() })
	// Deliberately bounded below the pipe capacity: this qualifies actual Go
	// pipe handles without pretending to be a general concurrent I/O pump.
	if _, e = inWriter.Write(streamBytes); e != nil {
		t.Fatal(e)
	}
	if e = inWriter.Close(); e != nil {
		t.Fatal(e)
	}
	s.Stdin = inReader
	s.Stdout = outWriter
	owner, _, e := lib.NewWithStdio(child, dir, []string{"stdio", filepath.Join(dir, "go-pipes")}, nil, s)
	if owner != nil {
		own(t, owner)
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = inReader.Close(); e != nil {
		t.Fatal(e)
	}
	if e = outWriter.Close(); e != nil {
		t.Fatal(e)
	}
	runtime.GC()
	if _, e = owner.AdmitChecked(func(ProcessObservation) error { return nil }); e != nil {
		t.Fatal(e)
	}
	assertStreamFinish(t, owner)
	done := make(chan error, 1)
	go func() {
		got, err := io.ReadAll(io.LimitReader(outReader, 4097))
		if err == nil && !bytes.Equal(got, streamBytes) {
			err = errors.New("Go pipe bytes changed")
		}
		done <- err
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-timer.C:
		_ = outReader.Close()
		e = <-done
		t.Fatalf("Go pipe did not reach EOF: %v", e)
	}
	assertBytes(t, filepath.Join(dir, "error.bin"), streamError)
}
