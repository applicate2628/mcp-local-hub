package tray

import (
	"bytes"
	"image/png"
	"testing"
)

// TestIconBytes_AllStatesHaveDistinctIcons asserts each TrayState
// produces a non-empty PNG and that no two states emit identical
// bytes (a regression that wired the same color twice would render
// as a tray that never actually changes when state changes).
func TestIconBytes_AllStatesHaveDistinctIcons(t *testing.T) {
	states := []TrayState{StateHealthy, StatePartial, StateDown, StateError}
	seen := make(map[string]TrayState, len(states))
	for _, s := range states {
		b := IconBytes(s)
		if len(b) == 0 {
			t.Errorf("%s: IconBytes returned empty", s)
			continue
		}
		key := string(b)
		if dup, exists := seen[key]; exists {
			t.Errorf("%s and %s produced identical bytes (regression: state→color map must be one-to-one)",
				s, dup)
		}
		seen[key] = s
	}
}

// TestIconBytes_DecodesAsPNG asserts the bytes are a valid PNG that
// the systray library can hand to Windows. Decoding via image/png
// catches a regression where the encoder writes truncated or
// malformed bytes.
func TestIconBytes_DecodesAsPNG(t *testing.T) {
	for _, s := range []TrayState{StateHealthy, StatePartial, StateDown, StateError} {
		b := IconBytes(s)
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: png.Decode failed: %v", s, err)
			continue
		}
		bounds := img.Bounds()
		if bounds.Dx() != 16 || bounds.Dy() != 16 {
			t.Errorf("%s: bounds = %dx%d, want 16x16", s, bounds.Dx(), bounds.Dy())
		}
	}
}

// TestIconBytes_LazyCacheReusesBytes asserts the second call for
// the same state returns the same byte slice (cached) — proves the
// generation cost is one-time, not per-event.
func TestIconBytes_LazyCacheReusesBytes(t *testing.T) {
	a := IconBytes(StateHealthy)
	b := IconBytes(StateHealthy)
	// Compare slice headers (pointer + length) via &slice[0]; a
	// freshly-allocated copy would have a different backing array.
	if &a[0] != &b[0] {
		t.Error("two calls to IconBytes(Healthy) returned different backing arrays — cache is not reusing")
	}
}
