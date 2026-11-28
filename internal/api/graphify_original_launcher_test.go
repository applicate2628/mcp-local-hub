package api

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestGraphifyOriginalLauncherRejectsUnpinnedBytesBeforePE(t *testing.T) {
	raw := make([]byte, 39936)
	copy(raw, "MZ")
	if path, err := GraphifyOriginalLauncherInterpreter(raw); path != "" || !errors.Is(err, errGraphifySourceCutChanged) {
		t.Fatal("an unpinned image reached resource decoding")
	}
}

func TestGraphifyLauncherResourcesUseNamedConsoleScript(t *testing.T) {
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	f, err := z.CreateHeader(&zip.FileHeader{Name: "__main__.py", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("import sys\nfrom graphify.serve import _main\nsys.exit(_main())\n")); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}

	// Ordinary synthetic directory layout: the unexported representation
	// reader is tested independently; it grants no admission for these bytes.
	const flag, base = uint32(1 << 31), uint32(4096)
	data := make([]byte, 1024)
	u32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(data[off:], v) }
	u16 := func(off int, v uint16) { binary.LittleEndian.PutUint16(data[off:], v) }
	u16(14, 1)
	u32(16, 10)
	u32(20, flag|24)
	u16(36, 3)
	path := "C:\\fixture\\venv\\Scripts\\python.exe"
	x := []struct {
		name string
		raw  []byte
	}{
		{"UV_PYTHON_PATH", []byte(path)},
		{"UV_SCRIPT_DATA", archive.Bytes()},
		{"UV_TRAMPOLINE_KIND", []byte{1}},
	}
	for i, e := range x {
		name, lang, leaf, start := 160+48*i, 64+24*i, 320+16*i, 400+200*i
		u32(40+8*i, flag|uint32(name))
		u32(44+8*i, flag|uint32(lang))
		u16(name, uint16(len(e.name)))
		for j := range len(e.name) {
			u16(name+2+2*j, uint16(e.name[j]))
		}
		u16(lang+14, 1)
		u32(lang+20, uint32(leaf))
		u32(leaf, base+uint32(start))
		u32(leaf+4, uint32(len(e.raw)))
		copy(data[start:], e.raw)
	}
	if got, err := readGraphifyLauncherResources(data, base); got != path || err != nil {
		t.Fatal("did not read the named interpreter for a Graphify console script")
	}

	// Locale 1033 is normal PE metadata but not the exact uv script contract.
	u32(64+16, 1033)
	if got, err := readGraphifyLauncherResources(data, base); got != "" || !errors.Is(err, errGraphifyLauncherResources) {
		t.Fatal("an unrelated language became a selected interpreter")
	}
}
