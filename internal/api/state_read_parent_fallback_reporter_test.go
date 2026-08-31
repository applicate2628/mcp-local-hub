package api

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestStateReadParentFallbackReporterSuppressesRepeatsAndReportsAtInterval(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var got []fallbackReport
	reporter := newStateReadParentFallbackReporter(func() time.Time { return now }, func(level, event string, fields map[string]any) error {
		got = append(got, fallbackReport{level: level, event: event, fields: cloneFallbackFields(fields)})
		return nil
	}, 256)
	fields := parentFallbackFields("C:/state/supervisor-intent.json", "read-only", "SID=S-1-5-11")

	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
		t.Fatalf("first fallback: %v", err)
	}
	now = now.Add(time.Minute)
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
		t.Fatalf("repeat fallback: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events after identical repeat = %d, want 1", len(got))
	}

	now = now.Add(14 * time.Minute)
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
		t.Fatalf("interval fallback: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events after interval = %d, want 2", len(got))
	}
	if aggregation, _ := got[1].fields[stateReadFallbackAggregationField].(string); aggregation != stateReadFallbackAggregationPeriodic {
		t.Fatalf("periodic aggregation = %q, want %q", aggregation, stateReadFallbackAggregationPeriodic)
	}
	if count, _ := got[1].fields[stateReadFallbackSuppressedCountField].(int); count != 2 {
		t.Fatalf("periodic suppressed count = %d, want 2", count)
	}
}

func TestStateReadParentFallbackReporterTransitionFinalizesPriorBeforeNewObservation(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var got []fallbackReport
	reporter := newStateReadParentFallbackReporter(func() time.Time { return now }, func(level, event string, fields map[string]any) error {
		got = append(got, fallbackReport{level: level, event: event, fields: cloneFallbackFields(fields)})
		return nil
	}, 256)
	old := parentFallbackFields("C:/state/supervisor-intent.json", "read-only", "SID=S-1-5-11")
	newer := parentFallbackFields("C:/state/supervisor-intent.json", "write-capable", "SID=S-1-5-32-545")
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, old); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, newer); err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("events = %d, want old immediate + old transition final + new immediate", len(got))
	}
	if aggregation, _ := got[1].fields[stateReadFallbackAggregationField].(string); aggregation != stateReadFallbackAggregationTransition {
		t.Fatalf("prior aggregation = %q, want transition", aggregation)
	}
	if reason, _ := got[2].fields["reason"].(string); reason != "write-capable" {
		t.Fatalf("new immediate reason = %q, want write-capable", reason)
	}
}

func TestStateReadParentFallbackReporterKeepsSimultaneousReasonsForOneReadIndependent(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var got []fallbackReport
	reporter := newStateReadParentFallbackReporter(func() time.Time { return now }, func(level, event string, fields map[string]any) error {
		got = append(got, fallbackReport{level: level, event: event, fields: cloneFallbackFields(fields)})
		return nil
	}, 256)
	path := "C:/state/supervisor-intent.json"
	writeBroadening := parentFallbackFields(path, "write-capable", "0777")
	readBroadening := parentFallbackFields(path, "read-exec", "0777")

	if err := reporter.observeParentFallbacks(path, []stateReadParentFallbackObservation{
		{level: "warn", event: stateReadUnhardenedParentFallbackEvent, fields: writeBroadening},
		{level: "warn", event: stateReadUnhardenedParentFallbackEvent, fields: readBroadening},
	}); err != nil {
		t.Fatalf("first dual observation: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("first dual observation events = %d, want one immediate event per reason", len(got))
	}
	for _, row := range got {
		if aggregation, _ := row.fields[stateReadFallbackAggregationField].(string); aggregation == stateReadFallbackAggregationTransition {
			t.Fatalf("simultaneous reason was misclassified as transition: %#v", row.fields)
		}
	}

	now = now.Add(time.Minute)
	if err := reporter.observeParentFallbacks(path, []stateReadParentFallbackObservation{
		{level: "warn", event: stateReadUnhardenedParentFallbackEvent, fields: writeBroadening},
		{level: "warn", event: stateReadUnhardenedParentFallbackEvent, fields: readBroadening},
	}); err != nil {
		t.Fatalf("repeat dual observation: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("repeat dual observation emitted %d events, want no churn", len(got))
	}

	if err := reporter.observeParentFallbacks(path, nil); err != nil {
		t.Fatalf("clean observation: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("clean observation events = %d, want a settled final for each reason", len(got))
	}
	for _, row := range got[2:] {
		if aggregation, _ := row.fields[stateReadFallbackAggregationField].(string); aggregation != stateReadFallbackAggregationSettled {
			t.Fatalf("clean final aggregation = %q, want settled", aggregation)
		}
		if count, _ := row.fields[stateReadFallbackSuppressedCountField].(int); count != 1 {
			t.Fatalf("clean final suppressed count = %d, want 1", count)
		}
	}
}

func TestStateReadParentFallbackReporterSettlesAndForgetsCleanPath(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var got []fallbackReport
	reporter := newStateReadParentFallbackReporter(func() time.Time { return now }, func(level, event string, fields map[string]any) error {
		got = append(got, fallbackReport{level: level, event: event, fields: cloneFallbackFields(fields)})
		return nil
	}, 256)
	fields := parentFallbackFields("C:/state/supervisor-intent.json", "read-only", "SID=S-1-5-11")
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
		t.Fatal(err)
	}
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
		t.Fatal(err)
	}
	if err := reporter.observeClean("C:/state/supervisor-intent.json"); err != nil {
		t.Fatalf("clean observation: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events after settle = %d, want 2", len(got))
	}
	if aggregation, _ := got[1].fields[stateReadFallbackAggregationField].(string); aggregation != stateReadFallbackAggregationSettled {
		t.Fatalf("settled aggregation = %q, want settled", aggregation)
	}
	if count, _ := got[1].fields[stateReadFallbackSuppressedCountField].(int); count != 1 {
		t.Fatalf("settled suppressed count = %d, want 1", count)
	}
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
		t.Fatalf("fallback after settled: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("fallback after settled must be a fresh immediate event, got %d events", len(got))
	}
}

func TestStateReadParentFallbackReporterEvictsLeastRecentlyObservedEntryVisibly(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var got []fallbackReport
	reporter := newStateReadParentFallbackReporter(func() time.Time { return now }, func(level, event string, fields map[string]any) error {
		got = append(got, fallbackReport{level: level, event: event, fields: cloneFallbackFields(fields)})
		return nil
	}, 2)
	for _, path := range []string{"C:/state/a.json", "C:/state/b.json", "C:/state/c.json"} {
		if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, parentFallbackFields(path, "read-only", "SID=S-1-5-11")); err != nil {
			t.Fatalf("fallback %s: %v", path, err)
		}
		now = now.Add(time.Minute)
	}
	if len(got) != 4 {
		t.Fatalf("events = %d, want a + b + visible a eviction + c", len(got))
	}
	if aggregation, _ := got[2].fields[stateReadFallbackAggregationField].(string); aggregation != stateReadFallbackAggregationEvicted {
		t.Fatalf("eviction aggregation = %q, want evicted", aggregation)
	}
	if path, _ := got[2].fields["path"].(string); path != "C:/state/a.json" {
		t.Fatalf("evicted path = %q, want oldest a", path)
	}
}

func TestStateReadParentFallbackReporterRetriesFailedEmissionAndCountsConcurrentRepeatsExactly(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	attempts := 0
	var got []fallbackReport
	reporter := newStateReadParentFallbackReporter(func() time.Time { return now }, func(level, event string, fields map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return errors.New("synthetic log failure")
		}
		got = append(got, fallbackReport{level: level, event: event, fields: cloneFallbackFields(fields)})
		return nil
	}, 256)
	fields := parentFallbackFields("C:/state/supervisor-intent.json", "read-only", "SID=S-1-5-11")
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err == nil {
		t.Fatal("first failed emit returned nil")
	}
	if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
		t.Fatalf("retry emit: %v", err)
	}

	const repeats = 32
	var wg sync.WaitGroup
	for range repeats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := reporter.observeFallback("warn", stateReadUnhardenedParentFallbackEvent, fields); err != nil {
				t.Errorf("concurrent repeat: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := reporter.observeClean("C:/state/supervisor-intent.json"); err != nil {
		t.Fatalf("clean after concurrent repeats: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("successful events = %d, want retry immediate + settled", len(got))
	}
	if count, _ := got[1].fields[stateReadFallbackSuppressedCountField].(int); count != repeats {
		t.Fatalf("settled suppressed count = %d, want %d", count, repeats)
	}
}

type fallbackReport struct {
	level  string
	event  string
	fields map[string]any
}

func parentFallbackFields(path, reason, securityFingerprint string) map[string]any {
	return map[string]any{
		"path":      path,
		"parent":    "C:/state",
		"reason":    reason,
		"err":       securityFingerprint,
		"note":      "handle-bound",
		"unrelated": fmt.Sprintf("fixture-%s", path),
	}
}

func cloneFallbackFields(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
