package gui

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
)

const occurrenceStoreLockStrandedEvent = "daemon-recovery-occurrence-store-lock-stranded"

type occurrenceStoreLockState string

const (
	occurrenceStoreLockReleased    occurrenceStoreLockState = "released"
	occurrenceStoreLockOutstanding occurrenceStoreLockState = "outstanding"
	occurrenceStoreLockStranded    occurrenceStoreLockState = "stranded"
)

type occurrenceStoreDataOutcome string

const (
	occurrenceStoreDataNotEntered    occurrenceStoreDataOutcome = "not_entered"
	occurrenceStoreDataUnproven      occurrenceStoreDataOutcome = "unproven"
	occurrenceStoreDataDurableProven occurrenceStoreDataOutcome = "durable_proven"
)

type occurrenceStoreLockHealthSnapshot struct {
	State           occurrenceStoreLockState
	Revision        uint64
	RestartRequired bool
}

type occurrenceStoreLockHealthRecord struct {
	Snapshot occurrenceStoreLockHealthSnapshot
	Cause    error
}

type occurrenceStoreLock interface {
	TryLockContext(context.Context, time.Duration) (bool, error)
	Close() error
}

type occurrenceStoreLockFactory func(string) occurrenceStoreLock

type occurrenceStoreLockHealthEvent struct {
	Operation   string
	DataOutcome occurrenceStoreDataOutcome
	Snapshot    occurrenceStoreLockHealthSnapshot
}

type occurrenceStoreLockHealthEmitter func(occurrenceStoreLockHealthEvent)

type occurrenceStoreLockHealth struct {
	mu             sync.Mutex
	active         uint64
	record         atomic.Pointer[occurrenceStoreLockHealthRecord]
	strandedHandle occurrenceStoreLock
	newLock        occurrenceStoreLockFactory
	emit           occurrenceStoreLockHealthEmitter
}

func newOccurrenceStoreLockHealth(factory occurrenceStoreLockFactory, emit occurrenceStoreLockHealthEmitter) *occurrenceStoreLockHealth {
	if factory == nil {
		factory = func(path string) occurrenceStoreLock { return flock.New(path) }
	}
	health := &occurrenceStoreLockHealth{newLock: factory, emit: emit}
	health.record.Store(&occurrenceStoreLockHealthRecord{
		Snapshot: occurrenceStoreLockHealthSnapshot{State: occurrenceStoreLockReleased},
	})
	return health
}

func (h *occurrenceStoreLockHealth) snapshot() occurrenceStoreLockHealthSnapshot {
	if h == nil {
		return occurrenceStoreLockHealthSnapshot{State: occurrenceStoreLockReleased}
	}
	record := h.record.Load()
	if record == nil {
		return occurrenceStoreLockHealthSnapshot{State: occurrenceStoreLockReleased}
	}
	return record.Snapshot
}

func (h *occurrenceStoreLockHealth) strandedError(operation string, outcome occurrenceStoreDataOutcome) *occurrenceStoreLockStrandedError {
	if h == nil {
		return nil
	}
	record := h.record.Load()
	if record == nil || record.Snapshot.State != occurrenceStoreLockStranded {
		return nil
	}
	return &occurrenceStoreLockStrandedError{
		Operation:   operation,
		DataOutcome: outcome,
		Health:      record.Snapshot,
		Cause:       record.Cause,
	}
}

func (h *occurrenceStoreLockHealth) begin(operation string) (*occurrenceStoreLockLease, *occurrenceStoreLockStrandedError) {
	h.mu.Lock()
	defer h.mu.Unlock()
	record := h.record.Load()
	if record != nil && record.Snapshot.State == occurrenceStoreLockStranded {
		return nil, &occurrenceStoreLockStrandedError{
			Operation:   operation,
			DataOutcome: occurrenceStoreDataNotEntered,
			Health:      record.Snapshot,
			Cause:       record.Cause,
		}
	}
	h.active++
	if record == nil || record.Snapshot.State == occurrenceStoreLockReleased {
		revision := uint64(1)
		if record != nil {
			revision = record.Snapshot.Revision + 1
		}
		h.record.Store(&occurrenceStoreLockHealthRecord{
			Snapshot: occurrenceStoreLockHealthSnapshot{
				State:    occurrenceStoreLockOutstanding,
				Revision: revision,
			},
		})
	}
	return &occurrenceStoreLockLease{health: h, operation: operation}, nil
}

// poison publishes the fail-closed state without taking health.mu. The common
// wrapper calls it after Close fails but before releasing storeMu, so a waiter
// that passed an earlier health check cannot acquire storeMu in the gap between
// the failed release and the authoritative stranded transition.
func (h *occurrenceStoreLockHealth) poison(
	operation string,
	releaseErr error,
	outcome occurrenceStoreDataOutcome,
) (*occurrenceStoreLockStrandedError, *occurrenceStoreLockHealthEvent) {
	for {
		record := h.record.Load()
		if record != nil && record.Snapshot.State == occurrenceStoreLockStranded {
			return &occurrenceStoreLockStrandedError{
				Operation:   operation,
				DataOutcome: outcome,
				Health:      record.Snapshot,
				Cause:       record.Cause,
			}, nil
		}
		revision := uint64(1)
		if record != nil {
			revision = record.Snapshot.Revision + 1
		}
		snapshot := occurrenceStoreLockHealthSnapshot{
			State:           occurrenceStoreLockStranded,
			Revision:        revision,
			RestartRequired: true,
		}
		next := &occurrenceStoreLockHealthRecord{Snapshot: snapshot, Cause: releaseErr}
		if h.record.CompareAndSwap(record, next) {
			return &occurrenceStoreLockStrandedError{
					Operation:   operation,
					DataOutcome: outcome,
					Health:      snapshot,
					Cause:       releaseErr,
				}, &occurrenceStoreLockHealthEvent{
					Operation:   operation,
					DataOutcome: outcome,
					Snapshot:    snapshot,
				}
		}
	}
}

func (h *occurrenceStoreLockHealth) emitEvent(event *occurrenceStoreLockHealthEvent) {
	if h != nil && event != nil && h.emit != nil {
		h.emit(*event)
	}
}

func (h *occurrenceStoreLockHealth) finish(
	operation string,
	lockedHandle occurrenceStoreLock,
	outcome occurrenceStoreDataOutcome,
) *occurrenceStoreLockStrandedError {
	h.mu.Lock()
	if h.active > 0 {
		h.active--
	}
	record := h.record.Load()
	if record != nil && record.Snapshot.State == occurrenceStoreLockStranded {
		if lockedHandle != nil && h.strandedHandle == nil {
			h.strandedHandle = lockedHandle
		}
		h.mu.Unlock()
		return &occurrenceStoreLockStrandedError{
			Operation:   operation,
			DataOutcome: outcome,
			Health:      record.Snapshot,
			Cause:       record.Cause,
		}
	}
	if h.active == 0 && record != nil && record.Snapshot.State == occurrenceStoreLockOutstanding {
		h.record.Store(&occurrenceStoreLockHealthRecord{
			Snapshot: occurrenceStoreLockHealthSnapshot{
				State:    occurrenceStoreLockReleased,
				Revision: record.Snapshot.Revision + 1,
			},
		})
	}
	h.mu.Unlock()
	return nil
}

type occurrenceStoreLockLease struct {
	health    *occurrenceStoreLockHealth
	operation string
	once      sync.Once
	err       *occurrenceStoreLockStrandedError
}

func (l *occurrenceStoreLockLease) stranded(outcome occurrenceStoreDataOutcome) *occurrenceStoreLockStrandedError {
	if l == nil {
		return nil
	}
	return l.health.strandedError(l.operation, outcome)
}

func (l *occurrenceStoreLockLease) poison(
	releaseErr error,
	outcome occurrenceStoreDataOutcome,
) (*occurrenceStoreLockStrandedError, *occurrenceStoreLockHealthEvent) {
	if l == nil {
		return nil, nil
	}
	return l.health.poison(l.operation, releaseErr, outcome)
}

func (l *occurrenceStoreLockLease) finish(
	lockedHandle occurrenceStoreLock,
	outcome occurrenceStoreDataOutcome,
) *occurrenceStoreLockStrandedError {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.err = l.health.finish(l.operation, lockedHandle, outcome)
	})
	return l.err
}

type occurrenceStoreLockStrandedError struct {
	Operation   string
	DataOutcome occurrenceStoreDataOutcome
	Health      occurrenceStoreLockHealthSnapshot
	Cause       error
}

func (e *occurrenceStoreLockStrandedError) Error() string {
	return fmt.Sprintf("%s: occurrence store lock release is unproven; process restart required: %v", e.Operation, e.Cause)
}

func (e *occurrenceStoreLockStrandedError) Unwrap() error { return e.Cause }

type auditLockStoreOperation struct {
	dataOutcome occurrenceStoreDataOutcome
}

func (o *auditLockStoreOperation) proveDurable() {
	if o != nil {
		o.dataOutcome = occurrenceStoreDataDurableProven
	}
}
