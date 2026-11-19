//go:build windows && r17_public_scheduler_probe

// Read-only public-API probe for the externally verified #660 source snapshot.
// Source contracts at 1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3:
// https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler.go
// https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_windows.go
// Build this standalone file inside that verified repository; do not run tests.
// Slice nilness and returned context error identities are observations, not gates.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"mcp-local-hub/internal/scheduler"
)

const targetHead = "1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3"

func main() {
	started := time.Now()
	passed := false
	defer func() {
		panicked := recover() != nil // Do not expose panic values or stacks.
		status := "FAIL"
		if passed && !panicked {
			status = "PASS"
		}
		emit(map[string]any{
			"event": "overall", "status": status, "pass": passed && !panicked,
			"duration_ns": time.Since(started).Nanoseconds(), "panic_recovered": panicked,
		})
		if !passed || panicked {
			os.Exit(1)
		}
	}()
	passed = probe()
}

func probe() bool {
	started := begin("preconditions")
	override := os.Getenv("MCPHUB_E2E_SCHEDULER")
	none := strings.EqualFold(strings.TrimSpace(override), "none")
	if !finish("preconditions", started, nil, runtime.GOOS == "windows" && !none, map[string]any{
		"target_head": targetHead, "windows": runtime.GOOS == "windows",
		"scheduler_override_empty": override == "", "scheduler_override_none": none,
	}) {
		return false
	}

	started = begin("new_windows_backend")
	s, err := scheduler.New()
	backend := fmt.Sprintf("%T", s)
	lister, contextLister := s.(scheduler.ContextLister)
	if !finish("new_windows_backend", started, err, err == nil && backend == "*scheduler.windowsScheduler" && contextLister, map[string]any{
		"backend_type": backend, "context_lister": contextLister,
	}) {
		return false
	}

	started = begin("random_prefix")
	var nonce [16]byte
	n, err := rand.Read(nonce[:])
	if !finish("random_prefix", started, err, err == nil && n == len(nonce), map[string]any{"complete": n == len(nonce)}) {
		return false
	}
	prefix := "mcp-r17-public-go-" + hex.EncodeToString(nonce[:]) + "-"
	name := `\` + prefix + "absent"

	started = begin("status_absent")
	status, err := s.Status(name)
	zeroStatus := status == (scheduler.TaskStatus{})
	if !finish("status_absent", started, err, errors.Is(err, scheduler.ErrTaskNotFound) && zeroStatus, map[string]any{"zero_result": zeroStatus}) {
		return false
	}

	started = begin("export_absent")
	xml, err := s.ExportXML(name)
	if !finish("export_absent", started, err, errors.Is(err, scheduler.ErrTaskNotFound) && len(xml) == 0, map[string]any{
		"zero_result": len(xml) == 0, "slice_nil": xml == nil,
	}) {
		return false
	}

	started = begin("list_absent_prefix")
	rows, err := s.List(prefix)
	if !finish("list_absent_prefix", started, err, err == nil && len(rows) == 0, map[string]any{
		"zero_result": len(rows) == 0, "slice_nil": rows == nil,
	}) {
		return false
	}

	started = begin("list_context_nil")
	rows, err = lister.ListContext(nil, prefix)
	if !finish("list_context_nil", started, err, err == nil && len(rows) == 0, map[string]any{
		"zero_result": len(rows) == 0, "slice_nil": rows == nil,
	}) {
		return false
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	for _, tc := range []struct {
		check string
		ctx   context.Context
		cause error
	}{
		{"list_pre_canceled", canceled, context.Canceled},
		{"list_pre_expired", expired, context.DeadlineExceeded},
	} {
		started = begin(tc.check)
		inputReady := errors.Is(tc.ctx.Err(), tc.cause)
		if !inputReady {
			return finish(tc.check, started, nil, false, map[string]any{"input_context_ready": false, "called": false})
		}
		rows, err = lister.ListContext(tc.ctx, prefix)
		if !finish(tc.check, started, err, err != nil && len(rows) == 0, map[string]any{
			"input_context_ready": true, "called": true, "zero_result": len(rows) == 0, "slice_nil": rows == nil,
		}) {
			return false
		}
	}
	return true
}

func begin(check string) time.Time {
	emit(map[string]any{"event": "begin", "check": check})
	return time.Now()
}

func finish(check string, started time.Time, err error, passed bool, observations map[string]any) bool {
	emit(map[string]any{
		"event": "result", "check": check, "pass": passed,
		"duration_ns": time.Since(started).Nanoseconds(), "error_present": err != nil,
		"is_task_not_found":            errors.Is(err, scheduler.ErrTaskNotFound),
		"is_unavailable":               errors.Is(err, scheduler.ErrUnavailable),
		"is_context_canceled":          errors.Is(err, context.Canceled),
		"is_context_deadline_exceeded": errors.Is(err, context.DeadlineExceeded),
		"observations":                 observations,
	})
	return passed
}

func emit(value any) {
	if json.NewEncoder(os.Stdout).Encode(value) != nil {
		os.Exit(2) // Broken evidence stream: no raw error and no invented final PASS.
	}
}
