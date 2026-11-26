package tray

import (
	"bytes"
	"image/color"
	"image/png"
	"runtime"
	"sync"

	"mcp-local-hub/internal/branding"
)

// IconBytes returns the bytes for a 16×16 state indicator in the
// format the host OS's tray surface expects:
//
//   - Windows: ICO container wrapping the PNG payload. The Win32
//     CreateIconFromResourceEx path used by tray_windows.go takes
//     ICO-format bytes; raw PNG is silently rejected by
//     Shell_NotifyIcon → empty tray square is the user-visible
//     symptom of getting this wrong.
//   - macOS / Linux: raw PNG bytes (kept for parity / future use;
//     the runtime tray is Windows-only today).
//
// Lazy-generated once per state and cached. The generated icon is
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
	// icon so a regression that adds a new TrayState without an entry
	// doesn't crash the tray loop. We do NOT reset iconCache.once —
	// buildIconCache already populated bytes[StateHealthy] on the
	// first .Do, and a reset would (a) race any concurrent .Do call,
	// (b) re-run buildIconCache pointlessly. Sonnet review on PR #24
	// P2 (icons.go:47).
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
//
// StateHealthy reads branding.BrandColor (the project's canonical
// green) so a future brand-color tweak propagates to the tray
// healthy icon AND the .exe / favicon icons in one place. The
// amber/gray/red entries are tray-specific state indicators with
// no brand equivalent — they are tray-owned and correctly inlined.
// Per D:\dev\rules\architectural-cleanliness.md: one canonical
// owner per value.
var stateColors = map[TrayState]color.RGBA{
	StateHealthy: branding.BrandColor,                  // canonical brand green
	StatePartial: {R: 0xbf, G: 0x87, B: 0x00, A: 0xff}, // amber (warning)
	StateDown:    {R: 0x57, G: 0x60, B: 0x6a, A: 0xff}, // gray (idle)
	StateError:   {R: 0xcf, G: 0x22, B: 0x2e, A: 0xff}, // red (danger)
}

func buildIconCache() {
	iconCache.bytes = make(map[TrayState]([]byte), len(stateColors))
	for state, col := range stateColors {
		png := renderStateIcon(col)
		if runtime.GOOS == "windows" {
			iconCache.bytes[state] = branding.PackPNGICO(branding.PNGIcon{Width: 16, Height: 16, PNG: png})
		} else {
			iconCache.bytes[state] = png
		}
	}
}

// renderStateIcon paints the mcphub hub-mark in the given color onto
// a 16×16 transparent canvas. The mark is a "hub-and-clients" motif:
// a central 4×4 filled square (the hub) plus four 2×2 satellite dots
// at the corners (the connected clients) plus diagonal connection
// pixels. The shape encodes the project's identity at the smallest
// tray size while staying readable at any DPI scale Windows rescales
// to.
//
// Replaced the placeholder filled-disc per user request "и для
// mcphub нужен лого/значок". The same shape is rasterized at
// 16/32/48/256 px for the .exe icon (cmd/mcphub/mcphub.ico, see
// tools/genicon/main.go) so tray, taskbar, Alt+Tab, and Explorer
// all show one consistent visual identity.
//
// No anti-aliasing: the shape is intentionally hard-edged so it
// stays crisp after Windows DPI rescale, which produces visible
// blur on thin AA pixels at small tray sizes.
func renderStateIcon(col color.RGBA) []byte {
	img := branding.NewHubMarkImage(16, col)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
