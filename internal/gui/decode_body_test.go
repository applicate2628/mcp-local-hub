package gui

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helperPayload is the decode target for the direct unit tests below.
type helperPayload struct {
	Name string `json:"name"`
}

// runDecodeHelper drives decodeJSONBodyLimited through a throwaway handler so
// the http.MaxBytesReader path (which needs a real ResponseWriter) is exercised
// exactly as production does, and returns the classified outcome.
func runDecodeHelper(body string, maxBytes int64) (outcome string) {
	h := func(w http.ResponseWriter, r *http.Request) {
		var p helperPayload
		err := decodeJSONBodyLimited(w, r, &p, maxBytes)
		switch {
		case err == nil:
			outcome = "ok"
		case errors.Is(err, errBodyTooLarge):
			outcome = "too_large"
		case errors.Is(err, errBodyTrailingData):
			outcome = "trailing"
		default:
			outcome = "bad_json"
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return outcome
}

func TestDecodeJSONBodyLimited_Classification(t *testing.T) {
	const cap = 1024
	cases := []struct {
		name string
		body string
		want string
	}{
		{"normal small value", `{"name":"x"}`, "ok"},
		{"normal with leading/trailing whitespace", "  \n{\"name\":\"x\"}\n  ", "ok"},
		{"empty body", ``, "bad_json"},
		{"malformed json", `{not json`, "bad_json"},
		{"oversized single value", `{"name":"` + strings.Repeat("A", 4000) + `"}`, "too_large"},
		{"valid plus small trailing garbage", `{"name":"x"}GARBAGE`, "trailing"},
		{"valid plus second json value", `{"name":"x"}{"name":"y"}`, "trailing"},
		{"valid plus huge trailing garbage", `{"name":"x"}` + strings.Repeat("Z", 8000), "trailing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runDecodeHelper(tc.body, cap); got != tc.want {
				t.Fatalf("body=%q: got %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestDecodeJSONBodyLimited_StatusMapping pins the sentinel→status mapping the
// handlers rely on (413 for the cap, 400 for everything else).
func TestDecodeJSONBodyLimited_StatusMapping(t *testing.T) {
	if got := decodeBodyStatusCode(errBodyTooLarge); got != http.StatusRequestEntityTooLarge {
		t.Errorf("too-large status = %d, want 413", got)
	}
	if got := decodeBodyStatusCode(errBodyTrailingData); got != http.StatusBadRequest {
		t.Errorf("trailing status = %d, want 400", got)
	}
	if got := decodeBodyStatusCode(errors.New("syntax error")); got != http.StatusBadRequest {
		t.Errorf("bad-json status = %d, want 400", got)
	}
	if code := decodeBodyErrorCode(errBodyTooLarge); code != "body_too_large" {
		t.Errorf("too-large code = %q, want body_too_large", code)
	}
	if code := decodeBodyErrorCode(errors.New("x")); code != "bad_json" {
		t.Errorf("bad-json code = %q, want bad_json", code)
	}
}

// TestControlEndpoint_OversizedBodyRejected drives a real control-cap handler
// (POST /api/dismiss, maxControlBodyBytes) end-to-end: an oversized body is
// rejected with 413 and never reaches the dismisser, while a normal body still
// works.
func TestControlEndpoint_OversizedBodyRejected(t *testing.T) {
	t.Run("oversized -> 413", func(t *testing.T) {
		fake := &fakeDismisser{}
		s := NewServer(Config{Port: 0})
		s.dismisser = fake
		oversized := `{"server":"` + strings.Repeat("A", int(maxControlBodyBytes)+4096) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/dismiss", strings.NewReader(oversized))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%q", rec.Code, rec.Body.String())
		}
		if len(fake.got) != 0 {
			t.Errorf("dismisser was called with %v; oversized body must be rejected before the handler body", fake.got)
		}
	})

	t.Run("valid plus megabyte trailing garbage -> rejected", func(t *testing.T) {
		fake := &fakeDismisser{}
		s := NewServer(Config{Port: 0})
		s.dismisser = fake
		body := `{"server":"fetch"}` + strings.Repeat("Z", 1<<20)
		req := httptest.NewRequest(http.MethodPost, "/api/dismiss", strings.NewReader(body))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		// Trailing garbage past a small valid value is rejected as a 400 (the
		// streaming decoder fails on the first trailing token before reaching the
		// cap). Either way it must NOT be a 2xx and must NOT reach the handler.
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 400 or 413; body=%q", rec.Code, rec.Body.String())
		}
		if len(fake.got) != 0 {
			t.Errorf("dismisser was called with %v; trailing-garbage body must be rejected", fake.got)
		}
	})

	t.Run("normal body still works -> 204", func(t *testing.T) {
		fake := &fakeDismisser{}
		s := NewServer(Config{Port: 0})
		s.dismisser = fake
		req := httptest.NewRequest(http.MethodPost, "/api/dismiss", strings.NewReader(`{"server":"fetch"}`))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%q", rec.Code, rec.Body.String())
		}
		if len(fake.got) != 1 || fake.got[0] != "fetch" {
			t.Errorf("dismisser got=%v, want [fetch]", fake.got)
		}
	})
}

// TestManifestEndpoint_BodyCap drives a real manifest-cap handler (POST
// /api/manifest/create, maxManifestBodyBytes) end-to-end: an oversized body is
// rejected with 413 and never reaches the creator, while a normal manifest body
// still creates.
func TestManifestEndpoint_BodyCap(t *testing.T) {
	t.Run("oversized -> 413", func(t *testing.T) {
		create := &fakeManifestCreator{}
		s := newManifestTestServer(create, &fakeManifestValidator{})
		oversizedYAML := strings.Repeat("a: b\n", (int(maxManifestBodyBytes)/5)+4096)
		body := `{"name":"demo","yaml":` + jsonQuote(oversizedYAML) + `}`
		req := httptest.NewRequest(http.MethodPost, "/api/manifest/create", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%q", rec.Code, truncate(rec.Body.String(), 200))
		}
		if create.name != "" || create.yaml != "" {
			t.Errorf("manifest creator was called (name=%q); oversized body must be rejected first", create.name)
		}
	})

	t.Run("normal manifest body still works -> 200", func(t *testing.T) {
		create := &fakeManifestCreator{}
		s := newManifestTestServer(create, &fakeManifestValidator{})
		rec := postJSON(t, s, "/api/manifest/create", `{"name":"demo","yaml":"name: demo\nkind: global\n"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if create.name != "demo" {
			t.Errorf("creator name=%q, want demo", create.name)
		}
	})

	// A manifest a few-KiB large (well under the 1 MiB cap) must NOT be clipped —
	// guards against the cap being set too tight for legitimate large configs.
	t.Run("few-KiB manifest under cap still works", func(t *testing.T) {
		create := &fakeManifestCreator{}
		s := newManifestTestServer(create, &fakeManifestValidator{})
		largeButValid := "name: demo\nkind: global\n" + strings.Repeat("# padding comment line\n", 200) // ~4.6 KiB
		body := `{"name":"demo","yaml":` + jsonQuote(largeButValid) + `}`
		rec := postJSON(t, s, "/api/manifest/create", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; a few-KiB manifest must not be clipped; body=%q", rec.Code, rec.Body.String())
		}
	})
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
