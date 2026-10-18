package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
)

type fakeClientEntryRemover struct {
	calls  int
	result *api.ClientEntryRemovalResult
}

func TestClientEntryRemovalHandlerPublishesTwoObservers(t *testing.T) {
	s := newEphemeralServer(t, Config{Port: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	one, two := s.events.Subscribe(ctx), s.events.Subscribe(ctx)
	s.clientEntryRemover = &fakeClientEntryRemover{result: &api.ClientEntryRemovalResult{Operation: "remove-client-entry", Server: "one", Client: "kimi-code-cli", Outcome: "removed", EffectiveAbsent: true, Code: "REMOVAL_SETTLED", Receipt: &api.ClientEntryRemovalReceipt{Version: 1, BeforeToken: "token", AfterToken: "after", SourceCount: 1}}}
	req := httptest.NewRequest(http.MethodPost, "/api/client-entry/remove", strings.NewReader(`{"apply":true,"server":"one","client":"kimi-code-cli","source_token":"token"}`))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, ch := range []<-chan Event{one, two} {
		timer := time.NewTimer(time.Second)
		found := false
		for !found {
			select {
			case ev := <-ch:
				found = ev.Type == "clients-rescan"
			case <-timer.C:
				t.Fatal("observer received audit but no rescan")
			}
		}
		timer.Stop()
	}
}

func TestClientEntryRemovalRealRouteFlockDeadline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", dir)
	path := filepath.Join(dir, "mcp.json")
	before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	s := newEphemeralServer(t, Config{Port: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/client-entry/remove", strings.NewReader(`{"apply":true,"server":"one","client":"kimi-code-cli","source_token":"observed-token"}`)).WithContext(ctx)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	started := time.Now()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if time.Since(started) > time.Second || w.Code != 207 || !strings.Contains(w.Body.String(), "REMOVAL_TIMEOUT") {
		t.Fatalf("route deadline: %d %s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatal("blocked route changed source")
	}
}

func (f *fakeClientEntryRemover) RemoveClientEntry(ctx context.Context, req api.ClientEntryRemovalRequest) (*api.ClientEntryRemovalResult, error) {
	f.calls++
	return f.result, nil
}

func TestClientEntryRemovalHandlerAuthorizationAndStatuses(t *testing.T) {
	for _, tc := range []struct {
		name, body, origin, outcome string
		want                        int
		calls                       int
	}{
		{"settled", `{"apply":true,"server":"one","client":"kimi-code-cli","source_token":"token"}`, "same-origin", "removed", 200, 1},
		{"refused", `{"apply":true,"server":"one","client":"kimi-code-cli","source_token":"token"}`, "same-origin", "refused", 207, 1},
		{"missing-token", `{"apply":true,"server":"one","client":"kimi-code-cli"}`, "same-origin", "", 400, 0},
		{"cross-origin", `{"apply":true,"server":"one","client":"kimi-code-cli","source_token":"token"}`, "cross-site", "", 403, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEphemeralServer(t, Config{Port: 0})
			fake := &fakeClientEntryRemover{result: &api.ClientEntryRemovalResult{Operation: "remove-client-entry", Server: "one", Client: "kimi-code-cli", Outcome: tc.outcome, EffectiveAbsent: tc.outcome == "removed", Code: "REMOVAL_SETTLED", Receipt: &api.ClientEntryRemovalReceipt{Version: 1, BeforeToken: "token", AfterToken: "after", SourceCount: 1}}}
			s.clientEntryRemover = fake
			req := httptest.NewRequest(http.MethodPost, "/api/client-entry/remove", strings.NewReader(tc.body))
			req.Header.Set("Sec-Fetch-Site", tc.origin)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			if w.Code != tc.want || fake.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, fake.calls, w.Body.String())
			}
		})
	}
}
