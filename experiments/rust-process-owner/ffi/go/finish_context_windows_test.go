//go:build windows && amd64 && cgo

package caller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type observedContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type contextFinishResult struct {
	out Outcome
	err error
}

func receiveFinish(t *testing.T, results <-chan contextFinishResult) contextFinishResult {
	t.Helper()
	select {
	case v := <-results:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not finish")
		return contextFinishResult{}
	}
}
func newContextOwner(t *testing.T, mode string) (*Owner, string) {
	t.Helper()
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "context-wait")
	args := []string{mode, marker}
	if mode == "exit" {
		args = append(args, "259", "terminal-bytes")
	}
	o, _, err := lib.New(child, dir, args)
	if o != nil {
		own(t, o)
	}
	if err != nil {
		t.Fatal(err)
	}
	return o, marker
}

func TestFinishContextCanceledBeforeObservationHasNoEffect(t *testing.T) {
	o, marker := newContextOwner(t, "exit")
	cause := errors.New("caller stopped observing")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	out, err := o.FinishContext(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation causes lost: %+v %v", out, err)
	}
	if out != (Outcome{}) {
		t.Fatalf("pre-cancel fabricated observation: %+v", out)
	}
	absent(t, marker)
	requireStatus(t, o.Release(), notSettled)
	if _, err = o.Admit(true); err != nil {
		t.Fatalf("observer cancellation consumed owner: %v", err)
	}
	out, err = o.FinishContext(context.Background())
	if err != nil || out.State != complete || out.RootExit != 259 || out.Flags&1 == 0 {
		t.Fatalf("legitimate exit259 lost: %+v %v", out, err)
	}
}
func TestFinishContextNilRejectedWithoutNativeEffect(t *testing.T) {
	o, marker := newContextOwner(t, "exit")
	out, err := o.FinishContext(nil)
	requireStatus(t, err, argument)
	if out != (Outcome{}) {
		t.Fatalf("nil context supplied observations: %+v", out)
	}
	absent(t, marker)
	if _, err = o.Admit(true); err != nil {
		t.Fatal(err)
	}
	out, err = o.FinishContext(context.Background())
	if err != nil || out.State != complete {
		t.Fatalf("nil call damaged owner: %+v %v", out, err)
	}
}
func TestFinishContextDeadlineRetainsSuspendedOwner(t *testing.T) {
	o, marker := newContextOwner(t, "exit")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	out, err := o.FinishContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || out.State != pending || out.Flags&1 != 0 {
		t.Fatalf("deadline/outcome collapsed: %+v %v", out, err)
	}
	absent(t, marker)
	requireStatus(t, o.Release(), notSettled)
	current, e := o.Finish(0)
	if e != nil || current.State != pending {
		t.Fatalf("observer implicitly terminated owner: %+v %v", current, e)
	}
	if _, e = o.Stop(84); e != nil {
		t.Fatal(e)
	}
	final, e := o.FinishContext(context.Background())
	if e != nil || final.State != complete || final.RootExit != 84 {
		t.Fatalf("explicit stop unavailable: %+v %v", final, e)
	}
}
func TestFinishContextDoesNotWaitOnBusyOwnerAfterCancel(t *testing.T) {
	o, _ := newContextOwner(t, "exit")
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &observedContext{Context: base, waiting: make(chan struct{})}
	o.mu.Lock()
	locked := true
	defer func() {
		if locked {
			o.mu.Unlock()
		}
	}()
	results := make(chan contextFinishResult, 1)
	go func() { out, err := o.FinishContext(ctx); results <- contextFinishResult{out, err} }()
	select {
	case <-ctx.waiting:
	case <-time.After(time.Second):
		t.Error("context was not consulted while owner was busy")
		cancel()
		o.mu.Unlock()
		locked = false
		_ = receiveFinish(t, results)
		return
	}
	cancel()
	var got contextFinishResult
	select {
	case got = <-results:
	case <-time.After(time.Second):
		t.Error("canceled observer stayed blocked on the owner mutex")
		o.mu.Unlock()
		locked = false
		_ = receiveFinish(t, results)
		return
	}
	if !errors.Is(got.err, context.Canceled) || got.out != (Outcome{}) {
		t.Fatalf("busy cancellation: %+v %v", got.out, got.err)
	}
	o.mu.Unlock()
	locked = false
	if _, err := o.Stop(85); err != nil {
		t.Fatalf("canceled waiter changed ownership: %v", err)
	}
}
func TestFinishContextRootExitDoesNotImplyDescendantSettlement(t *testing.T) {
	o, _ := newContextOwner(t, "detach")
	if _, err := o.Admit(true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, err := o.Finish(25)
		if err != nil {
			t.Fatal(err)
		}
		if v.Flags&1 != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("root exit not observed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	out, err := o.FinishContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || out.State != pending || out.Flags&1 == 0 || out.RootExit != 7 || out.Flags&2 == 0 || out.Active == 0 {
		t.Fatalf("known root/remaining child lost: %+v %v", out, err)
	}
	requireStatus(t, o.Release(), notSettled)
	if _, err = o.Stop(86); err != nil {
		t.Fatal(err)
	}
	final, err := o.FinishContext(context.Background())
	if err != nil || final.State != complete || final.RootExit != 7 {
		t.Fatalf("root result overwritten: %+v %v", final, err)
	}
}
func TestFinishContextOneObserverCancelDoesNotCancelAnother(t *testing.T) {
	o, _ := newContextOwner(t, "block")
	if _, err := o.Admit(true); err != nil {
		t.Fatal(err)
	}
	other := make(chan contextFinishResult, 1)
	safety, finishSafety := context.WithTimeout(context.Background(), 3*time.Second)
	defer finishSafety()
	go func() { out, err := o.FinishContext(safety); other <- contextFinishResult{out, err} }()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	out, err := o.FinishContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || out.State != pending {
		t.Errorf("observer cancellation: %+v %v", out, err)
	}
	if _, err = o.Stop(87); err != nil {
		t.Error(err)
	}
	got := receiveFinish(t, other)
	if got.err != nil || got.out.State != complete || got.out.RootExit != 87 {
		t.Fatalf("second observer lost original ownership: %+v %v", got.out, got.err)
	}
}
func TestFinishContextCanceledWaitDoesNotConsumeTerminalResult(t *testing.T) {
	o, _ := newContextOwner(t, "exit")
	if _, err := o.Admit(true); err != nil {
		t.Fatal(err)
	}
	final, err := o.Finish(3000)
	if err != nil || final.State != complete {
		t.Fatalf("setup: %+v %v", final, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := o.FinishContext(ctx)
	if !errors.Is(err, context.Canceled) || out != (Outcome{}) {
		t.Fatalf("pre-canceled terminal call did native work: %+v %v", out, err)
	}
	next, err := o.FinishContext(context.Background())
	if err != nil || next != final {
		t.Fatalf("terminal observation changed: %+v %v", next, err)
	}
	if err = o.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err = o.FinishContext(context.Background()); err == nil {
		t.Fatal("released owner accepted")
	}
}

func TestFinishContextCanCancelWhileAdmissionPolicyOwnsLock(t *testing.T) {
	o, marker := newContextOwner(t, "exit")
	entered, permit := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(permit) })
	admitted := make(chan error, 1)
	go func() {
		_, err := o.AdmitChecked(func(ProcessObservation) error { close(entered); <-permit; return nil })
		admitted <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("policy did not enter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	results := make(chan contextFinishResult, 1)
	go func() { out, err := o.FinishContext(ctx); results <- contextFinishResult{out, err} }()
	var got contextFinishResult
	select {
	case got = <-results:
	case <-time.After(time.Second):
		t.Error("observer is trapped behind unrelated policy execution")
		release.Do(func() { close(permit) })
		got = receiveFinish(t, results)
	}
	// No hidden resume, policy cancellation, file effect or owner stop.
	if !errors.Is(got.err, context.DeadlineExceeded) || got.out != (Outcome{}) {
		t.Errorf("busy policy observation: %+v %v", got.out, got.err)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Errorf("child effect during unreturned policy: %v", e)
	}
	release.Do(func() { close(permit) })
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("policy call did not join")
	}
	out, err := o.FinishContext(context.Background())
	if err != nil || out.State != complete || out.RootExit != 259 {
		t.Fatalf("observer affected eventual policy/child result: %+v %v", out, err)
	}
}
