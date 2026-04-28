package gui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/buildinfo"
)

// TestVersion_GET_ReturnsBuildInfo asserts the /api/version handler
// surfaces the buildinfo store + the static homepage/issues/license
// strings the About screen needs. Using buildinfo.Set in the test
// pins the version field; defaults for unset fields ("dev"/"unknown")
// would mask a regression that swapped the wrong source.
func TestVersion_GET_ReturnsBuildInfo(t *testing.T) {
	// Pin known values for the duration of this test. Restore in
	// defer so other tests see the original "dev"/"unknown" defaults.
	prevV, prevC, prevD := buildinfo.Get()
	defer buildinfo.Set(prevV, prevC, prevD)
	buildinfo.Set("0.99.test", "abcdef0", "2026-04-28T12:00:00Z")

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})

	req := httptest.NewRequest("GET", "/api/version", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var dto versionDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Version != "0.99.test" {
		t.Errorf("Version = %q, want 0.99.test", dto.Version)
	}
	if dto.Commit != "abcdef0" {
		t.Errorf("Commit = %q, want abcdef0", dto.Commit)
	}
	if dto.BuildDate != "2026-04-28T12:00:00Z" {
		t.Errorf("BuildDate = %q", dto.BuildDate)
	}
	// Static fields — assert presence so a regression that drops them
	// from versionDTO surfaces immediately.
	if !strings.Contains(dto.Homepage, "github.com/applicate2628") {
		t.Errorf("Homepage = %q, want github URL", dto.Homepage)
	}
	if dto.License != "Apache-2.0" {
		t.Errorf("License = %q, want Apache-2.0", dto.License)
	}
	// runtime-derived fields: GoVersion + Platform must be non-empty.
	if dto.GoVersion == "" || dto.Platform == "" {
		t.Errorf("GoVersion=%q Platform=%q (both should be non-empty)",
			dto.GoVersion, dto.Platform)
	}
}

// TestVersion_RejectsCrossOrigin guards the requireSameOrigin wrap.
// Same pattern as backups_test.go — if a future refactor accidentally
// drops the wrap, an unauthenticated cross-origin page could probe
// the binary's build metadata without going through the GUI.
func TestVersion_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	req := httptest.NewRequest("GET", "/api/version", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Errorf("cross-origin GET expected 403, got %d", rr.Code)
	}
}

// TestVersion_RejectsNonGet asserts only GET is accepted (read-only
// surface; any future PATCH/PUT/POST should be added explicitly with
// auth checks rather than smuggled in).
func TestVersion_RejectsNonGet(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(method, "/api/version", nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != 405 {
			t.Errorf("%s expected 405, got %d", method, rr.Code)
		}
	}
}
