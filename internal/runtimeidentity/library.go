package runtimeidentity

import (
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	WindowsRuntimeLibraryIdentitySchemaV1 = "windows-runtime-library-identity-v1"
	// IMAGE_FILE_DLL from the Windows PE/COFF specification.
	windowsPEDLLCharacteristic = 0x2000
)

var windowsProcessOwnerExportsV1 = [...]string{
	"mh_admit1",
	"mh_admit_checked1",
	"mh_admit_identity1",
	"mh_close1",
	"mh_create1",
	"mh_create_env1",
	"mh_create_io1",
	"mh_finish1",
	"mh_query_identity1",
	"mh_release1",
	"mh_stop1",
	"mh_version1",
	"mh_wait1",
}

var windowsProcessOwnerMemberExportsV1 = [...]string{
	"mh_admit1",
	"mh_admit_checked1",
	"mh_admit_identity1",
	"mh_close1",
	"mh_create1",
	"mh_create_env1",
	"mh_create_io1",
	"mh_finish1",
	"mh_members_close1",
	"mh_members_observe1",
	"mh_members_revalidate1",
	"mh_members_snapshot1",
	"mh_query_identity1",
	"mh_release1",
	"mh_stop1",
	"mh_version1",
	"mh_wait1",
}

var windowsProcessOwnerBorrowExportsV1 = [...]string{
	"mh_admit1",
	"mh_admit_checked1",
	"mh_admit_identity1",
	"mh_borrow_process1",
	"mh_close1",
	"mh_create1",
	"mh_create_env1",
	"mh_create_io1",
	"mh_finish1",
	"mh_members_close1",
	"mh_members_observe1",
	"mh_members_revalidate1",
	"mh_members_snapshot1",
	"mh_query_identity1",
	"mh_release1",
	"mh_stop1",
	"mh_version1",
	"mh_wait1",
}

// WindowsRuntimeLibraryIdentity is the persisted build-time identity consumed
// before loading the Rust process-owner DLL. Exports are sorted and exact.
type WindowsRuntimeLibraryIdentity struct {
	Schema       string   `json:"schema"`
	Filename     string   `json:"filename"`
	SourceCommit string   `json:"sourceCommit"`
	Machine      uint16   `json:"machine"`
	SHA256       string   `json:"sha256"`
	Exports      []string `json:"exports"`
}

// WindowsProcessOwnerExportsV1 returns a caller-owned copy of the legacy ABI V1
// export surface. The package retains the canonical source of truth.
func WindowsProcessOwnerExportsV1() []string {
	return append([]string(nil), windowsProcessOwnerExportsV1[:]...)
}

// WindowsProcessOwnerMemberExportsV1 returns a caller-owned copy of the complete
// ABI V1 export surface with the member observation schema V1 extension.
func WindowsProcessOwnerMemberExportsV1() []string {
	return append([]string(nil), windowsProcessOwnerMemberExportsV1[:]...)
}

// WindowsProcessOwnerBorrowExportsV1 is the complete member profile plus the
// synchronous original-process borrow extension. Legacy profiles stay exact.
func WindowsProcessOwnerBorrowExportsV1() []string {
	return append([]string(nil), windowsProcessOwnerBorrowExportsV1[:]...)
}

// IsWindowsProcessOwnerMemberExportProfileV1 requires either complete member
// profile. A partial or unknown superset never grants member capabilities.
func IsWindowsProcessOwnerMemberExportProfileV1(exports []string) bool {
	return equalStrings(exports, windowsProcessOwnerMemberExportsV1[:]) ||
		equalStrings(exports, windowsProcessOwnerBorrowExportsV1[:])
}

// IsWindowsProcessOwnerBorrowExportProfileV1 requires the exact admitted borrow
// profile; an export address alone cannot extend an older admitted surface.
func IsWindowsProcessOwnerBorrowExportProfileV1(exports []string) bool {
	return equalStrings(exports, windowsProcessOwnerBorrowExportsV1[:])
}

// IsWindowsProcessOwnerExportProfileV1 accepts only a complete, sorted canonical
// legacy, member or borrow profile. It never normalizes untrusted input.
func IsWindowsProcessOwnerExportProfileV1(exports []string) bool {
	return equalStrings(exports, windowsProcessOwnerExportsV1[:]) ||
		IsWindowsProcessOwnerMemberExportProfileV1(exports)
}

// AdmitWindowsRuntimeLibrary validates one already-openable ordinary PE DLL
// without executing it. The same file description supplies the PE view and hash.
// sourceCommit is a trusted build input; this function validates and records it.
func AdmitWindowsRuntimeLibrary(path string, expectedMachine uint16, expectedExports []string, sourceCommit string) (identity WindowsRuntimeLibraryIdentity, err error) {
	if err := ValidateWindowsRuntimeSourceCommit(sourceCommit); err != nil {
		return identity, err
	}
	if !filepath.IsAbs(path) {
		return identity, fmt.Errorf("runtime library path must be absolute")
	}
	exports, maxName, err := canonicalRuntimeExports(expectedExports)
	if err != nil {
		return identity, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return identity, fmt.Errorf("runtime library lstat: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return identity, fmt.Errorf("runtime library must be an ordinary regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return identity, fmt.Errorf("runtime library open: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			identity = WindowsRuntimeLibraryIdentity{}
			err = errors.Join(err, fmt.Errorf("runtime library close: %w", closeErr))
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return identity, fmt.Errorf("runtime library stat: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return identity, fmt.Errorf("runtime library identity changed while opening")
	}
	image, err := pe.NewFile(file)
	if err != nil {
		return identity, fmt.Errorf("runtime library PE: %w", err)
	}
	if image.Machine != expectedMachine {
		return identity, fmt.Errorf("runtime library machine %#x, want %#x", image.Machine, expectedMachine)
	}
	if image.Characteristics&windowsPEDLLCharacteristic == 0 {
		return identity, fmt.Errorf("runtime library PE image is not a DLL")
	}
	actualExports, err := readWindowsPEExports(file, opened.Size(), image, len(exports), maxName)
	if err != nil {
		return identity, err
	}
	if !equalStrings(actualExports, exports) {
		return identity, fmt.Errorf("runtime library exports differ: got %q, want %q", actualExports, exports)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(file, 0, opened.Size())); err != nil {
		return identity, fmt.Errorf("runtime library hash: %w", err)
	}
	identity = WindowsRuntimeLibraryIdentity{
		Schema:       WindowsRuntimeLibraryIdentitySchemaV1,
		Filename:     filepath.Base(path),
		SourceCommit: sourceCommit,
		Machine:      image.Machine,
		SHA256:       hex.EncodeToString(hash.Sum(nil)),
		Exports:      actualExports,
	}
	return identity, nil
}

// ValidateWindowsRuntimeSourceCommit rejects non-canonical source build identities.
func ValidateWindowsRuntimeSourceCommit(value string) error {
	if len(value) != 40 {
		return fmt.Errorf("runtime library source commit must be 40 lowercase hexadecimal characters")
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return fmt.Errorf("runtime library source commit must be 40 lowercase hexadecimal characters")
		}
	}
	return nil
}

func canonicalRuntimeExports(expected []string) ([]string, int, error) {
	if len(expected) == 0 {
		return nil, 0, fmt.Errorf("expected exports are empty")
	}
	result := append([]string(nil), expected...)
	sort.Strings(result)
	maxName := 0
	for i, name := range result {
		if name == "" || strings.IndexByte(name, 0) >= 0 {
			return nil, 0, fmt.Errorf("expected exports contain an invalid name")
		}
		if i > 0 && result[i-1] == name {
			return nil, 0, fmt.Errorf("expected exports contain duplicate %q", name)
		}
		if len(name) > maxName {
			maxName = len(name)
		}
	}
	return result, maxName, nil
}

func readWindowsPEExports(file *os.File, size int64, image *pe.File, expectedCount, maxName int) ([]string, error) {
	directory, ok := windowsPEExportDirectory(image)
	if !ok || directory.VirtualAddress == 0 || directory.Size < 40 {
		return nil, fmt.Errorf("runtime library export directory is missing")
	}
	raw, err := readWindowsPERange(file, size, image.Sections, directory.VirtualAddress, 40)
	if err != nil {
		return nil, fmt.Errorf("runtime library export directory: %w", err)
	}
	functionCount := binary.LittleEndian.Uint32(raw[20:24])
	nameCount := binary.LittleEndian.Uint32(raw[24:28])
	if uint64(functionCount) != uint64(expectedCount) || uint64(nameCount) != uint64(expectedCount) {
		return nil, fmt.Errorf("runtime library export count functions=%d names=%d, want %d", functionCount, nameCount, expectedCount)
	}
	nameTableRVA := binary.LittleEndian.Uint32(raw[32:36])
	if nameTableRVA == 0 {
		return nil, fmt.Errorf("runtime library export name table is missing")
	}
	tableBytes, err := readWindowsPERange(file, size, image.Sections, nameTableRVA, uint32(expectedCount)*4)
	if err != nil {
		return nil, fmt.Errorf("runtime library export name table: %w", err)
	}
	names := make([]string, 0, expectedCount)
	for i := 0; i < expectedCount; i++ {
		nameRVA := binary.LittleEndian.Uint32(tableBytes[i*4 : i*4+4])
		name, err := readWindowsPEExportName(file, size, image.Sections, nameRVA, maxName)
		if err != nil {
			return nil, fmt.Errorf("runtime library export name %d: %w", i, err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func windowsPEExportDirectory(image *pe.File) (pe.DataDirectory, bool) {
	switch optional := image.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		if optional.NumberOfRvaAndSizes < 1 {
			return pe.DataDirectory{}, false
		}
		return optional.DataDirectory[0], true
	case *pe.OptionalHeader32:
		if optional.NumberOfRvaAndSizes < 1 {
			return pe.DataDirectory{}, false
		}
		return optional.DataDirectory[0], true
	default:
		return pe.DataDirectory{}, false
	}
}

func readWindowsPERange(file *os.File, fileSize int64, sections []*pe.Section, rva, length uint32) ([]byte, error) {
	offset, available, err := windowsPERawWindow(fileSize, sections, rva)
	if err != nil {
		return nil, err
	}
	if length > available {
		return nil, fmt.Errorf("RVA %#x length %d exceeds raw section bytes %d", rva, length, available)
	}
	buffer := make([]byte, int(length))
	if _, err := file.ReadAt(buffer, offset); err != nil {
		return nil, err
	}
	return buffer, nil
}

func readWindowsPEExportName(file *os.File, fileSize int64, sections []*pe.Section, rva uint32, maxName int) (string, error) {
	if rva == 0 {
		return "", fmt.Errorf("zero RVA")
	}
	offset, available, err := windowsPERawWindow(fileSize, sections, rva)
	if err != nil {
		return "", err
	}
	limit := uint32(maxName + 1)
	if available < limit {
		limit = available
	}
	if limit == 0 {
		return "", fmt.Errorf("empty raw window")
	}
	buffer := make([]byte, int(limit))
	if _, err := file.ReadAt(buffer, offset); err != nil {
		return "", err
	}
	end := -1
	for i, value := range buffer {
		if value == 0 {
			end = i
			break
		}
	}
	if end <= 0 {
		return "", fmt.Errorf("unterminated or empty export name")
	}
	for _, value := range buffer[:end] {
		if value < 0x21 || value > 0x7e {
			return "", fmt.Errorf("non-ASCII export name")
		}
	}
	return string(buffer[:end]), nil
}

func windowsPERawWindow(fileSize int64, sections []*pe.Section, rva uint32) (int64, uint32, error) {
	for _, section := range sections {
		span := section.VirtualSize
		if section.Size > span {
			span = section.Size
		}
		if rva < section.VirtualAddress {
			continue
		}
		delta := uint64(rva - section.VirtualAddress)
		if delta >= uint64(span) || delta >= uint64(section.Size) {
			continue
		}
		offset := int64(section.Offset) + int64(delta)
		available := uint64(section.Size) - delta
		if offset < 0 || offset > fileSize || available > uint64(fileSize-offset) {
			return 0, 0, fmt.Errorf("RVA %#x maps outside file", rva)
		}
		return offset, uint32(available), nil
	}
	return 0, 0, fmt.Errorf("RVA %#x is not backed by a section", rva)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
