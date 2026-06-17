package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// swapSelfRestartSeams installs spawn + exit seams for the test scope and
// restores them on cleanup. The exit seam records whether the handler
// asked to exit (it must NOT call os.Exit in a test). The spawn seam
// returns the supplied (pid, err) without launching any real process.
func swapSelfRestartSeams(t *testing.T, pid int, spawnErr error) (exited *bool) {
	t.Helper()
	origSpawn := selfRestartSpawnFn
	origExit := selfRestartExitFn
	t.Cleanup(func() {
		selfRestartSpawnFn = origSpawn
		selfRestartExitFn = origExit
	})
	ex := false
	exited = &ex
	selfRestartSpawnFn = func() (int, error) { return pid, spawnErr }
	selfRestartExitFn = func() { ex = true } // never os.Exit in a test
	return exited
}

// TestGUISelfRestart_SpawnSuccess: a successful spawn returns 200 with
// restarting:true and the spawned PID, and the handler schedules the exit
// (via the seam) so the lock is handed off.
func TestGUISelfRestart_SpawnSuccess(t *testing.T) {
	s := NewServer(Config{Port: 9})
	registerGUISelfRestartRoutes(s)
	_ = swapSelfRestartSeams(t, 4242, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9")
	rr := httptest.NewRecorder()
	s.guiSelfRestartHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp guiSelfRestartResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Spawned || resp.SpawnedPID != 4242 || !resp.Restarting {
		t.Fatalf("resp = %+v, want spawned=true pid=4242 restarting=true", resp)
	}
	if resp.SpawnError != "" {
		t.Fatalf("unexpected spawn_error %q", resp.SpawnError)
	}
}

// TestGUISelfRestart_SpawnFailureNoExit: when the spawn fails the handler
// must NOT exit (the operator keeps the running GUI) and must surface the
// error in the body with spawned:false / restarting:false.
func TestGUISelfRestart_SpawnFailureNoExit(t *testing.T) {
	s := NewServer(Config{Port: 9})
	registerGUISelfRestartRoutes(s)
	exited := swapSelfRestartSeams(t, 0, errSelfRestartTest)

	req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9")
	rr := httptest.NewRecorder()
	s.guiSelfRestartHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp guiSelfRestartResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Spawned || resp.Restarting {
		t.Fatalf("resp = %+v, want spawned=false restarting=false", resp)
	}
	if resp.SpawnError == "" {
		t.Fatalf("want non-empty spawn_error")
	}
	if *exited {
		t.Fatalf("handler must NOT exit when spawn fails")
	}
}

// TestGUISelfRestart_MethodNotAllowed: GET is rejected 405.
func TestGUISelfRestart_MethodNotAllowed(t *testing.T) {
	s := NewServer(Config{Port: 9})
	_ = swapSelfRestartSeams(t, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/gui/restart", nil)
	rr := httptest.NewRecorder()
	s.guiSelfRestartHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

var errSelfRestartTest = errSelfRestart("boom")

type errSelfRestart string

func (e errSelfRestart) Error() string { return string(e) }
