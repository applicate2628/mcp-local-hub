// internal/gui/path_validate.go
//
// GET /api/path/validate?path=<dir> — live filesystem feedback for the
// Settings "Browse…" affordance on TypePath fields (e.g.
// appearance.default_home once it is wired). The GUI is a browser UI
// served by a headless windowsgui-subsystem HTTP server, with the actual
// UI living in a separate Chrome --app process (see browser.go /
// focus_window_windows.go). A native OS folder dialog spawned from this
// server has no parent-window relationship to that browser window, so it
// surfaces unreliably (often behind the browser or as an orphan taskbar
// entry) and would block a request goroutine — which is why we expose a
// READ-ONLY validation endpoint instead. The frontend renders a plain
// path text field with a "Browse…"-adjacent inline status (exists /
// is-directory) driven by this endpoint.
//
// This is a pure read: it os.Stat's the supplied path and never writes,
// deletes, or shells out. It validates the input syntactically (non-empty,
// no embedded control characters) BEFORE touching the filesystem, mirroring
// the TypePath validator in internal/api/settings_registry.go::validate so
// the GUI and CLI agree on what a syntactically-valid path is.

package gui

import (
	"net/http"
	"os"
	"strings"
	"unicode"
)

// pathValidateResponse is the JSON body returned by GET /api/path/validate.
// Both booleans are always emitted (no omitempty) so the frontend never
// has to distinguish "false" from "absent". `error` carries a short reason
// when the path could not be stat'd for a reason other than "does not
// exist" (e.g. a permission error); a plain not-exists is reported as
// exists:false with no error so the UI shows "path does not exist" rather
// than a scary error banner.
type pathValidateResponse struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	IsDir  bool   `json:"is_dir"`
	Error  string `json:"error,omitempty"`
}

// pathHasControlChars mirrors internal/api/settings_registry.go's
// stringHasControlChars: reject C0 controls (U+0000..U+001F), DEL
// (U+007F), and C1 controls (U+0080..U+009F). Embedded control characters
// in a path break CLI output and downstream launch-path consumers, so the
// GUI rejects them at the validation boundary exactly as the registry
// validator does on save.
func pathHasControlChars(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// registerPathValidateRoutes wires GET /api/path/validate onto the mux.
// Read-only endpoint, so it mirrors the existing GET read handlers (the
// settings list handler) by going through requireSameOrigin — that guard
// passes curl / native callers (no Origin / Sec-Fetch-Site headers) and
// only rejects genuine cross-origin browser requests, so a same-origin
// GET from the GUI's own page is unaffected.
func registerPathValidateRoutes(s *Server) {
	s.mux.HandleFunc("/api/path/validate", s.requireSameOrigin(s.pathValidateHandler))
}

// pathValidateHandler handles GET /api/path/validate?path=<dir>.
//
// Status mapping:
//   - 405 MethodNotAllowed — non-GET.
//   - 400 BadRequest        — empty path or path with control characters.
//   - 200 OK                — {path, exists, is_dir[, error]} for every
//                             other case, INCLUDING a path that does not
//                             exist (exists:false). A non-exists result is
//                             a normal validation outcome, not an HTTP
//                             error: the operator is mid-typing and the UI
//                             shows live "✓ directory" / "path not found"
//                             feedback.
func (s *Server) pathValidateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "path required",
			"code":  "PATH_REQUIRED",
		})
		return
	}
	if pathHasControlChars(path) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "path contains control characters",
			"code":  "PATH_INVALID",
		})
		return
	}

	resp := pathValidateResponse{Path: path}
	info, err := os.Stat(path)
	switch {
	case err == nil:
		resp.Exists = true
		resp.IsDir = info.IsDir()
	case os.IsNotExist(err):
		// Not an HTTP error — a not-yet-existing path is a normal
		// "exists:false" validation outcome the UI renders inline.
		resp.Exists = false
	default:
		// A real stat failure (permission denied, path-too-long, etc.).
		// Still 200 with exists:false, but surface the reason so the UI
		// can distinguish "not found" from "could not check". os.Stat
		// already includes the path in its error string.
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
