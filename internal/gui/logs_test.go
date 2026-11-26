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

func (f *fakeLogs) PollDaemonLog(server, daemon string, offset int64, prime bool) (api.DaemonLogPoll, error) {
	f.gotServer, f.gotDaemon = server, daemon
	return api.DaemonLogPoll{Offset: offset}, f.err
}

// scriptedLogs returns one incremental result per poll. An exhausted sequence
// leaves the current cursor unchanged.
type scriptedLogs struct {
	mu        sync.Mutex
	seq       []api.DaemonLogPoll
	idx       int
	gotServer string
	gotDaemon string
}

func (f *scriptedLogs) Logs(server, daemon string, tail int) (string, error) {
	panic("stream used the snapshot provider")
}

func (f *scriptedLogs) PollDaemonLog(server, daemon string, offset int64, prime bool) (api.DaemonLogPoll, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotServer, f.gotDaemon = server, daemon
	if f.idx < len(f.seq) {
		out := f.seq[f.idx]
		f.idx++
		return out, nil
	}
	return api.DaemonLogPoll{Offset: offset}, nil
}

func (f *scriptedLogs) observation() (calls int, server, daemon string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idx, f.gotServer, f.gotDaemon
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

// TestLogs_OpenFolder verifies that POST /api/logs-folder
// resolves api.DefaultLogDir, mkdir's it on first-run, and spawns
// the OS file manager via the same seam used by openpath. Returns
// 200 + {"opened": <dir>} on success. Audit gap (Round 1 #5):
// spec calls for an "Open folder" action on Logs.
//
// Sibling path (not under /api/logs/) avoids a prefix collision
// with a manifest legitimately named "open-folder" — Codex bot
// review on PR #47 P2.
func TestLogs_OpenFolder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	captured := struct {
		name string
		args []string
	}{}
	orig := spawnProcess
	spawnProcess = func(name string, args ...string) error {
		captured.name = name
		captured.args = args
		return nil
	}
	defer func() { spawnProcess = orig }()

	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/logs-folder", nil)
	// Same-origin gate requires Origin or no Sec-Fetch-Site; httptest
	// requests have no Origin so the gate's no-origin path applies.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if captured.name == "" {
		t.Fatal("spawnProcess seam was never invoked")
	}
	// The spawned arg must be the canonical DefaultLogDir under the
	// test's redirected LOCALAPPDATA (...mcp-local-hub/logs). Verifying
	// the suffix pins the path computation against api.defaultLogDir
	// without depending on the os-specific path separator the test
	// machine uses.
	if len(captured.args) == 0 || !strings.Contains(captured.args[0], "mcp-local-hub") {
		t.Errorf("spawn args do not point at the logs dir: %v", captured.args)
	}
	if !strings.Contains(rec.Body.String(), "opened") {
		t.Errorf("response body lacks opened field: %q", rec.Body.String())
	}
}

// TestLogs_ServerNamedOpenFolder pins the regression Codex bot
// flagged on PR #47: the open-logs-folder route lives at the
// SIBLING path /api/logs-folder, not under /api/logs/, so a
// manifest legitimately named "open-folder" can still hit
// /api/logs/open-folder for its log file. Without this contract
// the prefix-vs-exact-match shadow would 404 the manifest's logs.
func TestLogs_ServerNamedOpenFolder(t *testing.T) {
	fl := &fakeLogs{body: "open-folder log line\n"}
	s := NewServer(Config{})
	s.logs = fl
	req := httptest.NewRequest(http.MethodGet, "/api/logs/open-folder?tail=10", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("server-named-open-folder logs request: status = %d", rec.Code)
	}
	if fl.gotServer != "open-folder" {
		t.Errorf("server name not forwarded: got %q want open-folder", fl.gotServer)
	}
}

// TestLogs_OpenFolderRejectsGet pins the method gate — same-origin
// POST is the only accepted shape so the route does not become a
// drive-by GET that fires on a malicious page navigation.
func TestLogs_OpenFolderRejectsGet(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/logs-folder", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
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

// TestStreamLogs_DoesNotEmitOrAdvanceCursorOnPlaceholder verifies the stream
// stays quiet until the provider sees real bytes.
func TestStreamLogs_DoesNotEmitOrAdvanceCursorOnPlaceholder(t *testing.T) {
	// Exact placeholder body for (server=test, daemon=default).
	realLog := "first-line\nsecond-line\n"
	fl := &scriptedLogs{seq: []api.DaemonLogPoll{
		{}, // prime while the log is absent
		{}, // still absent
		{Offset: int64(len(realLog)), Suffix: realLog},
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

// TestStreamLogs_PreservesPartialLineAcrossTicks pins the R22 fix to
// streamLogs's per-tick split. Daemons commonly write one logical line
// in multiple writes — for example "Loading..." on one write and
// " done\n" on the next. Before R22 the per-tick split on "\n" emitted
// the unterminated "Loading..." fragment as its own log-line event,
// then the continuation " done" as a second event. The UI saw two
// corrupted events instead of the intended single "Loading... done"
// line.
//
// The fix carries any unterminated trailing fragment from one tick's
// suffix into the next tick's prepend via pendingLine. This test
// scripts the exact sequence:
//
//  1. prime: empty file (lastLen seeded at 0; pendingLine empty).
//  2. tick 1: "Loading..." — no newline, must NOT be emitted; held as
//     pendingLine for the next tick.
//  3. tick 2: "Loading... done\n" — len grew, suffix=" done\n" is
//     prepended with pendingLine → "Loading... done\n", split yields
//     one complete line emitted as a single log-line event.
//
// The test fails if the SSE stream contains an event whose data line
// is exactly "Loading..." (the premature partial), and passes once the
// merged "Loading... done" appears.
func TestStreamLogs_PreservesPartialLineAcrossTicks(t *testing.T) {
	fl := &scriptedLogs{seq: []api.DaemonLogPoll{
		{},
		{Offset: int64(len("Loading...")), Suffix: "Loading..."},
		{Offset: int64(len("Loading... done\n")), Suffix: " done\n"},
	}}

	s := NewServer(Config{})
	s.logs = fl

	rec := newSSERecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/test/stream", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for the merged "Loading... done" event. 1800ms is comfortably
	// above the two 500ms ticks plus the prime; the test cancels as
	// soon as the merge is observed so a passing run exits early.
	deadline := time.Now().Add(1800 * time.Millisecond)
	sawMerged := false
	for time.Now().Before(deadline) {
		body := rec.body()
		if strings.Contains(body, "Loading... done") {
			// Confirm the partial "Loading..." was NOT emitted on its
			// own. The SSE framing is "data: <payload>\n\n", so the
			// exact byte sequence "data: Loading...\n\n" is only
			// produced when the unterminated fragment was flushed
			// prematurely. The merged line emits as
			// "data: Loading... done\n\n" — a different substring.
			if strings.Contains(body, "data: Loading...\n\n") {
				cancel()
				<-done
				t.Fatalf("partial line emitted prematurely; body: %q", body)
			}
			sawMerged = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if !sawMerged {
		t.Fatalf("never saw merged line; body: %q", rec.body())
	}
}
