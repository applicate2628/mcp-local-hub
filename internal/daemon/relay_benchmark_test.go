package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

// Includes a complete relay lifetime against a shared local HTTP fixture:
// initialize, notification, 32 concurrent POSTs, stdout consumption and DELETE.
// Allocations include the Go fake server/driver; this is NOT per-executable RSS
// or external-server latency. A bounded, warm shared transport (64 connections)
// isolates the relay path from short-lived socket churn in the load generator.
// This is explicitly not the default production HTTP transport configuration.
func BenchmarkHTTPToStdioRelay(b *testing.B) {
	for _, stream := range []bool{false, true} {
		for _, size := range []int{1024, 65536} {
			b.Run(fmt.Sprintf("sse_%t/payload_%d", stream, size), func(b *testing.B) {
				const calls = 32
				f := newRelayLoadServer(b)
				client := f.client
				input := relayLoadInput(calls, size, stream)
				var output bytes.Buffer
				preflight := HTTPToStdioRelay{URL: f.server.URL, Stdin: bytes.NewReader(input), Stdout: &output, Stderr: io.Discard, HTTPClient: client}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := preflight.Run(ctx)
				cancel()
				if err != nil {
					b.Fatal(err)
				}
				if err = verifyRelayLoadOutput(output.Bytes(), calls, size); err != nil {
					b.Fatal(err)
				}
				output = bytes.Buffer{}
				startCalls, startDeletes := f.calls.Load(), f.deleted.Load()
				b.SetBytes(int64(calls * size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					beforeCalls, beforeDeletes := f.calls.Load(), f.deleted.Load()
					r := HTTPToStdioRelay{URL: f.server.URL, Stdin: bytes.NewReader(input), Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					err := r.Run(ctx)
					cancel()
					if err != nil || f.calls.Load()-beforeCalls != calls || f.deleted.Load()-beforeDeletes != 1 {
						b.Fatalf("iteration failed: err=%v calls=%d deletes=%d", err, f.calls.Load()-beforeCalls, f.deleted.Load()-beforeDeletes)
					}
				}
				b.StopTimer()
				if f.calls.Load()-startCalls != int64(b.N*calls) || f.deleted.Load()-startDeletes != int64(b.N) {
					b.Fatal("benchmark lost requests or session cleanup")
				}
				b.ReportMetric(calls, "calls/op")
			})
		}
	}
}
