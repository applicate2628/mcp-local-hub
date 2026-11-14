//go:build windows && amd64 && cgo

package caller

import (
	"context"
	"errors"
	"time"
)

// FinishContext observes the same owner until terminal settlement, a native
// error, or cancellation. Cancellation affects THIS observer only: no Stop,
// Release, stream close, retry or abandoned native-wait goroutine is introduced.
// The last completed observation is returned alongside cancellation; zero means
// no native observation was acquired (including a pre-canceled/busy owner).
//
// Each acquired probe is the existing Finish(0). The mutex is never read through
// after failed TryLock and is released between probes. This also permits stopping
// observation while another caller holds the owner in policy or native work.
// An already entered native call cannot be interrupted by Go; no hard real-time
// bound is claimed. A native terminal/error outcome actually observed by a probe
// wins over concurrent context cancellation. CleanupFailed stays a distinct state.
//
// This does NOT stop, cancel or drain any caller-owned I/O. Finish(milliseconds)
// retains its original synchronous semantics. Owner/Library still must not be
// copied; arbitrary policy reentry into the same owner remains unsupported.
func (o *Owner) FinishContext(ctx context.Context) (Outcome, error) {
	if ctx == nil {
		return Outcome{}, &CallError{argument, 0}
	}
	var last Outcome
	if ctx.Err() != nil {
		return last, finishCancellation(ctx)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return last, finishCancellation(ctx)
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return last, finishCancellation(ctx)
		}
		if o.mu.TryLock() {
			// Cancellation may have arrived between the readiness check and
			// acquisition. Do not perform a new probe after observing it.
			if ctx.Err() != nil {
				o.mu.Unlock()
				return last, finishCancellation(ctx)
			}
			out, err := o.callLocked(2, 0)
			o.mu.Unlock()
			last = out
			if err != nil {
				return last, err
			}
			switch out.State {
			case complete, cleanupFailed:
				return last, nil
			case pending:
			default:
				return last, errors.New("finish returned an invalid settlement state")
			}
		}
		// A polling interval, not a cancellation timeout or speed guarantee.
		// No mutex is held, no new goroutine is spawned, and the timer is reused.
		timer.Reset(10 * time.Millisecond)
	}
}
func finishCancellation(ctx context.Context) error {
	err, cause := ctx.Err(), context.Cause(ctx)
	if cause == nil || errors.Is(err, cause) {
		return err
	}
	return errors.Join(err, cause)
}
