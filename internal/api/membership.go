// Package api — weekly_refresh membership service. Memo D5.
//
// PUT /api/daemons/weekly-refresh-membership accepts a structured array
// body and applies it as an idempotent partial update against
// workspaces.yaml. Entries listed in the body are updated to the given
// enabled value; entries NOT in the body are unchanged. One registryMu
// acquire; one Registry.Save call.
package api

import (
	"fmt"

	"github.com/gofrs/flock"
)

// MembershipDelta is one (workspace_key, language) toggle. Memo D5.
type MembershipDelta struct {
	WorkspaceKey string `json:"workspace_key"`
	Language     string `json:"language"`
	Enabled      bool   `json:"enabled"`
}

// UpdateWeeklyRefreshMembership applies the deltas atomically against
// the registry at path. Returns the number of entries actually updated
// (may be less than len(deltas) if a delta's enabled value already
// matches), or an error if any delta names an unknown (key, language)
// pair (in which case the registry is NOT modified — fail-closed).
//
// Memo D5 contract:
//   - Idempotent partial update: entries not in deltas are unchanged.
//   - Atomic: one Registry.Save; either all named deltas persist or none.
//   - Validation: every (workspace_key, language) MUST exist in the
//     registry; otherwise return an error and skip the save.
func UpdateWeeklyRefreshMembership(path string, deltas []MembershipDelta) (int, error) {
	if len(deltas) == 0 {
		return 0, nil
	}

	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return 0, fmt.Errorf("acquire lock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	reg := NewRegistry(path)
	if err := reg.Load(); err != nil {
		return 0, fmt.Errorf("load registry: %w", err)
	}

	idx := make(map[[2]string]int, len(reg.Workspaces))
	for i, e := range reg.Workspaces {
		idx[[2]string{e.WorkspaceKey, e.Language}] = i
	}

	for _, d := range deltas {
		if _, ok := idx[[2]string{d.WorkspaceKey, d.Language}]; !ok {
			return 0, fmt.Errorf("unknown (workspace_key=%q, language=%q)", d.WorkspaceKey, d.Language)
		}
	}

	updated := 0
	for _, d := range deltas {
		i := idx[[2]string{d.WorkspaceKey, d.Language}]
		if reg.Workspaces[i].WeeklyRefresh != d.Enabled {
			reg.Workspaces[i].WeeklyRefresh = d.Enabled
			updated++
		}
	}

	if updated == 0 {
		return 0, nil
	}
	if err := reg.Save(); err != nil {
		return 0, fmt.Errorf("save registry: %w", err)
	}
	return updated, nil
}
