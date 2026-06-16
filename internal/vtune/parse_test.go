package vtune

import (
	"testing"
)

// cannedHotspotsCSV is a representative VTune `-report hotspots -format=csv`
// body, captured live from vtune.exe 2026.2.0. It is TAB-delimited (VTune's
// default "csv" delimiter is a tab, NOT a comma — verified live), so the
// constant uses literal \t between columns. Three data rows exercise the
// header-name column addressing and the CPU-Time float parse.
const cannedHotspotsCSV = "Function\tCPU Time\tCPU Time:Effective Time\tCPU Time:Spin Time\tCPU Time:Overhead Time\tModule\tFunction (Full)\tSource File\tStart Address\n" +
	"NtDeviceIoControlFile\t0.107988\t0.107988\t0.0\t0.0\tntdll.dll\tNtDeviceIoControlFile\t[Unknown]\t0x180161ea0\n" +
	"NtCreateFile\t0.015117\t0.015117\t0.0\t0.0\tntdll.dll\tNtCreateFile\t[Unknown]\t0x180162860\n" +
	"LoadLibraryExW\t0.013676\t0.013676\t0.0\t0.0\tKERNELBASE.dll\tLoadLibraryExW\t[Unknown]\t0x1800129e0\n"

func TestParseReport_HotspotsTabDelimited(t *testing.T) {
	res := parseReport(cannedHotspotsCSV)

	// Header columns captured in order.
	wantCols := []string{
		"Function", "CPU Time", "CPU Time:Effective Time", "CPU Time:Spin Time",
		"CPU Time:Overhead Time", "Module", "Function (Full)", "Source File", "Start Address",
	}
	if len(res.Columns) != len(wantCols) {
		t.Fatalf("Columns len = %d, want %d: %v", len(res.Columns), len(wantCols), res.Columns)
	}
	for i, c := range wantCols {
		if res.Columns[i] != c {
			t.Errorf("Columns[%d] = %q, want %q", i, res.Columns[i], c)
		}
	}

	if len(res.Hotspots) != 3 {
		t.Fatalf("expected 3 hotspots, got %d: %+v", len(res.Hotspots), res.Hotspots)
	}

	h0 := res.Hotspots[0]
	if h0.Function != "NtDeviceIoControlFile" {
		t.Errorf("hotspots[0].Function = %q, want NtDeviceIoControlFile", h0.Function)
	}
	if h0.Module != "ntdll.dll" {
		t.Errorf("hotspots[0].Module = %q, want ntdll.dll", h0.Module)
	}
	if h0.CPUTimeSeconds != 0.107988 {
		t.Errorf("hotspots[0].CPUTimeSeconds = %v, want 0.107988", h0.CPUTimeSeconds)
	}
	// Every column preserved verbatim in Metrics.
	if h0.Metrics["CPU Time:Effective Time"] != "0.107988" {
		t.Errorf("hotspots[0].Metrics[CPU Time:Effective Time] = %q, want 0.107988", h0.Metrics["CPU Time:Effective Time"])
	}
	if h0.Metrics["Start Address"] != "0x180161ea0" {
		t.Errorf("hotspots[0].Metrics[Start Address] = %q, want 0x180161ea0", h0.Metrics["Start Address"])
	}

	// Rows preserved in file order (VTune sorts heaviest-first).
	if res.Hotspots[1].Function != "NtCreateFile" || res.Hotspots[2].Function != "LoadLibraryExW" {
		t.Errorf("row order not preserved: %q, %q", res.Hotspots[1].Function, res.Hotspots[2].Function)
	}
}

// TestParseReport_ColumnOrderIndependent verifies the parser addresses
// columns by NAME, not position: a memory-access-style report whose Module
// column precedes the CPU Time column must still populate the typed fields.
func TestParseReport_ColumnOrderIndependent(t *testing.T) {
	// Module FIRST after Function, CPU Time LAST — different order than hotspots.
	blob := "Function\tModule\tLoads\tStores\tCPU Time\n" +
		"compute\tapp.exe\t100\t50\t2.5\n"
	res := parseReport(blob)
	if len(res.Hotspots) != 1 {
		t.Fatalf("expected 1 hotspot, got %d", len(res.Hotspots))
	}
	h := res.Hotspots[0]
	if h.Function != "compute" {
		t.Errorf("Function = %q, want compute", h.Function)
	}
	if h.Module != "app.exe" {
		t.Errorf("Module = %q, want app.exe (column addressed by name, not position)", h.Module)
	}
	if h.CPUTimeSeconds != 2.5 {
		t.Errorf("CPUTimeSeconds = %v, want 2.5 (CPU Time is the LAST column here)", h.CPUTimeSeconds)
	}
	if h.Metrics["Loads"] != "100" || h.Metrics["Stores"] != "50" {
		t.Errorf("analysis-specific columns not preserved: Loads=%q Stores=%q", h.Metrics["Loads"], h.Metrics["Stores"])
	}
}

// TestParseReport_Empty handles a report with only a header (no hotspots —
// e.g. a target too short to sample) and a fully empty body.
func TestParseReport_Empty(t *testing.T) {
	headerOnly := "Function\tCPU Time\tModule\n"
	res := parseReport(headerOnly)
	if len(res.Hotspots) != 0 {
		t.Errorf("header-only: expected 0 hotspots, got %d", len(res.Hotspots))
	}
	if len(res.Columns) != 3 {
		t.Errorf("header-only: expected 3 columns, got %d", len(res.Columns))
	}

	empty := parseReport("")
	if len(empty.Hotspots) != 0 || len(empty.Columns) != 0 {
		t.Errorf("empty body: expected no columns/hotspots, got %d cols %d rows", len(empty.Columns), len(empty.Hotspots))
	}
}

// TestParseFloat covers the numeric-cell parse, including the empty cell and
// a defensive unit-suffix strip.
func TestParseFloat(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0.107988", 0.107988},
		{"  2.5 ", 2.5},
		{"", 0},
		{"n/a", 0},
		{"0.5s", 0.5},
	}
	for _, c := range cases {
		if got := parseFloat(c.in); got != c.want {
			t.Errorf("parseFloat(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
