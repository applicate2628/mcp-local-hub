package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthSnapshot_DefaultExcludesProbesAndCapabilities(t *testing.T) {
	a := NewAPI()
	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{})
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
	snap, _ := a.HealthSnapshot(context.Background(), HealthOpts{})
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
	snap, _ := a.HealthSnapshot(context.Background(), HealthOpts{})
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

	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{})
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

	snap1, _ := a.HealthSnapshot(context.Background(), HealthOpts{})
	snap2, _ := a.HealthSnapshot(context.Background(), HealthOpts{})

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

	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{})
	now += 2001 // > daemons TTL of 2000ms
	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{})

	if calls != 2 {
		t.Errorf("underlying fn called %d times, want 2 (TTL expired)", calls)
	}
}

// TestHealthSnapshot_RefreshBustsCache: Refresh=true triggers a re-run
// even within TTL.
//
// NB: as of Phase 5 the per-section refresh rate-limit downgrades
// Refresh=true to a cache-read when fired within
// daemonsRefreshMinMs (1s) of the previous compute. To exercise the
// "Refresh busts within TTL" property without tripping the rate-limit,
// the test advances `now` past the rate-limit window but still within
// daemonsTTLMs (2s). Rate-limit-window-internal Refresh behavior has
// its own coverage in TestHealthSnapshot_RefreshRateLimited.
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

	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{})
	now += 1100 // past 1s rate-limit window, still within 2s TTL
	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{Refresh: true})

	if calls != 2 {
		t.Errorf("underlying fn called %d times, want 2 (Refresh must bust within TTL)", calls)
	}
}

// TestHealthSnapshot_RefreshRateLimited verifies that consecutive
// Refresh=true calls within the per-section minimum-interval get the
// cached value rather than triggering a fresh probe. Prevents local-DoS
// via repeated ?refresh=true on the handler.
func TestHealthSnapshot_RefreshRateLimited(t *testing.T) {
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

	// First Refresh: warms the cache.
	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{Refresh: true})
	if calls != 1 {
		t.Fatalf("first refresh: calls = %d, want 1", calls)
	}

	// Second Refresh within rate-limit window (1s for daemons): should
	// be downgraded to cached value, no new call.
	now += 100 // 100ms — well under 1000ms rate limit
	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{Refresh: true})
	if calls != 1 {
		t.Errorf("second refresh within rate limit: calls = %d, want 1 (rate-limit downgrade); now=%d", calls, now)
	}

	// Third Refresh after rate-limit window: triggers a real fetch.
	now += 1100 // total +1200ms from first call, > 1000ms rate limit
	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{Refresh: true})
	if calls != 2 {
		t.Errorf("third refresh after window: calls = %d, want 2", calls)
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
			_, _ = a.HealthSnapshot(context.Background(), HealthOpts{})
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

	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{IncludeProbes: true})
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

	snap, _ := a.HealthSnapshot(context.Background(), HealthOpts{IncludeProbes: true})
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

	snap, _ := a.HealthSnapshot(context.Background(), HealthOpts{IncludeProbes: true})
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

	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{IncludeCapabilities: true})
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

	snap, _ := a.HealthSnapshot(context.Background(), HealthOpts{IncludeCapabilities: true})
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
	_, _ = a.HealthSnapshot(context.Background(), HealthOpts{IncludeCapabilities: true})
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

	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{IncludeCapabilities: true})
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

// TestDaemonStatusSnapshot_SharesCacheWithHealthSnapshot verifies that
// /api/status (via DaemonStatusSnapshot) and /api/health's daemons
// section share the same cache slot. One StatusWithOpts call should
// serve both within TTL.
//
// Phase 6 of G2: per spec
// docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md
// the wire shape of /api/status is preserved (full DaemonStatus, including
// TaskName, NextRun, Health, and workspace-scoped fields), so we cache
// the canonical []DaemonStatus form and let HealthSnapshot project the
// thinner DaemonRow lazily. /api/status reads the canonical form
// directly, no projection-loss.
func TestDaemonStatusSnapshot_SharesCacheWithHealthSnapshot(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatus }()
	calls := 0
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{
			{Server: "x", Daemon: "x", TaskName: "mcp-local-hub-x-default",
				State: "Running", PID: 1, Port: 100, NextRun: "next-run-text"},
		}, nil
	}

	// First call: /api/health → warms the cache via computeDaemonsSection.
	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if len(snap.Daemons.Items) != 1 {
		t.Fatalf("Daemons.Items = %d, want 1", len(snap.Daemons.Items))
	}
	if calls != 1 {
		t.Fatalf("after HealthSnapshot: calls = %d, want 1", calls)
	}

	// Second call: /api/status → must hit the same cache.
	rows, err := a.DaemonStatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("DaemonStatusSnapshot: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Server != "x" || rows[0].State != "Running" {
		t.Errorf("rows[0] = %+v, want server=x state=Running", rows[0])
	}
	// Critical: full-fat fields (TaskName, NextRun) must survive — they
	// don't exist on DaemonRow, so a naive Snapshot.Daemons.Items
	// projection would have dropped them.
	if rows[0].TaskName != "mcp-local-hub-x-default" {
		t.Errorf("TaskName lost: %q (cache must hold canonical DaemonStatus, not the projected DaemonRow)", rows[0].TaskName)
	}
	if rows[0].NextRun != "next-run-text" {
		t.Errorf("NextRun lost: %q", rows[0].NextRun)
	}
	if calls != 1 {
		t.Errorf("DaemonStatusSnapshot triggered a new fetch: calls = %d, want 1 (cache must be shared)", calls)
	}

	// Mutating the returned slice must not poison the cache (defensive copy).
	rows[0].State = "MUTATED"
	rows2, _ := a.DaemonStatusSnapshot(context.Background())
	if rows2[0].State != "Running" {
		t.Errorf("cache leak: caller mutation affected next call: %q (defensive copy missing)", rows2[0].State)
	}
}

// TestDaemonStatusSnapshot_StatusFirstAlsoServesHealth verifies the
// reverse direction: /api/status warming the cache means /api/health's
// daemons section also reads from it (no extra StatusWithOpts call).
// Locks down the bidirectional cache-sharing contract.
func TestDaemonStatusSnapshot_StatusFirstAlsoServesHealth(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatus }()
	calls := 0
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{
			{Server: "y", Daemon: "y", State: "Running", PID: 7, Port: 200},
		}, nil
	}

	// First call: /api/status → warms cache.
	rows, err := a.DaemonStatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("DaemonStatusSnapshot: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if calls != 1 {
		t.Fatalf("after DaemonStatusSnapshot: calls = %d, want 1", calls)
	}

	// Second call: /api/health → must hit the cache primed by /api/status.
	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if len(snap.Daemons.Items) != 1 || snap.Daemons.Items[0].Server != "y" {
		t.Errorf("Daemons.Items = %+v, want one row with server=y", snap.Daemons.Items)
	}
	if calls != 1 {
		t.Errorf("HealthSnapshot triggered a new fetch: calls = %d, want 1 (cache must be shared bidirectionally)", calls)
	}
}

// TestDaemonStatusSnapshot_SurfacesFetchError verifies that
// /api/status's underlying DaemonStatusSnapshot propagates a fetch
// error from StatusWithOpts so the handler returns 500 STATUS_FAILED
// instead of 200 with empty []. Cloud bot P1 on PR #132 commit
// a8a54c1: the prior swallow behavior masked real backend failures
// (operational incidents looked like "no daemons running" instead
// of "status backend broken").
//
// Contract change vs. pre-fix: section.Errors[] still gets populated
// (so /api/health introspection clients see the failure scope), but
// the error ALSO bubbles out to the caller so /api/status restores
// its pre-G2 fail-loud HTTP-500 contract.
func TestDaemonStatusSnapshot_SurfacesFetchError(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatus }()

	sentinelErr := errors.New("status backend unavailable")
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return nil, sentinelErr
	}

	rows, err := a.DaemonStatusSnapshot(context.Background())
	if err == nil {
		t.Fatalf("DaemonStatusSnapshot returned nil error on backend failure; want propagated err. rows=%+v", rows)
	}
	if !strings.Contains(err.Error(), "status backend unavailable") {
		t.Errorf("err = %v, want to contain sentinel string", err)
	}
}

// TestHealthSnapshot_PropagatesDaemonFetchError verifies that a
// total backend failure on the daemons fetch surfaces as a non-nil
// error from HealthSnapshot too — Cloud bot P1 fix for PR #132.
// /api/health handler returns 500 + HEALTH_BACKEND_FAILED (not 200
// with empty Daemons.Items + section.Errors[] only).
func TestHealthSnapshot_PropagatesDaemonFetchError(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatus }()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return nil, errors.New("scheduler down")
	}
	_, err := a.HealthSnapshot(context.Background(), HealthOpts{})
	if err == nil {
		t.Fatalf("HealthSnapshot returned nil err on total backend failure; want propagated")
	}
	if !strings.Contains(err.Error(), "scheduler down") {
		t.Errorf("err = %v, want to contain sentinel", err)
	}
}

// TestHealthSnapshot_PropagatesProbeFetchError verifies probes path
// has the same fail-loud behavior on backend total failure. Cloud
// bot P1 fix for PR #132 — symmetric with the daemons-section fix.
func TestHealthSnapshot_PropagatesProbeFetchError(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	defer func() { a.HealthStatusFn = originalStatus }()
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		if !opts.ProbeHealth {
			// First call (daemons) succeeds; second call (probes) fails.
			return []DaemonStatus{}, nil
		}
		return nil, errors.New("probe backend down")
	}
	_, err := a.HealthSnapshot(context.Background(), HealthOpts{IncludeProbes: true})
	if err == nil {
		t.Fatalf("HealthSnapshot(IncludeProbes) returned nil err on probe fetch failure")
	}
	if !strings.Contains(err.Error(), "probe backend down") {
		t.Errorf("err = %v, want to contain sentinel", err)
	}
}

// TestDaemonStatusSnapshot_PropagatesErrorOnCacheHit is the regression
// test for Cloud bot P1 #1 on PR #132 commit 2062818: after a fetch
// failure, the next call within TTL must STILL return the error
// rather than the cached empty section. Otherwise /api/status flips
// from 500 to 200 within 2s while the backend is still down.
func TestDaemonStatusSnapshot_PropagatesErrorOnCacheHit(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	originalNow := a.HealthNowMs
	defer func() {
		a.HealthStatusFn = originalStatus
		a.HealthNowMs = originalNow
	}()
	now := int64(1_000_000)
	a.HealthNowMs = func() int64 { return now }
	callCount := 0
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		callCount++
		return nil, errors.New("scheduler down")
	}

	// First call: backend fails → err propagated.
	_, err1 := a.DaemonStatusSnapshot(context.Background())
	if err1 == nil {
		t.Fatalf("first call: expected err, got nil")
	}

	// Second call within TTL: must STILL return the err. The bug was
	// that the cache-hit fast-path returned (cached, nil) and the
	// 500 flipped to 200 even though the backend was still broken.
	now += 100 // well within 2s daemons TTL
	_, err2 := a.DaemonStatusSnapshot(context.Background())
	if err2 == nil {
		t.Fatalf("second call (cache hit) returned nil err — error masking regression on /api/status")
	}
	if !strings.Contains(err2.Error(), "scheduler down") {
		t.Errorf("err2 = %v, want propagated 'scheduler down'", err2)
	}

	// Third call after TTL expires: backend re-attempted (still failing,
	// call counter increments).
	now += 2000 // total 2100ms — past 2s TTL
	_, err3 := a.DaemonStatusSnapshot(context.Background())
	if err3 == nil {
		t.Fatalf("third call (TTL expired): expected err, got nil")
	}
	if callCount < 2 {
		t.Errorf("callCount = %d, want >= 2 (TTL expiry should re-attempt)", callCount)
	}
}

// TestHealthSnapshot_PropagatesProbeErrorOnCacheHit is the regression
// test for Cloud bot P1 #2 on PR #132 commit 2062818. Same pattern
// as daemons but for the probes section (10s TTL).
func TestHealthSnapshot_PropagatesProbeErrorOnCacheHit(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	originalNow := a.HealthNowMs
	defer func() {
		a.HealthStatusFn = originalStatus
		a.HealthNowMs = originalNow
	}()
	now := int64(2_000_000)
	a.HealthNowMs = func() int64 { return now }
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		if opts.ProbeHealth {
			return nil, errors.New("probe backend down")
		}
		// Daemons fetch (no probe) succeeds — empty list is fine.
		return []DaemonStatus{}, nil
	}

	// First call: probes fetch fails.
	_, err1 := a.HealthSnapshot(context.Background(), HealthOpts{IncludeProbes: true})
	if err1 == nil {
		t.Fatalf("first call: expected err on probe failure, got nil")
	}

	// Second call within probes TTL (10s): err must still propagate.
	now += 500 // well within 10s probes TTL
	_, err2 := a.HealthSnapshot(context.Background(), HealthOpts{IncludeProbes: true})
	if err2 == nil {
		t.Fatalf("second call (cache hit) returned nil err — probe error masking on /api/health")
	}
	if !strings.Contains(err2.Error(), "probe backend down") {
		t.Errorf("err2 = %v, want propagated 'probe backend down'", err2)
	}
}

// TestDaemonStatusSnapshot_RecoversAfterBackendComesBack verifies the
// happy path: when the backend recovers, subsequent calls succeed and
// the cached error is cleared.
func TestDaemonStatusSnapshot_RecoversAfterBackendComesBack(t *testing.T) {
	a := NewAPI()
	originalStatus := a.HealthStatusFn
	originalNow := a.HealthNowMs
	defer func() {
		a.HealthStatusFn = originalStatus
		a.HealthNowMs = originalNow
	}()
	now := int64(3_000_000)
	a.HealthNowMs = func() int64 { return now }
	failing := true
	a.HealthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		if failing {
			return nil, errors.New("transient failure")
		}
		return []DaemonStatus{{Server: "ok", Daemon: "ok", State: "Running"}}, nil
	}

	// Fail.
	_, err1 := a.DaemonStatusSnapshot(context.Background())
	if err1 == nil {
		t.Fatalf("expected initial error")
	}

	// Backend recovers; advance past TTL so the cache re-fetches.
	failing = false
	now += 2100 // past 2s daemons TTL

	rows, err2 := a.DaemonStatusSnapshot(context.Background())
	if err2 != nil {
		t.Fatalf("after recovery + TTL expiry: expected nil err, got %v", err2)
	}
	if len(rows) != 1 || rows[0].Server != "ok" {
		t.Errorf("expected one row Server=ok after recovery; got %+v", rows)
	}

	// Subsequent calls within TTL should also succeed (cached err cleared).
	now += 100
	rows3, err3 := a.DaemonStatusSnapshot(context.Background())
	if err3 != nil {
		t.Errorf("post-recovery cache hit: expected nil err, got %v", err3)
	}
	if len(rows3) != 1 {
		t.Errorf("post-recovery cache hit: expected 1 row, got %d", len(rows3))
	}
}

// TestNormalizeDaemonState_EnrichedAndUnexpectedStates verifies the
// closed wire enum contract for DaemonRow.State. The mapping covers
// the existing Title-Case vocabulary that bubbles up from the
// scheduler/health pipeline; unexpected inputs (and the empty string)
// must collapse to "failed" so the wire enum remains exhaustive.
//
// Codex Cloud bot P1 on PR #135 round 2: a permissive default branch
// that lower-cased and passed through arbitrary scheduler states
// (e.g. "Disabled", "Queued") leaked off-spec values onto the
// 4-value wire enum, breaking health consumers that branch on it.
func TestNormalizeDaemonState_EnrichedAndUnexpectedStates(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "Running", want: "running"},
		{in: "Starting", want: "starting"},
		{in: "Ready", want: "stopped"},
		{in: "Scheduled", want: "stopped"},
		{in: "Stopped", want: "stopped"},
		{in: "Failed", want: "failed"},
		// Unknown scheduler vocabulary collapses to the conservative
		// "failed" wire value rather than leaking through unchanged.
		{in: "Disabled", want: "failed"},
		{in: "Queued", want: "failed"},
		// Empty / blank source is also conservative-failed; never
		// promote no-data into a misleading wire enum slot.
		{in: "", want: "failed"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeDaemonState(tc.in); got != tc.want {
				t.Fatalf("normalizeDaemonState(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
