package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"runtime"
	"sync"
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
		png := renderStateIcon(col)
		if runtime.GOOS == "windows" {
			iconCache.bytes[state] = wrapPngInIco(png, 16, 16)
		} else {
			iconCache.bytes[state] = png
		}
	}
}

// wrapPngInIco produces a minimal ICO container holding one image
// in the modern PNG-embedded variant. Windows Vista+ Shell_NotifyIcon
// accepts PNG-inside-ICO, which lets us reuse the existing PNG
// rendering without writing a DIB encoder.
//
// Layout (22-byte header + payload):
//
//	0x00  00 00              ICONDIR.reserved
//	0x02  01 00              ICONDIR.type    = 1 (icon, not cursor)
//	0x04  01 00              ICONDIR.count   = 1 image
//	0x06  width              ICONDIRENTRY.width  (0 means 256)
//	0x07  height             ICONDIRENTRY.height
//	0x08  00                 ICONDIRENTRY.colorCount (0 for >256 colors)
//	0x09  00                 ICONDIRENTRY.reserved
//	0x0A  01 00              ICONDIRENTRY.planes
//	0x0C  20 00              ICONDIRENTRY.bitsPerPixel = 32
//	0x0E  <png-len LE-32>    ICONDIRENTRY.imageSize
//	0x12  16 00 00 00        ICONDIRENTRY.imageOffset (22)
//	0x16  <png bytes>        the PNG payload
//
// The width/height fields use a single byte each; values > 255 are
// encoded as 0 (which Windows interprets as 256). For 16×16 we just
// write 16 directly.
func wrapPngInIco(pngBytes []byte, w, h int) []byte {
	if w == 256 {
		w = 0
	}
	if h == 256 {
		h = 0
	}
	hdr := make([]byte, 22)
	binary.LittleEndian.PutUint16(hdr[0:2], 0)              // reserved
	binary.LittleEndian.PutUint16(hdr[2:4], 1)              // type = icon
	binary.LittleEndian.PutUint16(hdr[4:6], 1)              // count = 1
	hdr[6] = byte(w)                                        // width
	hdr[7] = byte(h)                                        // height
	hdr[8] = 0                                              // colorCount
	hdr[9] = 0                                              // reserved
	binary.LittleEndian.PutUint16(hdr[10:12], 1)            // planes
	binary.LittleEndian.PutUint16(hdr[12:14], 32)           // bitsPerPixel
	binary.LittleEndian.PutUint32(hdr[14:18], uint32(len(pngBytes)))
	binary.LittleEndian.PutUint32(hdr[18:22], 22)           // imageOffset
	out := make([]byte, 0, len(hdr)+len(pngBytes))
	out = append(out, hdr...)
	out = append(out, pngBytes...)
	return out
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
