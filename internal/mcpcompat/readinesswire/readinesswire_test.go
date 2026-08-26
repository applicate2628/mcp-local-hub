package readinesswire

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

type failIfRead struct{ t *testing.T }

func (r failIfRead) Read([]byte) (int, error) {
	r.t.Fatal("decoder read a non-readiness content type")
	return 0, nil
}

func canonicalFailure() Failure {
	return Failure{
		FailureID:          FailureBackingProtocolUnsupported,
		Stage:              StageInitialize,
		HTTPStatus:         502,
		Retryable:          false,
		ManifestName:       "wolfram",
		DaemonName:         "default",
		RequestedProtocol:  "2025-03-26",
		NegotiatedProtocol: "2024-11-05",
		SupportedFloor:     "2025-03-26",
		ProfileID:          "",
	}
}

func TestFailureV1CanonicalRoundTrip(t *testing.T) {
	failure := canonicalFailure()
	w := httptest.NewRecorder()
	if err := WriteFailure(w, failure); err != nil {
		t.Fatalf("WriteFailure: %v", err)
	}
	if got := w.Code; got != 502 {
		t.Fatalf("status = %d, want 502", got)
	}
	if got := w.Header().Get("Content-Type"); got != MediaTypeV1 {
		t.Fatalf("content type = %q, want %q", got, MediaTypeV1)
	}
	wantBody := "{\"schema_version\":\"mcp-readiness-failure-v1\",\"failure_id\":\"MCP_BACKING_PROTOCOL_UNSUPPORTED\",\"stage\":\"initialize\",\"http_status\":502,\"retryable\":false,\"manifest_name\":\"wolfram\",\"daemon_name\":\"default\",\"requested_protocol\":\"2025-03-26\",\"negotiated_protocol\":\"2024-11-05\",\"supported_floor\":\"2025-03-26\",\"profile_id\":\"\"}\n"
	if got := w.Body.String(); got != wantBody {
		t.Fatalf("body mismatch\n got: %q\nwant: %q", got, wantBody)
	}
	decoded := DecodeFailureResponse(w.Code, w.Header().Get("Content-Type"), bytes.NewReader(w.Body.Bytes()))
	if decoded != failure {
		t.Fatalf("decoded = %#v, want %#v", decoded, failure)
	}
}

func TestFailureV1DiagnosticUnsupportedProtocolRoundTrip(t *testing.T) {
	failure := canonicalFailure()
	failure.NegotiatedProtocol = "2024-10-07"
	body, err := EncodeFailure(failure)
	if err != nil {
		t.Fatalf("EncodeFailure: %v", err)
	}
	decoded := DecodeFailureResponse(failure.HTTPStatus, MediaTypeV1, bytes.NewReader(body))
	if decoded != failure {
		t.Fatalf("decoded = %#v, want %#v", decoded, failure)
	}
}

func TestFailureV1LegacyFallbackIsExactAndBounded(t *testing.T) {
	body := "initialize negotiated unsupported protocol version \"2024-11-05\"\n"
	got := DecodeFailureResponse(502, "text/plain; charset=utf-8", strings.NewReader(body))
	if got.FailureID != FailureBackingProtocolUnsupported || got.Stage != StageInitialize || got.NegotiatedProtocol != "2024-11-05" || got.HTTPStatus != 502 {
		t.Fatalf("legacy decode = %#v", got)
	}
	for _, bad := range []string{strings.TrimSuffix(body, "\n"), strings.Replace(body, "2024-11-05", "2099-01-01", 1), strings.Repeat("x", 257)} {
		got := DecodeFailureResponse(502, "text/plain; charset=utf-8", strings.NewReader(bad))
		if got.FailureID != FailureHTTPErrorBodyUnusable || got.HTTPStatus != 502 || got.ManifestName != "" || got.NegotiatedProtocol != "" {
			t.Fatalf("bad legacy body retained or misclassified: %#v", got)
		}
	}
}

func TestFailureV1InvalidClaimedWireFailsClosedWithoutRawContent(t *testing.T) {
	valid, err := EncodeFailure(canonicalFailure())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		status      int
		contentType string
		body        []byte
	}{
		{"status mismatch", 503, MediaTypeV1, valid},
		{"unknown key", 502, MediaTypeV1, bytes.Replace(valid, []byte("\"profile_id\":"), []byte("\"unknown\":\"x\",\"profile_id\":"), 1)},
		{"duplicate key", 502, MediaTypeV1, bytes.Replace(valid, []byte("\"profile_id\":"), []byte("\"stage\":\"initialize\",\"profile_id\":"), 1)},
		{"non canonical whitespace", 502, MediaTypeV1, bytes.Replace(valid, []byte("{\"schema"), []byte("{ \"schema"), 1)},
		{"extra media parameter", 502, MediaTypeV1 + "; x=y", valid},
		{"oversize", 502, MediaTypeV1, bytes.Repeat([]byte("x"), MaxBodyBytes+1)},
		{"malformed utf8", 502, MediaTypeV1, []byte{0xff, '\n'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeFailureResponse(tc.status, tc.contentType, bytes.NewReader(tc.body))
			if got.FailureID != FailureWireInvalid || got.HTTPStatus != tc.status {
				t.Fatalf("decode = %#v", got)
			}
			if got.ManifestName != "" || got.DaemonName != "" || got.NegotiatedProtocol != "" || got.ProfileID != "" {
				t.Fatalf("invalid wire retained content: %#v", got)
			}
		})
	}
}

func TestFailureV1WriterRejectsUnregisteredOrUnboundedValuesBeforeCommit(t *testing.T) {
	for _, mutate := range []func(*Failure){
		func(f *Failure) { f.FailureID = "UNREGISTERED" },
		func(f *Failure) { f.HTTPStatus = 200 },
		func(f *Failure) { f.ManifestName = strings.Repeat("a", 129) },
		func(f *Failure) { f.ManifestName = "path/name" },
		func(f *Failure) { f.RequestedProtocol = "2099-01-01" },
		func(f *Failure) { f.Stage = "other" },
	} {
		failure := canonicalFailure()
		mutate(&failure)
		w := httptest.NewRecorder()
		if err := WriteFailure(w, failure); err == nil {
			t.Fatalf("WriteFailure(%#v) succeeded", failure)
		}
		if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
			t.Fatalf("writer committed before validation: code=%d header=%q body=%q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
}

func TestFailureV1WrongContentTypeIsClassifiedWithoutReadingStreamingBody(t *testing.T) {
	got := DecodeFailureResponse(500, "text/event-stream", failIfRead{t})
	if got.FailureID != FailureHTTPErrorBodyUnusable || got.HTTPStatus != 500 {
		t.Fatalf("decode = %#v", got)
	}
}
