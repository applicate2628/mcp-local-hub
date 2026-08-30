package cmaketrace

import (
	"strconv"
	"unicode/utf8"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

const (
	traceProjectionIdentityEntries = 4
	traceProjectionIdentityBytes   = publicresult.MaxEncodedBytes / 16
)

// PublicResultRequiresProjection performs a saturating lower-bound admission
// check over every dynamic collection. A false result bounds the number of
// values and their source bytes before MarshalIndent allocates the full JSON;
// a true result skips that materialization and goes straight to projection.
func (r Result) PublicResultRequiresProjection(limit int) bool {
	if limit < 0 {
		return true
	}
	weight := 0
	add := func(n int) bool {
		if n < 0 || weight > limit-n {
			return true
		}
		weight += n
		return false
	}
	addString := func(value string) bool { return add(encodedJSONStringBytes(value)) }
	addInt := func(value int) bool { return add(len(strconv.Itoa(value))) }
	addExecutedLineArray := func(lines []int) bool {
		// MarshalIndent(result, "", "  ") emits this []int at nesting depth
		// four: '['; then newline + eight spaces + decimal digits for the
		// first value; a comma before each later newline; and finally newline
		// + six spaces + ']'. Count that representation exactly without ever
		// materializing the potentially large array.
		if add(1) { // '['
			return true
		}
		if len(lines) == 0 {
			return add(1) // ']'
		}
		for index, line := range lines {
			syntaxBytes := 1 + 8 // newline + value indentation
			if index != 0 {
				syntaxBytes++ // comma after the previous value
			}
			if add(syntaxBytes) || addInt(line) {
				return true
			}
		}
		return add(1 + 6 + 1) // newline + closing indentation + ']'
	}
	addFloat := func(value float64) bool {
		if value == 0 {
			return false
		}
		return add(len(strconv.FormatFloat(value, 'g', -1, 64)))
	}

	for _, item := range r.IncludeChain {
		if addString(string(item.Kind)) || addString(item.File) || addInt(item.Line) || addString(item.Argument) {
			return true
		}
	}
	for _, record := range r.Records {
		// A zero-valued Record still emits four field names, braces, commas,
		// the args null value, newlines, and indentation. Charge a conservative
		// complete structural allowance before its dynamic values; every arg
		// gets its own separator/newline/indent allowance as well. This keeps
		// thousands of short records from bypassing pre-marshal admission.
		if add(128) || addString(record.File) || addInt(record.Line) || addString(record.Cmd) || addFloat(record.Time) ||
			addInt(record.Frame) || addInt(record.GlobalFrame) || add(5) {
			return true
		}
		for _, arg := range record.Args {
			if add(16) || addString(arg) {
				return true
			}
		}
	}
	for _, item := range r.ExecutedLines {
		// Conservative upper allowance for the surrounding FileLines object:
		// braces, the two field names, colons, commas, newlines and indentation.
		// The dynamic file and array payloads are counted separately below.
		if add(64) || addString(item.File) || addExecutedLineArray(item.Lines) {
			return true
		}
	}
	for _, file := range r.FilesInTrace {
		if addString(file) {
			return true
		}
	}
	for _, reason := range r.InputIncompleteReasons {
		if addString(string(reason)) {
			return true
		}
	}
	for _, path := range r.Evidence.Paths {
		if addString(path) {
			return true
		}
	}
	for _, command := range r.Evidence.Commands {
		if addString(command) {
			return true
		}
	}
	for _, location := range r.Evidence.Locations {
		if addString(location.File) || addInt(location.Line) {
			return true
		}
	}
	return false
}

// encodedJSONStringBytes matches encoding/json's default string escaping
// without allocating the encoded string. Admission must charge wire bytes,
// not decoded source bytes.
func encodedJSONStringBytes(value string) int {
	encoded := 2 // opening and closing quote
	for i := 0; i < len(value); {
		b := value[i]
		if b < utf8.RuneSelf {
			switch b {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				encoded += 2
			case '<', '>', '&':
				encoded += 6
			default:
				if b < 0x20 {
					encoded += 6
				} else {
					encoded++
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			encoded += utf8.RuneLen(r)
		case r == '\u2028' || r == '\u2029':
			encoded += 6
		default:
			encoded += size
		}
		i += size
	}
	return encoded
}

// PublicResultProjection retains the trace-verdict completeness signals while
// omitting large record and index collections.
func (r Result) PublicResultProjection() any {
	projectedEvidence, evidenceOmissions := projectTraceEvidence(r.Evidence)
	omissions := []publicresult.Omission{
		wholeTraceOmission("include_chain", len(r.IncludeChain)),
		wholeTraceOmission("records", len(r.Records)),
		wholeTraceOmission("executed_lines", len(r.ExecutedLines)),
		wholeTraceOmission("files_in_trace", len(r.FilesInTrace)),
	}
	omissions = append(omissions, evidenceOmissions...)
	return struct {
		Status                 Status                  `json:"status"`
		Reason                 Reason                  `json:"reason,omitempty"`
		MalformedLineCount     int                     `json:"malformed_line_count"`
		InputIncomplete        bool                    `json:"input_incomplete"`
		InputIncompleteReasons []Reason                `json:"input_incomplete_reasons,omitempty"`
		VersionHeaderPresent   bool                    `json:"version_header_present"`
		Truncated              bool                    `json:"truncated"`
		Evidence               evidence.Evidence       `json:"evidence"`
		ResultProjection       publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, r.MalformedLineCount, r.InputIncomplete, r.InputIncompleteReasons,
		r.VersionHeaderPresent, r.Truncated, projectedEvidence,
		publicresult.Projection{Complete: false, Omissions: omissions}}
}

func wholeTraceOmission(field string, total int) publicresult.Omission {
	omitted := total
	return publicresult.Omission{
		Field: field, Reason: publicresult.InternalProjectionLimit,
		Retained: 0, Omitted: &omitted,
	}
}

func projectTraceEvidence(source evidence.Evidence) (evidence.Evidence, []publicresult.Omission) {
	var projected evidence.Evidence
	retained := len(source.Paths)
	if retained > traceProjectionIdentityEntries {
		retained = traceProjectionIdentityEntries
	}
	for _, path := range source.Paths[:retained] {
		projected.Paths = append(projected.Paths, publicresult.AbbreviateEncoded(path, traceProjectionIdentityBytes))
	}
	var omissions []publicresult.Omission
	if retained < len(source.Paths) {
		omitted := len(source.Paths) - retained
		omissions = append(omissions, publicresult.Omission{
			Field: "evidence.paths", Reason: publicresult.InternalProjectionLimit,
			Retained: retained, Omitted: &omitted,
		})
	}
	if len(source.Commands) != 0 {
		omissions = append(omissions, wholeTraceOmission("evidence.commands", len(source.Commands)))
	}
	if len(source.Locations) != 0 {
		omissions = append(omissions, wholeTraceOmission("evidence.locations", len(source.Locations)))
	}
	return projected, omissions
}
