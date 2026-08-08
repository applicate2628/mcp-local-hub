package portresolution

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestPortProjectionBoundsEveryRetainedScalarIndependently(t *testing.T) {
	values := map[string]string{
		"ascii":        strings.Repeat("a", publicresult.MaxEncodedBytes+1024),
		"control":      strings.Repeat("\x00\n\t", publicresult.MaxEncodedBytes/8),
		"invalid_utf8": strings.Repeat(string([]byte{0xff, 0xfe, 'x'}), publicresult.MaxEncodedBytes/3),
		"unicode":      strings.Repeat("界🙂", publicresult.MaxEncodedBytes/4),
	}
	setters := map[string]func(*Result, string){
		"winner.directory":   func(r *Result, v string) { r.Winner = &Winner{Directory: v} },
		"winner.source":      func(r *Result, v string) { r.Winner = &Winner{Source: v} },
		"blocking.directory": func(r *Result, v string) { r.BlockingCandidate = &CandidateLocation{Directory: v} },
		"blocking.source":    func(r *Result, v string) { r.BlockingCandidate = &CandidateLocation{Source: v} },
		"blocking.reason":    func(r *Result, v string) { r.BlockingCandidate = &CandidateLocation{Reason: v} },
		"invalid_root":       func(r *Result, v string) { r.InvalidRoot = v },
		"invalid_port":       func(r *Result, v string) { r.InvalidPort = v },
	}
	for field, set := range setters {
		for shape, value := range values {
			t.Run(field+"/"+shape, func(t *testing.T) {
				result := Result{Status: evidence.StatusUnknown}
				set(&result, value)
				body, err := publicresult.MarshalIndent(result)
				if err != nil {
					t.Fatal(err)
				}
				if len(body) > publicresult.MaxEncodedBytes {
					t.Fatalf("body=%d", len(body))
				}
				if !json.Valid(body) {
					t.Fatal("projected body is invalid JSON")
				}
				if !bytes.Contains(body, []byte("sha256=")) || !bytes.Contains(body, []byte("bytes=")) {
					t.Fatalf("projection lacks deterministic identity: %s", body)
				}
			})
		}
	}
}

func TestPortProjectionCombinedWorstCaseAndLegacyCompatibility(t *testing.T) {
	huge := strings.Repeat("safe-prefix/界", publicresult.MaxEncodedBytes/8) + "user:secret@host"
	result := Result{Status: evidence.StatusUnknown, Winner: &Winner{Directory: huge, Source: huge}, BlockingCandidate: &CandidateLocation{Directory: huge, Source: huge, Reason: huge}, InvalidRoot: huge, InvalidPort: huge}
	body, err := publicresult.MarshalIndent(result)
	if err != nil || len(body) > publicresult.MaxEncodedBytes || !json.Valid(body) {
		t.Fatalf("body=%d err=%v valid=%v", len(body), err, json.Valid(body))
	}
	if bytes.Contains(body, []byte("secret@host")) {
		t.Fatal("projected result retained caller-controlled tail")
	}

	ordinary := Result{Status: evidence.StatusOK, Winner: &Winner{Directory: "/ports/zlib", Source: "overlay-00"}}
	want, err := json.MarshalIndent(ordinary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := publicresult.MarshalIndent(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("under-budget JSON changed\nwant=%s\ngot=%s", want, got)
	}
}
