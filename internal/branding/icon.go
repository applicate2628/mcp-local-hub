// Package branding owns the mcphub visual identity assets — currently
// the canonical hub-and-clients icon shape used by:
//
//   - internal/tray/icons.go — 16×16 PNG state indicators in the
//     Windows tray (colored per TrayState).
//   - cmd/mcphub/mcphub.ico — multi-resolution Windows .exe icon
//     visible in Alt+Tab, taskbar, and Explorer (generated once by
//     tools/genicon).
//   - internal/gui/frontend/public/favicon.ico (future) — browser
//     tab indicator for the local Dashboard.
//
// Centralizing the shape here ensures one source of truth for the
// project's visual identity. Color is a per-call parameter so the
// same shape renders in green (healthy), amber (partial), red (down)
// for tray states AND in a single brand color (#1a7f37 green by
// default) for the .exe icon.
package branding

import (
	"image"
	"image/color"
	"image/draw"
)

// BrandColor is the canonical mcphub brand color. Matches the tray
// "healthy" state — green conveys "things are working" which is the
// expected default a Windows operator sees in their taskbar.
var BrandColor = color.RGBA{R: 0x1a, G: 0x7f, B: 0x37, A: 0xff}

// DrawHubMark paints the mcphub hub-and-clients shape into img.
// Shape design (16×16 base grid):
//
//	. . . . . . . . . . . . . . . .
//	. . . . . . . . . . . . . . . .
//	. . # # . . . . . . . . # # . .   ← corner client (top-left + top-right)
//	. . # # . . . . . . . . # # . .
//	. . . . # . . . . . . # . . . .   ← diagonal connection
//	. . . . . # . . . . # . . . . .
//	. . . . . . # # # # . . . . . .   ← central hub (4×4)
//	. . . . . . # # # # . . . . . .
//	. . . . . . # # # # . . . . . .
//	. . . . . . # # # # . . . . . .
//	. . . . . # . . . . # . . . . .
//	. . . . # . . . . . . # . . . .
//	. . # # . . . . . . . . # # . .   ← corner client (bottom-left + bottom-right)
//	. . # # . . . . . . . . # # . .
//	. . . . . . . . . . . . . . . .
//	. . . . . . . . . . . . . . . .
//
// Scaling: the function computes a scale factor s = img.Width / 16
// and renders each design-grid cell as an s×s block. Integer scaling
// preserves crisp edges — critical for tray icons where Windows DPI
// rescale amplifies any anti-aliasing artifacts. Canvas widths that
// are not exact multiples of 16 render at the largest integer scale
// that fits, leaving a small right/bottom margin.
//
// No anti-aliasing is applied. The shape is hard-edged by design;
// callers that want soft edges should rasterize at a higher
// resolution and downscale themselves.
func DrawHubMark(img *image.RGBA, col color.RGBA) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	s := w / 16
	if s < 1 {
		s = 1
	}
	block := func(gx, gy, gw, gh int) {
		for y := gy * s; y < (gy+gh)*s; y++ {
			for x := gx * s; x < (gx+gw)*s; x++ {
				if x < w && y < h {
					img.SetRGBA(x, y, col)
				}
			}
		}
	}
	// Central 4×4 hub at design grid (6,6)-(9,9).
	block(6, 6, 4, 4)
	// Four corner clients (2×2 each).
	block(2, 2, 2, 2)
	block(12, 2, 2, 2)
	block(2, 12, 2, 2)
	block(12, 12, 2, 2)
	// Diagonal connection pixels from corner clients toward hub.
	diagonals := [][2]int{
		{4, 4}, {5, 5}, // top-left → hub
		{11, 4}, {10, 5}, // top-right → hub
		{4, 11}, {5, 10}, // bottom-left → hub
		{11, 11}, {10, 10}, // bottom-right → hub
	}
	for _, d := range diagonals {
		block(d[0], d[1], 1, 1)
	}
}

// NewHubMarkImage allocates a fresh transparent RGBA canvas of the
// given size and paints the hub mark in col. Convenience wrapper for
// callers that don't already hold an *image.RGBA.
func NewHubMarkImage(size int, col color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), image.Transparent, image.Point{}, draw.Src)
	DrawHubMark(img, col)
	return img
}
