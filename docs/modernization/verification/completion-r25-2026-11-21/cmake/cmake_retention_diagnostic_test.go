package cmaketrace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

type generatedConsumerCase struct {
	Name       string   `json:"name"`
	File       string   `json:"file"`
	Command    string   `json:"command"`
	MaxRecords int      `json:"max_records"`
	Truncated  bool     `json:"truncated"`
	Projected  bool     `json:"projected"`
	Records    []Record `json:"records"`
}

type generatedConsumerOracle struct {
	Schema       int    `json:"schema"`
	TracePath    string `json:"trace_path"`
	TraceSHA256  string `json:"trace_sha256"`
	TraceBytes   int64  `json:"trace_bytes"`
	InputRecords int    `json:"input_records"`
	PayloadCount int    `json:"payload_records"`
	PublicLimit  int    `json:"public_limit"`
	Sources      []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"fixture_sources"`
	IncludeChain  []IncludeChainEntry     `json:"include_chain"`
	ExecutedLines []FileLines             `json:"executed_lines"`
	FilesInTrace  []string                `json:"files_in_trace"`
	Cases         []generatedConsumerCase `json:"cases"`
}

func generatedConsumerHash(tb testing.TB, path string) string {
	tb.Helper()
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		tb.Fatalf("hash %s: copy=%v close=%v", path, copyErr, closeErr)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func loadGeneratedConsumerOracle(tb testing.TB) generatedConsumerOracle {
	tb.Helper()
	path := `D:\dev\ChatGPT\mcp-r22-completion-20261121\cmake-consumer-work\producer\oracle.json`
	if !filepath.IsAbs(path) {
		tb.Fatal("absolute owned oracle path required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != "75cfff1d98fbc3ebd1e7d2acafc37d56f6999d0b8112de68ceefe80e634fb381" {
		tb.Fatal("retained original oracle changed")
	}
	var oracle generatedConsumerOracle
	if err := json.Unmarshal(raw, &oracle); err != nil {
		tb.Fatal(err)
	}
	if oracle.Schema != 1 || oracle.PublicLimit != publicresult.MaxEncodedBytes || oracle.PayloadCount != 16384 || len(oracle.Cases) != 3 || !filepath.IsAbs(oracle.TracePath) {
		tb.Fatal("oracle does not describe the pinned product/fixture contract")
	}
	for index, name := range []string{"selected-32", "selected-1000", "last-marker"} {
		if oracle.Cases[index].Name != name {
			tb.Fatal("unexpected case name/order")
		}
	}
	info, err := os.Stat(oracle.TracePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != oracle.TraceBytes || info.Size() > MaxTraceBytes {
		tb.Fatalf("trace file metadata mismatch: %v", err)
	}
	if got := generatedConsumerHash(tb, oracle.TracePath); got != oracle.TraceSHA256 {
		tb.Fatalf("trace SHA-256=%s, want %s", got, oracle.TraceSHA256)
	}
	for _, source := range oracle.Sources {
		if !filepath.IsAbs(source.Path) || generatedConsumerHash(tb, source.Path) != source.SHA256 {
			tb.Fatalf("fixture source changed: %s", source.Path)
		}
	}
	return oracle
}

func generatedConsumerExpected(oracle generatedConsumerOracle, query generatedConsumerCase) Result {
	return Result{
		Status: evidence.StatusOK, IncludeChain: oracle.IncludeChain, Records: query.Records,
		ExecutedLines: oracle.ExecutedLines, FilesInTrace: oracle.FilesInTrace,
		VersionHeaderPresent: true, Truncated: query.Truncated,
		Evidence: evidence.Evidence{Paths: []string{oracle.TracePath}},
	}
}

func checkGeneratedConsumer(tb testing.TB, oracle generatedConsumerOracle, query generatedConsumerCase, result Result, body []byte) int {
	tb.Helper()
	want := generatedConsumerExpected(oracle, query)
	if !reflect.DeepEqual(result, want) {
		tb.Fatalf("%s Trace result differs from independent CMake oracle: status=%s reason=%s records=%d incomplete=%v truncated=%v", query.Name, result.Status, result.Reason, len(result.Records), result.InputIncomplete, result.Truncated)
	}
	if len(body) == 0 || len(body) > publicresult.MaxEncodedBytes {
		tb.Fatalf("%s public bytes=%d exceed source-owned contract %d", query.Name, len(body), publicresult.MaxEncodedBytes)
	}
	var wire struct {
		Result
		Projection *publicresult.Projection `json:"result_projection"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		tb.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		tb.Fatal(err)
	}
	for _, field := range []string{"status", "malformed_line_count", "input_incomplete", "version_header_present", "truncated", "evidence"} {
		if _, present := fields[field]; !present {
			tb.Fatalf("%s omitted required public field %s", query.Name, field)
		}
	}
	if !query.Projected {
		if wire.Projection != nil || !reflect.DeepEqual(wire.Result, want) {
			tb.Fatalf("%s lost useful records, expanded arguments, indexes or completeness on the public boundary", query.Name)
		}
		return len(wire.Records)
	}
	if wire.Projection == nil || wire.Projection.Complete || len(wire.Projection.Omissions) != 4 {
		tb.Fatalf("%s missing explicit four-collection projection", query.Name)
	}
	want.Records, want.IncludeChain, want.ExecutedLines, want.FilesInTrace = nil, nil, nil, nil
	if !reflect.DeepEqual(wire.Result, want) {
		tb.Fatalf("%s projection lost verdict/evidence or retained an undeclared collection", query.Name)
	}
	wantOmitted := map[string]int{"include_chain": len(oracle.IncludeChain), "records": len(query.Records), "executed_lines": len(oracle.ExecutedLines), "files_in_trace": len(oracle.FilesInTrace)}
	for field := range wantOmitted {
		if _, present := fields[field]; present {
			tb.Fatalf("%s projected field %s must be omitted", query.Name, field)
		}
	}
	for _, omission := range wire.Projection.Omissions {
		count, found := wantOmitted[omission.Field]
		if !found || omission.Reason != publicresult.InternalProjectionLimit || omission.Retained != 0 || omission.Omitted == nil || *omission.Omitted != count {
			tb.Fatalf("%s wrong omission: %+v", query.Name, omission)
		}
		delete(wantOmitted, omission.Field)
	}
	return 0
}

// Diagnostic overlay only. The pinned trace and oracle are the retained R22
// inputs. This file is not an installed consumer and never invokes CMake.
// Forced collections deliberately reveal ownership transitions; the samples
// are not peak memory, resident working set, or an ordinary latency benchmark.
type consumerMemorySample struct {
	HeapAlloc    uint64 `json:"heap_alloc_bytes"`
	HeapObjects  uint64 `json:"heap_objects"`
	HeapInuse    uint64 `json:"heap_inuse_bytes"`
	HeapReleased uint64 `json:"heap_released_bytes"`
	StackInuse   uint64 `json:"stack_inuse_bytes"`
	Sys          uint64 `json:"runtime_sys_bytes"`
	TotalAlloc   uint64 `json:"total_alloc_bytes"`
	Mallocs      uint64 `json:"mallocs"`
	NumGC        uint32 `json:"gc_cycles"`
}

func consumerReadMemory() consumerMemorySample {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return consumerMemorySample{m.HeapAlloc, m.HeapObjects, m.HeapInuse, m.HeapReleased,
		m.StackInuse, m.Sys, m.TotalAlloc, m.Mallocs, m.NumGC}
}

func consumerCollectMemory() consumerMemorySample {
	// Two quiescent cycles reduce retained encoder-pool generations. These
	// remain whole-process observations, not exact sizes of individual values.
	runtime.GC()
	runtime.GC()
	return consumerReadMemory()
}

type consumerRetentionObservation struct {
	Case            string               `json:"case"`
	Clients         int                  `json:"clients"`
	Sample          int                  `json:"sample"`
	GoVersion       string               `json:"go_version"`
	GoMaxProcs      int                  `json:"gomaxprocs"`
	TraceSHA256     string               `json:"trace_sha256"`
	TraceBytes      int64                `json:"trace_bytes"`
	WireSHA256      string               `json:"wire_sha256"`
	WireBytes       int                  `json:"wire_bytes_each"`
	WireRecords     int                  `json:"wire_records_each"`
	InternalRecords int                  `json:"internal_records_each"`
	BackingCapacity int                  `json:"record_capacity_each"`
	WorkNanoseconds int64                `json:"work_ns_instrumented"`
	WorkerIntervals [][2]int64           `json:"worker_intervals_ns"`
	Before          consumerMemorySample `json:"before"`
	AfterWork       consumerMemorySample `json:"after_work"`
	ResultsAndWire  consumerMemorySample `json:"results_and_wire"`
	WireOnly        consumerMemorySample `json:"wire_only"`
	Released        consumerMemorySample `json:"released"`
}

func observeConsumerRetention(t *testing.T, oracle generatedConsumerOracle, query generatedConsumerCase, clients, sample int) consumerRetentionObservation {
	t.Helper()
	results := make([]Result, clients)
	bodies := make([][]byte, clients)
	errors := make([]error, clients)
	intervals := make([][2]int64, clients)
	start := make(chan struct{})
	var joined sync.WaitGroup
	// Keep the oracle and the fixed fixture infrastructure alive across every
	// snapshot. Each worker owns exactly one slice slot; Wait joins all access.
	row := consumerRetentionObservation{Case: query.Name, Clients: clients, Sample: sample,
		GoVersion: runtime.Version(), GoMaxProcs: runtime.GOMAXPROCS(0),
		TraceSHA256: oracle.TraceSHA256, TraceBytes: oracle.TraceBytes, WorkerIntervals: intervals, Before: consumerCollectMemory()}
	begin := time.Now()
	for index := range results {
		joined.Add(1)
		go func(index int) {
			defer joined.Done()
			<-start
			intervals[index][0] = time.Since(begin).Nanoseconds()
			results[index] = Trace(context.Background(), Args{TracePath: oracle.TracePath,
				File: query.File, Command: query.Command, MaxRecords: query.MaxRecords}, DefaultDeps())
			bodies[index], errors[index] = publicresult.MarshalIndent(results[index])
			intervals[index][1] = time.Since(begin).Nanoseconds()
		}(index)
	}
	close(start)
	joined.Wait()
	row.WorkNanoseconds = time.Since(begin).Nanoseconds()
	row.AfterWork = consumerReadMemory()
	for index := range results {
		if errors[index] != nil {
			t.Fatalf("consumer %d: %v", index, errors[index])
		}
		count := checkGeneratedConsumer(t, oracle, query, results[index], bodies[index])
		if index == 0 {
			row.WireRecords, row.WireBytes = count, len(bodies[index])
			row.InternalRecords, row.BackingCapacity = len(results[index].Records), cap(results[index].Records)
			digest := sha256.Sum256(bodies[index])
			row.WireSHA256 = hex.EncodeToString(digest[:])
		} else if !bytes.Equal(bodies[index], bodies[0]) {
			t.Fatal("concurrent callers produced different public bytes")
		}
	}
	row.ResultsAndWire = consumerCollectMemory()
	runtime.KeepAlive(results)
	runtime.KeepAlive(bodies)
	results = nil
	row.WireOnly = consumerCollectMemory()
	runtime.KeepAlive(bodies)
	bodies = nil
	row.Released = consumerCollectMemory()
	runtime.KeepAlive(oracle)
	runtime.KeepAlive(errors)
	runtime.KeepAlive(start)
	return row
}

func TestCMakeConsumerRetentionLifecycle(t *testing.T) {
	oracle := loadGeneratedConsumerOracle(t)
	for _, query := range oracle.Cases {
		// Warm the same filesystem and public encoder paths before observing.
		warm := Trace(context.Background(), Args{TracePath: oracle.TracePath,
			File: query.File, Command: query.Command, MaxRecords: query.MaxRecords}, DefaultDeps())
		body, err := publicresult.MarshalIndent(warm)
		if err != nil {
			t.Fatal(err)
		}
		checkGeneratedConsumer(t, oracle, query, warm, body)
		warm, body = Result{}, nil
		for _, clients := range []int{1, 4} {
			t.Run(fmt.Sprintf("%s/clients-%d", query.Name, clients), func(t *testing.T) {
				for sample := 1; sample <= 3; sample++ {
					row := observeConsumerRetention(t, oracle, query, clients, sample)
					raw, err := json.Marshal(row)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("R25_CMAKE_RETENTION %s", raw)
				}
			})
		}
	}
	// Recheck original oracle, trace and fixture digests after all consumers.
	loadGeneratedConsumerOracle(t)
}
