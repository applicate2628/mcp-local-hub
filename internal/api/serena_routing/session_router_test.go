package serena_routing

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func newWorkspaceFixture(port int) *api.WorkspaceEntry {
	return &api.WorkspaceEntry{
		WorkspaceKey:  "0123abcd",
		WorkspacePath: "D:/dev/fixture",
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          port,
		TaskName:      "mcp-local-hub-serena-fixture",
	}
}

func TestSessionRouter_BindOnPathCall(t *testing.T) {
	r := NewSessionRouter()
	ws := newWorkspaceFixture(9301)

	r.BindSession("sess-1", ws)

	got := r.LookupSession("sess-1")
	if got == nil {
		t.Fatal("LookupSession returned nil after Bind")
	}
	if got.Port != 9301 {
		t.Errorf("Port = %d, want 9301", got.Port)
	}
	if got.WorkspacePath != ws.WorkspacePath {
		t.Errorf("WorkspacePath = %q, want %q", got.WorkspacePath, ws.WorkspacePath)
	}
}

func TestSessionRouter_LookupUnboundReturnsNil(t *testing.T) {
	r := NewSessionRouter()
	if got := r.LookupSession("never-bound"); got != nil {
		t.Errorf("LookupSession(never-bound) = %+v, want nil", got)
	}
}

func TestSessionRouter_UnbindRemovesEntry(t *testing.T) {
	r := NewSessionRouter()
	r.BindSession("sess-1", newWorkspaceFixture(9301))

	if got := r.LookupSession("sess-1"); got == nil {
		t.Fatal("pre-unbind lookup returned nil")
	}

	r.UnbindSession("sess-1")

	if got := r.LookupSession("sess-1"); got != nil {
		t.Errorf("LookupSession after Unbind = %+v, want nil", got)
	}
	if got := r.Len(); got != 0 {
		t.Errorf("Len = %d, want 0 after Unbind", got)
	}
}

func TestSessionRouter_CleanupExpiresOldSessions(t *testing.T) {
	// Use an injectable clock so we can advance time without sleeping.
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	now := t0
	r := NewSessionRouterWithClock(func() time.Time { return now })

	r.BindSession("sess-A", newWorkspaceFixture(9301))
	r.BindSession("sess-B", newWorkspaceFixture(9302))
	if got := r.Len(); got != 2 {
		t.Fatalf("Len after 2 binds = %d, want 2", got)
	}

	// Advance 25h past the original bind time. Both bindings should expire.
	now = t0.Add(25 * time.Hour)
	expired := r.Cleanup(now)
	if expired != 2 {
		t.Errorf("Cleanup expired = %d, want 2", expired)
	}
	if got := r.Len(); got != 0 {
		t.Errorf("Len after Cleanup = %d, want 0", got)
	}
}

func TestSessionRouter_CleanupPreservesRecentSessions(t *testing.T) {
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	now := t0
	r := NewSessionRouterWithClock(func() time.Time { return now })

	r.BindSession("sess-old", newWorkspaceFixture(9301))

	// Advance past TTL, then bind a new session; the new one carries the
	// advanced timestamp.
	now = t0.Add(25 * time.Hour)
	r.BindSession("sess-new", newWorkspaceFixture(9302))

	// Cleanup at the new timestamp drops sess-old (older than 24h) but
	// keeps sess-new (bound at now).
	expired := r.Cleanup(now)
	if expired != 1 {
		t.Errorf("Cleanup expired = %d, want 1", expired)
	}
	if got := r.LookupSession("sess-new"); got == nil {
		t.Error("sess-new dropped by Cleanup, want preserved")
	}
	if got := r.LookupSession("sess-old"); got != nil {
		t.Error("sess-old retained by Cleanup, want dropped")
	}
}

func TestSessionRouter_RebindRefreshesTTL(t *testing.T) {
	// Rebinding still refreshes activity even though lookups now also
	// extend the idle TTL.
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	now := t0
	r := NewSessionRouterWithClock(func() time.Time { return now })

	r.BindSession("sess-rebind", newWorkspaceFixture(9301))

	// Advance 12h, then re-bind (same workspace).
	now = t0.Add(12 * time.Hour)
	r.BindSession("sess-rebind", newWorkspaceFixture(9301))

	// Advance to t0+32h (20h after the second bind; still inside the 24h
	// TTL window for the second bind, even though >24h since the first).
	now = t0.Add(32 * time.Hour)
	if expired := r.Cleanup(now); expired != 0 {
		t.Errorf("Cleanup expired = %d, want 0 (rebind refreshed TTL)", expired)
	}
	if got := r.LookupSession("sess-rebind"); got == nil {
		t.Error("rebind session dropped, want preserved")
	}
}

func TestSessionRouter_LookupRefreshesLastSeen(t *testing.T) {
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	now := t0
	r := NewSessionRouterWithClock(func() time.Time { return now })

	r.BindSession("sess-lookup", newWorkspaceFixture(9301))

	now = t0.Add(12 * time.Hour)
	if got := r.LookupSession("sess-lookup"); got == nil {
		t.Fatal("LookupSession returned nil, want bound workspace")
	}

	now = t0.Add(24 * time.Hour)
	if expired := r.Cleanup(now); expired != 0 {
		t.Fatalf("Cleanup expired = %d, want 0 after lookup refreshed lastSeen", expired)
	}
	if got := r.LookupSession("sess-lookup"); got == nil {
		t.Fatal("LookupSession after Cleanup returned nil, want preserved binding")
	}
}

func TestSessionRouter_NoLookupExpiresAtTTL(t *testing.T) {
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	now := t0
	r := NewSessionRouterWithClock(func() time.Time { return now })

	r.BindSession("sess-idle", newWorkspaceFixture(9301))

	now = t0.Add(DefaultSessionTTL + time.Nanosecond)
	if expired := r.Cleanup(now); expired != 1 {
		t.Fatalf("Cleanup expired = %d, want 1 for idle binding", expired)
	}
	if got := r.LookupSession("sess-idle"); got != nil {
		t.Fatalf("LookupSession(sess-idle) after Cleanup = %+v, want nil", got)
	}
}

func TestSessionRouter_BindNilWorkspaceIgnored(t *testing.T) {
	r := NewSessionRouter()
	r.BindSession("sess-nil", nil)
	if got := r.LookupSession("sess-nil"); got != nil {
		t.Errorf("LookupSession after BindSession(nil) = %+v, want nil", got)
	}
	if got := r.Len(); got != 0 {
		t.Errorf("Len after BindSession(nil) = %d, want 0", got)
	}
}

func TestSessionRouter_BindEmptySessionIDIgnored(t *testing.T) {
	r := NewSessionRouter()
	r.BindSession("", newWorkspaceFixture(9301))
	if got := r.Len(); got != 0 {
		t.Errorf("Len after BindSession(empty) = %d, want 0", got)
	}
}

func TestSessionRouter_BindOverwritesExistingBinding(t *testing.T) {
	r := NewSessionRouter()
	r.BindSession("sess-1", newWorkspaceFixture(9301))
	r.BindSession("sess-1", newWorkspaceFixture(9999))

	got := r.LookupSession("sess-1")
	if got == nil {
		t.Fatal("LookupSession returned nil")
	}
	if got.Port != 9999 {
		t.Errorf("Port = %d, want 9999 (rebind must overwrite)", got.Port)
	}
}

func TestSessionRouter_LookupReturnsValueCopy(t *testing.T) {
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	now := t0
	r := NewSessionRouterWithClock(func() time.Time { return now })
	ws := newWorkspaceFixture(9301)
	r.BindSession("sess-1", ws)

	now = t0.Add(12 * time.Hour)
	got := r.LookupSession("sess-1")
	if got == nil {
		t.Fatal("LookupSession returned nil")
	}

	// Mutate the returned copy; subsequent Lookup must NOT see the mutation.
	got.Port = 12345
	now = t0.Add(24 * time.Hour)
	if expired := r.Cleanup(now); expired != 0 {
		t.Fatalf("Cleanup expired = %d, want 0 because Lookup refreshed lastSeen", expired)
	}
	got2 := r.LookupSession("sess-1")
	if got2 == nil || got2.Port != 9301 {
		t.Errorf("Lookup after mutation = %+v, want fresh copy with Port=9301", got2)
	}
}

func TestSessionRouter_ConcurrentAccess(t *testing.T) {
	// Stress the lock discipline with N goroutines hammering Bind / Lookup
	// / Unbind concurrently. Failures show up as a panic (concurrent map
	// access) under -race; the assertion is "no panic, no race report".
	r := NewSessionRouter()
	const (
		goroutines  = 16
		opsPerGoro  = 500
		sessionPool = 32
	)
	var wg sync.WaitGroup
	var lookupHits atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for op := 0; op < opsPerGoro; op++ {
				sid := sessionIDName(op % sessionPool)
				switch (id + op) % 4 {
				case 0:
					r.BindSession(sid, newWorkspaceFixture(9000+(op%100)))
				case 1:
					if got := r.LookupSession(sid); got != nil {
						lookupHits.Add(1)
					}
				case 2:
					r.UnbindSession(sid)
				case 3:
					r.Cleanup(time.Now())
				}
			}
		}(g)
	}
	wg.Wait()

	// Sanity: at least some lookups hit (probabilistically very likely).
	// The exact number is undefined under interleavings; we only assert
	// the router did not deadlock or panic.
	if lookupHits.Load() < 0 {
		t.Fatalf("impossible: lookupHits = %d", lookupHits.Load())
	}
}

func sessionIDName(i int) string {
	// Tiny helper to avoid pulling in strconv just for one int->string.
	const digits = "0123456789"
	if i == 0 {
		return "sess-0"
	}
	out := []byte{'s', 'e', 's', 's', '-'}
	buf := []byte{}
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	out = append(out, buf...)
	return string(out)
}
