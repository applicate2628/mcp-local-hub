package cmaketrace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

type traceBenchmarkCase struct {
	pattern string
	records int
}

func traceBenchmarkCases() []traceBenchmarkCase {
	var cases []traceBenchmarkCase
	for _, pattern := range []string{"repeated", "unique", "mixed", "long_args"} {
		for _, records := range []int{1000, 50000} {
			cases = append(cases, traceBenchmarkCase{pattern, records})
		}
	}
	return append(cases, traceBenchmarkCase{"repeated", 200000})
}

// Synthetic json-v1 input; generation is excluded from every timed region.
// No external CMake invocation, project file or vendor installation is used.
func benchmarkTracePayload(tb testing.TB, records int, pattern string) string {
	tb.Helper()
	if records <= 0 {
		tb.Fatal("benchmark record count must be positive")
	}
	if pattern != "repeated" && pattern != "unique" && pattern != "mixed" && pattern != "long_args" {
		tb.Fatalf("unknown benchmark pattern %q", pattern)
	}
	var fixture strings.Builder
	fixture.Grow(records * 160)
	fixture.WriteString("{\"version\":{\"major\":1,\"minor\":0}}\n")
	encoder := json.NewEncoder(&fixture)
	longArg := strings.Repeat("λ", 256)
	commands := []string{"set", "include", "add_subdirectory", "MESSAGE"}
	for i := 0; i < records; i++ {
		record := Record{File: "/fixture/CMakeLists.txt", Line: i + 1, Cmd: "set", Args: []string{"X", "Y"}, Time: 1.25, Frame: 1, GlobalFrame: 1}
		switch pattern {
		case "unique":
			record.File = fmt.Sprintf("/fixture/unit-%06d.cmake", i)
		case "mixed":
			record.Cmd = commands[i%len(commands)]
			record.Defer = i%len(commands) == 3
		case "long_args":
			record.Args[1] = longArg
		}
		if err := encoder.Encode(record); err != nil {
			tb.Fatalf("encode fixture: %v", err)
		}
	}
	return fixture.String()
}

// Parser-only: the input string remains resident. B/op is allocation traffic,
// not peak RSS. Trace indexes, public encoding, filesystem and MCP are excluded.
func BenchmarkParseTraceStream(b *testing.B) {
	for _, tc := range traceBenchmarkCases() {
		b.Run(fmt.Sprintf("%s/records_%d", tc.pattern, tc.records), func(b *testing.B) {
			payload := benchmarkTracePayload(b, tc.records, tc.pattern)
			ctx := context.Background()
			preflight, err := parseTraceStream(ctx, strings.NewReader(payload), Limits{})
			if err != nil || len(preflight.records) != tc.records || preflight.incomplete() || !preflight.versionHeaderPresent {
				b.Fatalf("preflight: records=%d want=%d incomplete=%v err=%v", len(preflight.records), tc.records, preflight.incomplete(), err)
			}
			preflight = parseResult{}
			runtime.GC()
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := parseTraceStream(ctx, strings.NewReader(payload), Limits{})
				if err != nil || len(result.records) != tc.records || result.incomplete() {
					b.Fatalf("parse: records=%d want=%d incomplete=%v err=%v", len(result.records), tc.records, result.incomplete(), err)
				}
				runtime.KeepAlive(result)
			}
			b.ReportMetric(float64(tc.records), "records/op")
		})
	}
}

// Actual file open + Trace, including indexes and the default returned-record
// cap. Filesystem caches are warm; fixture generation/write are not timed.
func BenchmarkTracePublicResult(b *testing.B) { benchmarkTracePublic(b, "result") }

// The same bounded encoder the vcpkg MCP publisher uses, NOT json.Marshal of
// the unbounded internal Result. The prepared Result is deliberately retained.
func BenchmarkTraceResultJSON(b *testing.B) { benchmarkTracePublic(b, "json") }

// File open -> Trace -> bounded public JSON. This does not include the MCP SDK,
// HTTP/stdio transport, external CMake execution, or a remote consumer.
func BenchmarkTraceEndToEnd(b *testing.B) { benchmarkTracePublic(b, "end_to_end") }

func benchmarkTracePublic(b *testing.B, stage string) {
	b.Helper()
	for _, tc := range traceBenchmarkCases() {
		b.Run(fmt.Sprintf("%s/records_%d", tc.pattern, tc.records), func(b *testing.B) {
			payload := benchmarkTracePayload(b, tc.records, tc.pattern)
			inputBytes := len(payload)
			path := filepath.Join(b.TempDir(), "trace.jsonl")
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				b.Fatal(err)
			}
			payload = ""
			ctx := context.Background()
			args := Args{TracePath: path}
			deps := DefaultDeps()
			prepared := Trace(ctx, args, deps)
			checkBenchmarkTraceResult(b, prepared, tc.records)
			encodedBytes := 0
			if stage != "result" {
				encoded, err := publicresult.MarshalIndent(prepared)
				if err != nil || len(encoded) > publicresult.MaxEncodedBytes {
					b.Fatalf("public preflight: bytes=%d err=%v", len(encoded), err)
				}
				encodedBytes = len(encoded)
			}
			if stage != "json" {
				prepared = Result{}
			}
			runtime.GC()
			if stage == "json" {
				b.SetBytes(int64(encodedBytes))
			} else {
				b.SetBytes(int64(inputBytes))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result := prepared
				if stage != "json" {
					result = Trace(ctx, args, deps)
					checkBenchmarkTraceResult(b, result, tc.records)
				}
				if stage != "result" {
					encoded, err := publicresult.MarshalIndent(result)
					if err != nil || len(encoded) != encodedBytes {
						b.Fatalf("public encoding changed: bytes=%d want=%d err=%v", len(encoded), encodedBytes, err)
					}
					runtime.KeepAlive(encoded)
				}
				runtime.KeepAlive(result)
			}
			b.ReportMetric(float64(min(tc.records, DefaultMaxRecords)), "result-records/op")
			if stage != "result" {
				b.ReportMetric(float64(encodedBytes), "wire-B/op")
			}
		})
	}
}

func checkBenchmarkTraceResult(tb testing.TB, result Result, inputRecords int) {
	tb.Helper()
	if result.Status != evidence.StatusOK || result.InputIncomplete || len(result.Records) != min(inputRecords, DefaultMaxRecords) || result.Truncated != (inputRecords > DefaultMaxRecords) {
		tb.Fatalf("public result: status=%s reason=%s records=%d incomplete=%v truncated=%v", result.Status, result.Reason, len(result.Records), result.InputIncomplete, result.Truncated)
	}
}
