package publicresult

import (
	"encoding/json"
	"strings"
	"testing"
)

type measuredResult struct {
	Text string `json:"text"`
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
