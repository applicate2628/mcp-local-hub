package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type fakeLogs struct {
	body      string
	err       error
	gotServer string
	gotDaemon string
}

func (f *fakeLogs) Logs(server, daemon string, tail int) (string, error) {
	f.gotServer = server
	f.gotDaemon = daemon
	return f.body, f.err
}

// scriptedLogs returns a different body per call so a test can simulate
// the realistic sequence streamLogs observes when Follow is enabled
// before the daemon has written anything: prime returns the "no log
// yet" placeholder, the first few ticks keep returning the placeholder
// (the file still doesn't exist), and a later tick finally returns the
// real log once the daemon has written to stderr. Calls past the end of
// seq keep returning the last entry so the streaming loop does not
// observe an artificial truncation.
type scriptedLogs struct {
	mu        sync.Mutex
	seq       []string
	idx       int
	gotServer string
	gotDaemon string
}

func (f *scriptedLogs) Logs(server, daemon string, tail int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotServer, f.gotDaemon = server, daemon
	if f.idx < len(f.seq) {
		out := f.seq[f.idx]
		f.idx++
		return out, nil
	}
	return f.seq[len(f.seq)-1], nil
}

func TestLogs_GetReturnsText(t *testing.T) {
	s := NewServer(Config{})
	s.logs = &fakeLogs{body: "line1\nline2\n"}
	req := httptest.NewRequest(http.MethodGet, "/api/logs/memory?tail=100", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "line1") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestLogs_DaemonQueryParamForwarded confirms that ?daemon= on
// /api/logs/:server reaches the logsProvider. Multi-daemon servers
// (serena: claude + codex) depend on this — without it they get the
// adapter's default="default" fallback and see empty logs.
func TestLogs_DaemonQueryParamForwarded(t *testing.T) {
	fl := &fakeLogs{body: "x"}
	s := NewServer(Config{})
	s.logs = fl
	req := httptest.NewRequest(http.MethodGet, "/api/logs/serena?tail=10&daemon=claude", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if fl.gotDaemon != "claude" {
		t.Errorf("daemon param not forwarded: got %q want claude", fl.gotDaemon)
	}
}

// TestLogs_RejectsPathTraversalDaemon guards the log-path composition
// in api.LogsGet ("<logDir>/<server>-<daemon>.log") against a daemon
// query param that carries path separators, "..", or whitespace. The
// untrusted value is forwarded untouched into that path, so without
// validation a request like ?daemon=../etc/passwd could read
// "<logDir>/<server>-../etc/passwd.log" — outside the log directory.
// The handler must reject bad names with 400 BAD_REQUEST before the
// provider is called. Empty daemon remains allowed: the adapter falls
// back to "default" for single-daemon servers.
func TestLogs_RejectsPathTraversalDaemon(t *testing.T) {
	fl := &fakeLogs{body: "x"}
	s := NewServer(Config{})
	s.logs = fl

	bad := []string{
		"../etc/passwd",
		"..\\windows",
		"foo/bar",
		"claude bad space",
		"..",
		".",
		"a/b",
		"a\x00b",
	}
	for _, b := range bad {
		fl.gotDaemon = "" // reset
		// Use url.Values so special chars get percent-encoded correctly.
		q := url.Values{}
		q.Set("daemon", b)
		req := httptest.NewRequest(http.MethodGet, "/api/logs/serena?"+q.Encode(), nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("daemon=%q → status %d, want 400", b, rec.Code)
		}
		if fl.gotDaemon != "" {
			t.Errorf("daemon=%q: provider called with %q (want provider NOT called)", b, fl.gotDaemon)
		}
	}

	// Empty daemon is allowed — exercises the "fall back to default"
	// branch in the realLogs adapter. The handler forwards "" and the
	// provider is called with server="serena" daemon="".
	req := httptest.NewRequest(http.MethodGet, "/api/logs/serena", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("empty daemon should be allowed; got status %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestLogs_StreamRejectsNonGET verifies the method gate at the top of
// streamLogs. Spec §4.6 defines only GET /api/logs/:server/stream, and
// the outer route dispatches to streamLogs solely on the trailing path
// segment — so without an explicit method check a POST / PUT / DELETE
// to /api/logs/:server/stream would open a long-lived SSE response on
// an unintended verb. The handler must return 405 with Allow: GET for
// any non-GET method before any SSE headers are written.
func TestLogs_StreamRejectsNonGET(t *testing.T) {
	fl := &fakeLogs{body: "x"}
	s := NewServer(Config{})
	s.logs = fl
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/logs/serena/stream", nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/logs/serena/stream → status %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET" {
			t.Errorf("%s /api/logs/serena/stream → Allow=%q, want GET", method, got)
		}
	}
}

// TestLogs_AcceptsDottedServerName confirms that a server name with
// "." characters — legal under api.validManifestName (^[a-z0-9][a-z0-9._-]*$)
// — is also accepted by the GUI /api/logs/:server route. Before this
// regression guard the GUI validNameRe was ^[A-Za-z0-9_-]+$ (no dots),
// so servers like paper-search-mcp.io or foo.bar were valid everywhere
// except the logs endpoint, which 404'd them. The test fixes the
// validators' charset contract at the request boundary.
func TestLogs_AcceptsDottedServerName(t *testing.T) {
	fl := &fakeLogs{body: "x"}
	s := NewServer(Config{})
	s.logs = fl
	req := httptest.NewRequest(http.MethodGet, "/api/logs/paper-search-mcp.io", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("dotted server name should be accepted; got status %d", rec.Code)
	}
}

// TestLogs_RejectsDoubleDotSequence guards the "." character that the
// regex now permits. The charset alone would accept path-traversal
// sequences like "foo..bar" or "..hidden" once dots are legal, so
// validName has an explicit strings.Contains(s, "..") rejection. This
// test pins that secondary check — removing it would silently re-open
// a traversal surface because api.LogsGet composes
// "<logDir>/<server>-<daemon>.log" and a leaked ".." in either
// segment escapes the log directory.
//
// The bare ".." is intentionally omitted from this suite: net/http
// ServeMux canonicalizes literal ".." in URL.Path into a 301/307
// redirect before the handler runs, so the bare form never reaches
// validName as a raw server segment. That behavior is exercised by
// TestLogs_RejectsPathTraversalServer using encoded variants
// (..%2Fetc, %2E%2E%2Fetc) which do reach the handler.
func TestLogs_RejectsDoubleDotSequence(t *testing.T) {
	fl := &fakeLogs{body: "x"}
	s := NewServer(Config{})
	s.logs = fl
	for _, bad := range []string{"foo..bar", "..hidden", "trail..", "a..b..c"} {
		req := httptest.NewRequest(http.MethodGet, "/api/logs/"+bad, nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("server=%q (contains ..) → status %d, want 404", bad, rec.Code)
		}
	}
}

// TestLogs_RejectsPathTraversalServer guards the server path segment
// of /api/logs/:server against path-segment injection. A value like
// "foo/bar" lands as parts=["foo","bar"] inside the handler; without
// the extra-segment guard, "bar" is not "stream" so the handler falls
// through to the snapshot branch with server="foo" — silently
// misattributing the request. Reject with 404. Literal ".." is
// collapsed by net/http's ServeMux into a 301/307 before the handler
// runs, so this suite exercises the encoded and multi-segment
// variants the mux actually delivers to our handler.
func TestLogs_RejectsPathTraversalServer(t *testing.T) {
	fl := &fakeLogs{body: "x"}
	s := NewServer(Config{})
	s.logs = fl

	cases := []struct {
		path string
		want int
	}{
		// foo/bar: handler sees parts=["foo","bar"]; rejected because
		// the second segment is not "" and not "stream".
		{"/api/logs/foo/bar", http.StatusNotFound},
		// ..%2Fetc: URL.Path decodes to "/api/logs/../etc", so
		// parts=["..","etc"]; server=".." fails validName.
		{"/api/logs/..%2Fetc", http.StatusNotFound},
		// Literal ".." in a raw segment that the mux does NOT
		// canonicalize (double-encoded) still reaches the handler as
		// parts=["..", ...]; server=".." fails validName.
		{"/api/logs/%2E%2E%2Fetc", http.StatusNotFound},
		// Space in the server name fails validName.
		{"/api/logs/bad%20name", http.StatusNotFound},
		// Trailing slash / empty server fails validName (empty).
		{"/api/logs/", http.StatusNotFound},
	}
	for _, c := range cases {
		fl.gotServer = ""
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("path=%q → status %d, want %d", c.path, rec.Code, c.want)
		}
		if fl.gotServer != "" {
			t.Errorf("path=%q: provider called with server=%q (want provider NOT called)", c.path, fl.gotServer)
		}
	}
}

// TestStreamLogs_DoesNotEmitOrAdvanceCursorOnPlaceholder pins the R14
// fix to streamLogs's per-tick loop. R12 added the isLogPlaceholder
// check for the INITIAL prime of lastLen only; without an equivalent
// check inside the ticker.C branch, enabling Follow before the daemon
// writes anything produced this sequence:
//
//  1. prime skipped (body is placeholder)  -> lastLen = 0
//  2. tick 1: body is still placeholder; len(body) > lastLen=0 so the
//     emission branch ran, pushing "(no log output yet ...)" into the
//     SSE stream as a log-line event and seeding lastLen ~= 30.
//  3. tick 2: daemon has now written a short first line (typically
//     shorter than the placeholder); len(body) < lastLen hits the
//     rotation branch, which resets the cursor to the new size and
//     `continue`s -- the first real bytes of that session are never
//     emitted.
//
// The fix detects the placeholder inside the tick loop and `continue`s
// without advancing lastLen. This test confirms:
//   - the placeholder text never appears as a log-line payload in the
//     SSE output (it is not streamable content);
//   - once the real log appears, its bytes ARE emitted in full (the
//     rotation branch does not eat them).
func TestStreamLogs_DoesNotEmitOrAdvanceCursorOnPlaceholder(t *testing.T) {
	// A well-formed placeholder body matches api.LogPlaceholderPrefix
	// exactly; the rest of the string interpolates the (server, daemon)
	// pair and is irrelevant to isLogPlaceholder.
	placeholder := api.LogPlaceholderPrefix + " for test/default)"
	realLog := "first-line\nsecond-line\n"
	fl := &scriptedLogs{seq: []string{
		placeholder, // prime
		placeholder, // tick 1
		placeholder, // tick 2
		realLog,     // tick 3: daemon finally wrote; stays at this body afterwards
	}}
	s := NewServer(Config{})
	s.logs = fl

	// sseRecorder is defined in events_test.go (same package) and is a
	// goroutine-safe http.ResponseWriter+Flusher. httptest.NewRecorder
	// cannot be used directly: streamLogs type-asserts the writer to
	// http.Flusher and it is not safe for concurrent access between the
	// handler and the test goroutine under -race.
	rec := newSSERecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/test/stream", nil)
	// Bound the SSE handler; 2s is comfortably above three 500ms ticks
	// plus the prime. The test cancels as soon as it observes the real
	// log bytes so a passing run completes quickly.
	ctx, cancel := context.WithTimeout(req.Context(), 2500*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()

	deadline := time.Now().Add(2200 * time.Millisecond)
	sawReal := false
	for time.Now().Before(deadline) {
		body := rec.body()
		if strings.Contains(body, "first-line") && strings.Contains(body, "second-line") {
			sawReal = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	final := rec.body()
	if !sawReal {
		t.Fatalf("never saw real log lines; body: %q", final)
	}
	// The placeholder text must NEVER appear in the emitted stream.
	// Before R14 the tick loop emitted it as a log-line event; after
	// R14 the placeholder is dropped and the cursor is not advanced.
	if strings.Contains(final, api.LogPlaceholderPrefix) {
		t.Errorf("placeholder text leaked into SSE stream: %q", final)
	}
}
