package api

import "sync"

// DiskShadowWarningTracker keeps notification state for one long-lived caller.
// It is intentionally in-memory only: a new GUI lifetime starts fresh, while
// one-shot CLI and install operations retain their existing warnings.
type DiskShadowWarningTracker struct {
	mu     sync.Mutex
	active map[string][32]byte
}

func NewDiskShadowWarningTracker() *DiskShadowWarningTracker {
	return &DiskShadowWarningTracker{active: make(map[string][32]byte)}
}

// Complete applies one successful complete enumeration. It returns only new
// or changed observations, forgets absent observations without emitting, and
// is safe for concurrent scans. Enumeration failures must not call Complete.
func (t *DiskShadowWarningTracker) Complete(observations []DiskShadowWarningObservation) []DiskShadowWarningObservation {
	if t == nil {
		return observations
	}
	current := make(map[string][32]byte, len(observations))
	emitted := make([]DiskShadowWarningObservation, 0, len(observations))
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, observation := range observations {
		current[observation.Name] = observation.Identity
		if previous, seen := t.active[observation.Name]; !seen || previous != observation.Identity {
			emitted = append(emitted, observation)
		}
	}
	t.active = current
	return emitted
}
