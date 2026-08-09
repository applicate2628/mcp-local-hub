package cmakegraph

import "testing"

func TestR21LineIndexMatchesOffsetsWithoutPerEdgeRescan(t *testing.T) {
	data := []byte("first\nsecond\nthird")
	index := buildLineIndex(data)
	for offset, want := range map[int]int{0: 1, 5: 1, 6: 2, 12: 2, 13: 3, len(data): 3} {
		if got := lineFromIndex(index, offset); got != want {
			t.Fatalf("lineFromIndex(offset=%d)=%d, want %d", offset, got, want)
		}
	}
}
