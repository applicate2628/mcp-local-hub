// tools/genicon — Windows .ico generator for the mcphub brand icon.
//
// Reads the canonical hub-mark shape from internal/branding and
// rasterizes it at 16/32/48/256 px, then packages all four PNG
// payloads into a single Windows .ico container at
// cmd/mcphub/mcphub.ico.
//
// Run once after a shape change:
//
//	go run ./tools/genicon
//
// The generated .ico is checked into the repo (cmd/mcphub/mcphub.ico)
// so the build pipeline (build.sh → go generate → goversioninfo →
// resource.syso → go build) does not need to re-run this tool every
// time. goversioninfo reads cmd/mcphub/mcphub.ico via the IconPath
// field of versioninfo.json and embeds it into resource.syso, which
// the Go linker stitches into mcphub.exe.
//
// Why a separate tool instead of `go generate`-ing every build:
//   - ICO generation has no inputs that change between builds (the
//     shape is committed to internal/branding/icon.go). Re-running
//     it on every build wastes I/O.
//   - The tool depends on internal/branding, which is fine because
//     it's a tool inside the same module — but pulling it into the
//     hot build path would add an import-graph node.
//
// Format: ICONDIR header (6 bytes) + N×16-byte ICONDIRENTRY records
// + N PNG payloads packed sequentially. Vista+ Windows accepts
// PNG-inside-ICO, which lets us reuse the standard image/png encoder
// rather than writing a DIB serializer.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/branding"
)

// iconSizes are the per-resolution rasters Windows expects. 16/32/48
// cover taskbar / Alt+Tab / Start menu / smaller surfaces. 256 covers
// Explorer "Extra large icons" view and the Vista+ jumbo icon path.
// Skipping 128 because the Win32 size-selection picks the closest
// match from this set and 128 adds bytes for negligible coverage.
var iconSizes = []int{16, 32, 48, 256}

// outPaths lists every location that needs the .ico. One canonical
// build step writes the same bytes to each — single source of truth
// for the visual identity, no duplicate maintenance.
var outPaths = []string{
	filepath.Join("cmd", "mcphub", "mcphub.ico"),                            // Windows .exe embedded icon
	filepath.Join("internal", "gui", "frontend", "public", "favicon.ico"),  // Vite-served favicon
}

func main() {
	// One positional arg = override; no args = write all canonical
	// destinations from outPaths.
	targets := outPaths
	if len(os.Args) > 1 {
		targets = []string{os.Args[1]}
	}

	icoBytes, err := buildICO(iconSizes, branding.BrandColor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genicon: build ico: %v\n", err)
		os.Exit(1)
	}
	for _, p := range targets {
		// Ensure parent dir exists. Vite's public/ dir does not exist
		// in fresh repos and mkdir-all is a no-op when it does.
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "genicon: mkdir parent of %s: %v\n", p, err)
			os.Exit(1)
		}
		if err := os.WriteFile(p, icoBytes, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "genicon: write %s: %v\n", p, err)
			os.Exit(1)
		}
		fmt.Printf("genicon: wrote %s (%d sizes, %d bytes)\n", p, len(iconSizes), len(icoBytes))
	}
}

// buildICO assembles the multi-resolution ICO byte stream. Each image
// is rasterized via branding.NewHubMarkImage, PNG-encoded, then
// referenced from a fixed-position ICONDIRENTRY in the directory.
func buildICO(sizes []int, col color.RGBA) ([]byte, error) {
	// Encode each size to PNG bytes upfront so we know the imageSize
	// fields for the directory entries.
	pngs := make([][]byte, len(sizes))
	for i, size := range sizes {
		img := branding.NewHubMarkImage(size, col)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("encode png size=%d: %w", size, err)
		}
		pngs[i] = buf.Bytes()
	}

	// ICONDIR (6 bytes): reserved(2) + type(2) + count(2).
	const iconDirSize = 6
	const iconDirEntrySize = 16
	headerSize := iconDirSize + iconDirEntrySize*len(sizes)

	out := bytes.NewBuffer(nil)

	// ICONDIR.
	binary.Write(out, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(out, binary.LittleEndian, uint16(1)) // type = icon
	binary.Write(out, binary.LittleEndian, uint16(len(sizes)))

	// ICONDIRENTRY × N. imageOffset accumulates as we walk the list.
	offset := headerSize
	for i, size := range sizes {
		// Per the ICO format, the width and height fields are
		// single bytes; the value 0 means 256. Anything larger
		// than 256 is not representable in this slot.
		wByte := byte(size)
		hByte := byte(size)
		if size == 256 {
			wByte = 0
			hByte = 0
		}
		entry := []byte{
			wByte,
			hByte,
			0, // colorCount = 0 (32bpp)
			0, // reserved
		}
		out.Write(entry)
		binary.Write(out, binary.LittleEndian, uint16(1))                 // planes
		binary.Write(out, binary.LittleEndian, uint16(32))                // bitsPerPixel
		binary.Write(out, binary.LittleEndian, uint32(len(pngs[i])))      // imageSize
		binary.Write(out, binary.LittleEndian, uint32(offset))            // imageOffset
		offset += len(pngs[i])
	}

	// PNG payloads in the same order as the directory entries.
	for _, p := range pngs {
		out.Write(p)
	}

	return out.Bytes(), nil
}

