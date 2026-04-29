package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProcessID_SelfAlive verifies the current test process is reported
// as alive with a non-empty image path, an argv that includes "test"
// (the Go test binary's name pattern), and a non-zero start time.
func TestProcessID_SelfAlive(t *testing.T) {
	got, err := processID(os.Getpid())
	if err != nil {
		t.Fatalf("processID(self): %v", err)
	}
	if !got.Alive {
		t.Errorf("self.Alive = false, want true")
	}
	if got.Denied {
		t.Errorf("self.Denied = true, want false (we should always be able to query our own process)")
	}
	if got.ImagePath == "" {
		t.Errorf("self.ImagePath empty, want non-empty")
	}
	// Test binary name varies (TestMain.exe, foo.test, etc.) — assert
	// only that we got SOMETHING in argv.
	if len(got.Cmdline) == 0 {
		t.Errorf("self.Cmdline empty, want non-empty")
	}
	if got.StartTime.IsZero() {
		t.Errorf("self.StartTime zero, want non-zero")
	}
}

// TestProcessID_ImpossiblePIDDead verifies a known-impossible PID
// reports alive=false. math.MaxInt32 is far beyond the kernel's PID
// allocation range on every platform we support.
func TestProcessID_ImpossiblePIDDead(t *testing.T) {
	const impossible = 2147483646 // math.MaxInt32 - 1; one off avoids ANY kernel reuse risk
	got, err := processID(impossible)
	// Some platforms return a structured error here; others fill
	// got.Alive=false. Both are acceptable — the contract is
	// "callers can determine the process is not alive".
	if err == nil && got.Alive {
		t.Errorf("processID(impossible) reported alive; got=%+v", got)
	}
}

// TestProcessID_NegativePIDRejected verifies pid <= 0 is treated as
// a dead-PID without invoking platform syscalls (defensive: a negative
// PID through OpenProcess could otherwise crash the test).
func TestProcessID_NegativePIDRejected(t *testing.T) {
	got, _ := processID(0)
	if got.Alive {
		t.Errorf("processID(0).Alive = true, want false")
	}
	got, _ = processID(-1)
	if got.Alive {
		t.Errorf("processID(-1).Alive = true, want false")
	}
}

// TestPingIncumbent_Success spins up an httptest server that mimics
// /api/ping returning {ok:true, pid:<recordedPID>}; PingIncumbent
// must return matchedPID equal to the response body's PID.
func TestPingIncumbent_Success(t *testing.T) {
	const recordedPID = 4128
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": recordedPID})
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)
	got, err := pingIncumbent(context.Background(), port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("pingIncumbent: %v", err)
	}
	if got != recordedPID {
		t.Errorf("matchedPID = %d, want %d", got, recordedPID)
	}
}

// TestPingIncumbent_PortNotListening returns an error promptly when
// the port has no listener. We don't assert the specific error type
// (varies by platform: "connection refused" / "actively refused" etc.);
// only that it's non-nil and the call returns within ~1s deadline.
func TestPingIncumbent_PortNotListening(t *testing.T) {
	// Pick a port that's almost certainly closed: ephemeral range start.
	const probablyClosedPort = 1
	deadline := time.Now().Add(1 * time.Second)
	_, err := pingIncumbent(context.Background(), probablyClosedPort, 500*time.Millisecond)
	if err == nil {
		t.Errorf("pingIncumbent on closed port returned nil error")
	}
	if time.Now().After(deadline) {
		t.Errorf("pingIncumbent took >1s on closed port; expected fast-fail")
	}
}

// TestKillProcess_Stub verifies the killProcess function exists and
// returns an error for an impossible PID — we don't actually kill any
// real process here. (Test 8 in cli/gui_force_test.go covers the
// real-kill path via subprocess.)
func TestKillProcess_Stub(t *testing.T) {
	const impossible = 2147483646
	err := killProcess(impossible)
	if err == nil {
		t.Errorf("killProcess(impossible) returned nil error; expected non-nil")
	}
	// Skip on platforms where killProcess is a no-op stub.
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("killProcess unimplemented for %s", runtime.GOOS)
	}
}

// portFromURL extracts the TCP port number from an httptest server URL.
func portFromURL(t *testing.T, u string) int {
	t.Helper()
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("unexpected httptest URL %q", u)
	}
	port, err := strconv.Atoi(u[len(prefix):])
	if err != nil {
		t.Fatalf("parse port from %q: %v", u, err)
	}
	return port
}
