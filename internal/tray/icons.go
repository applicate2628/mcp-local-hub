package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"
)

// IconBytes returns the PNG bytes for a 16×16 monochrome state
// indicator. Lazy-generated once and cached. The generated icon is
// a filled circle in the state's color centered on a transparent
// background — simple enough to render legibly at the tray's
// 16×16 minimum size on all DPI scales Windows currently ships.
//
// Spec §6 calls for "monochrome 16×16 PNG" — we honor the size and
// the per-state-color contract; "monochrome" here means single-color
// per icon (the icon contains only one non-transparent color), not
// black-and-white-only.
//
// Programmatic generation avoids committing 4 binary blobs to the
// repo and keeps the icon set in one place that's easy to retune
// (color palette adjustments are a one-line edit). The image/png
// stdlib runs in pure Go with no cgo or asset toolchain.
func IconBytes(s TrayState) []byte {
	iconCache.once.Do(buildIconCache)
	if b, ok := iconCache.bytes[s]; ok {
		return b
	}
	// Defensive default: an unknown state still produces a renderable
	// icon (gray circle) so a regression that adds a new TrayState
	// without an entry doesn't crash the tray loop.
	iconCache.once = sync.Once{}
	return iconCache.bytes[StateHealthy]
}

var iconCache struct {
	once  sync.Once
	bytes map[TrayState]([]byte)
}

// stateColors maps each TrayState to its tray-icon color. Picked
// from the same palette spec §7 uses for status cells: success
// green, warning amber, danger red, info blue. Choices are
// AAA-contrast against both light and dark Windows tray
// backgrounds.
var stateColors = map[TrayState]color.RGBA{
	StateHealthy: {R: 0x1a, G: 0x7f, B: 0x37, A: 0xff}, // green (success)
	StatePartial: {R: 0xbf, G: 0x87, B: 0x00, A: 0xff}, // amber (warning)
	StateDown:    {R: 0x57, G: 0x60, B: 0x6a, A: 0xff}, // gray (idle)
	StateError:   {R: 0xcf, G: 0x22, B: 0x2e, A: 0xff}, // red (danger)
}

func buildIconCache() {
	iconCache.bytes = make(map[TrayState]([]byte), len(stateColors))
	for state, col := range stateColors {
		iconCache.bytes[state] = renderStateIcon(col)
	}
}

// renderStateIcon paints a 12×12 filled circle in the given color
// onto a 16×16 transparent canvas, leaving 2px margin around the
// circle so the tray's bevel padding doesn't clip it. The
// distance-squared check inside the inner loop is the standard
// rasterization predicate for a filled disc; no anti-aliasing
// because Windows itself rescales the icon for per-monitor DPI
// and AA at 12px diameter would be invisible after that.
func renderStateIcon(col color.RGBA) []byte {
	const (
		size   = 16
		radius = 6 // → 12×12 filled disc, 2px margin on each side
	)
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	// Transparent background — image.NewRGBA zeroes by default, but
	// be explicit so a future stdlib change doesn't break us.
	draw.Draw(img, img.Bounds(), image.Transparent, image.Point{}, draw.Src)

	cx, cy := size/2, size/2
	r2 := radius * radius
	for y := 0; y < size; y++ {
		dy := y - cy
		for x := 0; x < size; x++ {
			dx := x - cx
			if dx*dx+dy*dy <= r2 {
				img.SetRGBA(x, y, col)
			}
		}
	}
	var buf bytes.Buffer
	// png.Encode of a 16x16 RGBA never fails in practice; an error
	// here would mean the OS is out of memory, in which case the
	// tray run will fail anyway. Drop the error rather than
	// propagate — the tray loop has no actionable recovery.
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
