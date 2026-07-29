package lastfailure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// wireBodyCeiling is the whole-response ceiling this probe asserts, held as a
// test-local literal so the probe can be run against a tree that does not yet
// declare the production constant. TestWireSize_CeilingMatchesProduction pins
// the two together, so the instrument cannot drift away from the bound it
// claims to be measuring.
const wireBodyCeiling = 256 << 10

// probeOversize is the pathological payload size every case below plants in a
// DIFFERENT unbounded position. 3 MiB is chosen to sit under scanDiagnostics'
// and ParseWrapperContent's shared 4 MiB bufio line ceiling, so the scanner
// accepts the line whole and the value really does reach the Result — a larger
// payload would be rejected by the scanner and would prove nothing.
const probeOversize = 3 << 20

// The tool bounded ONE field of ONE participant of a class.
//
// applyResponseBudget charged len(d.Text) and truncateDiagnosticText bounded
// d.Text, but a Result carries several other strings lifted verbatim out of an
// unbounded log or wrapper line. This probe is parameterized over every
// PRODUCER of such a string rather than over the fields that were already
// known to be unsafe: the five matched diagnostic shapes, CMake's "Run Build
// Command(s)" line, and the wrapper file's `command:` line (which reaches the
// wire as exact_command, as overlay_chain, and a second time inside evidence).
//
// Measured on 99c9446f, before the bound (MarshalIndent, as
// vcpkgserver/helpers.go marshals it):
//
//	gcc-clang file prefix     3145758 B file   6293114 B body
//	msvc-compile file prefix  3145739 B file   6300960 B body
//	msvc-link file prefix     3145728 B file   6300890 B body
//	ninja FAILED target       3145728 B file   6301010 B body
//	Run Build Command(s)      3145734 B build_command  6292747 B body   <- no note at all
//	wrapper command:          3145758 B exact_command  9438938 B body
//	wrapper --overlay-ports   3145719 B overlay_chain[0]
//	tool-driver (control)           9 B file     15 KB body  <- the one bounded shape
func TestWireSize_EveryProducerIsBounded(t *testing.T) {
	pad := strings.Repeat("X", probeOversize)

	// A short, ordinary diagnostic so every case has a headline and reaches
	// status=failed — the cases that target a NON-diagnostic field would
	// otherwise return unknown(no_diagnostic_found) and never exercise the
	// field at all.
	const anchor = "C:\\src\\anchor.cpp(9,3): error C2065: 'anchor': undeclared identifier\n"

	for _, tc := range []struct {
		name    string
		log     string
		wrapper string
	}{
		{
			name: "gcc-clang file prefix",
			log:  anchor + pad + ":1:1: error: boom\n",
		},
		{
			name: "msvc-compile file prefix",
			log:  anchor + pad + "(1,1): error C2065: boom\n",
		},
		{
			name: "msvc-link file prefix",
			log:  anchor + pad + " : error LNK1120: boom\n",
		},
		{
			name: "tool-driver message (control: File is a closed allowlist)",
			log:  anchor + "lld-link: error: undefined symbol: " + pad + "\n",
		},
		{
			name: "ninja FAILED target",
			log:  anchor + "FAILED: [code=1] " + pad + "\n",
		},
		{
			name: "Run Build Command(s) tail",
			log:  anchor + "Run Build Command(s): " + pad + "\n",
		},
		{
			name:    "wrapper command: line (exact_command)",
			log:     anchor,
			wrapper: wrapperFixture("C:\\vcpkg\\vcpkg.exe install wireport " + pad),
		},
		{
			name:    "wrapper --overlay-ports value (overlay_chain)",
			log:     anchor,
			wrapper: wrapperFixture("C:\\vcpkg\\vcpkg.exe install wireport --overlay-ports=" + pad),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeInstallPhasePort(t, "wireport", 1, tc.log)
			args := Args{Port: "wireport", BuildtreesRoot: root, Triplet: "cl"}
			if tc.wrapper != "" {
				p := filepath.Join(t.TempDir(), "build_failed.log")
				if err := os.WriteFile(p, []byte(tc.wrapper), 0o644); err != nil {
					t.Fatal(err)
				}
				args.BuildFailedLog = p
			}
			res := LastFailure(args, Deps{FS: DefaultFS(), Getenv: func(string) string { return "" }})

			body, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			// Field-agnostic: the walk finds a string this test never named,
			// which is the whole point — the previous bound was written from an
			// enumeration of the fields already known to be unsafe.
			over := oversizeStrings(res, maxWireStringBytes())
			t.Logf("%-52s body=%8d B  oversize fields: %v", tc.name, len(body), over)

			if len(over) > 0 {
				t.Errorf("Result carries unbounded string(s) %v — every string a Result lifts out of a log or a "+
					"wrapper line must be bounded by the response budget, not only Diagnostic.Text", over)
			}
			if len(body) > wireBodyCeiling {
				t.Errorf("whole response is %d bytes (~%dk tokens), ceiling is %d — a single pathological line "+
					"still defeats the response budget", len(body), len(body)/4/1000, wireBodyCeiling)
			}
			// The bound must never be able to manufacture a different answer.
			if res.Status != Status("failed") {
				t.Errorf("status=%v reason=%v, want failed — the anchor diagnostic establishes the verdict and no "+
					"size bound may change it", res.Status, res.Reason)
			}
			if res.FirstError == nil {
				t.Fatal("first_error is nil on a failed verdict")
			}
			if len(res.Diagnostics) == 0 {
				t.Fatal("diagnostics is empty on a failed verdict")
			}
			if res.Diagnostics[0].Text != res.FirstError.Text || res.Diagnostics[0].File != res.FirstError.File {
				t.Errorf("diagnostics[0]=%+v but first_error=%+v — the headline must be the same diagnostic in "+
					"both fields, bounded identically", res.Diagnostics[0], *res.FirstError)
			}
		})
	}
}

// wrapperFixture renders a build_failed.log-shaped wrapper naming wireport as
// the single failed port, so the wrapper path is USED (a wrapper that named a
// different port would be discarded and the probe would measure nothing).
func wrapperFixture(command string) string {
	return "[2026-07-27 00:00:00] triplet=cl\n" +
		"command: " + command + "\n" +
		"exit_code: 1\n" +
		"build_failed_count: 1\n" +
		"failed_ports:\n" +
		"- wireport:cl\n"
}

// maxWireStringBytes is the largest any single string on the wire may be under
// the declared per-value caps, plus room for the in-band truncation marker.
// One number for the whole walk on purpose: the walk must not need to know
// which field it is looking at, or it becomes the same enumeration it exists
// to replace.
func maxWireStringBytes() int {
	m := MaxDiagnosticTextBytes
	// HEAD-MEASUREMENT SHIM: production constants not yet declared.
	if 32<<10 > m {
		m = 32 << 10
	}
	return m + len(truncationMarker) + 32
}

// oversizeStrings walks v reflectively and reports every string longer than
// max, named by its path within the value.
//
// It is deliberately reflective rather than a list of field accessors: the
// defect this file exists to catch is a field NOBODY listed. A walk sees a
// field added tomorrow; an enumeration sees only the fields whose danger was
// already understood, which is precisely how the previous bound came to cover
// one participant of its own class.
func oversizeStrings(v any, max int) []string {
	var out []string
	var walk func(rv reflect.Value, path string)
	walk = func(rv reflect.Value, path string) {
		switch rv.Kind() {
		case reflect.String:
			if rv.Len() > max {
				out = append(out, fmt.Sprintf("%s(%d bytes)", path, rv.Len()))
			}
		case reflect.Pointer, reflect.Interface:
			if !rv.IsNil() {
				walk(rv.Elem(), path)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < rv.Len(); i++ {
				walk(rv.Index(i), fmt.Sprintf("%s[%d]", path, i))
			}
		case reflect.Map:
			for _, k := range rv.MapKeys() {
				walk(rv.MapIndex(k), fmt.Sprintf("%s[%v]", path, k.Interface()))
			}
		case reflect.Struct:
			for i := 0; i < rv.NumField(); i++ {
				if !rv.Type().Field(i).IsExported() {
					continue
				}
				walk(rv.Field(i), path+"."+rv.Type().Field(i).Name)
			}
		}
	}
	walk(reflect.ValueOf(v), "result")
	sort.Strings(out)
	return out
}
