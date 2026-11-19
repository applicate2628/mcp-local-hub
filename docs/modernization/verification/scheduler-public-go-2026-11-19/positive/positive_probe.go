//go:build windows && r17_public_scheduler_positive_probe

// Five positive read-only calls through the public scheduler API on an owned pair.
// The PowerShell owner admits the image, supplies the private manifest, and owns cleanup.
// Source: https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler.go
// Mapping: https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"mcp-local-hub/internal/scheduler"
)

const targetHead = "1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3"

type expectedTask struct {
	Name   string `json:"name"`
	Hidden bool   `json:"hidden"`
	XML    string `json:"xml"`
	Owner  string `json:"owner"`
}

type manifest struct {
	Head   string         `json:"head"`
	Prefix string         `json:"prefix"`
	Tasks  []expectedTask `json:"tasks"`
}

func main() {
	started := time.Now()
	passed := false
	defer func() {
		panicked := recover() != nil
		status := "FAIL"
		if passed && !panicked {
			status = "PASS"
		}
		emit(map[string]any{
			"event": "overall", "status": status, "pass": passed && !panicked,
			"duration_ns": time.Since(started).Nanoseconds(), "panic_recovered": panicked,
			"task_status_subset_only": true, "last_result_semantics_qualified": false,
		})
		if !passed || panicked {
			os.Exit(1)
		}
	}()
	passed = probe()
}

func probe() bool {
	m, valid := readManifest()
	none := strings.EqualFold(strings.TrimSpace(os.Getenv("MCPHUB_E2E_SCHEDULER")), "none")
	if !valid || runtime.GOOS != "windows" || none {
		emit(map[string]any{"event": "admission", "pass": false, "manifest_valid": valid, "windows": runtime.GOOS == "windows", "scheduler_override_none": none})
		return false
	}
	s, err := scheduler.New()
	backend := fmt.Sprintf("%T", s)
	admitted := err == nil && backend == "*scheduler.windowsScheduler"
	emit(map[string]any{"event": "admission", "pass": admitted, "manifest_valid": true, "backend_type": backend, "error_present": err != nil, "target_head": targetHead})
	if !admitted {
		return false
	}

	for index, task := range m.Tasks {
		label := []string{"visible", "hidden"}[index]
		started := begin("status-" + label)
		status, err := s.Status(task.Name)
		observations, matches := inspectStatus(status, task)
		if !finish("status-"+label, started, err, err == nil && matches, observations) {
			return false
		}

		started = begin("export-" + label)
		xml, err := s.ExportXML(task.Name)
		exact := bytes.Equal(xml, []byte(task.XML))
		validUTF8 := utf8.Valid(xml)
		marker := bytes.Contains(xml, []byte("Ж§"))
		if !finish("export-"+label, started, err, err == nil && exact && validUTF8 && marker, map[string]any{
			"xml_exact": exact, "utf8_valid": validUTF8, "unicode_marker_preserved": marker,
		}) {
			return false
		}
	}

	started := begin("list-visible-only")
	rows, err := s.List(m.Prefix)
	observations := map[string]any{"exactly_one": len(rows) == 1}
	matches := false
	if len(rows) == 1 {
		observations, matches = inspectStatus(rows[0], m.Tasks[0])
		observations["exactly_one"] = true
	}
	return finish("list-visible-only", started, err, err == nil && len(rows) == 1 && matches, observations)
}

func readManifest() (manifest, bool) {
	var m manifest
	const limit = 64 * 1024
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, limit+1))
	if err != nil || len(raw) > limit || !utf8.Valid(raw) {
		return m, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&m) != nil {
		return m, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || m.Head != targetHead || len(m.Tasks) != 2 {
		return m, false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(m.Prefix, "mcp-r17-"), "-")
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 || m.Prefix != "mcp-r17-"+id+"-" {
		return m, false
	}
	for index, task := range m.Tasks {
		suffix := []string{"visible", "hidden"}[index]
		if task.Name != `\`+m.Prefix+suffix || task.Hidden != (index == 1) || strings.TrimSpace(task.Owner) == "" || !strings.Contains(task.XML, "Ж§") {
			return m, false
		}
	}
	return m, true
}

func inspectStatus(status scheduler.TaskStatus, task expectedTask) (map[string]any, bool) {
	name := status.Name == task.Name
	owner := status.Owner == task.Owner
	runtimeDisabled := status.RuntimeState == scheduler.TaskRuntimeDisabled
	displayDisabled := status.State == "Disabled"
	observations := map[string]any{"name_exact": name, "owner_exact": owner, "runtime_disabled": runtimeDisabled, "state_disabled": displayDisabled}
	if name && owner {
		// #660 does not populate LastResult in taskStatusFromCOM; never gate on its value.
		observations["last_result_observed"] = status.LastResult
	}
	return observations, name && owner && runtimeDisabled && displayDisabled
}

func begin(check string) time.Time {
	emit(map[string]any{"event": "begin", "check": check})
	return time.Now()
}

func finish(check string, started time.Time, err error, passed bool, observations map[string]any) bool {
	emit(map[string]any{
		"event": "result", "check": check, "pass": passed, "duration_ns": time.Since(started).Nanoseconds(),
		"error_present": err != nil, "is_task_not_found": errors.Is(err, scheduler.ErrTaskNotFound),
		"is_unavailable": errors.Is(err, scheduler.ErrUnavailable), "observations": observations,
	})
	return passed
}

func emit(value any) {
	if json.NewEncoder(os.Stdout).Encode(value) != nil {
		os.Exit(2)
	}
}
