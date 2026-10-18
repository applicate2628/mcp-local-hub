package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestGUIQuitHTTPAcceptedIntentAndStopPolarity(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			s := NewServer(Config{Port: 9081})
			calls := 0
			s.SetGUIQuitHandler(func(ctx context.Context, request GUIQuitRequest) (GUIQuitResult, error) {
				calls++
				if ctx == nil || request.ExpectedGeneration != "original" || request.StopDaemons != stop {
					t.Fatalf("request = %+v, want original generation and stop=%v", request, stop)
				}
				return GUIQuitResult{Accepted: true, Generation: "original", StopDaemons: stop}, nil
			})
			// Omission must preserve the safe false default; only positive true
			// requests the broader daemon-stop operation.
			body := `{"expected_generation":"original"}`
			if stop {
				body = `{"expected_generation":"original","stop_daemons":true}`
			}
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/gui/quit", strings.NewReader(body)))
			var response guiQuitHTTPResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusAccepted || calls != 1 || !response.Accepted || response.Generation != "original" || response.StopDaemons != stop || response.Code != "" {
				t.Fatalf("status=%d calls=%d response=%+v", recorder.Code, calls, response)
			}
		})
	}
}

func TestGUIQuitHTTPRejectsInvalidRequestWithoutCallingOwner(t *testing.T) {
	for _, body := range []string{
		``, `null`, `{}`, `{"expected_generation":""}`, `{"expected_generation":" "}`,
		`{"expected_generation":42}`, `{"expected_generation":"original","stop_daemons":"true"}`,
		`{"expected_generation":"original","unknown":true}`, `{"expected_generation":"original"} {}`,
		`{"expected_generation":"` + strings.Repeat("g", int(maxControlBodyBytes)) + `"}`,
	} {
		t.Run(fmt.Sprintf("body-%d", len(body)), func(t *testing.T) {
			s := NewServer(Config{Port: 9081})
			s.SetGUIQuitHandler(func(context.Context, GUIQuitRequest) (GUIQuitResult, error) {
				t.Fatal("invalid request reached Quit owner")
				return GUIQuitResult{}, nil
			})
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/gui/quit", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status=%d, want invalid body refusal", recorder.Code)
			}
		})
	}
}

func TestGUIQuitHTTPRetainsExistingRequestGuards(t *testing.T) {
	for _, tc := range []struct {
		name, method, host, origin, fetchSite string
		status                                int
	}{
		{name: "method", method: http.MethodGet, host: "127.0.0.1:9081", status: http.StatusMethodNotAllowed},
		{name: "host", method: http.MethodPost, host: "foreign.example:9081", status: http.StatusForbidden},
		{name: "origin", method: http.MethodPost, host: "127.0.0.1:9081", origin: "http://foreign.example:9081", fetchSite: "same-origin", status: http.StatusForbidden},
		{name: "fetch-site", method: http.MethodPost, host: "127.0.0.1:9081", fetchSite: "cross-site", status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9081})
			s.SetGUIQuitHandler(func(context.Context, GUIQuitRequest) (GUIQuitResult, error) {
				t.Fatal("guarded request reached Quit owner")
				return GUIQuitResult{}, nil
			})
			request := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/gui/quit", strings.NewReader(`{"expected_generation":"original"}`))
			request.Header.Set("Origin", tc.origin)
			request.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, request)
			if recorder.Code != tc.status {
				t.Fatalf("status=%d, want %d", recorder.Code, tc.status)
			}
			if tc.method == http.MethodGet && recorder.Header().Get("Allow") != "POST" {
				t.Fatal("method refusal omitted Allow: POST")
			}
		})
	}
}

func TestGUIQuitHTTPPreservesAcquiredResultAndTypedErrors(t *testing.T) {
	privateCause := errors.New("private-path-must-not-reach-wire")
	for _, tc := range []struct {
		name, code string
		result     GUIQuitResult
		err        error
		status     int
	}{
		{name: "stale", err: fmt.Errorf("owner: %w", ErrGUIQuitGeneration), code: "GUI_QUIT_GENERATION_CONFLICT", status: http.StatusConflict},
		{name: "policy-conflict", err: fmt.Errorf("owner: %w", api.ErrGUIControlConflict), code: "GUI_CONTROL_CONFLICT", status: http.StatusConflict},
		{name: "stopped", err: api.ErrGUIStoppedByUser, code: "GUI_STOPPED_BY_USER", status: http.StatusConflict},
		{name: "failed", err: privateCause, code: "GUI_QUIT_FAILED", status: http.StatusInternalServerError},
		{name: "refused", code: "GUI_QUIT_REFUSED", status: http.StatusServiceUnavailable},
		{name: "partial", result: GUIQuitResult{Accepted: true, Generation: "original", StopDaemons: true}, err: privateCause, code: "GUI_QUIT_PARTIAL", status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9081})
			s.SetGUIQuitHandler(func(context.Context, GUIQuitRequest) (GUIQuitResult, error) { return tc.result, tc.err })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/gui/quit", strings.NewReader(`{"expected_generation":"original","stop_daemons":true}`)))
			var response guiQuitHTTPResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != tc.status || response.Code != tc.code || response.GUIQuitResult != tc.result {
				t.Fatalf("status=%d response=%+v", recorder.Code, response)
			}
			if strings.Contains(recorder.Body.String(), privateCause.Error()) {
				t.Fatal("raw callback cause leaked to HTTP response")
			}
		})
	}
}

func TestGUIQuitHTTPUnavailableWithoutComposition(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/gui/quit", strings.NewReader(`{"expected_generation":"original"}`)))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "GUI_QUIT_UNAVAILABLE") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type guiQuitFailingWriter struct{ header http.Header }

func (w *guiQuitFailingWriter) Header() http.Header { return w.header }
func (*guiQuitFailingWriter) WriteHeader(int)       {}
func (*guiQuitFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("response unavailable")
}

func TestGUIQuitHTTPResponseFailureDoesNotGateAcceptedOwnerOperation(t *testing.T) {
	s := NewServer(Config{Port: 9081})
	ownerCalls := 0
	s.SetGUIQuitHandler(func(context.Context, GUIQuitRequest) (GUIQuitResult, error) {
		ownerCalls++
		return GUIQuitResult{Accepted: true, Generation: "original"}, nil
	})
	s.ServeHTTP(&guiQuitFailingWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9081/api/gui/quit", strings.NewReader(`{"expected_generation":"original"}`)))
	if ownerCalls != 1 {
		t.Fatalf("original owner calls=%d, want 1", ownerCalls)
	}
}
