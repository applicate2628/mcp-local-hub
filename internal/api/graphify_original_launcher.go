package api

import (
	"archive/zip"
	"bytes"
	"debug/pe"
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

const graphifyOriginalLauncherSHA256 = "f9fc52be9aa3bb13b3d06df0b7870cc60594db850daba8c7b7f096ec9112d298"

var errGraphifySourceCutChanged = errors.New("PROJECT_SERVICE_GRAPHIFY_SOURCE_CUT_CHANGED")
var errGraphifyLauncherResources = errors.New("PROJECT_SERVICE_GRAPHIFY_LAUNCHER_RESOURCES_UNQUALIFIED")

// GraphifyOriginalLauncherInterpreter returns acquisition data, not authority.
// Both callers must use their original observed bytes and retain the normal
// prepared-launch and original-file checks.
func GraphifyOriginalLauncherInterpreter(raw []byte) (string, error) {
	if len(raw) != 39936 || ManifestHashContent(raw) != graphifyOriginalLauncherSHA256 {
		return "", errGraphifySourceCutChanged
	}
	f, err := pe.NewFile(bytes.NewReader(raw))
	if err != nil {
		return "", errGraphifyLauncherResources
	}
	defer f.Close()
	h, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || h.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI ||
		h.NumberOfRvaAndSizes <= pe.IMAGE_DIRECTORY_ENTRY_RESOURCE {
		return "", errGraphifyLauncherResources
	}
	dir := h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE]
	for _, section := range f.Sections {
		if dir.VirtualAddress < section.VirtualAddress || dir.Size == 0 {
			continue
		}
		delta := uint64(dir.VirtualAddress - section.VirtualAddress)
		if delta+uint64(dir.Size) > uint64(section.Size) || delta+uint64(dir.Size) > uint64(section.VirtualSize) {
			continue
		}
		start := uint64(section.Offset) + delta
		end := start + uint64(dir.Size)
		if end > uint64(len(raw)) {
			return "", errGraphifyLauncherResources
		}
		interpreter, err := readGraphifyLauncherResources(raw[start:end], dir.VirtualAddress)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(interpreter) || !strings.EqualFold(filepath.Base(interpreter), "python.exe") {
			return "", errGraphifyLauncherResources
		}
		return interpreter, nil
	}
	return "", errGraphifyLauncherResources
}

func graphifyResourceSlice(raw []byte, off, size uint32) []byte {
	if uint64(off)+uint64(size) > uint64(len(raw)) {
		return nil
	}
	return raw[off : off+size]
}

func graphifyResourceEntries(raw []byte, off uint32) []byte {
	const headerSize, entrySize = 16, 8
	h := graphifyResourceSlice(raw, off, headerSize)
	if h == nil {
		return nil
	}
	count := uint32(binary.LittleEndian.Uint16(h[12:])) + uint32(binary.LittleEndian.Uint16(h[14:]))
	if count == 0 || count > 16 {
		return nil
	}
	return graphifyResourceSlice(raw, off+headerSize, count*entrySize)
}

func graphifyResourceNameEquals(raw []byte, off uint32, name string) bool {
	n := graphifyResourceSlice(raw, off, 2)
	if n == nil || int(binary.LittleEndian.Uint16(n)) != len(name) {
		return false
	}
	wide := graphifyResourceSlice(raw, off+2, uint32(2*len(name)))
	if wide == nil {
		return false
	}
	for i := range len(name) {
		if binary.LittleEndian.Uint16(wide[2*i:]) != uint16(name[i]) {
			return false
		}
	}
	return true
}

func readGraphifyLauncherResources(raw []byte, baseRVA uint32) (string, error) {
	const directory = uint32(1 << 31)
	const rcData = 10
	var resources []byte
	root := graphifyResourceEntries(raw, 0)
	for i := 0; i < len(root); i += 8 {
		if binary.LittleEndian.Uint32(root[i:]) != rcData {
			continue
		}
		sub := binary.LittleEndian.Uint32(root[i+4:])
		if resources != nil || sub&directory == 0 {
			return "", errGraphifyLauncherResources
		}
		resources = graphifyResourceEntries(raw, sub&^directory)
		if resources == nil {
			return "", errGraphifyLauncherResources
		}
	}
	names := [...]string{"UV_PYTHON_PATH", "UV_SCRIPT_DATA", "UV_TRAMPOLINE_KIND"}
	if len(resources) != len(names)*8 {
		return "", errGraphifyLauncherResources
	}
	var values [len(names)][]byte
	for i := 0; i < len(resources); i += 8 {
		id := binary.LittleEndian.Uint32(resources[i:])
		sub := binary.LittleEndian.Uint32(resources[i+4:])
		if id&directory == 0 || sub&directory == 0 {
			return "", errGraphifyLauncherResources
		}
		index := -1
		for n, name := range names {
			if graphifyResourceNameEquals(raw, id&^directory, name) {
				index = n
				break
			}
		}
		if index < 0 || values[index] != nil {
			return "", errGraphifyLauncherResources
		}
		lang := graphifyResourceEntries(raw, sub&^directory)
		if len(lang) != 8 || binary.LittleEndian.Uint32(lang) != 0 {
			return "", errGraphifyLauncherResources
		}
		leaf := binary.LittleEndian.Uint32(lang[4:])
		if leaf&directory != 0 {
			return "", errGraphifyLauncherResources
		}
		data := graphifyResourceSlice(raw, leaf, 16)
		if data == nil {
			return "", errGraphifyLauncherResources
		}
		rva, size := binary.LittleEndian.Uint32(data), binary.LittleEndian.Uint32(data[4:])
		if rva < baseRVA || size == 0 {
			return "", errGraphifyLauncherResources
		}
		values[index] = graphifyResourceSlice(raw, rva-baseRVA, size)
		if values[index] == nil {
			return "", errGraphifyLauncherResources
		}
	}
	path, script, kind := values[0], values[1], values[2]
	if !bytes.Equal(kind, []byte{1}) || len(path) == 0 || len(path) > 260 {
		return "", errGraphifyLauncherResources
	}
	for _, c := range path {
		if c < 0x20 || c > 0x7e {
			return "", errGraphifyLauncherResources
		}
	}
	if !graphifyLauncherScript(script) {
		return "", errGraphifyLauncherResources
	}
	return string(path), nil
}

func graphifyLauncherScript(script []byte) bool {
	const maxScript = 4096
	if len(script) == 0 || len(script) > maxScript || !bytes.HasPrefix(script, []byte("PK\x03\x04")) {
		return false
	}
	z, err := zip.NewReader(bytes.NewReader(script), int64(len(script)))
	if err != nil || len(z.File) != 1 {
		return false
	}
	entry := z.File[0]
	if entry.Name != "__main__.py" || !entry.Mode().IsRegular() || entry.Method != zip.Store || entry.UncompressedSize64 > maxScript {
		return false
	}
	r, err := entry.Open()
	if err != nil {
		return false
	}
	raw, readErr := io.ReadAll(io.LimitReader(r, maxScript+1))
	if err := errors.Join(readErr, r.Close()); err != nil || len(raw) > maxScript {
		return false
	}
	importOK, callOK := false, false
	for _, line := range strings.Split(string(raw), "\n") {
		switch strings.TrimSpace(line) {
		case "from graphify.serve import _main":
			importOK = true
		case "sys.exit(_main())":
			callOK = true
		}
	}
	return importOK && callOK
}
