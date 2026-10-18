package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func newSupervisorRestartTransportTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(root))
	s := NewServer(Config{Port: 9081})
	s.events.DisableGUIEventLog = true
	t.Cleanup(s.events.Close)
	return s, root
}

func TestSupervisorRestartHTTPGuardsAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, method, host, origin, fetchSite string
		status                                int
	}{
		{name: "method", method: http.MethodGet, host: "127.0.0.1:9081", status: http.StatusMethodNotAllowed},
		{name: "host", method: http.MethodPost, host: "foreign.example:9081", status: http.StatusForbidden},
		{name: "origin", method: http.MethodPost, host: "127.0.0.1:9081", origin: "http://foreign.example:9081", status: http.StatusForbidden},
		{name: "fetch-site", method: http.MethodPost, host: "127.0.0.1:9081", fetchSite: "cross-site", status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newSupervisorRestartTransportTestServer(t)
			s.SetSupervisorRestartHandler(func(context.Context) (SupervisorRestartResult, error) {
				t.Fatal("rejected request reached lifecycle owner")
				return SupervisorRestartResult{}, nil
			})
			req := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/supervisor/restart", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d, want %d", rec.Code, tc.status)
			}
			if tc.method == http.MethodGet && rec.Header().Get("Allow") != http.MethodPost {
				t.Fatal("method refusal lost Allow: POST")
			}
		})
	}
	t.Run("unavailable", func(t *testing.T) {
		s, root := newSupervisorRestartTransportTestServer(t)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/supervisor/restart", nil))
		var result SupervisorRestartResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || result.Outcome != SupervisorRestartRefused || result.Code != "RESTART_UNAVAILABLE" || result.Spawned || result.Killed || result.Ready {
			t.Fatalf("unavailable response=%s status=%d", rec.Body, rec.Code)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("uncomposed route touched state: entries=%v err=%v", entries, err)
		}
	})
}

func readySupervisorRestartResultForTest() SupervisorRestartResult {
	return SupervisorRestartResult{
		Outcome: SupervisorRestartRestarted, SpawnedPID: 202, Spawned: true,
		Ready: true, OriginalSettled: true, Phase: "complete",
		OriginalGeneration:       &api.SupervisorLockOwner{PID: 101, StartedAt: "2026-01-01T00:00:00Z"},
		SelectedGeneration:       &api.SupervisorLockOwner{PID: 202, StartedAt: "2026-01-01T00:01:00Z"},
		OriginalChildDisposition: "retained", NamespaceDisposition: "occupied",
	}
}

func TestSupervisorRestartHTTPProjectsLifecycleOutcomes(t *testing.T) {
	complete := readySupervisorRestartResultForTest()
	standalone := complete
	standalone.TransferCommitted, standalone.Transferred = true, true
	standalone.OriginalChildDisposition = "transferred"
	ownerCause := errors.New("private-native-path-and-argv")
	for _, tc := range []struct {
		name    string
		result  SupervisorRestartResult
		err     error
		outcome SupervisorRestartOutcome
	}{
		{name: "retained-ready", result: complete, outcome: SupervisorRestartRestarted},
		{name: "transferred-ready", result: standalone, outcome: SupervisorRestartRestarted},
		{name: "creation-only", result: SupervisorRestartResult{Spawned: true, SpawnedPID: 202}, outcome: SupervisorRestartFailed},
		{name: "pending-child", result: SupervisorRestartResult{Outcome: SupervisorRestartPending, Spawned: true, SpawnedPID: 202, Phase: "settlement", OriginalChildDisposition: "pending"}, err: ownerCause, outcome: SupervisorRestartPending},
		{name: "refused", result: SupervisorRestartResult{Outcome: SupervisorRestartRefused, Code: "RESTART_IDENTITY_UNCONFIRMED", Phase: "admission", OriginalChildDisposition: "none"}, err: ownerCause, outcome: SupervisorRestartRefused},
		{name: "settled-loser-foreign-winner", result: SupervisorRestartResult{Outcome: SupervisorRestartFailed, Spawned: true, SpawnedPID: 202, Phase: "namespace", OriginalChildDisposition: "settled", NamespaceDisposition: "occupied", Code: "RESTART_NAMESPACE_CONFLICT"}, err: ownerCause, outcome: SupervisorRestartFailed},
		{name: "ready-with-owner-error", result: complete, err: ownerCause, outcome: SupervisorRestartFailed},
		{name: "ready-with-step-error", result: func() SupervisorRestartResult {
			r := complete
			r.PerStepError = map[string]string{"kill": ownerCause.Error()}
			return r
		}(), outcome: SupervisorRestartFailed},
		{name: "commit-without-transfer", result: func() SupervisorRestartResult { r := standalone; r.Transferred = false; return r }(), outcome: SupervisorRestartFailed},
		{name: "unknown-private-classification", result: SupervisorRestartResult{Outcome: SupervisorRestartFailed, Phase: ownerCause.Error(), Code: ownerCause.Error(), PerStepError: map[string]string{ownerCause.Error(): ownerCause.Error()}, OriginalChildDisposition: ownerCause.Error(), NamespaceDisposition: ownerCause.Error()}, err: ownerCause, outcome: SupervisorRestartFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newSupervisorRestartTransportTestServer(t)
			actions := make(chan Event, 1)
			s.events.subs[actions] = struct{}{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.SetSupervisorRestartHandler(func(actual context.Context) (SupervisorRestartResult, error) {
				calls++
				if actual != ctx {
					t.Fatal("transport replaced original request context")
				}
				return tc.result, tc.err
			})
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/supervisor/restart", nil).WithContext(ctx))
			var result SupervisorRestartResult
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || calls != 1 || result.Outcome != tc.outcome || result.Spawned != tc.result.Spawned || result.SpawnedPID != tc.result.SpawnedPID || result.Killed != tc.result.Killed ||
				result.Ready != tc.result.Ready || result.OriginalSettled != tc.result.OriginalSettled || result.TransferCommitted != tc.result.TransferCommitted || result.Transferred != tc.result.Transferred {
				t.Fatalf("status=%d calls=%d result=%s", rec.Code, calls, rec.Body)
			}
			if strings.Contains(rec.Body.String(), ownerCause.Error()) {
				t.Fatalf("private owner detail leaked: %s", rec.Body)
			}
			wantActions := 0
			if tc.outcome == SupervisorRestartRestarted {
				wantActions = 1
			} else if len(result.PerStepError) == 0 {
				t.Fatal("non-clean result lost failure classification")
			}
			if len(actions) != wantActions {
				t.Fatalf("successful audit actions=%d, want %d", len(actions), wantActions)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"killed_pid", "killed", "spawned_pid", "spawned", "outcome", "ready", "original_settled", "transfer_committed", "transferred", "original_child_disposition", "namespace_disposition"} {
				if _, ok := wire[key]; !ok {
					t.Errorf("missing actual JSON field %s", key)
				}
			}
			if result.SelectedGeneration != nil {
				var generation map[string]json.RawMessage
				if err := json.Unmarshal(wire["selected_generation"], &generation); err != nil || len(generation["pid"]) == 0 || len(generation["started_at"]) == 0 {
					t.Fatalf("generation wire=%s err=%v", wire["selected_generation"], err)
				}
			}
			if tc.name == "retained-ready" {
				t.Logf("RESTART_TRANSPORT_WIRE_SAMPLE=%s", rec.Body.String())
			}
		})
	}
}

type supervisorRestartFailingWriter struct {
	header http.Header
	err    error
}

func (w *supervisorRestartFailingWriter) Header() http.Header       { return w.header }
func (w *supervisorRestartFailingWriter) WriteHeader(int)           {}
func (w *supervisorRestartFailingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestSupervisorRestartHTTPReceivesDeliveryAndOutputFailures(t *testing.T) {
	t.Run("required-log", func(t *testing.T) {
		s, root := newSupervisorRestartTransportTestServer(t)
		if err := os.Mkdir(filepath.Join(root, "hub-mcp.log"), 0o700); err != nil {
			t.Fatal(err)
		}
		actions := make(chan Event, 1)
		s.events.subs[actions] = struct{}{}
		s.SetSupervisorRestartHandler(func(context.Context) (SupervisorRestartResult, error) {
			return readySupervisorRestartResultForTest(), nil
		})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/supervisor/restart", nil))
		var result SupervisorRestartResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Code != "RESTART_DIAGNOSTIC_UNCONFIRMED" || result.PerStepError["diagnostic"] == "" || result.Outcome != SupervisorRestartFailed || !result.Spawned || result.SpawnedPID != 202 || result.OriginalChildDisposition != "retained" || len(actions) != 0 {
			t.Fatalf("lost delivery/effect outcome: %s", rec.Body)
		}
	})
	t.Run("response-write", func(t *testing.T) {
		s, _ := newSupervisorRestartTransportTestServer(t)
		ownerErr := errors.New("original-callback-cause")
		writeErr := errors.New("original-response-write-cause")
		s.SetSupervisorRestartHandler(func(context.Context) (SupervisorRestartResult, error) {
			return SupervisorRestartResult{Outcome: SupervisorRestartPending, Phase: "settlement", OriginalChildDisposition: "pending"}, ownerErr
		})
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		t.Cleanup(func() { log.SetOutput(previous) })
		writer := &supervisorRestartFailingWriter{header: make(http.Header), err: writeErr}
		s.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/supervisor/restart", nil))
		if !strings.Contains(output.String(), ownerErr.Error()) || !strings.Contains(output.String(), writeErr.Error()) {
			t.Fatalf("original acquired causes not received: %s", &output)
		}
	})
}
