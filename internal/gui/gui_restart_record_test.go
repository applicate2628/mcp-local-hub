package gui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func restartTestDeadlines(now *time.Time) RestartDeadlines {
	d := DefaultRestartDeadlines()
	d.Now = func() time.Time { return *now }
	return d
}

func TestRestartDeadlines_DefaultPolicy(t *testing.T) {
	d := DefaultRestartDeadlines()
	if d.Now == nil {
		t.Fatal("DefaultRestartDeadlines().Now is nil")
	}

	want := map[string]time.Duration{
		"record_lock": 5 * time.Second,
		"freshness":   3 * time.Minute,
		"reservation": 10 * time.Second,
		"proof":       10 * time.Second,
		"bind":        2 * time.Second,
		"quiesce":     5 * time.Second,
		"rollback":    5 * time.Second,
		"grace":       5 * time.Second,
	}
	got := map[string]time.Duration{
		"record_lock": d.RecordLock,
		"freshness":   d.Freshness,
		"reservation": d.Reservation,
		"proof":       d.Proof,
		"bind":        d.Bind,
		"quiesce":     d.Quiesce,
		"rollback":    d.Rollback,
		"grace":       d.Grace,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultRestartDeadlines() durations = %#v, want %#v", got, want)
	}
}

func TestHandoffMarkerStore_ReserveAndOwnedFreeInterruptCAS(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	store := NewHandoffMarkerStore(apitest.HardenedTempDir(t), restartTestDeadlines(&now))

	begin, err := store.Begin(HandoffBegin{
		Generation: "generation-a",
		Route:      HandoffRoutePortChange,
		OldPort:    9125,
		NewPort:    9230,
		OldPID:     101,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	assertCASMismatch := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrHandoffMarkerCASMismatch) {
			t.Fatalf("%s error = %v, want ErrHandoffMarkerCASMismatch", name, err)
		}
		var typed *HandoffMarkerError
		if !errors.As(err, &typed) {
			t.Fatalf("%s error type = %T, want *HandoffMarkerError", name, err)
		}
	}

	_, err = store.Reserve("other-generation", begin.Sequence, now.Add(10*time.Second), "child-hash", 202)
	assertCASMismatch("Reserve changed generation", err)
	_, err = store.Reserve(begin.Generation, begin.Sequence+1, now.Add(10*time.Second), "child-hash", 202)
	assertCASMismatch("Reserve changed sequence", err)

	reserved, err := store.Reserve(begin.Generation, begin.Sequence, now.Add(10*time.Second), "child-hash", 202)
	if err != nil {
		t.Fatalf("Reserve matching CAS: %v", err)
	}
	if reserved.Phase != HandoffPhaseReserved || reserved.Sequence != begin.Sequence+1 {
		t.Fatalf("Reserve result = phase %q sequence %d, want %q/%d", reserved.Phase, reserved.Sequence, HandoffPhaseReserved, begin.Sequence+1)
	}

	_, err = store.InterruptFromOwnedFreeProbe(reserved.Generation, reserved.Sequence+1, "reservation-expired", "mcphub gui")
	assertCASMismatch("InterruptFromOwnedFreeProbe changed sequence", err)

	if _, err := store.Begin(HandoffBegin{
		Generation: "generation-b",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     303,
	}); err != nil {
		t.Fatalf("Begin changed generation: %v", err)
	}
	_, err = store.InterruptFromOwnedFreeProbe(reserved.Generation, reserved.Sequence, "reservation-expired", "mcphub gui")
	assertCASMismatch("InterruptFromOwnedFreeProbe changed generation", err)

	current, err := store.Read()
	if err != nil {
		t.Fatalf("Read generation-b: %v", err)
	}
	reserved, err = store.Reserve(current.Generation, current.Sequence, now.Add(10*time.Second), "child-b-hash", 404)
	if err != nil {
		t.Fatalf("Reserve generation-b: %v", err)
	}
	interrupted, err := store.InterruptFromOwnedFreeProbe(reserved.Generation, reserved.Sequence, "reservation-expired", "mcphub gui")
	if err != nil {
		t.Fatalf("InterruptFromOwnedFreeProbe matching CAS: %v", err)
	}
	if interrupted.Phase != HandoffPhaseInterrupted || interrupted.Sequence != reserved.Sequence+1 {
		t.Fatalf("owned-free interrupt result = phase %q sequence %d, want %q/%d", interrupted.Phase, interrupted.Sequence, HandoffPhaseInterrupted, reserved.Sequence+1)
	}
}

func TestHandoffMarkerStore_FourPhasesRoundTripV31FieldsOnly(t *testing.T) {
	now := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	stateDir := apitest.HardenedTempDir(t)
	store := NewHandoffMarkerStore(stateDir, restartTestDeadlines(&now))

	inProgress, err := store.Begin(HandoffBegin{
		Generation: "round-trip-a",
		Route:      HandoffRoutePortChange,
		OldPort:    9125,
		NewPort:    0,
		OldPID:     111,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	assertHandoffMarkerReadEquals(t, store, inProgress)

	now = now.Add(time.Second)
	reserved, err := store.Reserve(inProgress.Generation, inProgress.Sequence, now.Add(10*time.Second), "sha256:designated-child", 222)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	assertHandoffMarkerReadEquals(t, store, reserved)
	if reserved.DesignatedChildHash != "sha256:designated-child" || reserved.ChildPID != 222 || !reserved.ReservationExpiresAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("Reserve fields = hash %q child_pid %d reservation_expires_at %s", reserved.DesignatedChildHash, reserved.ChildPID, reserved.ReservationExpiresAt)
	}

	now = now.Add(time.Second)
	committed, err := store.Commit(reserved.Generation, 9230)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertHandoffMarkerReadEquals(t, store, committed)
	if committed.NewPort != 9230 {
		t.Fatalf("Commit NewPort = %d, want bound port 9230", committed.NewPort)
	}

	now = now.Add(time.Second)
	inProgress, err = store.Begin(HandoffBegin{
		Generation: "round-trip-b",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     333,
	})
	if err != nil {
		t.Fatalf("Begin second generation: %v", err)
	}
	interrupted, err := store.Interrupt(inProgress.Generation, "standby-proof-failed", "mcphub gui")
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	assertHandoffMarkerReadEquals(t, store, interrupted)
	if interrupted.ReasonCode != "standby-proof-failed" || interrupted.OperatorAction != "mcphub gui" {
		t.Fatalf("Interrupt reason/action = %q/%q", interrupted.ReasonCode, interrupted.OperatorAction)
	}

	raw, err := os.ReadFile(filepath.Join(stateDir, handoffMarkerFileLeaf))
	if err != nil {
		t.Fatalf("read raw marker: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode raw marker: %v", err)
	}
	wantFields := []string{
		"child_pid", "created_at", "designated_child_hash", "fresh_until", "generation", "new_port",
		"old_pid", "old_port", "operator_action", "phase", "reason_code", "reservation_expires_at",
		"route", "sequence", "updated_at", "version",
	}
	if got := sortedMapKeys(fields); !reflect.DeepEqual(got, wantFields) {
		t.Fatalf("on-disk v3.1 fields = %v, want exactly %v", got, wantFields)
	}

	now = now.Add(time.Second)
	clearable, err := store.Begin(HandoffBegin{
		Generation: "round-trip-c",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     444,
	})
	if err != nil {
		t.Fatalf("Begin clearable generation: %v", err)
	}
	if err := store.ClearAfterProvedPreReleaseRollback(clearable.Generation); err != nil {
		t.Fatalf("ClearAfterProvedPreReleaseRollback: %v", err)
	}
	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read after clear: %v", err)
	}
	if got != nil {
		t.Fatalf("Read after clear = %#v, want absent", got)
	}
}

func TestHandoffMarkerStore_UsesInjectedClockForFreshness(t *testing.T) {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.FixedZone("test", 3*60*60))
	now := base
	deadlines := restartTestDeadlines(&now)
	deadlines.Freshness = 17 * time.Minute
	store := NewHandoffMarkerStore(apitest.HardenedTempDir(t), deadlines)

	record, err := store.Begin(HandoffBegin{
		Generation: "clock-generation",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     555,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	wantCreated := base.UTC()
	if !record.CreatedAt.Equal(wantCreated) || !record.UpdatedAt.Equal(wantCreated) {
		t.Fatalf("created/updated = %s/%s, want injected %s", record.CreatedAt, record.UpdatedAt, wantCreated)
	}
	if want := wantCreated.Add(17 * time.Minute); !record.FreshUntil.Equal(want) {
		t.Fatalf("FreshUntil = %s, want %s", record.FreshUntil, want)
	}

	now = now.Add(2 * time.Minute)
	updated, err := store.Interrupt(record.Generation, "proof-timeout", "mcphub gui")
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if !updated.UpdatedAt.Equal(now.UTC()) {
		t.Fatalf("UpdatedAt = %s, want advanced injected time %s", updated.UpdatedAt, now.UTC())
	}
	if !updated.FreshUntil.Equal(record.FreshUntil) {
		t.Fatalf("FreshUntil changed across transition: got %s, want %s", updated.FreshUntil, record.FreshUntil)
	}
}

func TestHandoffMarkerStore_AbsentUnknownVersionAndStateDirMismatchFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	absentStore := NewHandoffMarkerStore(apitest.HardenedTempDir(t), restartTestDeadlines(&now))
	got, err := absentStore.Read()
	if err != nil || got != nil {
		t.Fatalf("absent Read = %#v, %v; want nil, nil", got, err)
	}

	unknownDir := apitest.HardenedTempDir(t)
	if err := api.WriteStateFileAtomic(filepath.Join(unknownDir, handoffMarkerFileLeaf), map[string]any{
		"version": "3.0",
	}); err != nil {
		t.Fatalf("seed unknown version: %v", err)
	}
	unknownStore := NewHandoffMarkerStore(unknownDir, restartTestDeadlines(&now))
	got, err = unknownStore.Read()
	if got != nil {
		t.Fatalf("unknown-version Read returned recovery-qualifying marker %#v", got)
	}
	var typed *HandoffMarkerError
	if !errors.As(err, &typed) || typed.FailureID != HandoffMarkerFailureRead {
		t.Fatalf("unknown-version Read error = %T %v, want typed read failure", err, err)
	}

	corruptDir := apitest.HardenedTempDir(t)
	if err := api.WriteStateFileBytesAtomic(filepath.Join(corruptDir, handoffMarkerFileLeaf), []byte("{")); err != nil {
		t.Fatalf("seed corrupt marker: %v", err)
	}
	corruptStore := NewHandoffMarkerStore(corruptDir, restartTestDeadlines(&now))
	got, err = corruptStore.Read()
	if got != nil {
		t.Fatalf("corrupt Read returned recovery-qualifying marker %#v", got)
	}
	if !errors.As(err, &typed) || typed.FailureID != HandoffMarkerFailureRead {
		t.Fatalf("corrupt Read error = %T %v, want typed read failure", err, err)
	}

	sourceDir := apitest.HardenedTempDir(t)
	sourceStore := NewHandoffMarkerStore(sourceDir, restartTestDeadlines(&now))
	if _, err := sourceStore.Begin(HandoffBegin{
		Generation: "state-dir-a",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     666,
	}); err != nil {
		t.Fatalf("seed source state dir: %v", err)
	}
	differentStore := NewHandoffMarkerStore(apitest.HardenedTempDir(t), restartTestDeadlines(&now))
	got, err = differentStore.Read()
	if err != nil || got != nil {
		t.Fatalf("different-state-dir Read = %#v, %v; want absent and no fallback to source dir", got, err)
	}
}

func TestHandoffMarkerStore_ReadAndWriteFailuresAreTyped(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	stateDir := apitest.HardenedTempDir(t)
	markerPath := filepath.Join(stateDir, handoffMarkerFileLeaf)
	if err := os.Mkdir(markerPath, 0o700); err != nil {
		t.Fatalf("seed marker-path directory: %v", err)
	}
	store := NewHandoffMarkerStore(stateDir, restartTestDeadlines(&now))

	if got, err := store.Read(); got != nil {
		t.Fatalf("Read failure returned marker %#v", got)
	} else {
		assertTypedHandoffFailure(t, err, HandoffMarkerFailureRead)
	}

	_, err := store.Begin(HandoffBegin{
		Generation: "write-failure",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     777,
	})
	assertTypedHandoffFailure(t, err, HandoffMarkerFailureWrite)
}

func TestHandoffMarkerStore_TrailingJSONUnknownFieldAndBeginOverCorruptFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)

	// Derive malformed variants from one genuinely valid on-disk marker.
	srcDir := apitest.HardenedTempDir(t)
	if _, err := NewHandoffMarkerStore(srcDir, restartTestDeadlines(&now)).Begin(HandoffBegin{
		Generation: "valid-src",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     4242,
	}); err != nil {
		t.Fatalf("seed valid marker: %v", err)
	}
	validBytes, err := os.ReadFile(filepath.Join(srcDir, handoffMarkerFileLeaf))
	if err != nil {
		t.Fatalf("read seeded marker: %v", err)
	}

	// (1) A valid object followed by trailing JSON must fail closed (ensureJSONEOF).
	trailingDir := apitest.HardenedTempDir(t)
	trailing := append(append([]byte{}, validBytes...), []byte("\n{\"x\":1}")...)
	if err := api.WriteStateFileBytesAtomic(filepath.Join(trailingDir, handoffMarkerFileLeaf), trailing); err != nil {
		t.Fatalf("seed trailing-json marker: %v", err)
	}
	if got, err := NewHandoffMarkerStore(trailingDir, restartTestDeadlines(&now)).Read(); got != nil {
		t.Fatalf("trailing-json Read returned marker %#v", got)
	} else {
		assertTypedHandoffFailure(t, err, HandoffMarkerFailureRead)
	}

	// (2) An otherwise-valid marker carrying an unknown field must fail closed
	// (DisallowUnknownFields), so a CUT field can never round-trip.
	var asMap map[string]any
	if err := json.Unmarshal(validBytes, &asMap); err != nil {
		t.Fatalf("unmarshal valid marker: %v", err)
	}
	asMap["bogus_field"] = 1
	unknownFieldDir := apitest.HardenedTempDir(t)
	if err := api.WriteStateFileAtomic(filepath.Join(unknownFieldDir, handoffMarkerFileLeaf), asMap); err != nil {
		t.Fatalf("seed unknown-field marker: %v", err)
	}
	if got, err := NewHandoffMarkerStore(unknownFieldDir, restartTestDeadlines(&now)).Read(); got != nil {
		t.Fatalf("unknown-field Read returned marker %#v", got)
	} else {
		assertTypedHandoffFailure(t, err, HandoffMarkerFailureRead)
	}

	// (3) Begin over an unreadable prior marker must fail closed WITHOUT erasing it.
	corruptDir := apitest.HardenedTempDir(t)
	corruptPath := filepath.Join(corruptDir, handoffMarkerFileLeaf)
	if err := api.WriteStateFileBytesAtomic(corruptPath, []byte("{ not json")); err != nil {
		t.Fatalf("seed corrupt marker: %v", err)
	}
	if _, err := NewHandoffMarkerStore(corruptDir, restartTestDeadlines(&now)).Begin(HandoffBegin{
		Generation: "over-corrupt",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     555,
	}); err == nil {
		t.Fatal("Begin over a corrupt marker succeeded; want fail-closed refusal")
	} else {
		assertTypedHandoffFailure(t, err, HandoffMarkerFailureWrite)
	}
	if _, err := os.ReadFile(corruptPath); err != nil {
		t.Fatalf("Begin erased the unreadable prior marker: %v", err)
	}
}

func assertHandoffMarkerReadEquals(t *testing.T, store *HandoffMarkerStore, want *HandoffMarkerRecord) {
	t.Helper()
	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v, want %#v", got, want)
	}
}

func assertTypedHandoffFailure(t *testing.T, err error, want HandoffMarkerFailureID) {
	t.Helper()
	var typed *HandoffMarkerError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %T %v, want *HandoffMarkerError", err, err)
	}
	if typed.FailureID != want {
		t.Fatalf("FailureID = %q, want %q (error: %v)", typed.FailureID, want, err)
	}
	if !typed.FailClosed() {
		t.Fatalf("FailClosed() = false for %q", typed.FailureID)
	}
}

func sortedMapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	// The expected list is kept alphabetical to make any forbidden field obvious.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
