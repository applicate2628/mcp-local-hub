package gui

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type recoverySettlementPhase string

const (
	recoverySettlementPhaseAdmitted     recoverySettlementPhase = "admitted"
	recoverySettlementPhaseCommitted    recoverySettlementPhase = "committed"
	recoverySettlementPhaseNotCommitted recoverySettlementPhase = "not_committed"
	recoverySettlementPhaseSettled      recoverySettlementPhase = "settled"
	recoverySettlementPhaseDrainTimeout recoverySettlementPhase = "drain_timeout"

	recoverySettlementEventType         = "daemon-recovery-settlement-state"
	recoverySettlementDrainTimeoutCode  = "RECOVERY_SETTLEMENT_DRAIN_TIMEOUT"
	recoverySettlementDrainTimeoutEvent = "daemon-recovery-settlement-drain-timeout"
)

// ErrRecoverySettlementDrainTimeout is returned by Server.Start or
// ContinueWithGUIListener when committed recovery could not settle before the
// process-owned drain budget expired.
var ErrRecoverySettlementDrainTimeout = errors.New(recoverySettlementDrainTimeoutCode)

type recoverySettlementSnapshot struct {
	leaseID     uint64
	taskName    string
	correlation auditLockCorrelation
	phase       recoverySettlementPhase
}

func (s recoverySettlementSnapshot) eventBody() map[string]any {
	body := map[string]any{
		"task_name":       s.taskName,
		"attempt_id":      s.correlation.AttemptID,
		"occurrence_id":   s.correlation.OccurrenceID,
		"server_instance": s.correlation.ServerInstance,
		"phase":           string(s.phase),
	}
	if s.phase == recoverySettlementPhaseDrainTimeout {
		body["event"] = recoverySettlementDrainTimeoutEvent
		body["failure_id"] = recoverySettlementDrainTimeoutCode
	}
	return body
}

type recoverySettlementDrainTimeoutError struct {
	snapshot recoverySettlementSnapshot
}

func (*recoverySettlementDrainTimeoutError) Error() string {
	return recoverySettlementDrainTimeoutCode
}

func (*recoverySettlementDrainTimeoutError) Is(target error) bool {
	return target == ErrRecoverySettlementDrainTimeout
}

// recoverySettlementRegistry is the process owner of recovery attempts that
// may cross the daemon termination point of no return. Its mutex only protects
// lease state; recovery, occurrence storage, HTTP, and event publication all
// happen after it is released.
type recoverySettlementRegistry struct {
	mu                        sync.Mutex
	admitting                 bool
	nextLeaseID               uint64
	leases                    map[uint64]recoverySettlementSnapshot
	changed                   chan struct{}
	postCommitBudget          time.Duration
	terminalizationLockBudget time.Duration
	timedOut                  *recoverySettlementDrainTimeoutError
	publish                   func(Event)
}

type recoverySettlementLease struct {
	registry *recoverySettlementRegistry
	id       uint64
}

func newRecoverySettlementRegistry(postCommitBudget, terminalizationLockBudget time.Duration, publish func(Event)) *recoverySettlementRegistry {
	return &recoverySettlementRegistry{
		admitting:                 true,
		leases:                    make(map[uint64]recoverySettlementSnapshot),
		changed:                   make(chan struct{}),
		postCommitBudget:          postCommitBudget,
		terminalizationLockBudget: terminalizationLockBudget,
		publish:                   publish,
	}
}

func (r *recoverySettlementRegistry) admit(taskName string, correlation auditLockCorrelation) (*recoverySettlementLease, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.admitting {
		return nil, false
	}
	r.nextLeaseID++
	id := r.nextLeaseID
	r.leases[id] = recoverySettlementSnapshot{
		leaseID:     id,
		taskName:    taskName,
		correlation: correlation,
		phase:       recoverySettlementPhaseAdmitted,
	}
	r.signalLocked()
	return &recoverySettlementLease{registry: r, id: id}, true
}

func (r *recoverySettlementRegistry) closeAdmission() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.admitting {
		r.admitting = false
		r.signalLocked()
	}
	r.mu.Unlock()
}

func (r *recoverySettlementRegistry) wait() error {
	if r == nil {
		return nil
	}
	drainBudget := r.postCommitBudget + r.terminalizationLockBudget
	drainCtx, cancel := context.WithTimeout(context.Background(), drainBudget)
	defer cancel()
	for {
		r.mu.Lock()
		if r.timedOut != nil {
			err := r.timedOut
			r.mu.Unlock()
			return err
		}
		if len(r.leases) == 0 {
			r.mu.Unlock()
			return nil
		}
		changed := r.changed
		r.mu.Unlock()

		select {
		case <-changed:
		case <-drainCtx.Done():
			return r.failDrainTimeout()
		}
	}
}

func (r *recoverySettlementRegistry) failDrainTimeout() error {
	r.mu.Lock()
	if r.timedOut != nil {
		err := r.timedOut
		r.mu.Unlock()
		return err
	}
	// A lease can settle after wait observed it but before select chooses the
	// concurrently-ready deadline arm. In that interleaving, settlement won the
	// process race: do not synthesize a timeout or index an empty diagnostic.
	if len(r.leases) == 0 {
		r.mu.Unlock()
		return nil
	}
	snapshot := r.firstUnsettledLocked()
	snapshot.phase = recoverySettlementPhaseDrainTimeout
	err := &recoverySettlementDrainTimeoutError{snapshot: snapshot}
	r.timedOut = err
	r.signalLocked()
	r.mu.Unlock()
	r.publishSnapshot(snapshot)
	return err
}

func (r *recoverySettlementRegistry) signalLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *recoverySettlementRegistry) firstUnsettledLocked() recoverySettlementSnapshot {
	snapshots := make([]recoverySettlementSnapshot, 0, len(r.leases))
	for _, snapshot := range r.leases {
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		left, right := snapshots[i], snapshots[j]
		if left.correlation.ServerInstance != right.correlation.ServerInstance {
			return left.correlation.ServerInstance < right.correlation.ServerInstance
		}
		if left.correlation.AttemptID != right.correlation.AttemptID {
			return left.correlation.AttemptID < right.correlation.AttemptID
		}
		if left.correlation.OccurrenceID != right.correlation.OccurrenceID {
			return left.correlation.OccurrenceID < right.correlation.OccurrenceID
		}
		if left.taskName != right.taskName {
			return left.taskName < right.taskName
		}
		return left.leaseID < right.leaseID
	})
	return snapshots[0]
}

func (r *recoverySettlementRegistry) publishSnapshot(snapshot recoverySettlementSnapshot) {
	if r.publish != nil {
		r.publish(Event{Type: recoverySettlementEventType, Body: snapshot.eventBody()})
	}
}

func (l *recoverySettlementLease) markCommitted() {
	if l == nil || l.registry == nil {
		return
	}
	r := l.registry
	r.mu.Lock()
	snapshot, ok := r.leases[l.id]
	if !ok || snapshot.phase != recoverySettlementPhaseAdmitted {
		r.mu.Unlock()
		return
	}
	snapshot.phase = recoverySettlementPhaseCommitted
	r.leases[l.id] = snapshot
	r.signalLocked()
	r.mu.Unlock()
	r.publishSnapshot(snapshot)
}

func (l *recoverySettlementLease) committed() bool {
	if l == nil || l.registry == nil {
		return false
	}
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, ok := r.leases[l.id]
	return ok && snapshot.phase == recoverySettlementPhaseCommitted
}

func (l *recoverySettlementLease) complete() {
	if l == nil || l.registry == nil {
		return
	}
	r := l.registry
	r.mu.Lock()
	snapshot, ok := r.leases[l.id]
	if !ok {
		r.mu.Unlock()
		return
	}
	wasCommitted := snapshot.phase == recoverySettlementPhaseCommitted
	if wasCommitted {
		snapshot.phase = recoverySettlementPhaseSettled
	} else {
		snapshot.phase = recoverySettlementPhaseNotCommitted
	}
	delete(r.leases, l.id)
	r.signalLocked()
	r.mu.Unlock()
	if wasCommitted {
		r.publishSnapshot(snapshot)
	}
}
