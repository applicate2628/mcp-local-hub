package gui

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExportConfigBundleHandler_ContentTypeAndDisposition(t *testing.T) {
	srv := newDaemonsTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/export-config-bundle", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", rec.Header().Get("Content-Type"))
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="mcphub-bundle-`) {
		t.Errorf("Content-Disposition = %q, missing mcphub-bundle prefix", cd)
	}
}

func TestExportConfigBundleHandler_BodyIsValidZip(t *testing.T) {
	srv := newDaemonsTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/export-config-bundle", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("response body is not a valid zip: %v", err)
	}
}
