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

func TestHealthSnapshot_IncludeProbes_AddsProbesSection(t *testing.T) {
	a := NewAPI()
	original := a.HealthStatusFn
	defer func() { a.HealthStatusFn = original }()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		// When ProbeHealth=true, populate the Health field; when false,
		// leave it nil. Phase 3 must call StatusWithOpts(ProbeHealth=true)
		// only when IncludeProbes is set.
		if opts.ProbeHealth {
			return []DaemonStatus{
				{Server: "fs", Daemon: "fs-default", PID: 1234, Port: 9100, State: "Running",
					Health: &HealthProbe{OK: true, ToolCount: 5}},
			}, nil
		}
		return []DaemonStatus{
			{Server: "fs", Daemon: "fs-default", PID: 1234, Port: 9100, State: "Running"},
		}, nil
	}

	snap, err := a.HealthSnapshot(HealthOpts{IncludeProbes: true})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.Probes == nil {
		t.Fatal("Probes is nil, want populated section")
	}
	if len(snap.Probes.Items) != 1 {
		t.Fatalf("Probes.Items = %d, want 1: %+v", len(snap.Probes.Items), snap.Probes.Items)
	}
	if !snap.Probes.Items[0].OK || snap.Probes.Items[0].ToolCount != 5 {
		t.Errorf("probe row = %+v, want OK=true tool_count=5", snap.Probes.Items[0])
	}
	if snap.Probes.TTLMs != 10000 {
		t.Errorf("Probes.TTLMs = %d, want 10000", snap.Probes.TTLMs)
	}
}

func TestHealthSnapshot_PartialFailureDoesNotPoisonProbes(t *testing.T) {
	a := NewAPI()
	original := a.HealthStatusFn
	defer func() { a.HealthStatusFn = original }()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "ok", Daemon: "ok", State: "Running",
				Health: &HealthProbe{OK: true, ToolCount: 3}},
			{Server: "broken", Daemon: "broken", State: "Running",
				Health: &HealthProbe{OK: false, Err: "connection refused"}},
			// Third row: Health == nil. Exercises the else-branch where
			// the projection synthesizes the "no probe" sentinel so
			// downstream consumers don't see a phantom successful probe.
			{Server: "noprobe", Daemon: "noprobe", State: "Stopped"},
		}, nil
	}

	snap, _ := a.HealthSnapshot(HealthOpts{IncludeProbes: true})
	if snap.Probes == nil || len(snap.Probes.Items) != 3 {
		t.Fatalf("expected three rows, got: %+v", snap.Probes)
	}
	gotOK := snap.Probes.Items[0].OK && snap.Probes.Items[0].ToolCount == 3
	gotBroken := !snap.Probes.Items[1].OK && snap.Probes.Items[1].Err == "connection refused"
	if !gotOK || !gotBroken {
		t.Errorf("partial-failure rows mis-projected: %+v", snap.Probes.Items)
	}
	noProbe := snap.Probes.Items[2]
	if noProbe.OK || noProbe.Err != "no probe (daemon not running or probe disabled)" {
		t.Errorf("nil-health row mis-projected: %+v, want OK=false err=%q",
			noProbe, "no probe (daemon not running or probe disabled)")
	}
}

func TestHealthSnapshot_LazyProxyDoesNotMaterialize(t *testing.T) {
	a := NewAPI()
	original := a.HealthStatusFn
	defer func() { a.HealthStatusFn = original }()

	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		// Critical: ForceMaterialize must remain false when G2 calls in,
		// even when IncludeProbes is true. Synthetic rows answer
		// initialize+tools/list without spawning the heavy backend.
		if opts.ForceMaterialize {
			t.Fatalf("HealthSnapshot must NOT pass ForceMaterialize=true (lazy-proxy preservation)")
		}
		return []DaemonStatus{
			{Server: "ws-proxy", Daemon: "ws-proxy", State: "Ready",
				Health: &HealthProbe{OK: true, ToolCount: 7, Source: "proxy-synthetic"}},
		}, nil
	}

	snap, _ := a.HealthSnapshot(HealthOpts{IncludeProbes: true})
	if snap.Probes == nil || len(snap.Probes.Items) != 1 {
		t.Fatalf("want 1 probe row: %+v", snap.Probes)
	}
	if snap.Probes.Items[0].Source != "proxy-synthetic" {
		t.Errorf("Source = %q, want proxy-synthetic (preserve lazy-proxy semantic)",
			snap.Probes.Items[0].Source)
	}
}

func TestHealthSnapshot_IncludeCapabilities_AddsBothProbesAndCapabilities(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	originalCap := a.HealthCapabilityFn
	defer func() {
		a.HealthStatusFn = originalStatus
		a.HealthCapabilityFn = originalCap
	}()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "fs", Daemon: "fs-default", State: "Running",
				Health: &HealthProbe{OK: true, ToolCount: 1}},
		}, nil
	}
	a.HealthCapabilityFn = func(_ DaemonStatus) (CapabilityRow, error) {
		return CapabilityRow{
			Server: "fs", Daemon: "fs-default",
			Tools: CapabilitySubSection{State: "ok",
				Items: []CapabilityItem{{Name: "read_file",
					ID: "fs/fs-default/tool/read_file", Namespace: "fs", Kind: "tool"}}},
			Prompts:   CapabilitySubSection{State: "unsupported"},
			Resources: CapabilitySubSection{State: "empty"},
		}, nil
	}

	snap, err := a.HealthSnapshot(HealthOpts{IncludeCapabilities: true})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.Probes == nil {
		t.Errorf("IncludeCapabilities must imply IncludeProbes (Probes section missing)")
	}
	if snap.Capabilities == nil || len(snap.Capabilities.Items) != 1 {
		t.Fatalf("Capabilities = %+v, want 1 row", snap.Capabilities)
	}
	row := snap.Capabilities.Items[0]
	if row.Tools.State != "ok" || len(row.Tools.Items) != 1 {
		t.Errorf("tools = %+v, want state=ok with 1 item", row.Tools)
	}
	if row.Tools.Items[0].ID != "fs/fs-default/tool/read_file" {
		t.Errorf("canonical id = %q, want fs/fs-default/tool/read_file", row.Tools.Items[0].ID)
	}
	if row.Prompts.State != "unsupported" || row.Resources.State != "empty" {
		t.Errorf("state vocab = prompts:%s resources:%s, want unsupported/empty",
			row.Prompts.State, row.Resources.State)
	}
	if snap.Capabilities.TTLMs != 60000 {
		t.Errorf("Capabilities.TTLMs = %d, want 60000", snap.Capabilities.TTLMs)
	}
}

func TestHealthSnapshot_PartialFailureDoesNotPoisonCapabilities(t *testing.T) {
	a := NewAPI()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "ok", Daemon: "ok", State: "Running",
				Health: &HealthProbe{OK: true}},
			{Server: "broken", Daemon: "broken", State: "Running",
				Health: &HealthProbe{OK: true}},
		}, nil
	}
	a.HealthCapabilityFn = func(d DaemonStatus) (CapabilityRow, error) {
		if d.Server == "broken" {
			return CapabilityRow{Server: d.Server, Daemon: d.Daemon,
				Tools: CapabilitySubSection{State: "error", Err: "tools/list timeout"}}, nil
		}
		return CapabilityRow{Server: d.Server, Daemon: d.Daemon,
			Tools: CapabilitySubSection{State: "ok",
				Items: []CapabilityItem{{Name: "x", ID: "ok/ok/tool/x", Kind: "tool"}}}}, nil
	}

	snap, _ := a.HealthSnapshot(HealthOpts{IncludeCapabilities: true})
	if snap.Capabilities == nil || len(snap.Capabilities.Items) != 2 {
		t.Fatalf("want 2 rows: %+v", snap.Capabilities)
	}
	if snap.Capabilities.Items[0].Tools.State != "ok" {
		t.Errorf("ok row = %+v, want state=ok", snap.Capabilities.Items[0].Tools)
	}
	if snap.Capabilities.Items[1].Tools.State != "error" {
		t.Errorf("broken row = %+v, want state=error", snap.Capabilities.Items[1].Tools)
	}
}

func TestHealthSnapshot_CapabilitySkipsFailedProbe(t *testing.T) {
	a := NewAPI()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "down", Daemon: "down", State: "Failed",
				Health: &HealthProbe{OK: false, Err: "refused"}},
		}, nil
	}
	calls := 0
	a.HealthCapabilityFn = func(d DaemonStatus) (CapabilityRow, error) {
		calls++
		return CapabilityRow{}, nil
	}
	_, _ = a.HealthSnapshot(HealthOpts{IncludeCapabilities: true})
	if calls != 0 {
		t.Errorf("capability fn called %d times for failed-probe daemon, want 0", calls)
	}
}

// TestHealthSnapshot_SyntheticCapability_PopulatedFromCatalog is the
// regression test for the Task-4 review's Critical finding: Backend
// was dropped on the ProbeRow projection, so realCapabilityRow saw
// Backend="" for workspace-scoped lazy proxies and returned an empty
// synthetic tool list instead of the embedded catalog. Without the
// fix, every mcp-language-server / gopls-mcp daemon would silently
// report state="empty" tools in production.
//
// This test runs realCapabilityRow (via the default fn=nil path,
// NOT HealthCapabilityFn) so the synthetic branch is exercised
// end-to-end with a real Backend value.
func TestHealthSnapshot_SyntheticCapability_PopulatedFromCatalog(t *testing.T) {
	// Pick any backend that ToolCatalogForBackend has tools for; the
	// test asserts Tools.State == "ok" with at least one item — the
	// exact tool count is implementation-dependent (the catalog can
	// grow), but it must NOT be zero.
	backendKind := "mcp-language-server"
	catalog, ok := ToolCatalogForBackend(backendKind)
	if !ok || len(catalog.Tools) == 0 {
		t.Skipf("no synthetic catalog for backend kind %q in this build; skipping", backendKind)
	}

	a := NewAPI()
	originalStatus := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatus }()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{
				Server:  "lsp-go",
				Daemon:  "lsp-go-default",
				Backend: backendKind,
				Port:    9999, // never used: synthetic branch returns before any HTTP call
				State:   "Ready",
				Health:  &HealthProbe{OK: true, ToolCount: len(catalog.Tools), Source: "proxy-synthetic"},
			},
		}, nil
	}
	// Critical: leave HealthCapabilityFn nil so the production
	// realCapabilityRow path runs end-to-end.

	snap, err := a.HealthSnapshot(HealthOpts{IncludeCapabilities: true})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.Capabilities == nil || len(snap.Capabilities.Items) != 1 {
		t.Fatalf("Capabilities = %+v, want 1 row", snap.Capabilities)
	}
	row := snap.Capabilities.Items[0]
	if row.Tools.State != "ok" {
		t.Errorf("Tools.State = %q, want \"ok\" (synthetic catalog must populate, not return empty); items=%+v",
			row.Tools.State, row.Tools.Items)
	}
	if len(row.Tools.Items) == 0 {
		t.Errorf("Tools.Items empty — Backend=%q was dropped during projection?", backendKind)
	}
	// Sanity: every populated item has a canonical ID.
	for i, it := range row.Tools.Items {
		wantID := capabilityID(row.Server, row.Daemon, "tool", it.Name)
		if it.ID != wantID {
			t.Errorf("item[%d] ID = %q, want %q", i, it.ID, wantID)
		}
	}
}
