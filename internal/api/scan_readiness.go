package api

import (
	"context"
	"time"

	"mcp-local-hub/internal/clients"
)

func (a *API) applyReadinessToScanEntries(entries []ScanEntry, rows []DaemonStatus) error {
	for i := range entries {
		bindings := make([]BindingObservationV1, 0, len(entries[i].ClientPresence))
		for client, entry := range entries[i].ClientPresence {
			bindings = append(bindings, BindingObservationV1{Client: client, Present: true, Readable: true, Enabled: !entry.Disabled, Disabled: entry.Disabled, Route: scanBindingRoute(entry), ExactHubRoute: entry.Transport == "http" && clients.IsHubHTTPURL(entry.Endpoint)})
		}
		selected := make([]DaemonStatus, 0)
		for _, row := range rows {
			if row.Server == entries[i].Name {
				selected = append(selected, row)
			}
		}
		snapshot, err := a.AssessReadinessFromStatusRowsWithOptions(context.Background(), selected, bindings, ReadinessStatusRowsOptionsV1{
			CanMigrate: entries[i].CanMigrate,
			Now:        time.Now().UTC(),
		})
		if err := statusReadinessError(err); err != nil {
			return err
		}
		entries[i].Classification = snapshot.Classification
		entries[i].MaterializationState = snapshot.MaterializationState
		entries[i].BindingState = snapshot.BindingState
		entries[i].Readiness = &snapshot
	}
	return nil
}

func scanBindingRoute(entry ClientEntry) BindingRouteV1 {
	if entry.Disabled {
		return BindingRouteNone
	}
	if entry.Transport == "http" && clients.IsHubHTTPURL(entry.Endpoint) {
		return BindingRouteHub
	}
	if entry.Transport == "stdio" || entry.Transport == "relay" {
		return BindingRouteDirect
	}
	return BindingRouteNone
}
