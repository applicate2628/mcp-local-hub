//go:build windows && amd64 && cgo

package caller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

type ioOutcome struct {
	n   int64
	err error
}
type joinedIO struct {
	done   chan ioOutcome
	result ioOutcome
	joined bool
}

// Only test-owned file endpoints are interrupted here. This helper is not a
// new library pipe/pump abstraction and never closes an arbitrary caller stream.
func beginOwnedIO(t *testing.T, file *os.File, work func() ioOutcome) *joinedIO {
	t.Helper()
	job := &joinedIO{done: make(chan ioOutcome, 1)}
	go func() { job.done <- work() }()
	t.Cleanup(func() {
		if !job.joined {
			_ = file.Close()
			select {
			case job.result = <-job.done:
				job.joined = true
			case <-time.After(3 * time.Second):
				t.Error("owned test I/O failed to join after explicit endpoint close")
			}
		}
	})
	return job
}
func (j *joinedIO) await(t *testing.T) ioOutcome {
	t.Helper()
	if !j.joined {
		select {
		case j.result = <-j.done:
			j.joined = true
		case <-time.After(3 * time.Second):
			t.Fatal("I/O did not settle before watchdog")
		}
	}
	return j.result
}
func (j *joinedIO) stillPending(t *testing.T) {
	t.Helper()
	select {
	case j.result = <-j.done:
		j.joined = true
		t.Fatalf("expected live I/O, already got %+v", j.result)
	default:
	}
}
func goPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}
func waitPipeData(t *testing.T, file *os.File) uint32 {
	t.Helper()
	peek := testKernel.NewProc("PeekNamedPipe")
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count uint32
		var nativeErr error
		rc, e := file.SyscallConn()
		if e != nil {
			t.Fatal(e)
		}
		e = rc.Control(func(handle uintptr) {
			ok, _, err := peek.Call(handle, 0, 0, 0, uintptr(unsafe.Pointer(&count)), 0)
			if ok == 0 {
				nativeErr = err
			}
		})
		if e != nil || nativeErr != nil {
			t.Fatalf("owned pipe observation: %v %v", e, nativeErr)
		}
		if count > 0 {
			return count
		}
		if time.Now().After(deadline) {
			t.Fatal("no actual data reached the native pipe")
		}
		time.Sleep(time.Millisecond)
	}
}
func observeDeadline(t *testing.T, o *Owner) Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	out, e := o.FinishContext(ctx)
	if !errors.Is(e, context.DeadlineExceeded) || out.State != pending {
		t.Fatalf("observer did not retain pending operation: %+v %v", out, e)
	}
	requireStatus(t, o.Release(), notSettled)
	return out
}
func finishWithin(t *testing.T, o *Owner, code uint32) Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, e := o.FinishContext(ctx)
	if e != nil || out.State != complete || out.Flags&1 == 0 || out.RootExit != code {
		t.Fatalf("native settlement: %+v %v", out, e)
	}
	return out
}

func TestIOCancelWaitDoesNotCloseBlockedWriter(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	r, w := goPipe(t)
	s.Stdin = r
	owner, _, e := lib.NewWithStdio(child, dir, []string{"stdio", filepath.Join(dir, "proof")}, nil, s)
	if owner != nil {
		own(t, owner)
	}
	if e != nil {
		t.Fatal(e)
	}
	// Child remains suspended and holds its read endpoint. The native pipe may
	// expose the whole queued buffer even while Write is still pending. Observe
	// request arrival AND actual completion separately; do not infer one from size.
	data := bytes.Repeat([]byte{0, 255, 17, 128}, 1<<18)
	job := beginOwnedIO(t, w, func() ioOutcome { n, e := w.Write(data); return ioOutcome{int64(n), e} })
	_ = waitPipeData(t, r)

	job.stillPending(t)
	observeDeadline(t, owner)
	job.stillPending(t)
	if e = w.Close(); e != nil {
		t.Fatal(e)
	} // only the actual endpoint owner requests interruption
	got := job.await(t)
	if !errors.Is(got.err, os.ErrClosed) || got.n < 0 || got.n > int64(len(data)) {
		t.Fatalf("owned writer did not observe interruption: %+v", got)
	}
	current, e := owner.Finish(0)
	if e != nil || current.State != pending {
		t.Fatalf("closing caller writer stopped unrelated native owner: %+v %v", current, e)
	}
	if _, e = owner.Stop(88); e != nil {
		t.Fatal(e)
	}
	finishWithin(t, owner, 88)
}

func TestIOCanceledObserverCanResumeNormalDrainWithoutStop(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	inputR, inputW := goPipe(t)
	outputR, outputW := goPipe(t)
	s.Stdin = inputR
	s.Stdout = outputW
	marker := filepath.Join(dir, "drain-proof")
	owner, _, e := lib.NewWithStdio(child, dir, []string{"stdio", marker}, nil, s)
	if owner != nil {
		own(t, owner)
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = inputR.Close(); e != nil {
		t.Fatal(e)
	}
	if e = outputW.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = owner.AdmitChecked(func(ProcessObservation) error { return nil }); e != nil {
		t.Fatal(e)
	}
	var output bytes.Buffer
	reader := beginOwnedIO(t, outputR, func() ioOutcome { n, e := io.Copy(&output, io.LimitReader(outputR, 4097)); return ioOutcome{n, e} })
	observeDeadline(t, owner)
	reader.stillPending(t)
	payload := []byte("after-observer-cancel\x00\xff\x80")
	if n, e := inputW.Write(payload); e != nil || n != len(payload) {
		t.Fatalf("input write: %d %v", n, e)
	}
	if e = inputW.Close(); e != nil {
		t.Fatal(e)
	}
	finishWithin(t, owner, 0)
	got := reader.await(t)
	if got.err != nil || !bytes.Equal(output.Bytes(), payload) {
		t.Fatalf("EOF/drain lost final bytes: %+v %x", got, output.Bytes())
	}
	// Native settlement did not itself close this caller-owned endpoint.
	if _, e = outputR.Read(make([]byte, 1)); !errors.Is(e, io.EOF) {
		t.Fatalf("borrowed output was closed instead of EOF: %v", e)
	}
}

func TestIORootExitIsNotOutputEOFWhileDescendantLives(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	r, w := goPipe(t)
	s.Stdout = w
	s.Stderr = w
	owner, _, e := lib.NewWithStdio(child, dir, []string{"detach", filepath.Join(dir, "root-exited")}, nil, s)
	if owner != nil {
		own(t, owner)
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = owner.Admit(true); e != nil {
		t.Fatal(e)
	}
	reader := beginOwnedIO(t, r, func() ioOutcome { n, e := io.Copy(io.Discard, io.LimitReader(r, 1)); return ioOutcome{n, e} })
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, e := owner.Finish(25)
		if e != nil {
			t.Fatal(e)
		}
		if v.Flags&1 != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("root did not exit")
		}
	}
	reader.stillPending(t)
	out := observeDeadline(t, owner)
	if out.RootExit != 7 || out.Flags&1 == 0 {
		t.Fatalf("root result missing: %+v", out)
	}
	reader.stillPending(t)
	if _, e = owner.Stop(89); e != nil {
		t.Fatal(e)
	}
	finishWithin(t, owner, 7)
	got := reader.await(t)
	if got.err != nil || got.n != 0 {
		t.Fatalf("native descendant writer did not close: %+v", got)
	}
}

func TestIONativeSettlementDoesNotDiscardUnreadFinalBytes(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	r, w := goPipe(t)
	s.Stdout = w
	owner, _, e := lib.NewWithStdio(child, dir, []string{"stdio", filepath.Join(dir, "finished")}, nil, s)
	if owner != nil {
		own(t, owner)
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = owner.Admit(true); e != nil {
		t.Fatal(e)
	}
	finishWithin(t, owner, 0) // do not start reading until process ownership settled
	var output bytes.Buffer
	reader := beginOwnedIO(t, r, func() ioOutcome {
		n, err := io.Copy(&output, io.LimitReader(r, 4097))
		return ioOutcome{n, err}
	})
	got := reader.await(t)
	if got.err != nil || !bytes.Equal(output.Bytes(), streamBytes) {
		t.Fatalf("native finish consumed/closed caller data: %x %v", output.Bytes(), got.err)
	}
}

func TestIOEndpointOwnerCanInterruptReadWithoutStoppingChild(t *testing.T) {
	lib, child, dir := setup(t)
	s := streamFiles(t, dir)
	r, w := goPipe(t)
	s.Stdout = w
	s.Stderr = w
	owner, _, e := lib.NewWithStdio(child, dir, []string{"block", filepath.Join(dir, "no-output")}, nil, s)
	if owner != nil {
		own(t, owner)
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = owner.Admit(true); e != nil {
		t.Fatal(e)
	}
	reader := beginOwnedIO(t, r, func() ioOutcome { n, e := io.Copy(io.Discard, io.LimitReader(r, 1)); return ioOutcome{n, e} })
	observeDeadline(t, owner)
	reader.stillPending(t)
	if e = r.Close(); e != nil {
		t.Fatal(e)
	} // actual endpoint owner, not FinishContext
	got := reader.await(t)
	if !errors.Is(got.err, os.ErrClosed) || got.n != 0 {
		t.Fatalf("owned read interruption was not reported/joined: %+v", got)
	}
	current, e := owner.Finish(0)
	if e != nil || current.State != pending {
		t.Fatalf("I/O close changed process ownership: %+v %v", current, e)
	}
	if _, e = owner.Stop(90); e != nil {
		t.Fatal(e)
	}
	finishWithin(t, owner, 90)
}
