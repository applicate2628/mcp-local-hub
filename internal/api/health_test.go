package api

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthSnapshot_DefaultExcludesProbesAndCapabilities(t *testing.T) {
	a := NewAPI()
	snap, err := a.HealthSnapshot(HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.SchemaVersion != "1" {
		t.Errorf("SchemaVersion = %q, want \"1\"", snap.SchemaVersion)
	}
	if snap.Probes != nil {
		t.Errorf("Probes = %+v, want nil (default opts must omit expensive sections)", snap.Probes)
	}
	if snap.Capabilities != nil {
		t.Errorf("Capabilities = %+v, want nil", snap.Capabilities)
	}
}

func TestHealthSnapshot_JSONOmitsNilSections(t *testing.T) {
	a := NewAPI()
	snap, _ := a.HealthSnapshot(HealthOpts{})
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := string(b)
	if strings.Contains(body, `"probes"`) {
		t.Errorf("default JSON contains probes key, must be omitted: %s", body)
	}
	if strings.Contains(body, `"capabilities"`) {
		t.Errorf("default JSON contains capabilities key, must be omitted: %s", body)
	}
	if !strings.Contains(body, `"schema_version":"1"`) {
		t.Errorf("missing schema_version=1: %s", body)
	}
}

func TestCapabilityID_CanonicalForm(t *testing.T) {
	got := capabilityID("fs", "fs-default", "tool", "read_file")
	want := "fs/fs-default/tool/read_file"
	if got != want {
		t.Errorf("capabilityID = %q, want %q", got, want)
	}
}

func TestHealthSnapshot_HubSectionShape(t *testing.T) {
	a := NewAPI()
	snap, _ := a.HealthSnapshot(HealthOpts{})
	// Hub section is present (not optional) and carries schema-required fields,
	// even when zero-valued, so consumers can rely on the structure.
	if snap.Hub.Version == "" && snap.Hub.Commit == "" && snap.Hub.BuildDate == "" {
		// All three empty is acceptable in unit tests where build info isn't injected;
		// but the *fields* must exist (will fail compile if they don't).
		_ = snap.Hub.Version
		_ = snap.Hub.Commit
		_ = snap.Hub.BuildDate
		_ = snap.Hub.StartedAt
		_ = snap.Hub.Lock
		_ = snap.Hub.GeneratedAt
		_ = snap.Hub.TTLMs
	}
}

// TestHealthSnapshot_DaemonsSectionPopulated asserts the daemons
// section carries DaemonRow entries projected from the test-seam-injected
// statusFn. Uses HealthStatusFn (test seam introduced in Phase 2).
func TestHealthSnapshot_DaemonsSectionPopulated(t *testing.T) {
	a := NewAPI()
	originalStatusFn := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatusFn }()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "fs", Daemon: "fs-default", PID: 1234, Port: 9100,
				State: "Running", RAMBytes: 50_000_000, UptimeSec: 300},
		}, nil
	}

	snap, err := a.HealthSnapshot(HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if len(snap.Daemons.Items) != 1 {
		t.Fatalf("Daemons.Items = %d, want 1: %+v", len(snap.Daemons.Items), snap.Daemons.Items)
	}
	if snap.Daemons.Items[0].Server != "fs" || snap.Daemons.Items[0].PID != 1234 {
		t.Errorf("row 0 = %+v, want server=fs pid=1234", snap.Daemons.Items[0])
	}
	if snap.Daemons.TTLMs != 2000 {
		t.Errorf("Daemons.TTLMs = %d, want 2000", snap.Daemons.TTLMs)
	}
}

// TestHealthSnapshot_CacheServesWithinTTL: a second call within TTL
// returns the same GeneratedAt without invoking the underlying fn again.
func TestHealthSnapshot_CacheServesWithinTTL(t *testing.T) {
	a := NewAPI()
	original := a.HealthStatusFn
	defer func() { a.HealthStatusFn = original }()
	calls := 0
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{{Server: "x", Daemon: "x"}}, nil
	}

	snap1, _ := a.HealthSnapshot(HealthOpts{})
	snap2, _ := a.HealthSnapshot(HealthOpts{})

	if calls != 1 {
		t.Errorf("underlying fn called %d times, want 1 (second call must hit cache)", calls)
	}
	if snap1.Daemons.GeneratedAt != snap2.Daemons.GeneratedAt {
		t.Errorf("GeneratedAt diverged across cached calls: %d vs %d",
			snap1.Daemons.GeneratedAt, snap2.Daemons.GeneratedAt)
	}
}

// TestHealthSnapshot_CacheExpiresAfterTTL: forcing the now-clock past
// the TTL boundary triggers a re-run.
func TestHealthSnapshot_CacheExpiresAfterTTL(t *testing.T) {
	a := NewAPI()
	originalNow := a.HealthNowMs
	originalStatus := a.HealthStatusFn
	defer func() {
		a.HealthNowMs = originalNow
		a.HealthStatusFn = originalStatus
	}()

	now := int64(1_000_000)
	a.HealthNowMs = func() int64 { return now }

	calls := 0
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{}, nil
	}

	_, _ = a.HealthSnapshot(HealthOpts{})
	now += 2001 // > daemons TTL of 2000ms
	_, _ = a.HealthSnapshot(HealthOpts{})

	if calls != 2 {
		t.Errorf("underlying fn called %d times, want 2 (TTL expired)", calls)
	}
}

// TestHealthSnapshot_RefreshBustsCache: Refresh=true triggers a re-run
// even within TTL.
func TestHealthSnapshot_RefreshBustsCache(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	originalNow := a.HealthNowMs
	defer func() {
		a.HealthStatusFn = originalStatus
		a.HealthNowMs = originalNow
	}()
	now := int64(1_000_000)
	a.HealthNowMs = func() int64 { return now }
	calls := 0
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{}, nil
	}

	_, _ = a.HealthSnapshot(HealthOpts{})
	now += 100 // well within TTL
	_, _ = a.HealthSnapshot(HealthOpts{Refresh: true})

	if calls != 2 {
		t.Errorf("underlying fn called %d times, want 2 (Refresh must bust within TTL)", calls)
	}
}

// TestHealthSnapshot_SingleflightCollapsesConcurrent: N goroutines
// hitting expired-cache trigger exactly one underlying call.
func TestHealthSnapshot_SingleflightCollapsesConcurrent(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatus }()
	var calls atomic.Int32
	gate := make(chan struct{})
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls.Add(1)
		<-gate // hold first caller; concurrent goroutines pile up on singleflight
		return []DaemonStatus{}, nil
	}

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = a.HealthSnapshot(HealthOpts{})
		}()
	}
	time.Sleep(50 * time.Millisecond) // let goroutines reach the singleflight wait
	close(gate)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("underlying fn called %d times across %d goroutines, want 1 (singleflight)", got, N)
	}
}
