package publicresult

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type measuredResult struct {
	Text string `json:"text"`
}

type countedAdmissionValue struct{ calls *int }

func (v countedAdmissionValue) MarshalJSON() ([]byte, error) {
	(*v.calls)++
	return []byte(`"tail"`), nil
}

func TestProjectionAdmissionDoesNotMarshalTailAfterSaturation(t *testing.T) {
	admission := NewProjectionAdmission(1)
	if !admission.AddJSON("too-large") {
		t.Fatal("first value did not saturate admission")
	}
	calls := 0
	if !admission.AddJSON(countedAdmissionValue{calls: &calls}) {
		t.Fatal("saturated admission became open")
	}
	if calls != 0 {
		t.Fatalf("tail MarshalJSON calls = %d, want 0", calls)
	}
}

func (r measuredResult) PublicResultRequiresProjection(int) bool { return false }

type admissionResult struct {
	fullMarshalCalls *int
}

var _ Projectable = admissionResult{}

func (r admissionResult) PublicResultProjection() any {
	return struct {
		ResultProjection Projection `json:"result_projection"`
	}{MinimalProjection("payload")}
}

func (r admissionResult) PublicResultRequiresProjection(int) bool { return true }

func (r admissionResult) MarshalJSON() ([]byte, error) {
	(*r.fullMarshalCalls)++
	return nil, errors.New("full result must not be materialized after pre-admission rejects it")
}

func TestMarshalIndentProjectsBeforeFullMarshalWhenAdmissionRejects(t *testing.T) {
	fullMarshalCalls := 0
	body, err := MarshalIndent(admissionResult{fullMarshalCalls: &fullMarshalCalls})
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if fullMarshalCalls != 0 {
		t.Fatalf("full MarshalJSON calls = %d, want 0", fullMarshalCalls)
	}
	if !strings.Contains(string(body), `"result_projection"`) {
		t.Fatalf("body = %s, want projected result", body)
	}
}

func (r measuredResult) PublicResultProjection() any {
	return struct {
		ResultProjection Projection `json:"result_projection"`
	}{MinimalProjection("text")}
}

func TestMarshalIndentExactEncodedBoundary(t *testing.T) {
	maxText := largestCompleteText(t)
	for _, tc := range []struct {
		name       string
		textLength int
		projected  bool
	}{
		{name: "N-1", textLength: maxText - 1},
		{name: "N", textLength: maxText},
		{name: "N+1", textLength: maxText + 1, projected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := MarshalIndent(measuredResult{Text: strings.Repeat("x", tc.textLength)})
			if err != nil {
				t.Fatal(err)
			}
			if len(body) > MaxEncodedBytes {
				t.Fatalf("encoded bytes=%d, limit=%d", len(body), MaxEncodedBytes)
			}
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			_, gotProjected := decoded["result_projection"]
			if gotProjected != tc.projected {
				t.Fatalf("projected=%v, want %v; bytes=%d", gotProjected, tc.projected, len(body))
			}
		})
	}
}

func largestCompleteText(t *testing.T) int {
	t.Helper()
	low, high := 0, MaxEncodedBytes
	for low < high {
		middle := low + (high-low+1)/2
		body, err := json.MarshalIndent(measuredResult{Text: strings.Repeat("x", middle)}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if len(body) <= MaxEncodedBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}
