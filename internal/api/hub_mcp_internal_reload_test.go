// hub_mcp_internal_reload_test.go — Phase 4 Task 4.3 (G4 unified hub MCP).
//
// Verifies the control endpoint contract from spec §"Control endpoint
// contract `/internal/reload-tokens`". Covers every threat-model row
// (RFC-9110 method-not-allowed shape, separate keyspace from per-
// client header, rate-limit, concurrent serialization, no-log-leak).
//
// Tests use the daemonStateRootOverride seam + hardenedTempDir helper
// (same shape as hub_mcp_state_test.go) so the state-file writes pass
// the load-time DACL gate on Windows and 0600 + ownership on POSIX.

package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupReloadTestEnv overrides the state-dir to a hardened temp dir
// + seeds an initial tokens file via EnsureHubTokens. Returns the
// state-dir path + a fresh handler.
func setupReloadTestEnv(t *testing.T) (string, *InternalReloadHandler) {
	t.Helper()
	dir := hubMcpStateTestHelper(t)
	// Seed an initial tokens file so the reload path has something to
	// re-read (otherwise ReloadHubTokens succeeds with an empty table,
	// which is correct but observationally identical to a no-op).
	if _, err := EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	h := NewInternalReloadHandler()
	return dir, h
}

// controlTokenForTest returns the in-memory control token the handler
// generated at construction. Tests use this directly rather than
// reading the file from disk — the file write is exercised by a
// separate test.
func controlTokenForTest(h *InternalReloadHandler) string {
	p := h.controlTok.Load()
	if p == nil {
		return ""
	}
	return *p
}

// TestInternalReloadRequiresPOST asserts every method other than POST
// returns 405 with `Allow: POST` per RFC 9110 §15.5.6.
func TestInternalReloadRequiresPOST(t *testing.T) {
	_, h := setupReloadTestEnv(t)
	tok := controlTokenForTest(h)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/internal/reload-tokens", nil)
			req.Host = "127.0.0.1:9120"
			req.Header.Set("X-Mcphub-Control-Token", tok)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: got %d, want 405", method, w.Code)
			}
			if allow := w.Header().Get("Allow"); allow != "POST" {
				t.Errorf("%s: Allow header = %q, want %q", method, allow, "POST")
			}
			if w.Body.Len() != 0 {
				t.Errorf("%s: body must be empty; got %q", method, w.Body.String())
			}
		})
	}
}

// TestInternalReloadConstantTimeRejectsWrongToken — gate the shape +
// compare paths return the same 401 empty body. Earlier drafts split
// the rejection between 400 (shape) and 401 (compare); spec demands
// identical shape so an attacker cannot distinguish.
func TestInternalReloadConstantTimeRejectsWrongToken(t *testing.T) {
	_, h := setupReloadTestEnv(t)
	cases := []struct {
		name string
		tok  string
	}{
		{"empty", ""},
		{"63-hex", strings.Repeat("a", 63)},
		{"65-hex", strings.Repeat("a", 65)},
		{"uppercase", strings.Repeat("A", 64)},
		{"non-hex", strings.Repeat("g", 64)},
		{"shape-valid-wrong-value", strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
			req.Host = "127.0.0.1:9120"
			req.Header.Set("X-Mcphub-Control-Token", c.tok)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s: got %d, want 401", c.name, w.Code)
			}
			if w.Body.Len() != 0 {
				t.Errorf("%s: body must be empty; got %q", c.name, w.Body.String())
			}
		})
	}
}

// TestInternalReloadAcceptsCorrectTokenSwaps writes a fresh tokens
// file under the state-dir THEN POSTs to /internal/reload-tokens with
// the control token. After the 204 the live token table should
// reflect the on-disk change without restarting the hub.
func TestInternalReloadAcceptsCorrectTokenSwaps(t *testing.T) {
	dir, h := setupReloadTestEnv(t)
	// Rotate the on-disk token for codex-cli (a client absent from
	// the initial table) so we can observe the post-reload swap.
	tbl, err := RotateHubToken("codex-cli")
	if err != nil {
		t.Fatalf("RotateHubToken: %v", err)
	}
	freshTok := tbl.Tokens["codex-cli"]
	if freshTok == "" {
		t.Fatalf("RotateHubToken did not produce codex-cli token")
	}
	// Simulate "the live snapshot is stale": clobber the package-
	// global with an empty table so we can prove the reload actually
	// re-reads.
	publishTokenTable(HubTokenTable{Tokens: map[string]string{}})
	// Sanity: codex-cli not present in live snapshot.
	if ConstantTimeCompareToken("codex-cli", freshTok) == 1 {
		t.Fatalf("test setup: live snapshot was not cleared")
	}

	// Reload via the endpoint.
	req := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req.Host = "127.0.0.1:9120"
	req.Header.Set("X-Mcphub-Control-Token", controlTokenForTest(h))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("reload: got %d, want 204; body=%q", w.Code, w.Body.String())
	}

	// codex-cli now present in live snapshot.
	if ConstantTimeCompareToken("codex-cli", freshTok) != 1 {
		t.Errorf("reload did not republish: codex-cli token not in live snapshot")
	}

	// Sanity: the on-disk file is still where we left it (the reload
	// path is read-only on disk).
	if _, err := loadHubTokensLocked(); err != nil {
		t.Errorf("on-disk tokens file unreadable after reload: %v / dir=%s", err, dir)
	}
}

// TestInternalReloadRateLimited5s — two consecutive valid reloads
// within 5s: first 204, second 429 with Retry-After: 5. After 5s
// elapses the next valid reload returns 204 again.
func TestInternalReloadRateLimited5s(t *testing.T) {
	_, h := setupReloadTestEnv(t)
	tok := controlTokenForTest(h)

	// First reload: 204.
	req1 := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req1.Host = "127.0.0.1:9120"
	req1.Header.Set("X-Mcphub-Control-Token", tok)
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req1)
	if w1.Code != http.StatusNoContent {
		t.Fatalf("first reload: got %d, want 204", w1.Code)
	}

	// Second reload within the 5s cooldown: 429 + Retry-After: 5.
	req2 := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req2.Host = "127.0.0.1:9120"
	req2.Header.Set("X-Mcphub-Control-Token", tok)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second reload: got %d, want 429", w2.Code)
	}
	if retry := w2.Header().Get("Retry-After"); retry != "5" {
		t.Errorf("second reload: Retry-After = %q, want %q", retry, "5")
	}

	// Reach into the handler state to simulate the 5s elapsed without
	// actually sleeping. Faster than a sleep + still proves the cooldown
	// branch was entered above and that the next reload would succeed.
	h.reloadMutex.Lock()
	h.lastReload = time.Now().Add(-internalReloadCooldown - time.Millisecond)
	h.reloadMutex.Unlock()

	req3 := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req3.Host = "127.0.0.1:9120"
	req3.Header.Set("X-Mcphub-Control-Token", tok)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNoContent {
		t.Errorf("post-cooldown reload: got %d, want 204", w3.Code)
	}
}

// TestInternalReloadConcurrentSerialize — fires N parallel POSTs.
// Exactly one returns 204; the others either return 204 (if the
// cooldown window opened in between, which is rare in a synchronous
// race) or 429 (most common). Asserts no panic and no exception
// (reloadMutex serialization works under contention).
func TestInternalReloadConcurrentSerialize(t *testing.T) {
	_, h := setupReloadTestEnv(t)
	tok := controlTokenForTest(h)

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
			req.Host = "127.0.0.1:9120"
			req.Header.Set("X-Mcphub-Control-Token", tok)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	var got204, got429 int
	for _, c := range codes {
		switch c {
		case http.StatusNoContent:
			got204++
		case http.StatusTooManyRequests:
			got429++
		default:
			t.Errorf("unexpected status: %d", c)
		}
	}
	// At least one valid reload should land; the rest either pass
	// (extremely unlikely without ≥5s gap) or 429.
	if got204 == 0 {
		t.Errorf("no concurrent request succeeded: %d 429s", got429)
	}
	if got204+got429 != n {
		t.Errorf("status mix unexpected: 204=%d 429=%d total=%d", got204, got429, n)
	}
}

// TestInternalReloadIgnoresPerClientHeader — the per-client
// X-Mcphub-Hub-Token header is NOT accepted on this path. Same
// shape, same constant-time mechanism, but a separate keyspace.
// Passing a per-client token value as the control header yields 401.
func TestInternalReloadIgnoresPerClientHeader(t *testing.T) {
	_, h := setupReloadTestEnv(t)
	// Pretend the operator pulled the per-client token out of the
	// live table and sent it as the control header.
	tbl := CurrentTokenTable()
	clientTok, ok := tbl.Tokens["claude-code"]
	if !ok {
		t.Fatal("test setup: claude-code token absent from live snapshot")
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req.Host = "127.0.0.1:9120"
	// Note: per-client header is set with the per-client value, the
	// control header carries the SAME value (would-be confusion attempt).
	req.Header.Set("X-Mcphub-Hub-Token", clientTok)
	req.Header.Set("X-Mcphub-Control-Token", clientTok) // wrong keyspace
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("per-client header injection: got %d, want 401", w.Code)
	}
}

// TestInternalReloadRejectsNonLoopback — the loopback-guard middleware
// runs before any token check, so a non-loopback Host returns 403
// regardless of the control header.
func TestInternalReloadRejectsNonLoopback(t *testing.T) {
	_, h := setupReloadTestEnv(t)
	tok := controlTokenForTest(h)
	req := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req.Host = "evil.example.com"
	req.Header.Set("X-Mcphub-Control-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-loopback Host: got %d, want 403", w.Code)
	}
}

// TestInternalReloadNoLeaksToHubMcpLog — the successful reload log
// line has source:"internal-reload" and NO token bytes. Reading the
// hub-mcp.log file after a successful POST + grep'ing for token
// substrings should return empty.
func TestInternalReloadNoLeaksToHubMcpLog(t *testing.T) {
	dir, h := setupReloadTestEnv(t)
	tok := controlTokenForTest(h)

	req := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req.Host = "127.0.0.1:9120"
	req.Header.Set("X-Mcphub-Control-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("reload: got %d, want 204", w.Code)
	}

	logPath := filepath.Join(dir, hubMcpLogFileLeaf)
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("hub-mcp.log: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, `"event":"tokens-reloaded"`) {
		t.Errorf("hub-mcp.log missing tokens-reloaded event: %s", content)
	}
	if !strings.Contains(content, `"source":"internal-reload"`) {
		t.Errorf("hub-mcp.log missing source:internal-reload: %s", content)
	}
	if strings.Contains(content, tok) {
		t.Errorf("hub-mcp.log leaked control token bytes")
	}
	// The per-client token MUST also not appear (it never enters this
	// path but verify defense-in-depth).
	for client, ctok := range CurrentTokenTable().Tokens {
		if strings.Contains(content, ctok) {
			t.Errorf("hub-mcp.log leaked per-client token for %s", client)
		}
	}
}

// TestInternalReloadShutdownRemovesFile — Shutdown removes the
// hub-mcp-control.token file from the state-dir. Idempotent on a
// second call (file already gone).
func TestInternalReloadShutdownRemovesFile(t *testing.T) {
	dir, h := setupReloadTestEnv(t)
	path := filepath.Join(dir, hubMcpControlTokenFileLeaf)
	// Sanity: file exists post-construction.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("hub-mcp-control.token missing post-construction: %v", err)
	}
	if err := h.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("Shutdown did not remove hub-mcp-control.token")
	}
	// Idempotent.
	if err := h.Shutdown(); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// TestInternalReloadShutdownClearsInMemoryToken — after Shutdown the
// in-memory atomic.Pointer holds an empty token so any racing late
// POST gets 401.
func TestInternalReloadShutdownClearsInMemoryToken(t *testing.T) {
	_, h := setupReloadTestEnv(t)
	tok := controlTokenForTest(h)
	if tok == "" {
		t.Fatal("test setup: control token empty before Shutdown")
	}
	if err := h.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Late POST with the captured token: 401 (mismatch against empty).
	req := httptest.NewRequest(http.MethodPost, "/internal/reload-tokens", nil)
	req.Host = "127.0.0.1:9120"
	req.Header.Set("X-Mcphub-Control-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("post-shutdown POST: got %d, want 401", w.Code)
	}
}
