//go:build windows

package main

import (
	"encoding/json"
	"fmt"
)

const fixtureName = "scan-readiness-fixture"
const expectedFailure = "MCP_READINESS_HOST_UNAVAILABLE"

type failure struct {
	Stage     string `json:"stage"`
	ID        string `json:"failure_id"`
	Retryable bool   `json:"retryable"`
}

type daemonRow struct {
	Server  string   `json:"server"`
	Daemon  string   `json:"daemon"`
	Task    string   `json:"task_name"`
	State   string   `json:"state"`
	PID     int      `json:"pid"`
	Port    int      `json:"port"`
	Service string   `json:"service_state"`
	Stage   string   `json:"readiness_stage"`
	Settled bool     `json:"readiness_settled"`
	Failure *failure `json:"readiness_failure,omitempty"`
}

type scanDaemon struct {
	Task       string   `json:"task_name"`
	PID        int      `json:"pid"`
	Port       int      `json:"port"`
	Generation uint64   `json:"pid_generation"`
	Service    string   `json:"service_state"`
	Stage      string   `json:"readiness_stage"`
	Settled    bool     `json:"readiness_settled"`
	Failure    *failure `json:"failure,omitempty"`
}

type pair struct {
	Task       string `json:"task"`
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
	Generation uint64 `json:"generation"`
	StartID    string `json:"os_start_id"`
}

func parsePair(statusJSON, scanJSON []byte, recovered bool) (pair, error) {
	var statuses []daemonRow
	if err := json.Unmarshal(statusJSON, &statuses); err != nil {
		return pair{}, fmt.Errorf("plain status JSON: %w", err)
	}
	var scan struct {
		Entries []struct {
			Name            string `json:"name"`
			Materialization string `json:"materialization_state"`
			Readiness       *struct {
				Settled bool         `json:"settled"`
				Daemons []scanDaemon `json:"daemons"`
				Primary *failure     `json:"primary_failure"`
			} `json:"readiness"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(scanJSON, &scan); err != nil {
		return pair{}, fmt.Errorf("plain scan JSON: %w", err)
	}
	var selected *daemonRow
	for i := range statuses {
		if statuses[i].Server == fixtureName && statuses[i].Daemon == "default" {
			if selected != nil {
				return pair{}, fmt.Errorf("duplicate fixture status rows")
			}
			selected = &statuses[i]
		}
	}
	if selected == nil || selected.Task == "" || selected.PID <= 0 || selected.Port <= 0 {
		return pair{}, fmt.Errorf("missing or incomplete fixture status row")
	}
	var entry *struct {
		Name            string `json:"name"`
		Materialization string `json:"materialization_state"`
		Readiness       *struct {
			Settled bool         `json:"settled"`
			Daemons []scanDaemon `json:"daemons"`
			Primary *failure     `json:"primary_failure"`
		} `json:"readiness"`
	}
	for i := range scan.Entries {
		if scan.Entries[i].Name == fixtureName {
			if entry != nil {
				return pair{}, fmt.Errorf("duplicate fixture scan entries")
			}
			entry = &scan.Entries[i]
		}
	}
	if entry == nil || entry.Readiness == nil || len(entry.Readiness.Daemons) != 1 {
		return pair{}, fmt.Errorf("missing or ambiguous fixture scan readiness")
	}
	daemon := entry.Readiness.Daemons[0]
	if daemon.Task != selected.Task || daemon.PID != selected.PID || daemon.Port != selected.Port || daemon.Generation == 0 {
		return pair{}, fmt.Errorf("status/scan fixture identity mismatch")
	}
	if !selected.Settled || !entry.Readiness.Settled || !daemon.Settled {
		return pair{}, fmt.Errorf("fixture readiness not settled")
	}
	if recovered {
		if selected.Service != "Running" || daemon.Service != "Running" || selected.Stage != "complete" || daemon.Stage != "complete" || entry.Materialization != "ready" || selected.Failure != nil || daemon.Failure != nil || entry.Readiness.Primary != nil {
			return pair{}, fmt.Errorf("fixture has not recovered")
		}
	} else {
		if selected.Service != "Degraded" || daemon.Service != "Degraded" || selected.Stage != "mcp_initialize" || daemon.Stage != "mcp_initialize" || entry.Materialization != "degraded" || !isExpectedFailure(selected.Failure) || !isExpectedFailure(daemon.Failure) || !isExpectedFailure(entry.Readiness.Primary) {
			return pair{}, fmt.Errorf("fixture lacks settled retryable initialize failure")
		}
	}
	return pair{Task: selected.Task, PID: selected.PID, Port: selected.Port, Generation: daemon.Generation}, nil
}

func isExpectedFailure(f *failure) bool { return f != nil && f.ID == expectedFailure && f.Retryable }

func sameGeneration(failed, recovered pair) error {
	if failed.Task == "" || failed.PID <= 0 || failed.Port <= 0 || failed.Generation == 0 || failed.StartID == "" || failed != recovered {
		return fmt.Errorf("fixture task/PID/port/generation/OS start identity changed")
	}
	return nil
}

func focusStatus(raw []byte) ([]byte, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	var found *daemonRow
	for _, row := range rows {
		var decoded daemonRow
		if err := json.Unmarshal(row, &decoded); err != nil {
			return nil, err
		}
		if decoded.Server == fixtureName && decoded.Daemon == "default" {
			if found != nil {
				return nil, fmt.Errorf("duplicate fixture status rows")
			}
			found = &decoded
		}
	}
	if found == nil {
		return nil, fmt.Errorf("fixture status row missing")
	}
	return json.Marshal(found)
}

type focusedReadiness struct {
	Settled bool         `json:"settled"`
	Daemons []scanDaemon `json:"daemons"`
	Primary *failure     `json:"primary_failure,omitempty"`
}
type focusedScanEntry struct {
	Name            string           `json:"name"`
	ManifestExists  bool             `json:"manifest_exists"`
	Classification  string           `json:"classification,omitempty"`
	Materialization string           `json:"materialization_state"`
	Readiness       focusedReadiness `json:"readiness"`
}

func focusScan(raw []byte) ([]byte, error) {
	var scan struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &scan); err != nil {
		return nil, err
	}
	var found *focusedScanEntry
	for _, entry := range scan.Entries {
		var decoded struct {
			Name            string           `json:"name"`
			ManifestExists  bool             `json:"manifest_exists"`
			Classification  string           `json:"classification"`
			Materialization string           `json:"materialization_state"`
			Readiness       focusedReadiness `json:"readiness"`
		}
		if err := json.Unmarshal(entry, &decoded); err != nil {
			return nil, err
		}
		if decoded.Name == fixtureName {
			if found != nil {
				return nil, fmt.Errorf("duplicate fixture scan entries")
			}
			found = &focusedScanEntry{decoded.Name, decoded.ManifestExists, decoded.Classification, decoded.Materialization, decoded.Readiness}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("fixture scan entry missing")
	}
	return json.Marshal(found)
}
