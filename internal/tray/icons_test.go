package tray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"runtime"
	"testing"
)

// TestIconBytes_AllStatesHaveDistinctIcons asserts each TrayState
// produces a non-empty payload and that no two states emit
// identical bytes (a regression that wired the same color twice
// would render as a tray that never actually changes when state
// changes).
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

// TestIconBytes_FormatPerOS asserts the output format matches the
// systray library's expectation for the host OS:
//
//   - Windows: ICO header (22-byte ICONDIR + ICONDIRENTRY) followed
//     by a PNG payload. Without the ICO wrap, Shell_NotifyIcon
//     silently rejects the bytes and the tray shows an empty
//     square — the user-visible regression observed on PR #22
//     pre-fix.
//   - Other OS: raw PNG bytes (image/png decodable to 16×16).
//
// Both branches assert the embedded PNG is a valid 16×16 image so
// a regression in renderStateIcon also surfaces here.
func TestIconBytes_FormatPerOS(t *testing.T) {
	for _, s := range []TrayState{StateHealthy, StatePartial, StateDown, StateError} {
		b := IconBytes(s)
		var pngBody []byte
		if runtime.GOOS == "windows" {
			if len(b) < 22 {
				t.Errorf("%s: ICO output too short (%d bytes, need ≥22 header)", s, len(b))
				continue
			}
			// ICONDIR.type at offset 2-3 must be 1 (icon).
			if got := binary.LittleEndian.Uint16(b[2:4]); got != 1 {
				t.Errorf("%s: ICO type = %d, want 1", s, got)
			}
			// ICONDIR.count at offset 4-5 must be 1 (one image).
			if got := binary.LittleEndian.Uint16(b[4:6]); got != 1 {
				t.Errorf("%s: ICO count = %d, want 1", s, got)
			}
			// ICONDIRENTRY width/height at offsets 6/7.
			if b[6] != 16 || b[7] != 16 {
				t.Errorf("%s: ICO dimensions = %dx%d, want 16x16", s, b[6], b[7])
			}
			// imageOffset at offset 18-21 must be 22 (header size).
			if got := binary.LittleEndian.Uint32(b[18:22]); got != 22 {
				t.Errorf("%s: ICO imageOffset = %d, want 22", s, got)
			}
			pngBody = b[22:]
		} else {
			pngBody = b
		}
		img, err := png.Decode(bytes.NewReader(pngBody))
		if err != nil {
			t.Errorf("%s: PNG payload decode failed: %v", s, err)
			continue
		}
		bounds := img.Bounds()
		if bounds.Dx() != 16 || bounds.Dy() != 16 {
			t.Errorf("%s: PNG bounds = %dx%d, want 16x16", s, bounds.Dx(), bounds.Dy())
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
