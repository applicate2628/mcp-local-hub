package api

import "testing"

func stateReadParentFallbackRows(t *testing.T, target string) []map[string]any {
	t.Helper()
	events, err := RecentHubMcpEvents(20)
	if err != nil {
		t.Fatalf("recent hub-mcp events: %v", err)
	}
	var rows []map[string]any
	for _, event := range events {
		if event["event"] == stateReadUnhardenedParentFallbackEvent && event["path"] == target {
			rows = append(rows, event)
		}
	}
	return rows
}
