package cli

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// spawnShortLivedChild starts a child process that exits in ~100ms.
// Returns the *exec.Cmd so the test can capture the PID and Wait on
// reaping at the end. Cross-platform: PowerShell `Start-Sleep` on
// Windows; `sleep` on POSIX.
//
// 100ms is short enough that the 5-second Drain test deadline has
// margin, long enough that the child is observable as alive at the
// start of Drain (50ms initial probe granularity).
func spawnShortLivedChild(t *testing.T) *exec.Cmd {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// PowerShell takes ~100-150ms to spawn; Start-Sleep 0.2 gives
		// the test a bounded but observable window.
		cmd = exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep -Milliseconds 200")
	} else {
		cmd = exec.Command("sleep", "0.2")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn short-lived child: %v", err)
	}
	return cmd
}

// spawnLongLivedChild starts a child process that runs for ~60s.
// The caller is responsible for killing it at the end of the test.
func spawnLongLivedChild(t *testing.T) *exec.Cmd {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep -Seconds 60")
	} else {
		cmd = exec.Command("sleep", "60")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn long-lived child: %v", err)
	}
	return cmd
}

// TestQuiesceHandler_EmptyStateReturnsImmediately verifies the
// fast-path: when no TransientPIDs are present, Drain returns with
// drained=0, still_running=[] regardless of the timeout.
func TestQuiesceHandler_EmptyStateReturnsImmediately(t *testing.T) {
	state := &api.SupervisorStateFile{
		Version:       1,
		Daemons:       map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{},
	}
	handler := NewQuiesceHandler(state, "")
	if handler.InProgress() {
		t.Fatal("InProgress should be false before Drain")
	}

	start := time.Now()
	result := handler.Drain(context.Background(), 5000)
	elapsed := time.Since(start)

	if handler.InProgress() {
		t.Fatal("InProgress should be false after Drain returns")
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("empty-state Drain took %v; expected near-instant", elapsed)
	}
	if result.Drained != 0 {
		t.Fatalf("expected drained=0, got %d", result.Drained)
	}
	if len(result.StillRunning) != 0 {
		t.Fatalf("expected still_running empty, got %v", result.StillRunning)
	}
}

// TestQuiesceHandler_NilStateReturnsImmediately verifies the
// defensive nil-guard: Drain on a nil state returns drained=0
// without panicking.
func TestQuiesceHandler_NilStateReturnsImmediately(t *testing.T) {
	handler := NewQuiesceHandler(nil, "")
	result := handler.Drain(context.Background(), 5000)
	if result.Drained != 0 {
		t.Fatalf("nil state: expected drained=0, got %d", result.Drained)
	}
	if len(result.StillRunning) != 0 {
		t.Fatalf("nil state: expected still_running empty, got %v", result.StillRunning)
	}
}

// TestQuiesceHandler_DrainsTransients spawns a real short-lived
// child, registers it as a transient PID, then verifies that Drain
// observes its exit within the timeout and reports drained=1.
func TestQuiesceHandler_DrainsTransients(t *testing.T) {
	cmd := spawnShortLivedChild(t)
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{
			{
				PID:       cmd.Process.Pid,
				Kind:      "workspace-weekly-refresh",
				StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	handler := NewQuiesceHandler(state, "")
	if handler.InProgress() {
		t.Fatal("InProgress should be false before Drain")
	}

	// 5-second timeout — child should exit in ~200ms.
	result := handler.Drain(context.Background(), 5000)

	if handler.InProgress() {
		t.Fatal("InProgress should be false after Drain returns")
	}
	if result.Drained != 1 {
		t.Fatalf("expected drained=1, got %d (still_running=%v)", result.Drained, result.StillRunning)
	}
	if len(result.StillRunning) != 0 {
		t.Fatalf("expected still_running empty, got %v", result.StillRunning)
	}

	_ = cmd.Wait() // reap.
}

// TestQuiesceHandler_TimeoutWithStillRunning spawns a long-lived
// child, registers it as a transient PID, then verifies that Drain
// times out with the PID still in still_running.
func TestQuiesceHandler_TimeoutWithStillRunning(t *testing.T) {
	cmd := spawnLongLivedChild(t)
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	pid := cmd.Process.Pid

	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{
			{
				PID:       pid,
				Kind:      "workspace-weekly-refresh",
				StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	handler := NewQuiesceHandler(state, "")

	// 300ms timeout — child runs for 60s, so it should still be alive
	// when the deadline expires.
	start := time.Now()
	result := handler.Drain(context.Background(), 300)
	elapsed := time.Since(start)

	// Verify the deadline was actually approached (sanity check —
	// catches a bug where Drain returns early).
	if elapsed < 250*time.Millisecond {
		t.Fatalf("Drain returned in %v; expected near 300ms timeout", elapsed)
	}
	if result.Drained != 0 {
		t.Fatalf("long-lived child should not be 'drained', got drained=%d", result.Drained)
	}
	if len(result.StillRunning) != 1 || result.StillRunning[0] != pid {
		t.Fatalf("expected still_running=[%d], got %v", pid, result.StillRunning)
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// TestQuiesceHandler_ContextCancellation verifies that Drain returns
// promptly when its context is cancelled, even if the deadline
// hasn't expired.
func TestQuiesceHandler_ContextCancellation(t *testing.T) {
	cmd := spawnLongLivedChild(t)
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	pid := cmd.Process.Pid

	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{
			{PID: pid, Kind: "test", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		},
	}
	handler := NewQuiesceHandler(state, "")

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 100ms.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result := handler.Drain(ctx, 30_000) // 30s timeout — cancel should win.
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel: Drain took %v; expected ~100ms", elapsed)
	}
	// Long-lived child still running — should be in still_running.
	if len(result.StillRunning) != 1 || result.StillRunning[0] != pid {
		t.Fatalf("ctx cancel: expected still_running=[%d], got %v", pid, result.StillRunning)
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// TestQuiesceHandler_InProgressFlag verifies that InProgress is
// true during the Drain call and false before/after. The FIFO event
// loop reads this flag to suppress new timer fires per spec
// §"Graceful exit + quiesce drain" step 1.
func TestQuiesceHandler_InProgressFlag(t *testing.T) {
	cmd := spawnLongLivedChild(t)
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	pid := cmd.Process.Pid

	state := &api.SupervisorStateFile{
		Version: 1,
		TransientPIDs: []api.TransientPID{
			{PID: pid, Kind: "test", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		},
	}
	handler := NewQuiesceHandler(state, "")

	if handler.InProgress() {
		t.Fatal("InProgress should be false before Drain")
	}

	// Run Drain in a goroutine; sample InProgress while it's running.
	doneCh := make(chan struct{})
	go func() {
		handler.Drain(context.Background(), 500)
		close(doneCh)
	}()

	// Give the goroutine a beat to enter the Drain body.
	time.Sleep(50 * time.Millisecond)
	if !handler.InProgress() {
		t.Fatal("InProgress should be true during Drain")
	}

	<-doneCh
	if handler.InProgress() {
		t.Fatal("InProgress should be false after Drain returns")
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// TestQuiesceHandler_MixedDrainedAndStillRunning verifies the
// drained count math when SOME transients exit and OTHERS don't:
// drained = (initial count) - (still_alive at deadline).
func TestQuiesceHandler_MixedDrainedAndStillRunning(t *testing.T) {
	shortChild := spawnShortLivedChild(t)
	longChild := spawnLongLivedChild(t)
	defer func() {
		if shortChild.Process != nil {
			_ = shortChild.Process.Kill()
		}
		if longChild.Process != nil {
			_ = longChild.Process.Kill()
		}
	}()

	state := &api.SupervisorStateFile{
		Version: 1,
		TransientPIDs: []api.TransientPID{
			{PID: shortChild.Process.Pid, Kind: "test-short", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)},
			{PID: longChild.Process.Pid, Kind: "test-long", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		},
	}
	handler := NewQuiesceHandler(state, "")

	// 2-second timeout — short child exits in ~200ms, long child
	// keeps running.
	result := handler.Drain(context.Background(), 2000)

	if result.Drained != 1 {
		t.Fatalf("expected drained=1 (short child), got %d (still_running=%v)", result.Drained, result.StillRunning)
	}
	if len(result.StillRunning) != 1 || result.StillRunning[0] != longChild.Process.Pid {
		t.Fatalf("expected still_running=[%d] (long child), got %v", longChild.Process.Pid, result.StillRunning)
	}

	_ = shortChild.Wait() // already exited.
	_ = longChild.Process.Kill()
	_ = longChild.Wait()
}

// TestQuiesceHandler_DeadPIDsTreatedAsDrained verifies that PIDs
// that are already dead (or were never alive — e.g. PID 0 sentinel)
// at the start of Drain are immediately counted as drained.
func TestQuiesceHandler_DeadPIDsTreatedAsDrained(t *testing.T) {
	state := &api.SupervisorStateFile{
		Version: 1,
		TransientPIDs: []api.TransientPID{
			{PID: 0, Kind: "test-zero", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		},
	}
	handler := NewQuiesceHandler(state, "")

	start := time.Now()
	result := handler.Drain(context.Background(), 5000)
	elapsed := time.Since(start)

	// PID 0 is treated as not-alive by isPIDAlive, so the first probe
	// should fire and exit immediately.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("dead-PIDs Drain took %v; expected near-instant", elapsed)
	}
	if result.Drained != 1 {
		t.Fatalf("expected drained=1, got %d (still_running=%v)", result.Drained, result.StillRunning)
	}
	if len(result.StillRunning) != 0 {
		t.Fatalf("expected still_running empty, got %v", result.StillRunning)
	}
}
