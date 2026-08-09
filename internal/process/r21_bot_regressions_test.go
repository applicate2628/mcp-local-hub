package process

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestR21CleanupDeadlineBoundsAStuckWait(t *testing.T) {
	waitCh := make(chan containedWaitResult)
	child := &scriptedContainedChild{
		waitCh: waitCh,
		terminateFn: func(uint32) error {
			return nil
		},
	}
	h := &containedDependencyHarness{child: child}
	started := time.Now()
	err := runHarness(context.Background(), h, 20*time.Millisecond, io.Discard, func(io.Reader) error {
		return errors.New("force teardown")
	})
	close(waitCh)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stuck wait held request for %v after cleanup deadline", elapsed)
	}
	if !errors.Is(err, ErrCleanupTimeout) {
		t.Fatalf("err=%v, want ErrCleanupTimeout", err)
	}
}
