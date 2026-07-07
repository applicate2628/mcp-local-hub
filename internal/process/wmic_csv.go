package process

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// WmicProcessCSVFields is the shared comma-tolerant parse result for WMIC
// process rows that contain CommandLine, CreationDate, and ExecutablePath,
// followed by a caller-defined fixed-width tail.
type WmicProcessCSVFields struct {
	CommandLine    string
	CreationDate   string
	ExecutablePath string
	Tail           []string
}

// WmicCSVHeader is a parsed WMIC /format:csv header. WMIC's emitted column
// order is the source of truth; callers should not assume the requested
// property order survived.
type WmicCSVHeader struct {
	columns []string
	index   map[string]int
}

var wmicCreationDateRE = regexp.MustCompile(`^\d{14}\.\d+[+-]\d+$`)

// ReadWmicCSVRecords reads WMIC/PowerShell CSV output into logical records.
// CommandLine can contain an embedded newline when the producer quotes it, so a
// plain line scanner would split one process row into multiple records.
func ReadWmicCSVRecords(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var records []string
	var cur strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if cur.Len() > 0 &&
			wmicCSVRecordQuoteOpen(cur.String()) &&
			wmicCSVLineStartsRecord(line, wmicCSVRecordNodePrefix(cur.String())) {
			records = append(records, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(line)
		if wmicCSVRecordQuoteOpen(cur.String()) {
			continue
		}
		records = append(records, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		records = append(records, cur.String())
	}
	return records, scanner.Err()
}

func wmicCSVRecordNodePrefix(record string) string {
	firstLine := record
	if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
		firstLine = firstLine[:idx]
	}
	firstLine = strings.TrimSuffix(firstLine, "\r")
	node, _, ok := strings.Cut(firstLine, ",")
	if !ok || node == "" {
		return ""
	}
	return node + ","
}

func wmicCSVLineStartsRecord(line, nodePrefix string) bool {
	if nodePrefix == "" {
		return false
	}
	return strings.HasPrefix(strings.TrimSuffix(line, "\r"), nodePrefix)
}

// ParseWmicCSVHeader parses and validates a WMIC header line. required names
// are case-sensitive WMI property names plus the synthetic Node column.
func ParseWmicCSVHeader(line string, required ...string) (WmicCSVHeader, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return WmicCSVHeader{}, false
	}
	fields := SplitWmicCSVLine(line)
	if len(fields) == 0 {
		return WmicCSVHeader{}, false
	}
	columns := make([]string, 0, len(fields))
	index := make(map[string]int, len(fields))
	for _, f := range fields {
		name := strings.TrimSpace(f)
		if name == "" {
			return WmicCSVHeader{}, false
		}
		if _, exists := index[name]; exists {
			return WmicCSVHeader{}, false
		}
		index[name] = len(columns)
		columns = append(columns, name)
	}
	for _, name := range required {
		if _, ok := index[name]; !ok {
			return WmicCSVHeader{}, false
		}
	}
	return WmicCSVHeader{columns: columns, index: index}, true
}

// ParseWmicProcessCSVRecord maps one process CSV record by the emitted header.
// CommandLine and ExecutablePath are variable-width because legacy WMIC can
// leave comma-bearing values unquoted; fixed columns are anchored by their
// header positions and value shape.
func ParseWmicProcessCSVRecord(line string, header WmicCSVHeader) (map[string]string, bool) {
	line = strings.Trim(line, "\r\n")
	if strings.TrimSpace(line) == "" || len(header.columns) == 0 {
		return nil, false
	}
	fields := SplitWmicCSVLine(line)
	if len(fields) < len(header.columns) {
		return nil, false
	}
	out := make(map[string]string, len(header.columns))
	if !assignWmicRecordFields(header.columns, fields, 0, 0, out) {
		return nil, false
	}
	return out, true
}

// ParseWmicProcessCSVFields parses a single WMIC /format:csv process row whose
// middle fields are CommandLine, CreationDate, ExecutablePath. WMIC does not
// reliably quote comma-bearing CommandLine or ExecutablePath values, so callers
// must not use csv.Reader/header indexes for this shape. This parser anchors
// the fixed-width tail from the right, scans left for the CIM CreationDate
// field, then rejoins the CommandLine and ExecutablePath spans around it.
func ParseWmicProcessCSVFields(line string, tailCount int) (WmicProcessCSVFields, bool) {
	line = strings.TrimSpace(line)
	if line == "" || tailCount < 0 || strings.HasPrefix(line, "Node,") {
		return WmicProcessCSVFields{}, false
	}
	fields := SplitWmicCSVLine(line)
	if len(fields) < 3+tailCount {
		return WmicProcessCSVFields{}, false
	}

	tailStart := len(fields) - tailCount
	createdIdx := -1
	for i := tailStart - 1; i >= 1; i-- {
		if isWmicCreationDateField(fields[i]) {
			createdIdx = i
			break
		}
	}
	if createdIdx == -1 {
		return WmicProcessCSVFields{}, false
	}

	tail := make([]string, tailCount)
	for i := range tail {
		tail[i] = strings.TrimSpace(fields[tailStart+i])
	}
	return WmicProcessCSVFields{
		CommandLine:    strings.Join(fields[1:createdIdx], ","),
		CreationDate:   strings.TrimSpace(fields[createdIdx]),
		ExecutablePath: strings.TrimSpace(strings.Join(fields[createdIdx+1:tailStart], ",")),
		Tail:           tail,
	}, true
}

// SplitWmicCSVLine splits one wmic /format:csv line on commas while preserving
// commas inside quoted spans. WMIC is not RFC 4180 compliant, so this minimal
// state machine intentionally tolerates unescaped quotes.
func SplitWmicCSVLine(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' {
			if inQuote && i+1 < len(line) && line[i+1] == '"' {
				cur.WriteByte('"')
				i++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if c == ',' && !inQuote {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

func wmicCSVRecordQuoteOpen(record string) bool {
	quotes := 0
	for i := 0; i < len(record); i++ {
		if record[i] != '"' {
			continue
		}
		if i+1 < len(record) && record[i+1] == '"' {
			i++
			continue
		}
		quotes++
	}
	return quotes%2 == 1
}

func assignWmicRecordFields(columns, fields []string, colIdx, fieldIdx int, out map[string]string) bool {
	if colIdx == len(columns) {
		return fieldIdx == len(fields)
	}
	if fieldIdx >= len(fields) {
		return false
	}
	name := columns[colIdx]
	if isFlexibleWmicColumn(name) {
		remainingCols := len(columns) - colIdx - 1
		maxEnd := len(fields) - remainingCols
		for end := maxEnd; end >= fieldIdx+1; end-- {
			value := strings.Join(fields[fieldIdx:end], ",")
			if !isValidWmicColumnValue(name, value) {
				continue
			}
			out[name] = normalizeWmicColumnValue(name, value)
			if assignWmicRecordFields(columns, fields, colIdx+1, end, out) {
				return true
			}
			delete(out, name)
		}
		return false
	}

	value := fields[fieldIdx]
	if !isValidWmicColumnValue(name, value) {
		return false
	}
	out[name] = normalizeWmicColumnValue(name, value)
	if assignWmicRecordFields(columns, fields, colIdx+1, fieldIdx+1, out) {
		return true
	}
	delete(out, name)
	return false
}

func isFlexibleWmicColumn(name string) bool {
	return name == "CommandLine" || name == "ExecutablePath"
}

func isValidWmicColumnValue(name, value string) bool {
	value = strings.TrimSpace(value)
	switch name {
	case "CreationDate":
		return isWmicCreationDateField(value)
	case "ParentProcessId", "ProcessId", "WorkingSetSize":
		return isUnsignedDecimalWmicField(value)
	default:
		return true
	}
}

func normalizeWmicColumnValue(name, value string) string {
	switch name {
	case "CommandLine":
		return value
	default:
		return strings.TrimSpace(value)
	}
}

func isWmicCreationDateField(s string) bool {
	return wmicCreationDateRE.MatchString(strings.TrimSpace(s))
}

func isUnsignedDecimalWmicField(s string) bool {
	_, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return err == nil
}
