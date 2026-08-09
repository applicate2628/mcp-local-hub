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

func TestR27ProjectionPreservesShadowingAndNamesEveryOmittedCollection(t *testing.T) {
	result := Result{
		Status:                            evidence.StatusOK,
		OverlayToOverlayShadowingOccurred: true,
		AllCandidates:                     []CandidateLocation{{Directory: strings.Repeat("candidate/", publicresult.MaxEncodedBytes)}},
		Shadows:                           []Shadow{{Directory: "/lower-overlay"}},
		Evidence:                          evidence.Evidence{Paths: []string{"/evidence"}},
	}
	body, err := publicresult.MarshalIndent(result)
	if err != nil {
		t.Fatal(err)
	}
	var projected struct {
		Shadowing  bool                    `json:"overlay_to_overlay_shadowing_occurred"`
		Projection publicresult.Projection `json:"result_projection"`
	}
	if err := json.Unmarshal(body, &projected); err != nil {
		t.Fatal(err)
	}
	if !projected.Shadowing {
		t.Fatal("projected result erased established overlay shadowing")
	}
	want := map[string]bool{"all_candidates": false, "shadows": false, "evidence": false}
	for _, omission := range projected.Projection.Omissions {
		if _, ok := want[omission.Field]; ok {
			want[omission.Field] = true
		}
	}
	for field, present := range want {
		if !present {
			t.Fatalf("projection omissions=%+v, missing %s", projected.Projection.Omissions, field)
		}
	}
}
