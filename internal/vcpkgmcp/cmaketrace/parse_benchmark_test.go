package cmaketrace

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkParseTraceStream(b *testing.B) {
	for _, records := range []int{1000, 50000} {
		b.Run(fmt.Sprintf("records_%d", records), func(b *testing.B) {
			var fixture strings.Builder
			fixture.Grow(records * 130)
			fixture.WriteString("{\"version\":{\"major\":1,\"minor\":0}}\n")
			for i := 0; i < records; i++ {
				fmt.Fprintf(&fixture, "{\"file\":\"/tmp/cmake/CMakeLists.txt\",\"line\":%d,\"cmd\":\"set\",\"args\":[\"X\",\"Y\"],\"time\":1.0,\"frame\":1,\"global_frame\":1}\n", i+1)
			}
			payload := fixture.String()
			ctx := context.Background()
			preflight, err := parseTraceStream(ctx, strings.NewReader(payload), Limits{})
			if err != nil || len(preflight.records) != records || preflight.incomplete() {
				b.Fatalf("preflight records=%d expected=%d incomplete=%v err=%v", len(preflight.records), records, preflight.incomplete(), err)
			}
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := parseTraceStream(ctx, strings.NewReader(payload), Limits{})
				if err != nil || len(result.records) != records {
					b.Fatalf("parse result len=%d expected=%d err=%v", len(result.records), records, err)
				}
			}
		})
	}
}
