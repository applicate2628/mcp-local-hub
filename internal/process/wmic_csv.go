package process

import (
	"regexp"
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

var wmicCreationDateRE = regexp.MustCompile(`^\d{14}\.\d+[+-]\d+$`)

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

func isWmicCreationDateField(s string) bool {
	return wmicCreationDateRE.MatchString(strings.TrimSpace(s))
}
