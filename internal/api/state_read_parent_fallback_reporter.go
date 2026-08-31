package api

import (
	"sort"
	"sync"
	"time"
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
// reportMu serializes a decision with its corresponding best-effort write so
// concurrent observations cannot lose suppressed counts. stateMu never covers
// I/O: log rotation and its flock remain outside the reporter state lock.
type stateReadParentFallbackReporter struct {
	reportMu sync.Mutex
	stateMu  sync.Mutex

	now   func() time.Time
	sink  func(level, event string, fields map[string]any) error
	limit int
	seq   uint64

	entries map[string]*stateReadParentFallbackEntry
}

type stateReadParentFallbackEntry struct {
	key         string
	level       string
	event       string
	fields      map[string]any
	lastEmitted time.Time
	suppressed  int
	touched     uint64
}

type stateReadParentFallbackObservation struct {
	level  string
	event  string
	fields map[string]any
}

func newStateReadParentFallbackReporter(now func() time.Time, sink func(level, event string, fields map[string]any) error, limit int) *stateReadParentFallbackReporter {
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
		now: now, sink: sink, limit: limit, entries: make(map[string]*stateReadParentFallbackEntry),
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
	desired := make(map[string]stateReadParentFallbackObservation, len(observations))
	ordered := make([]string, 0, len(observations))
	for _, observation := range observations {
		observedPath, fingerprint, ok := stateReadParentFallbackIdentity(observation.fields)
		if !ok || observedPath != path {
			if err := r.sink(observation.level, observation.event, observation.fields); err != nil {
				return err
			}
			continue
		}
		key := stateReadParentFallbackKey(observedPath, fingerprint)
		if _, exists := desired[key]; !exists {
			desired[key] = observation
			ordered = append(ordered, key)
		}
	}
	now := r.now().UTC()

	r.reportMu.Lock()
	defer r.reportMu.Unlock()

	r.stateMu.Lock()
	prior := r.entriesForPathLocked(path)
	r.stateMu.Unlock()
	finalAggregation := stateReadFallbackAggregationTransition
	if len(desired) == 0 {
		finalAggregation = stateReadFallbackAggregationSettled
	}
	for _, entry := range prior {
		if _, stillObserved := desired[entry.key]; stillObserved {
			continue
		}
		if err := r.emitAndRemove(entry, finalAggregation); err != nil {
			return err
		}
	}
	for _, key := range ordered {
		if err := r.observeOne(key, desired[key], now); err != nil {
			return err
		}
	}
	return nil
}

func (r *stateReadParentFallbackReporter) observeOne(key string, observation stateReadParentFallbackObservation, now time.Time) error {
	r.stateMu.Lock()
	entry := r.entries[key]
	if entry == nil {
		victim := r.leastRecentlyTouchedLocked()
		if victim != nil && len(r.entries) >= r.limit {
			victimFields := stateReadFallbackAggregateFields(victim.fields, victim.suppressed, stateReadFallbackAggregationEvicted)
			r.stateMu.Unlock()
			if err := r.sink(victim.level, victim.event, victimFields); err != nil {
				return err
			}
			r.stateMu.Lock()
			delete(r.entries, victim.key)
		}
		r.seq++
		entry = &stateReadParentFallbackEntry{
			key: key, level: observation.level, event: observation.event, fields: cloneStateReadFallbackFields(observation.fields),
			lastEmitted: now, touched: r.seq,
		}
		r.entries[key] = entry
		r.stateMu.Unlock()
		if err := r.sink(observation.level, observation.event, observation.fields); err != nil {
			r.stateMu.Lock()
			if r.entries[key] == entry {
				delete(r.entries, key)
			}
			r.stateMu.Unlock()
			return err
		}
		return nil
	}

	r.seq++
	entry.touched = r.seq
	entry.suppressed++
	if now.Sub(entry.lastEmitted) < stateReadFallbackReportInterval {
		r.stateMu.Unlock()
		return nil
	}
	periodic := stateReadFallbackAggregateFields(entry.fields, entry.suppressed, stateReadFallbackAggregationPeriodic)
	r.stateMu.Unlock()
	if err := r.sink(entry.level, entry.event, periodic); err != nil {
		return err
	}
	r.stateMu.Lock()
	entry.lastEmitted = now
	entry.suppressed = 0
	r.stateMu.Unlock()
	return nil
}

func (r *stateReadParentFallbackReporter) emitAndRemove(entry *stateReadParentFallbackEntry, aggregation string) error {
	fields := stateReadFallbackAggregateFields(entry.fields, entry.suppressed, aggregation)
	if err := r.sink(entry.level, entry.event, fields); err != nil {
		return err
	}
	r.stateMu.Lock()
	if r.entries[entry.key] == entry {
		delete(r.entries, entry.key)
	}
	r.stateMu.Unlock()
	return nil
}

func (r *stateReadParentFallbackReporter) leastRecentlyTouchedLocked() *stateReadParentFallbackEntry {
	if len(r.entries) < r.limit {
		return nil
	}
	var victim *stateReadParentFallbackEntry
	for _, entry := range r.entries {
		if victim == nil || entry.touched < victim.touched {
			victim = entry
		}
	}
	return victim
}

func (r *stateReadParentFallbackReporter) entriesForPathLocked(path string) []*stateReadParentFallbackEntry {
	entries := make([]*stateReadParentFallbackEntry, 0)
	for _, entry := range r.entries {
		if stateReadFallbackPath(entry.fields) == path {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].touched != entries[j].touched {
			return entries[i].touched < entries[j].touched
		}
		return entries[i].key < entries[j].key
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

func stateReadParentFallbackKey(path, fingerprint string) string {
	return path + "\x00" + fingerprint
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
