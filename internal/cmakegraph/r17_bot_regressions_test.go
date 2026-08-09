package cmakegraph

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestR17CoverageAggregateIsBoundedAndSignalsOmission(t *testing.T) {
	w := &walker{ctx: context.Background(), files: map[string]bool{}}
	for i := 0; i < MaxCoverageHolesLimit+3; i++ {
		w.recordCoverage(fmt.Sprintf("missing-%06d.cmake", i), CoverageEnumerateFailed, "denied")
	}
	if len(w.unscanned) != MaxCoverageHolesLimit || !w.coverageCapTruncated || w.droppedCoverageHoles != 3 {
		t.Fatalf("retained=%d truncated=%v dropped=%d", len(w.unscanned), w.coverageCapTruncated, w.droppedCoverageHoles)
	}
	if w.retainedCoverageBytes > MaxRetainedCoverageBytesLimit {
		t.Fatalf("retained coverage bytes=%d exceeds %d", w.retainedCoverageBytes, MaxRetainedCoverageBytesLimit)
	}
	result := w.result("root")
	if !result.CoverageCapTruncated || result.DroppedCoverageHoles != 3 || result.RetainedCoverageBytes != w.retainedCoverageBytes {
		t.Fatalf("result coverage accounting=%+v, want walker accounting forwarded", result)
	}

	bytesOnly := &walker{ctx: context.Background(), files: map[string]bool{}}
	bytesOnly.recordCoverage("missing.cmake", CoverageEnumerateFailed, strings.Repeat("x", int(MaxRetainedCoverageBytesLimit)))
	if len(bytesOnly.unscanned) != 0 || !bytesOnly.coverageCapTruncated || bytesOnly.droppedCoverageHoles != 1 {
		t.Fatalf("byte admission retained=%d truncated=%v dropped=%d", len(bytesOnly.unscanned), bytesOnly.coverageCapTruncated, bytesOnly.droppedCoverageHoles)
	}
}
