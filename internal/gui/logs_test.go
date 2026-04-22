package gui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
