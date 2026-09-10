package binaryadmission

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	windowsPEResourceTypeVersion = 16
	windowsPEResourceMaxBytes    = 1 << 20
	windowsPEMaxSections         = 96
	windowsPEMaxOptionalHeader   = 4096
	windowsVersionLanguage       = 0x0409
	windowsVersionStringTable    = "040904B0"
)

type windowsBuildIdentityV1 struct {
	Version          string
	Commit           string
	BuildDate        string
	Role             WindowsArtifactRole
	InternalName     string
	OriginalFilename string
}

type windowsPESection struct {
	virtualAddress uint32
	virtualSize    uint32
	rawOffset      uint32
	rawSize        uint32
}

type versionInfoBlock struct {
	key        string
	value      []byte
	valueWords uint16
	blockType  uint16
	children   []versionInfoBlock
}

func align4Offset(offset int) int { return (offset + 3) &^ 3 }

func readWindowsBuildIdentity(path string) (windowsBuildIdentityV1, error) {
	f, err := os.Open(path)
	if err != nil {
		return windowsBuildIdentityV1{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return windowsBuildIdentityV1{}, err
	}
	if !info.Mode().IsRegular() {
		return windowsBuildIdentityV1{}, fmt.Errorf("not a regular file")
	}
	resource, err := readWindowsVersionResource(f, info.Size())
	if err != nil {
		return windowsBuildIdentityV1{}, err
	}
	return parseWindowsBuildIdentity(resource)
}

func readWindowsVersionResource(r io.ReaderAt, size int64) ([]byte, error) {
	if r == nil || size < 64 {
		return nil, fmt.Errorf("truncated DOS header")
	}
	var dos [64]byte
	if _, err := r.ReadAt(dos[:], 0); err != nil {
		return nil, fmt.Errorf("read DOS header: %w", err)
	}
	if string(dos[:2]) != "MZ" {
		return nil, fmt.Errorf("missing MZ signature")
	}
	peOffset := int64(binary.LittleEndian.Uint32(dos[0x3c:0x40]))
	if peOffset < 64 || peOffset > maxWindowsPEHeaderOffset || peOffset > size-24 {
		return nil, fmt.Errorf("invalid PE header offset %d", peOffset)
	}
	var coff [24]byte
	if _, err := r.ReadAt(coff[:], peOffset); err != nil {
		return nil, fmt.Errorf("read PE/COFF header: %w", err)
	}
	if string(coff[:4]) != "PE\x00\x00" {
		return nil, fmt.Errorf("missing PE signature")
	}
	sectionCount := int(binary.LittleEndian.Uint16(coff[6:8]))
	optionalSize := int(binary.LittleEndian.Uint16(coff[20:22]))
	if sectionCount < 1 || sectionCount > windowsPEMaxSections {
		return nil, fmt.Errorf("invalid PE section count %d", sectionCount)
	}
	if optionalSize < 136 || optionalSize > windowsPEMaxOptionalHeader {
		return nil, fmt.Errorf("invalid optional header size %d", optionalSize)
	}
	headerBytes := optionalSize + sectionCount*40
	headerOffset := peOffset + 24
	if headerOffset > size-int64(headerBytes) {
		return nil, fmt.Errorf("truncated optional/section headers")
	}
	header := make([]byte, headerBytes)
	if _, err := r.ReadAt(header, headerOffset); err != nil {
		return nil, fmt.Errorf("read optional/section headers: %w", err)
	}
	optional := header[:optionalSize]
	magic := binary.LittleEndian.Uint16(optional[:2])
	dataDirectoryOffset := 96
	numberOfRVAsOffset := 92
	if magic == 0x20b {
		dataDirectoryOffset = 112
		numberOfRVAsOffset = 108
	} else if magic != 0x10b {
		return nil, fmt.Errorf("unsupported optional-header magic %#x", magic)
	}
	if optionalSize < dataDirectoryOffset+24 || binary.LittleEndian.Uint32(optional[numberOfRVAsOffset:numberOfRVAsOffset+4]) < 3 {
		return nil, fmt.Errorf("PE resource data directory is missing")
	}
	resourceRVA := binary.LittleEndian.Uint32(optional[dataDirectoryOffset+16 : dataDirectoryOffset+20])
	resourceSize := binary.LittleEndian.Uint32(optional[dataDirectoryOffset+20 : dataDirectoryOffset+24])
	if resourceRVA == 0 || resourceSize < 24 || resourceSize > windowsPEResourceMaxBytes {
		return nil, fmt.Errorf("invalid PE resource directory rva=%#x size=%d", resourceRVA, resourceSize)
	}
	sections := make([]windowsPESection, 0, sectionCount)
	sectionHeaders := header[optionalSize:]
	for i := 0; i < sectionCount; i++ {
		row := sectionHeaders[i*40 : (i+1)*40]
		sections = append(sections, windowsPESection{
			virtualSize:    binary.LittleEndian.Uint32(row[8:12]),
			virtualAddress: binary.LittleEndian.Uint32(row[12:16]),
			rawSize:        binary.LittleEndian.Uint32(row[16:20]),
			rawOffset:      binary.LittleEndian.Uint32(row[20:24]),
		})
	}
	resourceOffset, err := windowsPERVAToOffset(resourceRVA, resourceSize, size, sections)
	if err != nil {
		return nil, fmt.Errorf("map PE resource directory: %w", err)
	}
	resource := make([]byte, resourceSize)
	if _, err := r.ReadAt(resource, resourceOffset); err != nil {
		return nil, fmt.Errorf("read PE resource directory: %w", err)
	}
	typeEntry, err := findSingleResourceEntry(resource, 0, windowsPEResourceTypeVersion)
	if err != nil {
		return nil, fmt.Errorf("RT_VERSION: %w", err)
	}
	nameOffset, err := resourceSubdirectoryOffset(typeEntry)
	if err != nil {
		return nil, fmt.Errorf("RT_VERSION name: %w", err)
	}
	nameEntry, err := singleResourceIDEntry(resource, nameOffset, 1)
	if err != nil {
		return nil, fmt.Errorf("RT_VERSION name: %w", err)
	}
	languageOffset, err := resourceSubdirectoryOffset(nameEntry)
	if err != nil {
		return nil, fmt.Errorf("RT_VERSION language: %w", err)
	}
	languageEntry, err := singleResourceIDEntry(resource, languageOffset, windowsVersionLanguage)
	if err != nil {
		return nil, fmt.Errorf("RT_VERSION language: %w", err)
	}
	if languageEntry&0x80000000 != 0 {
		return nil, fmt.Errorf("RT_VERSION language points to a directory")
	}
	dataEntryOffset := int(languageEntry & 0x7fffffff)
	if dataEntryOffset < 0 || dataEntryOffset > len(resource)-16 {
		return nil, fmt.Errorf("RT_VERSION data entry is out of range")
	}
	dataEntry := resource[dataEntryOffset : dataEntryOffset+16]
	payloadRVA := binary.LittleEndian.Uint32(dataEntry[0:4])
	payloadSize := binary.LittleEndian.Uint32(dataEntry[4:8])
	if payloadSize < 6 || payloadSize > windowsPEResourceMaxBytes || binary.LittleEndian.Uint32(dataEntry[12:16]) != 0 {
		return nil, fmt.Errorf("RT_VERSION data entry is malformed")
	}
	payloadOffset, err := windowsPERVAToOffset(payloadRVA, payloadSize, size, sections)
	if err != nil {
		return nil, fmt.Errorf("map RT_VERSION payload: %w", err)
	}
	payload := make([]byte, payloadSize)
	if _, err := r.ReadAt(payload, payloadOffset); err != nil {
		return nil, fmt.Errorf("read RT_VERSION payload: %w", err)
	}
	return payload, nil
}

func windowsPERVAToOffset(rva, length uint32, fileSize int64, sections []windowsPESection) (int64, error) {
	end := uint64(rva) + uint64(length)
	if end > 1<<32 {
		return 0, fmt.Errorf("RVA range overflows")
	}
	var found *windowsPESection
	for i := range sections {
		section := &sections[i]
		span := section.virtualSize
		if section.rawSize > span {
			span = section.rawSize
		}
		sectionEnd := uint64(section.virtualAddress) + uint64(span)
		if uint64(rva) >= uint64(section.virtualAddress) && end <= sectionEnd {
			if found != nil {
				return 0, fmt.Errorf("RVA range maps to duplicate sections")
			}
			found = section
		}
	}
	if found == nil {
		return 0, fmt.Errorf("RVA range is not backed by one section")
	}
	delta := uint64(rva - found.virtualAddress)
	if delta+uint64(length) > uint64(found.rawSize) {
		return 0, fmt.Errorf("RVA range exceeds section raw data")
	}
	offset := uint64(found.rawOffset) + delta
	if offset+uint64(length) > uint64(fileSize) {
		return 0, fmt.Errorf("RVA range exceeds file")
	}
	return int64(offset), nil
}

func resourceDirectoryEntries(resource []byte, offset int) ([]uint64, error) {
	if offset < 0 || offset > len(resource)-16 {
		return nil, fmt.Errorf("directory offset is out of range")
	}
	dir := resource[offset : offset+16]
	count := int(binary.LittleEndian.Uint16(dir[12:14])) + int(binary.LittleEndian.Uint16(dir[14:16]))
	if count < 1 || count > 256 || offset+16+count*8 > len(resource) {
		return nil, fmt.Errorf("directory entry count %d is invalid", count)
	}
	entries := make([]uint64, count)
	for i := range count {
		entries[i] = binary.LittleEndian.Uint64(resource[offset+16+i*8 : offset+24+i*8])
	}
	return entries, nil
}

func findSingleResourceEntry(resource []byte, offset int, id uint32) (uint32, error) {
	entries, err := resourceDirectoryEntries(resource, offset)
	if err != nil {
		return 0, err
	}
	var result uint32
	found := 0
	for _, entry := range entries {
		name := uint32(entry)
		if name&0x80000000 == 0 && name == id {
			result = uint32(entry >> 32)
			found++
		}
	}
	if found != 1 {
		return 0, fmt.Errorf("expected exactly one id %d entry, found %d", id, found)
	}
	return result, nil
}

func singleResourceIDEntry(resource []byte, offset int, wantID uint32) (uint32, error) {
	entries, err := resourceDirectoryEntries(resource, offset)
	if err != nil {
		return 0, err
	}
	if len(entries) != 1 || uint32(entries[0])&0x80000000 != 0 || uint32(entries[0]) != wantID {
		return 0, fmt.Errorf("expected exactly one id %d entry", wantID)
	}
	return uint32(entries[0] >> 32), nil
}

func resourceSubdirectoryOffset(entry uint32) (int, error) {
	if entry&0x80000000 == 0 {
		return 0, fmt.Errorf("entry does not point to a directory")
	}
	return int(entry & 0x7fffffff), nil
}

func parseWindowsBuildIdentity(payload []byte) (windowsBuildIdentityV1, error) {
	root, consumed, err := parseVersionInfoBlock(payload, 0)
	if err != nil {
		return windowsBuildIdentityV1{}, err
	}
	if consumed != len(payload) || root.key != "VS_VERSION_INFO" || root.blockType != 0 || len(root.value) != 52 || binary.LittleEndian.Uint32(root.value[:4]) != 0xFEEF04BD {
		return windowsBuildIdentityV1{}, fmt.Errorf("VS_VERSION_INFO root is malformed")
	}
	var stringInfo, varInfo *versionInfoBlock
	for i := range root.children {
		switch root.children[i].key {
		case "StringFileInfo":
			if stringInfo != nil {
				return windowsBuildIdentityV1{}, fmt.Errorf("duplicate StringFileInfo")
			}
			stringInfo = &root.children[i]
		case "VarFileInfo":
			if varInfo != nil {
				return windowsBuildIdentityV1{}, fmt.Errorf("duplicate VarFileInfo")
			}
			varInfo = &root.children[i]
		default:
			return windowsBuildIdentityV1{}, fmt.Errorf("unexpected VERSIONINFO child %q", root.children[i].key)
		}
	}
	if stringInfo == nil || varInfo == nil || len(stringInfo.children) != 1 || stringInfo.children[0].key != windowsVersionStringTable {
		return windowsBuildIdentityV1{}, fmt.Errorf("expected one %s string table", windowsVersionStringTable)
	}
	if len(varInfo.children) != 1 || varInfo.children[0].key != "Translation" || len(varInfo.children[0].value) != 4 ||
		binary.LittleEndian.Uint16(varInfo.children[0].value[0:2]) != windowsVersionLanguage || binary.LittleEndian.Uint16(varInfo.children[0].value[2:4]) != 0x04b0 {
		return windowsBuildIdentityV1{}, fmt.Errorf("VERSIONINFO translation must be exactly 0409/04B0")
	}
	values := make(map[string]string)
	for _, child := range stringInfo.children[0].children {
		if _, duplicate := values[child.key]; duplicate {
			return windowsBuildIdentityV1{}, fmt.Errorf("duplicate VERSIONINFO key %q", child.key)
		}
		value, err := decodeVersionString(child)
		if err != nil {
			return windowsBuildIdentityV1{}, fmt.Errorf("VERSIONINFO key %q: %w", child.key, err)
		}
		values[child.key] = value
	}
	required := []string{"ProductName", "ProductVersion", "PrivateBuild", "SpecialBuild", "InternalName", "OriginalFilename"}
	for _, key := range required {
		if values[key] == "" {
			return windowsBuildIdentityV1{}, fmt.Errorf("VERSIONINFO key %q is missing or empty", key)
		}
	}
	if values["ProductName"] != "mcp-local-hub" || !validProductSemVer(values["ProductVersion"]) {
		return windowsBuildIdentityV1{}, fmt.Errorf("VERSIONINFO product name/version is invalid")
	}
	commit, buildDate, err := parseBuildIdentityField(values["PrivateBuild"])
	if err != nil {
		return windowsBuildIdentityV1{}, err
	}
	role, err := parseRoleIdentityField(values["SpecialBuild"])
	if err != nil {
		return windowsBuildIdentityV1{}, err
	}
	wantInternal, wantFilename := "mcphub-cli", "mcphub.exe"
	if role == WindowsArtifactRoleWindowless {
		wantInternal, wantFilename = "mcphub-windowless", "mcphub-windowless.exe"
	}
	if values["InternalName"] != wantInternal || values["OriginalFilename"] != wantFilename {
		return windowsBuildIdentityV1{}, fmt.Errorf("VERSIONINFO role/name mismatch for %s", role)
	}
	return windowsBuildIdentityV1{Version: values["ProductVersion"], Commit: commit, BuildDate: buildDate, Role: role, InternalName: wantInternal, OriginalFilename: wantFilename}, nil
}

func parseVersionInfoBlock(data []byte, offset int) (versionInfoBlock, int, error) {
	if offset < 0 || offset > len(data)-6 {
		return versionInfoBlock{}, 0, fmt.Errorf("VERSIONINFO block header is out of range")
	}
	length := int(binary.LittleEndian.Uint16(data[offset : offset+2]))
	valueLength := binary.LittleEndian.Uint16(data[offset+2 : offset+4])
	blockType := binary.LittleEndian.Uint16(data[offset+4 : offset+6])
	if length < 8 || offset+length > len(data) || (blockType != 0 && blockType != 1) {
		return versionInfoBlock{}, 0, fmt.Errorf("VERSIONINFO block length/type is invalid")
	}
	end := offset + length
	key, cursor, err := readUTF16Z(data, offset+6, end)
	if err != nil || key == "" {
		return versionInfoBlock{}, 0, fmt.Errorf("VERSIONINFO block key is invalid: %w", err)
	}
	cursor = align4Offset(cursor)
	valueBytes := int(valueLength)
	if blockType == 1 {
		valueBytes *= 2
	}
	if cursor > end-valueBytes {
		return versionInfoBlock{}, 0, fmt.Errorf("VERSIONINFO value is out of range")
	}
	value := append([]byte(nil), data[cursor:cursor+valueBytes]...)
	cursor = align4Offset(cursor + valueBytes)
	block := versionInfoBlock{key: key, value: value, valueWords: valueLength, blockType: blockType}
	for cursor < end {
		if end-cursor < 2 {
			return versionInfoBlock{}, 0, fmt.Errorf("VERSIONINFO child padding is malformed")
		}
		if binary.LittleEndian.Uint16(data[cursor:cursor+2]) == 0 {
			for cursor < end && data[cursor] == 0 {
				cursor++
			}
			if cursor != end {
				return versionInfoBlock{}, 0, fmt.Errorf("VERSIONINFO trailing padding is malformed")
			}
			break
		}
		child, consumed, err := parseVersionInfoBlock(data, cursor)
		if err != nil {
			return versionInfoBlock{}, 0, err
		}
		block.children = append(block.children, child)
		cursor = align4Offset(cursor + consumed)
		if cursor > end {
			return versionInfoBlock{}, 0, fmt.Errorf("VERSIONINFO child alignment exceeds parent")
		}
	}
	return block, length, nil
}

func readUTF16Z(data []byte, offset, end int) (string, int, error) {
	if offset < 0 || end > len(data) || offset >= end || offset%2 != 0 {
		return "", 0, fmt.Errorf("UTF-16 range is invalid")
	}
	units := make([]uint16, 0, 32)
	for cursor := offset; cursor+2 <= end; cursor += 2 {
		unit := binary.LittleEndian.Uint16(data[cursor : cursor+2])
		if unit == 0 {
			for i := 0; i < len(units); i++ {
				if 0xD800 <= units[i] && units[i] <= 0xDBFF {
					if i+1 >= len(units) || units[i+1] < 0xDC00 || units[i+1] > 0xDFFF {
						return "", 0, fmt.Errorf("unpaired UTF-16 surrogate")
					}
					i++
				} else if 0xDC00 <= units[i] && units[i] <= 0xDFFF {
					return "", 0, fmt.Errorf("unpaired UTF-16 surrogate")
				}
			}
			return string(utf16.Decode(units)), cursor + 2, nil
		}
		units = append(units, unit)
	}
	return "", 0, fmt.Errorf("unterminated UTF-16 string")
}

func decodeVersionString(block versionInfoBlock) (string, error) {
	if block.blockType != 1 || block.valueWords == 0 || len(block.children) != 0 || len(block.value) != int(block.valueWords)*2 {
		return "", fmt.Errorf("string block shape is invalid")
	}
	value, cursor, err := readUTF16Z(block.value, 0, len(block.value))
	if err != nil || cursor != len(block.value) {
		return "", fmt.Errorf("string value is invalid: %w", err)
	}
	return value, nil
}

func parseBuildIdentityField(value string) (string, string, error) {
	const prefix = "mcphub-build-v1;commit="
	if !strings.HasPrefix(value, prefix) {
		return "", "", fmt.Errorf("PrivateBuild grammar is invalid")
	}
	rest := strings.TrimPrefix(value, prefix)
	parts := strings.Split(rest, ";build_date=")
	if len(parts) != 2 || !validLowerHex(parts[0], 7, 64) || !validUTCSeconds(parts[1]) {
		return "", "", fmt.Errorf("PrivateBuild grammar is invalid")
	}
	return parts[0], parts[1], nil
}

func parseRoleIdentityField(value string) (WindowsArtifactRole, error) {
	const prefix = "mcphub-role-v1;role="
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("SpecialBuild grammar is invalid")
	}
	role := WindowsArtifactRole(strings.TrimPrefix(value, prefix))
	if role != WindowsArtifactRoleCLI && role != WindowsArtifactRoleWindowless {
		return "", fmt.Errorf("SpecialBuild role is invalid")
	}
	return role, nil
}

func validLowerHex(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, r := range value {
		if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}

func validUTCSeconds(value string) bool {
	if len(value) != len("2006-01-02T15:04:05Z") || value[4] != '-' || value[7] != '-' || value[10] != 'T' || value[13] != ':' || value[16] != ':' || value[19] != 'Z' {
		return false
	}
	for i, r := range value {
		if i == 4 || i == 7 || i == 10 || i == 13 || i == 16 || i == 19 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	parsed, err := time.Parse("2006-01-02T15:04:05Z", value)
	return err == nil && parsed.UTC().Format("2006-01-02T15:04:05Z") == value
}

func validProductSemVer(value string) bool {
	if value == "" || strings.HasPrefix(value, "v") || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	mainAndBuild := strings.Split(value, "+")
	if len(mainAndBuild) > 2 || (len(mainAndBuild) == 2 && !validSemVerIdentifiers(mainAndBuild[1], false)) {
		return false
	}
	mainAndPre := strings.SplitN(mainAndBuild[0], "-", 2)
	if len(mainAndPre) > 2 || (len(mainAndPre) == 2 && !validSemVerIdentifiers(mainAndPre[1], true)) {
		return false
	}
	core := strings.Split(mainAndPre[0], ".")
	if len(core) != 3 {
		return false
	}
	for _, part := range core {
		if !validNumericIdentifier(part) {
			return false
		}
	}
	return true
}

func validSemVerIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	parts := strings.Split(value, ".")
	for _, part := range parts {
		if part == "" {
			return false
		}
		numeric := true
		for _, r := range part {
			if !('0' <= r && r <= '9') {
				numeric = false
			}
			if !('0' <= r && r <= '9') && !('A' <= r && r <= 'Z') && !('a' <= r && r <= 'z') && r != '-' {
				return false
			}
		}
		if rejectNumericLeadingZero && numeric && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

func validNumericIdentifier(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
