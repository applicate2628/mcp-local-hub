package godbolt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGodbolt is a minimal godbolt-like stub that echoes the Accept
// header and payload back so tests can assert we sent the right request
// and still receive a valid JSON response to exercise the parser.
func fakeGodbolt(t *testing.T, gotAccept *string, gotPayload *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAccept = r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, gotPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"asm":[{"text":"ret"}],"stdout":[],"stderr":[]}`))
	}))
}

func TestCompileTool_SendsAcceptJSON(t *testing.T) {
	var gotAccept string
	var gotPayload map[string]interface{}
	srv := fakeGodbolt(t, &gotAccept, &gotPayload)
	defer srv.Close()

	gs := &GodboltServer{httpClient: srv.Client(), baseURL: srv.URL + "/api"}
	out, err := gs.invokeCompile(t.Context(), srv.URL+"/api/compiler/gcc/compile", []byte(`{"source":"int main(){}"}`))
	if err != nil {
		t.Fatalf("invokeCompile: %v", err)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q, want application/json", gotAccept)
	}
	if !strings.Contains(string(out), `"asm":[{"text":"ret"}]`) {
		t.Errorf("response body missing structured asm field: %s", out)
	}
}
