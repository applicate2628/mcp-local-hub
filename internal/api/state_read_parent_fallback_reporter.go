package api

import (
	"context"
	"errors"
	"sort"
	"time"

	"golang.org/x/sync/semaphore"
)

const (
	stateReadUnhardenedParentFallbackEvent = "hub-mcp-state-read-unhardened-parent-fallback"

	stateReadFallbackAggregationField     = "fallback_aggregation"
	stateReadFallbackSuppressedCountField = "suppressed_count"

	stateReadFallbackAggregationPeriodic   = "periodic"
	stateReadFallbackAggregationTransition = "transition"
	stateReadFallbackAggregationSettled    = "settled"
	stateReadFallbackAggregationEvicted    = "evicted"

	stateReadFallbackReportInterval = 15 * time.Minute
	stateReadFallbackReporterCap    = 256
)

// stateReadParentFallbackReporter condenses repeated default-sink parent-DACL
// relax warnings for one process lifetime. It is deliberately not persisted:
// every process start emits a fresh first observation. Caller-owned audit
// sinks bypass it and retain their per-call diagnostic contract.
//
// reportGate serializes the cached state and corresponding best-effort
// writes. A bound read can cancel its wait for ordinary log I/O.
type stateReadParentFallbackReporter struct {
	reportGate *semaphore.Weighted

	now   func() time.Time
	sink  stateReadFallbackSink
	limit int
	seq   uint64

	entries map[stateReadFallbackKey]*stateReadParentFallbackEntry
}

type stateReadFallbackKey struct {
	namespaceSHA256 string // Empty only for the ordinary sink.
	path            string
	fingerprint     string
}

type stateReadParentFallbackEntry struct {
	key         stateReadFallbackKey
	level       string
	event       string
	fields      map[string]any
	lastEmitted time.Time
	suppressed  int
	touched     uint64
}

type stateReadFallbackSink func(level, event string, fields map[string]any) error

type stateReadParentFallbackObservation struct {
	level  string
	event  string
	fields map[string]any
}

func newStateReadParentFallbackReporter(now func() time.Time, sink stateReadFallbackSink, limit int) *stateReadParentFallbackReporter {
	if now == nil {
		now = time.Now
	}
	if sink == nil {
		sink = LogHubMcpEvent
	}
	if limit <= 0 {
		limit = stateReadFallbackReporterCap
	}
	return &stateReadParentFallbackReporter{
		reportGate: semaphore.NewWeighted(1),
		now:        now, sink: sink, limit: limit, entries: make(map[stateReadFallbackKey]*stateReadParentFallbackEntry),
	}
}

var defaultStateReadParentFallbackReporter = newStateReadParentFallbackReporter(time.Now, LogHubMcpEvent, stateReadFallbackReporterCap)

func stateReadDefaultParentFallbacksObserved(path string, observations []stateReadParentFallbackObservation) {
	_ = defaultStateReadParentFallbackReporter.observeParentFallbacks(path, observations)
}

func (r *stateReadParentFallbackReporter) observeFallback(level, event string, fields map[string]any) error {
	path, _, ok := stateReadParentFallbackIdentity(fields)
	if !ok {
		return r.sink(level, event, fields)
	}
	return r.observeParentFallbacks(path, []stateReadParentFallbackObservation{{level: level, event: event, fields: fields}})
}

func (r *stateReadParentFallbackReporter) observeClean(path string) error {
	return r.observeParentFallbacks(path, nil)
}

// observeParentFallbacks receives every parent-DACL fallback found during one
// inode-anchored read. A POSIX 0777 parent legitimately yields both a write and
// a read/exec reason; keeping that set together prevents one reason from being
// misclassified as the other's transition.
func (r *stateReadParentFallbackReporter) observeParentFallbacks(path string, observations []stateReadParentFallbackObservation) error {
	return r.observeParentFallbacksWithSink(context.Background(), "", r.sink, path, observations)
}

func (r *stateReadParentFallbackReporter) observeParentFallbacksWithSink(ctx context.Context, namespaceSHA256 string, sink stateReadFallbackSink, path string, observations []stateReadParentFallbackObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	emit := func(level, event string, fields map[string]any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sink(level, event, fields)
	}
	desired := make(map[stateReadFallbackKey]stateReadParentFallbackObservation, len(observations))
	ordered := make([]stateReadFallbackKey, 0, len(observations))
	for _, observation := range observations {
		observedPath, fingerprint, ok := stateReadParentFallbackIdentity(observation.fields)
		if !ok || observedPath != path {
			if namespaceSHA256 != "" {
				return errors.New("state read fallback does not match bound observation path")
			}
			if err := emit(observation.level, observation.event, observation.fields); err != nil {
				return err
			}
			continue
		}
		key := stateReadFallbackKey{namespaceSHA256: namespaceSHA256, path: observedPath, fingerprint: fingerprint}
		if _, exists := desired[key]; !exists {
			desired[key] = observation
			ordered = append(ordered, key)
		}
	}
	now := r.now().UTC()

	if err := r.reportGate.Acquire(ctx, 1); err != nil {
		return err
	}
	defer r.reportGate.Release(1)

	prior := r.entriesForPath(namespaceSHA256, path)
	finalAggregation := stateReadFallbackAggregationTransition
	if len(desired) == 0 {
		finalAggregation = stateReadFallbackAggregationSettled
	}
	for _, entry := range prior {
		if _, stillObserved := desired[entry.key]; stillObserved {
			continue
		}
		if err := r.emitAndRemove(emit, entry, finalAggregation); err != nil {
			return err
		}
	}
	for _, key := range ordered {
		if err := r.observeOne(ctx, emit, key, desired[key], now); err != nil {
			return err
		}
	}
	return nil
}

func (r *stateReadParentFallbackReporter) observeOne(ctx context.Context, emit stateReadFallbackSink, key stateReadFallbackKey, observation stateReadParentFallbackObservation, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entry := r.entries[key]
	if entry == nil {
		full, victim := r.capacityForDomain(key.namespaceSHA256)
		if full && victim == nil {
			// The bound pool is full of other roots. Emit under the current
			// authority and leave their cached warnings for their own sinks.
			return emit(observation.level, observation.event, observation.fields)
		}
		if full {
			if err := r.emitAndRemove(emit, victim, stateReadFallbackAggregationEvicted); err != nil {
				return err
			}
		}
		entry = &stateReadParentFallbackEntry{
			key: key, level: observation.level, event: observation.event, fields: cloneStateReadFallbackFields(observation.fields),
			lastEmitted: now,
		}
		if err := emit(observation.level, observation.event, observation.fields); err != nil {
			return err
		}
		r.seq++
		entry.touched = r.seq
		r.entries[key] = entry
		return nil
	}

	suppressed := entry.suppressed + 1
	var err error
	if now.Sub(entry.lastEmitted) >= stateReadFallbackReportInterval {
		periodic := stateReadFallbackAggregateFields(entry.fields, suppressed, stateReadFallbackAggregationPeriodic)
		err = emit(entry.level, entry.event, periodic)
		if err != nil && ctx.Err() != nil {
			return err
		}
		if err == nil {
			entry.lastEmitted = now
			suppressed = 0
		}
	}
	r.seq++
	entry.touched = r.seq
	entry.suppressed = suppressed
	return err
}

func (r *stateReadParentFallbackReporter) emitAndRemove(emit stateReadFallbackSink, entry *stateReadParentFallbackEntry, aggregation string) error {
	fields := stateReadFallbackAggregateFields(entry.fields, entry.suppressed, aggregation)
	if err := emit(entry.level, entry.event, fields); err != nil {
		return err
	}
	delete(r.entries, entry.key)
	return nil
}

// The ordinary domain has its own capacity. All bound domains share one
// additional capacity, and an eviction must stay within the current domain.
func (r *stateReadParentFallbackReporter) capacityForDomain(namespaceSHA256 string) (full bool, victim *stateReadParentFallbackEntry) {
	if len(r.entries) < r.limit {
		return false, nil
	}
	count := 0
	for key, entry := range r.entries {
		if (key.namespaceSHA256 == "") != (namespaceSHA256 == "") {
			continue
		}
		count++
		if key.namespaceSHA256 == namespaceSHA256 && (victim == nil || entry.touched < victim.touched) {
			victim = entry
		}
	}
	return count >= r.limit, victim
}

func (r *stateReadParentFallbackReporter) entriesForPath(namespaceSHA256, path string) []*stateReadParentFallbackEntry {
	entries := make([]*stateReadParentFallbackEntry, 0)
	for key, entry := range r.entries {
		if key.namespaceSHA256 == namespaceSHA256 && key.path == path {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].touched != entries[j].touched {
			return entries[i].touched < entries[j].touched
		}
		return entries[i].key.fingerprint < entries[j].key.fingerprint
	})
	return entries
}

func stateReadParentFallbackIdentity(fields map[string]any) (path, fingerprint string, ok bool) {
	path = stateReadFallbackPath(fields)
	reason, reasonOK := fields["reason"].(string)
	security, securityOK := fields["err"].(string)
	if !securityOK {
		security, securityOK = fields["parent_mode"].(string)
	}
	if path == "" || !reasonOK || !securityOK {
		return "", "", false
	}
	return path, reason + "\x00" + security, true
}

func stateReadFallbackPath(fields map[string]any) string {
	path, _ := fields["path"].(string)
	return path
}

func stateReadFallbackAggregateFields(fields map[string]any, suppressed int, aggregation string) map[string]any {
	out := cloneStateReadFallbackFields(fields)
	out[stateReadFallbackAggregationField] = aggregation
	out[stateReadFallbackSuppressedCountField] = suppressed
	return out
}

func cloneStateReadFallbackFields(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		out[key] = value
	}
	return out
}
