package vtune

import (
	"bufio"
	"strconv"
	"strings"
)

// Hotspot is one parsed row of a VTune `-report <type> -format=csv` table —
// a single function (or module) and its per-metric values.
type Hotspot struct {
	// Function is the value of the "Function" column (or, when the report
	// groups by module, the "Module" column). It is the row's primary label.
	Function string `json:"function"`
	// Module is the value of the "Module" column when present (the DLL/exe the
	// function lives in). Empty when the report has no Module column.
	Module string `json:"module"`
	// CPUTimeSeconds is the parsed "CPU Time" column in seconds. VTune emits it
	// as a plain float (e.g. "0.107988"). 0 when the column is absent or
	// unparseable (the raw value is still preserved in Metrics).
	CPUTimeSeconds float64 `json:"cpu_time_seconds"`
	// Metrics is the full per-column map for this row, keyed by the CSV header
	// name (e.g. "CPU Time", "CPU Time:Effective Time", "Start Address"), with
	// the raw string value. This preserves EVERY metric column VTune emitted —
	// the set differs per analysis_type (memory-access has "Loads"/"Stores",
	// threading has wait/idle columns, etc.) — so no data is lost regardless of
	// which analysis was run.
	Metrics map[string]string `json:"metrics"`
}

// ParsedReport is the structured form of a VTune CSV report table.
type ParsedReport struct {
	// Columns is the header row, in file order, so a consumer can see exactly
	// which metric columns the analysis produced.
	Columns []string `json:"columns"`
	// Hotspots is the parsed data rows, in the order VTune emitted them (VTune
	// sorts hotspots by the primary metric descending, so the first rows are
	// the heaviest).
	Hotspots []Hotspot `json:"hotspots"`
}

// vtuneCSVDelimiter is the column separator VTune writes for `-format=csv`.
// Despite the "csv" name, VTune defaults to TAB-delimited output (verified
// live against vtune.exe 2026.2.0). Tab is collision-resistant: function and
// module names routinely contain commas/spaces but never tabs, so splitting
// on tab needs no quote handling.
const vtuneCSVDelimiter = '\t'

// functionColumns are the header names (lower-cased) that can serve as a
// row's primary "Function" label, in priority order. VTune labels the
// grouping column "Function" for a function-grouped report; some analysis
// types group by a differently-named first column.
var functionColumns = []string{"function", "function (full)"}

// parseReport turns a raw VTune CSV report body into structured hotspots. It
// is a PURE function (no I/O) so it can be unit-tested against a canned blob
// without ever running vtune.exe.
//
// The body shape (real VTune `-report hotspots -format=csv` output, TAB
// separated):
//
//	Function\tCPU Time\tCPU Time:Effective Time\t...\tModule\tFunction (Full)\t...
//	NtDeviceIoControlFile\t0.107988\t0.107988\t...\tntdll.dll\tNtDeviceIoControlFile\t...
//	NtCreateFile\t0.015117\t...\tntdll.dll\t...
//
// Columns are addressed BY HEADER NAME (not position), because the column
// set and order differ per analysis_type. The "Function" and "CPU Time"
// columns are surfaced as typed fields; every column is preserved verbatim
// in Hotspot.Metrics.
func parseReport(raw string) ParsedReport {
	var res ParsedReport

	scanner := bufio.NewScanner(strings.NewReader(raw))
	// A report line stays short, but a very wide column set (uarch-exploration
	// emits dozens of event columns) could exceed the default 64 KiB token cap;
	// raise it so no row is silently dropped.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var header []string
	var funcIdx, moduleIdx, cpuTimeIdx int = -1, -1, -1

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" {
			continue
		}
		fields := strings.Split(line, string(vtuneCSVDelimiter))

		if header == nil {
			header = fields
			res.Columns = append(res.Columns, header...)
			funcIdx = indexOfColumn(header, functionColumns)
			moduleIdx = indexOfColumn(header, []string{"module"})
			cpuTimeIdx = indexOfColumn(header, []string{"cpu time"})
			continue
		}

		hs := Hotspot{Metrics: make(map[string]string, len(header))}
		for i, col := range header {
			if i < len(fields) {
				hs.Metrics[col] = fields[i]
			}
		}
		if funcIdx >= 0 && funcIdx < len(fields) {
			hs.Function = fields[funcIdx]
		}
		if moduleIdx >= 0 && moduleIdx < len(fields) {
			hs.Module = fields[moduleIdx]
		}
		if cpuTimeIdx >= 0 && cpuTimeIdx < len(fields) {
			hs.CPUTimeSeconds = parseFloat(fields[cpuTimeIdx])
		}
		res.Hotspots = append(res.Hotspots, hs)
	}

	return res
}

// indexOfColumn returns the index of the FIRST header whose lower-cased name
// equals one of wants (also lower-cased), or -1 when none match. Used to
// locate a logical column by name regardless of its position, since VTune's
// column order varies per analysis type.
func indexOfColumn(header []string, wants []string) int {
	for _, want := range wants {
		for i, col := range header {
			if strings.EqualFold(strings.TrimSpace(col), want) {
				return i
			}
		}
	}
	return -1
}

// parseFloat parses a VTune numeric cell ("0.107988") into a float, returning
// 0 for an empty or unparseable cell. VTune occasionally appends a unit suffix
// (rare for CPU Time, common for byte-count columns); we strip a trailing
// non-numeric run defensively so "0.107988s" still parses.
func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	// Strip a trailing unit run (anything after the last digit/'.').
	end := strings.LastIndexAny(s, "0123456789.")
	if end >= 0 {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s[:end+1]), 64); err == nil {
			return f
		}
	}
	return 0
}
