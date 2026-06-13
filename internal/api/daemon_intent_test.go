// Tests for Task 2 (daemon_intent.go) — three-state read, TTL with
// clock-skew, UTC enforcement, post-rename quarantine + non-fatal prune,
// and identity-oversize rejection.
//
// Tests run with the production state-path resolver bypassed via the
// daemonStateRootOverride seam from state_paths.go (Task 1) so each
// test owns its own per-test directory under t.TempDir().
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// daemonIntentTestHelper resets every package-level seam touched by the
// daemon-intent tests and routes the state-dir resolver to a fresh
// per-test temp dir. Returns the resolved state directory so the test
// can manipulate the on-disk file directly when needed.
func daemonIntentTestHelper(t *testing.T) string {
	t.Helper()
	statePathsHelper(t)
	// Hardened (single-user-safe) state root, mirroring production's owner-only
	// %LOCALAPPDATA%. This was added in pr301 r5 when an absent intent on a
	// delete-capable dir resolved strict=TRUE (so a plain t.TempDir() on a
	// broadened RAM-disk test host made gated state-file writes refuse). pr301
	// r9 reverted that absent-strict over-reach (an absent intent now relaxes
	// regardless of broadening), so the hardened root is now REDUNDANT for the
	// strict verdict — retained because a single-user-safe root is the correct
	// production-mirroring posture and keeps gated writes on the relax path.
	root := hardenedTempDir(t)
	daemonStateRootOverride = root

	prevQuarantineLog := quarantinePruneLogFn
	t.Cleanup(func() { quarantinePruneLogFn = prevQuarantineLog })

	prevQuarantineRemove := quarantineRemoveFileFn
	t.Cleanup(func() { quarantineRemoveFileFn = prevQuarantineRemove })

	return root
}

// ---------------------------------------------------------------------------
// Roundtrip
// ---------------------------------------------------------------------------

func TestDaemonIntent_Roundtrip(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	now := time.Now().UTC().Truncate(time.Nanosecond)
	intent := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}
	if err := a.WriteDaemonIntent("\\mcp-local-hub-time-default", intent, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.Err != nil {
		t.Fatalf("ReadDaemonIntent err: %v", res.Err)
	}
	if res.State != IntentStateValid {
		t.Fatalf("State = %q, want %q", res.State, IntentStateValid)
	}
	got, ok := res.File.Tasks["\\mcp-local-hub-time-default"]
	if !ok {
		t.Fatalf("missing entry after roundtrip")
	}
	if got.Desired != intent.Desired {
		t.Errorf("Desired: got %q, want %q", got.Desired, intent.Desired)
	}
	if got.Reason != intent.Reason {
		t.Errorf("Reason: got %q, want %q", got.Reason, intent.Reason)
	}
	if !got.UpdatedAt.Equal(intent.UpdatedAt) {
		t.Errorf("UpdatedAt: got %v, want %v", got.UpdatedAt, intent.UpdatedAt)
	}
	if got.UpdatedAt.Location() != time.UTC {
		t.Errorf("UpdatedAt location = %v, want UTC", got.UpdatedAt.Location())
	}
}

// ---------------------------------------------------------------------------
// Concurrent write — gofrs/flock prevents corruption.
// ---------------------------------------------------------------------------

func TestDaemonIntent_ConcurrentWrite(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	const writers = 8
	const perWriter = 12
	var wg sync.WaitGroup
	wg.Add(writers)
	now := time.Now().UTC()

	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				name := fmt.Sprintf("\\mcp-local-hub-task-%d-%d", w, i)
				if err := a.WriteDaemonIntent(name, DaemonIntent{
					Desired:   IntentDesiredStopped,
					Reason:    IntentReasonUserStop,
					UpdatedAt: now,
				}, "tester"); err != nil {
					t.Errorf("WriteDaemonIntent: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	res := a.ReadDaemonIntent()
	if res.Err != nil {
		t.Fatalf("ReadDaemonIntent err: %v", res.Err)
	}
	if res.State != IntentStateValid {
		t.Fatalf("State = %q, want valid (file should be readable after concurrent writes)", res.State)
	}
	if got := len(res.File.Tasks); got != writers*perWriter {
		t.Errorf("entries: got %d, want %d", got, writers*perWriter)
	}
}

// ---------------------------------------------------------------------------
// Three-state semantics
// ---------------------------------------------------------------------------

func TestDaemonIntent_Read_Missing(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	res := a.ReadDaemonIntent()
	if res.Err != nil {
		t.Fatalf("err = %v, want nil for missing file", res.Err)
	}
	if res.State != IntentStateMissing {
		t.Fatalf("State = %q, want %q", res.State, IntentStateMissing)
	}
	if res.File.Tasks == nil {
		t.Errorf("File.Tasks is nil; want empty (non-nil) map for caller convenience")
	}
	if len(res.File.Tasks) != 0 {
		t.Errorf("File.Tasks: got %d entries, want 0", len(res.File.Tasks))
	}
}

func TestDaemonIntent_Read_CorruptJSON(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	if err := writeIntentRaw(t, []byte("{this is not valid json}")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateCorrupt {
		t.Fatalf("State = %q, want %q (Err=%v)", res.State, IntentStateCorrupt, res.Err)
	}
	if res.QuarantinePath == "" {
		t.Fatalf("QuarantinePath empty; want path to renamed file")
	}
	if _, err := os.Stat(res.QuarantinePath); err != nil {
		t.Fatalf("quarantine file missing on disk: %v", err)
	}
	if !strings.Contains(filepath.Base(res.QuarantinePath), ".corrupt-") {
		t.Errorf("quarantine filename %q lacks .corrupt-{ts} suffix", res.QuarantinePath)
	}
}

func TestDaemonIntent_Read_SchemaInvalid_BadEnum(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	bad := []byte(`{"tasks":{"\\mcp-local-hub-x":{"desired":"weird","reason":"unknown","updated_at":"2026-01-01T00:00:00Z"}}}`)
	if err := writeIntentRaw(t, bad); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateCorrupt {
		t.Fatalf("State = %q, want %q (Err=%v)", res.State, IntentStateCorrupt, res.Err)
	}
	if res.QuarantinePath == "" {
		t.Errorf("QuarantinePath empty; want path to renamed file after schema rejection")
	}
}

func TestDaemonIntent_Read_InvalidUTF8(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Invalid UTF-8 byte sequence (lone continuation byte) inside what
	// would otherwise be a JSON string. json.Unmarshal accepts arbitrary
	// bytes inside string values per RFC 7159 (it produces invalid runes
	// rather than erroring), so we have to detect invalid UTF-8 ourselves
	// per the Task 2 spec.
	bad := []byte("{\"tasks\":{\"\\\\mcp-local-hub-x\":{\"desired\":\"stopped\",\"reason\":\"user-stop\",\"updated_at\":\"2026-01-01T00:00:00Z\",\"_note\":\"bad\xC3\x28\"}}}")
	if err := writeIntentRaw(t, bad); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateCorrupt {
		t.Fatalf("State = %q, want %q for invalid UTF-8 input (Err=%v)", res.State, IntentStateCorrupt, res.Err)
	}
	if res.QuarantinePath == "" {
		t.Errorf("QuarantinePath empty; want path to renamed file after UTF-8 rejection")
	}
}

func TestDaemonIntent_Read_DisallowUnknownFields(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	bad := []byte(`{"tasks":{"\\mcp-local-hub-x":{"desired":"running","reason":"register","updated_at":"2026-01-01T00:00:00Z","extra":"x"}}}`)
	if err := writeIntentRaw(t, bad); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateCorrupt {
		t.Fatalf("State = %q, want %q (Err=%v)", res.State, IntentStateCorrupt, res.Err)
	}
}

// ---------------------------------------------------------------------------
// IsActiveStop — TTL + clock-skew
// ---------------------------------------------------------------------------

func TestIsActiveStop_DesiredRunning_NotActive(t *testing.T) {
	now := time.Now().UTC()
	in := DaemonIntent{Desired: IntentDesiredRunning, Reason: IntentReasonRegister, UpdatedAt: now}
	active, reason := in.IsActiveStop(now)
	if active {
		t.Errorf("IsActiveStop: want false for desired=running, got true (reason=%q)", reason)
	}
}

func TestIsActiveStop_UserStop_WithinTTL(t *testing.T) {
	now := time.Now().UTC()
	in := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-2 * time.Hour)}
	active, reason := in.IsActiveStop(now)
	if !active {
		t.Errorf("IsActiveStop: want true within TTL, got false")
	}
	if reason != IntentReasonUserStop {
		t.Errorf("reason: got %q, want %q", reason, IntentReasonUserStop)
	}
}

func TestIsActiveStop_UserStop_PastTTL(t *testing.T) {
	now := time.Now().UTC()
	// 25h is past the 24h StopIntentTTL.
	in := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-25 * time.Hour)}
	active, _ := in.IsActiveStop(now)
	if active {
		t.Errorf("IsActiveStop: want false past TTL for user-stop")
	}
}

func TestIsActiveStop_UserDisabled_NeverExpires(t *testing.T) {
	now := time.Now().UTC()
	// 30 days ago — well past StopIntentTTL but within ClockSkewStaleBound.
	in := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-30 * 24 * time.Hour)}
	active, reason := in.IsActiveStop(now)
	if !active {
		t.Errorf("IsActiveStop: want true for user-disabled (no TTL expiry)")
	}
	if reason != IntentReasonUserDisabled {
		t.Errorf("reason: got %q, want %q", reason, IntentReasonUserDisabled)
	}
}

func TestIsActiveStop_ClockSkewFuture(t *testing.T) {
	now := time.Now().UTC()
	// UpdatedAt is 6 minutes in the future relative to now → fail-closed.
	in := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(6 * time.Minute)}
	active, reason := in.IsActiveStop(now)
	if !active {
		t.Errorf("IsActiveStop: want true (fail-closed) for future-skew, got false")
	}
	if reason != ClockSkewFutureSuspectReason {
		t.Errorf("reason: got %q, want %q", reason, ClockSkewFutureSuspectReason)
	}
}

func TestIsActiveStop_ClockSkewStale_366Days(t *testing.T) {
	now := time.Now().UTC()
	// 366 days ago — outside ClockSkewStaleBound → treat as stale, not active.
	in := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-366 * 24 * time.Hour)}
	active, _ := in.IsActiveStop(now)
	if active {
		t.Errorf("IsActiveStop: want false for stale-skew (>365d), got true")
	}
}

// ---------------------------------------------------------------------------
// UTC enforcement — write writes UTC; read of file with non-UTC ts → corrupt
// ---------------------------------------------------------------------------

func TestDaemonIntent_Write_NormalizesToUTC(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Caller supplies a non-UTC timestamp; we expect the writer to
	// preserve the instant but the on-disk JSON to read back in UTC.
	tz, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("LoadLocation failed: %v", err)
	}
	supplied := time.Now().In(tz)

	if err := a.WriteDaemonIntent("\\mcp-local-hub-x", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: supplied,
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("State = %q after write, want valid (Err=%v)", res.State, res.Err)
	}
	got := res.File.Tasks["\\mcp-local-hub-x"]
	if got.UpdatedAt.Location() != time.UTC {
		t.Errorf("read-back location = %v, want UTC", got.UpdatedAt.Location())
	}
	if !got.UpdatedAt.Equal(supplied) {
		t.Errorf("instant changed: got %v, want %v", got.UpdatedAt, supplied)
	}
}

func TestDaemonIntent_Read_NonUTC_IsCorrupt(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Hand-craft a file whose RFC3339Nano timestamp carries a non-Z offset
	// — the read path must reject it as corrupt per the UTC requirement.
	bad := []byte(`{"tasks":{"\\mcp-local-hub-x":{"desired":"stopped","reason":"user-stop","updated_at":"2026-01-01T00:00:00-08:00"}}}`)
	if err := writeIntentRaw(t, bad); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateCorrupt {
		t.Fatalf("State = %q, want %q (Err=%v)", res.State, IntentStateCorrupt, res.Err)
	}
}

// ---------------------------------------------------------------------------
// Quarantine cap = 5
// ---------------------------------------------------------------------------

func TestDaemonIntent_QuarantineCap_PrunesToFiveNewest(t *testing.T) {
	a := NewAPI()
	root := daemonIntentTestHelper(t)
	if _, err := DaemonStateDir(); err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	stem := intentFileStem(root)

	// Pre-seed 6 stale .corrupt-* files with descending mtime so we can
	// distinguish "newest" vs "oldest" deterministically.
	now := time.Now()
	staleNames := make([]string, 6)
	for i := range staleNames {
		ts := time.Now().UTC().Add(time.Duration(-i) * time.Minute).Format("2006-01-02T15-04-05.000000000Z")
		name := stem + ".corrupt-" + ts + fmt.Sprintf(".%d", i)
		if err := os.WriteFile(name, []byte(fmt.Sprintf("stale-%d", i)), 0o600); err != nil {
			t.Fatalf("seed stale: %v", err)
		}
		// Mtime descending: index 0 is newest, index 5 is oldest.
		mt := now.Add(time.Duration(-i) * time.Minute)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		staleNames[i] = name
	}

	// Trigger quarantine by writing a corrupt main file and reading it.
	if err := writeIntentRaw(t, []byte("{not valid")); err != nil {
		t.Fatalf("seed corrupt main: %v", err)
	}
	res := a.ReadDaemonIntent()
	if res.State != IntentStateCorrupt {
		t.Fatalf("State = %q, want %q", res.State, IntentStateCorrupt)
	}

	// Surviving set: should be exactly 5. The new quarantine (just made
	// from the corrupt main file) is the newest; the 4 next newest of the
	// 6 pre-seeded files survive. The oldest pre-seeded file is pruned.
	matches, err := filepath.Glob(stem + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if got := len(matches); got != QuarantineCap {
		t.Fatalf("survivor count: got %d, want %d (matches=%v)", got, QuarantineCap, matches)
	}

	// Sort survivors by mtime DESC and ensure none of them is the OLDEST
	// pre-seed (staleNames[5]), which must have been pruned.
	survivorBases := map[string]bool{}
	for _, m := range matches {
		survivorBases[filepath.Base(m)] = true
	}
	if survivorBases[filepath.Base(staleNames[5])] {
		t.Errorf("oldest stale file %q survived; want pruned", staleNames[5])
	}
}

// ---------------------------------------------------------------------------
// Quarantine prune failure non-fatal
// ---------------------------------------------------------------------------

func TestDaemonIntent_QuarantinePrune_FailureNonFatal(t *testing.T) {
	a := NewAPI()
	root := daemonIntentTestHelper(t)
	if _, err := DaemonStateDir(); err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	stem := intentFileStem(root)

	// Pre-seed 6 .corrupt-* files (one to be pruned).
	now := time.Now()
	for i := 0; i < 6; i++ {
		ts := time.Now().UTC().Add(time.Duration(-i) * time.Minute).Format("2006-01-02T15-04-05.000000000Z")
		name := stem + ".corrupt-" + ts + fmt.Sprintf(".%d", i)
		if err := os.WriteFile(name, []byte("stale"), 0o600); err != nil {
			t.Fatalf("seed stale: %v", err)
		}
		mt := now.Add(time.Duration(-i) * time.Minute)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	// Force per-file delete to fail.
	var pruneEvents []string
	var deleteAttempts int
	quarantineRemoveFileFn = func(path string) error {
		deleteAttempts++
		return errors.New("synthetic disk-full")
	}
	quarantinePruneLogFn = func(event, path string, err error) {
		pruneEvents = append(pruneEvents, event)
	}

	// Trigger quarantine.
	if err := writeIntentRaw(t, []byte("not valid")); err != nil {
		t.Fatalf("seed corrupt main: %v", err)
	}
	res := a.ReadDaemonIntent()

	// Quarantine succeeded (rename completed) — function returns
	// Err=nil and State=corrupt, even though prune failed.
	if res.State != IntentStateCorrupt {
		t.Fatalf("State = %q, want %q (Err=%v)", res.State, IntentStateCorrupt, res.Err)
	}
	if res.QuarantinePath == "" {
		t.Fatalf("QuarantinePath empty; rename should have succeeded")
	}
	if _, err := os.Stat(res.QuarantinePath); err != nil {
		t.Fatalf("quarantine target missing: %v", err)
	}

	// Prune was attempted on at least one file (the oldest of the
	// pre-seeded 6 once the new quarantine bumps the survivor count past 5).
	if deleteAttempts == 0 {
		t.Errorf("expected at least one delete attempt; got 0")
	}
	hasNonFatal := false
	for _, ev := range pruneEvents {
		if ev == "quarantine-prune-failed-non-fatal" {
			hasNonFatal = true
			break
		}
	}
	if !hasNonFatal {
		t.Errorf("expected quarantine-prune-failed-non-fatal event; got %v", pruneEvents)
	}
}

// ---------------------------------------------------------------------------
// Mixed-bootstrap — absent task lookup returns zero DaemonIntent
// ---------------------------------------------------------------------------

func TestDaemonIntent_MixedBootstrap_AbsentTaskZero(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Seed a valid file with a single entry.
	if err := a.WriteDaemonIntent("\\mcp-local-hub-only", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("State = %q, want valid", res.State)
	}

	// Absent task lookup must return the zero DaemonIntent (caller treats
	// as default-running per plan §3 mixed-bootstrap semantics).
	missing := res.File.Tasks["\\mcp-local-hub-absent"]
	if missing.Desired != "" || missing.Reason != "" || !missing.UpdatedAt.IsZero() {
		t.Errorf("absent lookup: got %+v, want zero DaemonIntent", missing)
	}
}

// ---------------------------------------------------------------------------
// Identity oversize — task name >1KB
// ---------------------------------------------------------------------------

func TestDaemonIntent_Write_IdentityOversize(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Seed a known-good file first so we can verify the failed write
	// leaves the file unchanged.
	if err := a.WriteDaemonIntent("\\mcp-local-hub-good", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prePath, _ := DaemonStateDir()
	preRaw, err := os.ReadFile(filepath.Join(prePath, intentFileLeaf))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	// Build an oversized task name — 32KB.
	huge := strings.Repeat("a", 32*1024)
	err = a.WriteDaemonIntent(huge, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester")
	if err == nil {
		t.Fatalf("WriteDaemonIntent: expected ErrEntryOversize, got nil")
	}
	if !errors.Is(err, ErrEntryOversize) {
		t.Fatalf("WriteDaemonIntent: want ErrEntryOversize, got %v", err)
	}

	postRaw, err := os.ReadFile(filepath.Join(prePath, intentFileLeaf))
	if err != nil {
		t.Fatalf("read post: %v", err)
	}
	if string(preRaw) != string(postRaw) {
		t.Errorf("intent file changed by oversize write; want unchanged\nbefore=%q\nafter =%q", preRaw, postRaw)
	}
}

// ---------------------------------------------------------------------------
// PR #135 round 3 P2 — canonical-form size recheck.
//
// canonicalIntentTaskKey prepends "\\" to a bare task name when needed.
// The pre-fix code size-checked the RAW input, so a bare 1024-byte name
// passed validation and became a 1025-byte canonical key — over the
// AuditIdentityFieldByteCap (intent_audit.go) ceiling. WriteDaemonIntent
// ignores audit append errors, so the audit record was silently dropped
// for max-length valid task identifiers. The fix is to canonicalize
// FIRST, then size-check.
// ---------------------------------------------------------------------------

// TestWriteDaemonIntent_CanonicalSizeRecheck_RejectsAt1024PreCanonical
// verifies the round-3 P2 boundary: a bare task name of exactly the
// IdentityFieldByteCap (1024 bytes, no leading "\") becomes 1025 bytes
// after canonicalIntentTaskKey prepends "\". The post-canonicalization
// size-check must reject it with ErrEntryOversize so the audit log
// can never silently drop the set-intent record.
func TestWriteDaemonIntent_CanonicalSizeRecheck_RejectsAt1024PreCanonical(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Snapshot the file before the bad write so we can verify the
	// rejection leaves the canonical state untouched.
	if err := a.WriteDaemonIntent("\\mcp-local-hub-anchor", DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	prePath, _ := DaemonStateDir()
	preRaw, err := os.ReadFile(filepath.Join(prePath, intentFileLeaf))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	// Bare 1024-byte name → canonical "\" + 1024 = 1025 bytes.
	bareAt1024 := strings.Repeat("a", IdentityFieldByteCap)
	if len(bareAt1024) != IdentityFieldByteCap {
		t.Fatalf("test prep: bare name length = %d, want %d", len(bareAt1024), IdentityFieldByteCap)
	}
	canonical := canonicalIntentTaskKey(bareAt1024)
	if len(canonical) != IdentityFieldByteCap+1 {
		t.Fatalf("test prep: canonical length = %d, want %d", len(canonical), IdentityFieldByteCap+1)
	}

	err = a.WriteDaemonIntent(bareAt1024, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester")
	if err == nil {
		t.Fatalf("WriteDaemonIntent(bareAt1024): want ErrEntryOversize on canonical recheck, got nil")
	}
	if !errors.Is(err, ErrEntryOversize) {
		t.Fatalf("WriteDaemonIntent(bareAt1024): want ErrEntryOversize, got %v", err)
	}

	// File must be unchanged.
	postRaw, err := os.ReadFile(filepath.Join(prePath, intentFileLeaf))
	if err != nil {
		t.Fatalf("read post: %v", err)
	}
	if string(preRaw) != string(postRaw) {
		t.Errorf("intent file changed by oversize-canonical write; want unchanged\nbefore=%q\nafter =%q", preRaw, postRaw)
	}
}

// TestWriteDaemonIntent_AlreadyCanonical1024_Accepted is the boundary
// counterpart: an already-canonical 1024-byte key (leading "\" + 1023
// bytes of payload) must STILL be accepted because canonicalIntentTaskKey
// is a no-op on inputs that already start with "\". This pins the
// round-3 P2 fix as a tightening, not an across-the-board reduction.
func TestWriteDaemonIntent_AlreadyCanonical1024_Accepted(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// "\" + 1023-byte name = 1024 bytes total — exactly at the cap.
	canonicalAt1024 := "\\" + strings.Repeat("a", IdentityFieldByteCap-1)
	if len(canonicalAt1024) != IdentityFieldByteCap {
		t.Fatalf("test prep: canonical length = %d, want %d", len(canonicalAt1024), IdentityFieldByteCap)
	}
	// canonicalIntentTaskKey on already-canonical input must be the
	// identity function — pin that contract here.
	if got := canonicalIntentTaskKey(canonicalAt1024); got != canonicalAt1024 {
		t.Fatalf("canonicalIntentTaskKey(already-canonical): got len=%d, want identity (len=%d)", len(got), len(canonicalAt1024))
	}

	if err := a.WriteDaemonIntent(canonicalAt1024, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent(canonicalAt1024): want accepted, got %v", err)
	}

	res := a.ReadDaemonIntent()
	if res.State != IntentStateValid {
		t.Fatalf("State = %q, want valid", res.State)
	}
	if _, ok := res.File.Tasks[canonicalAt1024]; !ok {
		t.Errorf("Tasks[canonicalAt1024]: missing — write accepted but key absent. Tasks=%+v", res.File.Tasks)
	}
}

// TestClearDaemonIntent_CanonicalSizeRecheck_RejectsAt1024PreCanonical
// mirrors the recheck on the Clear path. ClearDaemonIntent shares the
// same audit-write step (clear-intent action) and would otherwise drop
// audit records for the same edge case as Write.
func TestClearDaemonIntent_CanonicalSizeRecheck_RejectsAt1024PreCanonical(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	bareAt1024 := strings.Repeat("a", IdentityFieldByteCap)
	err := a.ClearDaemonIntent(bareAt1024, "tester")
	if err == nil {
		t.Fatalf("ClearDaemonIntent(bareAt1024): want ErrEntryOversize on canonical recheck, got nil")
	}
	if !errors.Is(err, ErrEntryOversize) {
		t.Fatalf("ClearDaemonIntent(bareAt1024): want ErrEntryOversize, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// TryReadDaemonIntent — bounded-timeout variant (PR #142 round 2 P2).
// ---------------------------------------------------------------------------

// TestTryReadDaemonIntent_TimesOutWhenLockHeld guards the regression
// from PR #142 round 2 P2: the tray aggregator's snapshot loop must
// not stall behind a long-held daemon-intent.json flock. The test
// holds the flock on a sibling goroutine for 2 s, calls
// TryReadDaemonIntent with a 50 ms budget, and asserts the read
// returns within ~100 ms with State=missing and a timeout-flavoured
// error — never the 2 s blocking stall the prior wiring exhibited.
//
// The lock-holder goroutine acquires the same `daemon-intent.json.lock`
// sibling file the API uses, so the production code path is exercised
// without any test-only seam.
func TestTryReadDaemonIntent_TimesOutWhenLockHeld(t *testing.T) {
	a := NewAPI()
	root := daemonIntentTestHelper(t)

	// Resolve the same lock path the API uses. DaemonStateDir() is
	// already redirected to the per-test temp dir by the helper.
	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	lockPath := filepath.Join(dir, intentLockLeaf)

	// Hold the lock for 2 s on a sibling goroutine. The release
	// happens unconditionally; even if the test's assertions fail
	// the goroutine still unwinds cleanly within the 2 s window.
	holder := flock.New(lockPath)
	if err := holder.Lock(); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		time.Sleep(2 * time.Second)
		_ = holder.Unlock()
	}()
	t.Cleanup(func() {
		// Defensive: ensure the holder releases even if the test
		// panics earlier than expected. Unlock is idempotent in the
		// "already unlocked" case at the gofrs/flock level.
		_ = holder.Unlock()
		<-holderDone
	})

	// Sanity: confirm the holder actually has the lock by trying a
	// non-blocking acquire on a probe — if the holder failed silently
	// the rest of the test would race a phantom timeout. We don't
	// fail the test on this branch; just emit a hint in the log so a
	// future regression in the helper is visible.
	probe := flock.New(lockPath)
	if got, _ := probe.TryLock(); got {
		t.Logf("warning: holder did not appear to hold the lock; the timeout assertion may pass for the wrong reason")
		_ = probe.Unlock()
	}

	// Tight per-call budget: 50 ms timeout. The wallclock budget for
	// the call itself adds the cost of TryLockContext's last poll
	// before ctx fires (~10 ms retryDelay) plus our own measurement
	// overhead, so we accept up to 200 ms before declaring the
	// implementation broken. The point is that 50 ms must NOT
	// degenerate into the holder's full 2 s hold.
	const callBudget = 50 * time.Millisecond
	const wallclockCap = 200 * time.Millisecond

	start := time.Now()
	res := a.TryReadDaemonIntent(callBudget)
	elapsed := time.Since(start)

	if elapsed > wallclockCap {
		t.Fatalf("TryReadDaemonIntent took %s with %s timeout (cap %s) — should not block on held lock",
			elapsed, callBudget, wallclockCap)
	}
	if res.State != IntentStateMissing {
		t.Errorf("State = %q, want %q (timeout fallback)", res.State, IntentStateMissing)
	}
	if res.File.Tasks == nil {
		t.Errorf("File.Tasks = nil, want empty (non-nil) map for graceful-degrade contract")
	}
	if res.Err == nil {
		t.Fatalf("Err = nil, want a non-nil timeout error")
	}
	// Round 3 codex finding R6+Q3: assert via errors.Is(res.Err,
	// context.DeadlineExceeded) — the canonical timeout taxonomy.
	// The prior strings.Contains("timeout") check was both fragile
	// (locale/wording-sensitive) and would tolerate a non-timeout
	// flock error mislabelled as a timeout in the message body. The
	// %w-wrapped chain inside TryReadDaemonIntent guarantees the
	// errors.Is relation.
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want errors.Is(_, context.DeadlineExceeded) for timeout taxonomy", res.Err)
	}
	_ = root
}

// TestTryReadDaemonIntent_SucceedsWhenLockFree confirms the happy path:
// no contention → TryReadDaemonIntent returns the parsed file with
// State=valid, the seeded entry, and Err=nil. Same shape as the
// roundtrip test but routed through the bounded-timeout method.
func TestTryReadDaemonIntent_SucceedsWhenLockFree(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	now := time.Now().UTC().Truncate(time.Nanosecond)
	intent := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}
	taskName := "\\mcp-local-hub-time-default"
	if err := a.WriteDaemonIntent(taskName, intent, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	res := a.TryReadDaemonIntent(1 * time.Second)
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil for uncontested read", res.Err)
	}
	if res.State != IntentStateValid {
		t.Fatalf("State = %q, want %q", res.State, IntentStateValid)
	}
	got, ok := res.File.Tasks[taskName]
	if !ok {
		t.Fatalf("missing entry %q after roundtrip; got tasks: %v", taskName, res.File.Tasks)
	}
	if got.Desired != intent.Desired {
		t.Errorf("Desired: got %q, want %q", got.Desired, intent.Desired)
	}
	if got.Reason != intent.Reason {
		t.Errorf("Reason: got %q, want %q", got.Reason, intent.Reason)
	}
	if !got.UpdatedAt.Equal(intent.UpdatedAt) {
		t.Errorf("UpdatedAt: got %v, want %v", got.UpdatedAt, intent.UpdatedAt)
	}
}

// TestTryReadDaemonIntent_ZeroTimeoutTriesOnce exercises the round 3
// codex finding R2 fix: timeout==0 must take a single non-blocking
// TryLock() attempt instead of going through context.WithTimeout(0)
// (which fires immediately and would short-circuit even on a free
// lock). With no contention the call must succeed and return the
// seeded entry.
func TestTryReadDaemonIntent_ZeroTimeoutTriesOnce(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	now := time.Now().UTC().Truncate(time.Nanosecond)
	intent := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}
	taskName := "\\mcp-local-hub-zero-default"
	if err := a.WriteDaemonIntent(taskName, intent, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	res := a.TryReadDaemonIntent(0)
	if res.State != IntentStateValid {
		t.Errorf("State = %q, want %q with timeout=0 when lock free; err=%v",
			res.State, IntentStateValid, res.Err)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil for free-lock zero-timeout", res.Err)
	}
	if _, ok := res.File.Tasks[taskName]; !ok {
		t.Errorf("entry %q missing from result; got tasks: %v", taskName, res.File.Tasks)
	}
}

// TestTryReadDaemonIntent_NegativeTimeoutTreatedAsZero confirms that
// negative timeouts share the zero-timeout non-blocking path. Same
// expectation as the zero case — a free lock yields the seeded entry,
// never a fake DeadlineExceeded.
func TestTryReadDaemonIntent_NegativeTimeoutTreatedAsZero(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	now := time.Now().UTC().Truncate(time.Nanosecond)
	intent := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}
	taskName := "\\mcp-local-hub-neg-default"
	if err := a.WriteDaemonIntent(taskName, intent, "tester"); err != nil {
		t.Fatalf("WriteDaemonIntent: %v", err)
	}

	res := a.TryReadDaemonIntent(-time.Second)
	if res.State != IntentStateValid {
		t.Errorf("State = %q, want %q with negative timeout when lock free; err=%v",
			res.State, IntentStateValid, res.Err)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil for free-lock negative-timeout", res.Err)
	}
	if _, ok := res.File.Tasks[taskName]; !ok {
		t.Errorf("entry %q missing from result; got tasks: %v", taskName, res.File.Tasks)
	}
}

// TestTryReadDaemonIntent_LockFreeMissingFile is the round 3 codex
// finding Q1 boundary case: no on-disk file, lock free, expect
// State=missing with Err=nil and a non-nil empty Tasks map (the
// graceful-degrade contract callers depend on).
func TestTryReadDaemonIntent_LockFreeMissingFile(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	res := a.TryReadDaemonIntent(time.Second)
	if res.State != IntentStateMissing {
		t.Errorf("State = %q, want %q for absent file", res.State, IntentStateMissing)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil for genuinely missing file", res.Err)
	}
	if res.File.Tasks == nil {
		t.Errorf("File.Tasks = nil, want empty (non-nil) map for missing file")
	}
	if got := len(res.File.Tasks); got != 0 {
		t.Errorf("len(File.Tasks) = %d, want 0 for missing file", got)
	}
	if res.QuarantinePath != "" {
		t.Errorf("QuarantinePath = %q, want empty for missing file", res.QuarantinePath)
	}
}

// TestTryReadDaemonIntent_LockFreeCorruptFile is the round 3 codex
// finding Q2 boundary case: garbage bytes on disk, lock free, expect
// State=corrupt with QuarantinePath set, a non-nil parse error, an
// empty Tasks map, and the original file moved aside under the
// `.corrupt-*` quarantine sibling.
func TestTryReadDaemonIntent_LockFreeCorruptFile(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Seed garbage directly via os.WriteFile (bypasses WriteDaemonIntent's
	// validation so we land genuine corruption on disk).
	if err := writeIntentRaw(t, []byte("not-json-{{{}")); err != nil {
		t.Fatalf("writeIntentRaw: %v", err)
	}

	res := a.TryReadDaemonIntent(time.Second)
	if res.State != IntentStateCorrupt {
		t.Errorf("State = %q, want %q for garbage file", res.State, IntentStateCorrupt)
	}
	if res.Err == nil {
		t.Fatalf("Err = nil, want a parse error for corrupt file")
	}
	if res.QuarantinePath == "" {
		t.Fatalf("QuarantinePath = empty, want non-empty for quarantined corrupt file")
	}
	if res.File.Tasks == nil {
		t.Errorf("File.Tasks = nil, want empty (non-nil) map for corrupt file")
	}
	if got := len(res.File.Tasks); got != 0 {
		t.Errorf("len(File.Tasks) = %d, want 0 for corrupt file", got)
	}
	if _, statErr := os.Stat(res.QuarantinePath); statErr != nil {
		t.Errorf("quarantine sibling missing on disk at %q: %v", res.QuarantinePath, statErr)
	}

	// Original canonical path should no longer exist (the rename moved
	// it to QuarantinePath).
	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, intentFileLeaf)); statErr == nil {
		t.Errorf("original canonical intent file still exists after corrupt-quarantine rename")
	}
}

// TestTryReadDaemonIntent_CorruptFileUnderLockContention guards the
// round 3 codex finding Q1 contention boundary: when a holder pins
// the flock for longer than the read budget, the corrupt file MUST
// remain on disk (no quarantine without the lock) and the call MUST
// return the timeout-flavoured fallback. This proves the quarantine
// rename only happens under successful lock acquisition — a guard
// against splitting the lock-held invariants of
// readIntentParseAndQuarantine.
func TestTryReadDaemonIntent_CorruptFileUnderLockContention(t *testing.T) {
	a := NewAPI()
	daemonIntentTestHelper(t)

	// Seed garbage on disk first; we do not want the holder's lock
	// dance to influence whether the file is corrupt.
	if err := writeIntentRaw(t, []byte("not-json-{{{}")); err != nil {
		t.Fatalf("writeIntentRaw: %v", err)
	}

	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	statePath := filepath.Join(dir, intentFileLeaf)
	lockPath := filepath.Join(dir, intentLockLeaf)

	// Snapshot pre-call quarantine sibling count so we can assert no
	// fresh `.corrupt-*` files appear during the contended call.
	prefix := intentFileLeaf + ".corrupt-"
	preEntries, _ := os.ReadDir(dir)
	preCount := 0
	for _, e := range preEntries {
		if strings.HasPrefix(e.Name(), prefix) {
			preCount++
		}
	}

	holder := flock.New(lockPath)
	if lockErr := holder.Lock(); lockErr != nil {
		t.Fatalf("holder lock: %v", lockErr)
	}
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		time.Sleep(2 * time.Second)
		_ = holder.Unlock()
	}()
	t.Cleanup(func() {
		_ = holder.Unlock()
		<-holderDone
	})

	// 50ms budget — far smaller than the 2s holder grip. The call
	// must return promptly with a timeout-flavoured error.
	res := a.TryReadDaemonIntent(50 * time.Millisecond)
	if res.State != IntentStateMissing {
		t.Errorf("State = %q, want %q (timeout fallback under contention)", res.State, IntentStateMissing)
	}
	if res.Err == nil {
		t.Fatalf("Err = nil, want a timeout error under contention")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want errors.Is(_, context.DeadlineExceeded)", res.Err)
	}
	if res.QuarantinePath != "" {
		t.Errorf("QuarantinePath = %q, want empty (no quarantine without lock)", res.QuarantinePath)
	}

	// The original corrupt file MUST still be on disk — quarantine
	// rename requires the lock, which we never acquired.
	if _, statErr := os.Stat(statePath); statErr != nil {
		t.Errorf("corrupt canonical file unexpectedly removed under contention: %v", statErr)
	}

	// No new `.corrupt-*` siblings should have appeared.
	postEntries, _ := os.ReadDir(dir)
	postCount := 0
	for _, e := range postEntries {
		if strings.HasPrefix(e.Name(), prefix) {
			postCount++
		}
	}
	if postCount != preCount {
		t.Errorf("found %d new corrupt-* siblings under contention; want %d (no rename without lock)",
			postCount-preCount, 0)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeIntentRaw seeds the intent file with arbitrary bytes (used to set
// up corrupt-state cases). Goes through DaemonStateDir to honor whatever
// state-root the test configured via daemonIntentTestHelper.
func writeIntentRaw(t *testing.T, raw []byte) error {
	t.Helper()
	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, intentFileLeaf), raw, 0o600)
}

// intentFileStem returns the absolute path to the daemon-intent.json
// file (without the .corrupt-* suffix) under the given state root.
// Used by quarantine tests that need to seed sibling .corrupt-* files.
//
// When daemonStateRootOverride is in play (set by daemonIntentTestHelper),
// state_paths_*.go's ensureStateRoot returns the override path verbatim,
// so the canonical state dir is the override itself — NOT a nested
// `${override}/mcp-local-hub` subdirectory. Use DaemonStateDir() to
// resolve the actual on-disk dir; the function below mirrors that
// resolution for the non-Windows case where DaemonStateDir's first
// invocation has already created the dir.
func intentFileStem(root string) string {
	dir, err := DaemonStateDir()
	if err != nil {
		// Fall back to the override path. Tests that hit this branch are
		// already broken (DaemonStateDir failed); the synthetic path lets
		// them produce a clearer assertion failure than a nil-deref.
		_ = runtime.GOOS
		return filepath.Join(root, intentFileLeaf)
	}
	return filepath.Join(dir, intentFileLeaf)
}

// assertJSONFieldOrder is a smoke check used by some tests to confirm
// the encoder respects the expected field order. Currently consumed by
// roundtrip-style assertions; kept here so the helper is reusable.
//
//nolint:unused // retained for follow-up tests
func assertJSONFieldOrder(t *testing.T, raw []byte, want []string) {
	t.Helper()
	var anyMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &anyMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := make([]string, 0, len(anyMap))
	for k := range anyMap {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("field set: got %v, want %v", got, want)
	}
}
