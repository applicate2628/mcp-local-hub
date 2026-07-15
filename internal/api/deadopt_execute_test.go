package api

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"

	"github.com/gofrs/flock"
)

func TestExecuteDeAdoptT1HappyPathClosesAllOwnedState(t *testing.T) {
	name := "deadopt-execute-happy"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 || len(report.Accepted) != 0 {
		t.Fatalf("report = %+v, want one restored client", report)
	}
	got := mustReadFileForAdoptTest(t, codexPath)
	mutator, ok := clients.AsCASEntryMutator(clients.AllClients()["codex-cli"])
	if !ok {
		t.Fatal("codex-cli does not expose CASEntryMutator")
	}
	gotSubtree, gotPresent, gotErr := mutator.EntryRawSubtree(got, name)
	wantSubtree, wantPresent, wantErr := mutator.EntryRawSubtree(snapshot, name)
	if gotErr != nil || wantErr != nil || !gotPresent || !wantPresent || !reflect.DeepEqual(gotSubtree, wantSubtree) {
		t.Fatalf("restored entry is not functionally equivalent\ngot subtree: %#v (present=%v err=%v)\nwant subtree: %#v (present=%v err=%v)", gotSubtree, gotPresent, gotErr, wantSubtree, wantPresent, wantErr)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)

	intent, err := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(intent.Daemons) != 0 {
		t.Fatalf("supervisor intent still has daemon rows: %+v", intent.Daemons)
	}
	logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if !bytes.Contains(logBytes, []byte(`"source":"deadopt"`)) || !bytes.Contains(logBytes, []byte(`"event":"deadopt-executed"`)) {
		t.Fatalf("deadopt-executed event missing:\n%s", logBytes)
	}
}

func TestExecuteDeAdoptT7RollForwardSkipsRestoreDoneAndCompletes(t *testing.T) {
	name := "deadopt-execute-resume"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	before := mustReadFileForAdoptTest(t, codexPath)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt resume: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
		t.Fatalf("resume report = %+v", report)
	}
	if !strings.Contains(out.String(), "already restored; skipping") {
		t.Fatalf("resume output did not record the skip: %s", out.String())
	}
	after := mustReadFileForAdoptTest(t, codexPath)
	if !bytes.Equal(before, after) {
		t.Fatalf("RESTORE-DONE client was rewritten\nbefore:\n%s\nafter:\n%s", before, after)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptReclassifiesStaleRestoreDonePlanAtE3(t *testing.T) {
	t.Run("StillHub is restored instead of silently skipped", func(t *testing.T) {
		name := "deadopt-execute-plan-drift-still-hub"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte(deAdoptHubConfig(name)), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 StillHub drift: %v", err)
		}

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
		if err != nil {
			t.Fatalf("execute stale plan: %v", err)
		}
		if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
			t.Fatalf("StillHub drift report = %+v", report)
		}
		live := mustReadFileForAdoptTest(t, codexPath)
		if !bytes.Contains(live, []byte("pre-adopt-native")) || bytes.Contains(live, []byte("127.0.0.1")) {
			t.Fatalf("StillHub drift was silently skipped instead of restored:\n%s", live)
		}
		assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
	})

	t.Run("genuine conflict fails and preserves recovery state", func(t *testing.T) {
		name := "deadopt-execute-plan-drift-conflict"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte(deAdoptNativeConfig(name, "operator-conflict")), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 conflict drift: %v", err)
		}
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
		if err != nil {
			t.Fatalf("execute stale conflict plan: %v", err)
		}
		if len(report.Failed) != 1 || report.Failed[0].Client != "codex-cli" || len(report.Restored) != 0 {
			t.Fatalf("conflict drift report = %+v", report)
		}
		assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, "")
	})

	t.Run("unreadable gate refuses before E2", func(t *testing.T) {
		name := "deadopt-execute-plan-drift-unreadable"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte("[mcp_servers.\n"), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 unreadable drift: %v", err)
		}
		before := mustReadFileForAdoptTest(t, codexPath)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
		if err == nil || !strings.Contains(err.Error(), "cannot prove gate-OFF") || !strings.Contains(err.Error(), "codex-cli") || report != nil {
			t.Fatalf("execute stale unreadable plan = report=%+v err=%v", report, err)
		}
		if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
			t.Fatalf("unreadable-gate refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
		}
		persisted, found, readErr := ReadAdoptProvenance(name)
		if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
			t.Fatalf("unreadable-gate refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
		}
		if _, statErr := os.Stat(snapshotPath); statErr != nil {
			t.Fatalf("unreadable-gate refusal removed snapshot: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
			t.Fatalf("unreadable-gate refusal removed manifest: %v", statErr)
		}
	})

	t.Run("accept conflict cannot bypass unreadable gate", func(t *testing.T) {
		name := "deadopt-execute-accept-drift-unreadable"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte("[mcp_servers.\n"), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 unreadable drift: %v", err)
		}
		before := mustReadFileForAdoptTest(t, codexPath)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err == nil || !strings.Contains(err.Error(), "cannot prove gate-OFF") || !strings.Contains(err.Error(), "codex-cli") || report != nil {
			t.Fatalf("execute stale unreadable acceptance plan = report=%+v err=%v", report, err)
		}
		if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
			t.Fatalf("unreadable-gate acceptance refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
		}
		persisted, found, readErr := ReadAdoptProvenance(name)
		if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
			t.Fatalf("unreadable-gate acceptance refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
		}
		if _, statErr := os.Stat(snapshotPath); statErr != nil {
			t.Fatalf("unreadable-gate acceptance refusal removed snapshot: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
			t.Fatalf("unreadable-gate acceptance refusal removed manifest: %v", statErr)
		}
	})
}

func TestExecuteDeAdoptCallerWriterFlushesAfterLeaseUnlock(t *testing.T) {
	name := "deadopt-execute-writer-after-unlock"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, _, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	writer := newBlockingDeAdoptWriter()
	type result struct {
		report *DeAdoptReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := NewAPI().ExecuteDeAdopt(name, writer)
		done <- result{report: report, err: err}
	}()

	select {
	case <-writer.started:
	case <-time.After(5 * time.Second):
		close(writer.release)
		t.Fatal("timed out waiting for caller writer invocation")
	}

	lease, leased, leaseErr := tryAcquireAdoptManifestLease(name)
	if leased {
		_ = lease.Unlock()
	}
	close(writer.release)
	select {
	case got := <-done:
		if got.err != nil || got.report == nil {
			t.Fatalf("ExecuteDeAdopt after writer release = report=%+v err=%v", got.report, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteDeAdopt remained blocked after caller writer was released")
	}
	if leaseErr != nil || !leased {
		t.Fatalf("manifest lease remained held while caller writer blocked: leased=%v err=%v", leased, leaseErr)
	}
}

func TestExecuteDeAdoptEventsDoNotBlockCompletedCall(t *testing.T) {
	name := "deadopt-execute-events-nonblocking"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, _, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	eventLock := flock.New(filepath.Join(stateRoot, SupervisorEventLogFileLeaf) + supervisorEventLogLockSuffix)
	locked, lockErr := eventLock.TryLock()
	if lockErr != nil || !locked {
		t.Fatalf("hold supervisor event-log lock: locked=%v err=%v", locked, lockErr)
	}
	defer func() { _ = eventLock.Unlock() }()

	type result struct {
		report *DeAdoptReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := NewAPI().ExecuteDeAdopt(name, io.Discard)
		done <- result{report: report, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil || got.report == nil {
			t.Fatalf("ExecuteDeAdopt under event-log contention = report=%+v err=%v", got.report, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteDeAdopt blocked on best-effort de-adopt event emission")
	}
	lease, leased, leaseErr := tryAcquireAdoptManifestLease(name)
	if leased {
		_ = lease.Unlock()
	}
	if leaseErr != nil || !leased {
		t.Fatalf("manifest lease remained held after completed de-adopt: leased=%v err=%v", leased, leaseErr)
	}
}

type blockingDeAdoptWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingDeAdoptWriter() *blockingDeAdoptWriter {
	return &blockingDeAdoptWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingDeAdoptWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestExecuteDeAdoptE1ConcurrentOperationRefusesBeforeMark(t *testing.T) {
	name := "deadopt-execute-concurrent"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	before := mustReadFileForAdoptTest(t, codexPath)
	lease, leased, err := tryAcquireAdoptManifestLease(name)
	if err != nil || !leased {
		t.Fatalf("hold manifest lease: leased=%v err=%v", leased, err)
	}
	t.Cleanup(func() { _ = lease.Unlock() })

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err == nil || !strings.Contains(err.Error(), "concurrent operation") || report != nil {
		t.Fatalf("concurrent ExecuteDeAdopt = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("client config changed before E1 refusal\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
		t.Fatalf("E1 refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); err != nil {
		t.Fatalf("E1 refusal removed manifest: %v", err)
	}
}

func TestExecuteDeAdoptReprobesHubGateUnderLease(t *testing.T) {
	name := "deadopt-execute-gate-flip"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}

	gateOn := []byte("[mcp_servers.mcphub-hub]\nurl = \"http://127.0.0.1:1/mcp\"\n")
	if err := os.WriteFile(codexPath, gateOn, 0o600); err != nil {
		t.Fatalf("flip hub gate ON after planning: %v", err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err == nil || !strings.Contains(err.Error(), "gate OFF first") || !strings.Contains(err.Error(), "codex-cli") || report != nil {
		t.Fatalf("execute gate-flipped plan = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("gate-flip refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
		t.Fatalf("gate-flip refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); statErr != nil {
		t.Fatalf("gate-flip refusal removed manifest: %v", statErr)
	}
}

func TestExecuteDeAdoptDerivesResumeRoutingFromLeasedRow(t *testing.T) {
	name := "deadopt-execute-fresh-plan-resume-row"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}

	rec.OperationState = AdoptOperationStateDeAdopting
	writeDeAdoptExecutorRecord(t, rec)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatalf("simulate completed manifest delete: %v", err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err != nil {
		t.Fatalf("execute FRESH plan against leased resume row: %v", err)
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
		t.Fatalf("resume-derived report = %+v", report)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("already-restored resume client was rewritten\nbefore:\n%s\nafter:\n%s", before, got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptRereadsProvenanceUnderLease(t *testing.T) {
	name := "deadopt-execute-row-replaced"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	newer := rec
	newer.Port++
	writeDeAdoptExecutorRecord(t, newer)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err == nil || !strings.Contains(err.Error(), "provenance changed under the lease") || report != nil {
		t.Fatalf("execute replaced-row plan = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("replaced-row refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted || persisted.Port != newer.Port {
		t.Fatalf("replaced row changed: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
		t.Fatalf("replaced-row refusal removed manifest: %v", statErr)
	}
}

func TestExecuteDeAdoptRejectsReplacementAdoptionByCreatedAt(t *testing.T) {
	name := "deadopt-execute-replacement-created-at"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	rec.CreatedAt = time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	rec.UpdatedAt = rec.CreatedAt
	writeDeAdoptExecutorRecord(t, rec)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	// Model a completed de-adopt followed by an otherwise-identical re-adopt:
	// capture assigns the replacement row a new immutable creation identity.
	replacement := rec
	replacement.CreatedAt = rec.CreatedAt.Add(time.Second)
	replacement.UpdatedAt = replacement.CreatedAt
	writeDeAdoptExecutorRecord(t, replacement)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err == nil || !strings.Contains(err.Error(), "provenance changed under the lease") || report != nil {
		t.Fatalf("execute replacement-adoption plan = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("replacement-adoption refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted || !persisted.CreatedAt.Equal(replacement.CreatedAt) {
		t.Fatalf("replacement adoption changed: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
		t.Fatalf("replacement-adoption refusal removed manifest: %v", statErr)
	}
}

func TestExecuteDeAdoptManifestReadinessRefusesBeforeE2E3(t *testing.T) {
	tests := []struct {
		name                 string
		manifestPresent      bool
		expectedHashOverride string
		wantReason           string
	}{
		{name: "fresh manifest absent", manifestPresent: false, wantReason: "manifest is absent"},
		{name: "manifest hash mismatch", manifestPresent: true, expectedHashOverride: strings.Repeat("a", 64), wantReason: "manifest hash does not match adopt provenance"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "deadopt-readiness-" + strings.ReplaceAll(tc.name, " ", "-")
			snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
			codexPath, _, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
				state:                AdoptOperationStateAdopted,
				originalState:        AdoptOriginalStatePresent,
				liveConfig:           deAdoptHubConfig(name),
				snapshotBytes:        snapshot,
				writeSnapshot:        true,
				manifestPresent:      tc.manifestPresent,
				expectedHashOverride: tc.expectedHashOverride,
			})
			before := mustReadFileForAdoptTest(t, codexPath)
			snapshotPath := deAdoptSnapshotPathForTest(t, rec)

			report, err := NewAPI().ExecuteDeAdopt(name, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "is not delete-ready") || !strings.Contains(err.Error(), tc.wantReason) || report != nil {
				t.Fatalf("ExecuteDeAdopt readiness refusal = report=%+v err=%v", report, err)
			}
			if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
				t.Fatalf("readiness refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
			}
			persisted, found, readErr := ReadAdoptProvenance(name)
			if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
				t.Fatalf("readiness refusal changed row: found=%v rec=%+v err=%v", found, persisted, readErr)
			}
			if _, statErr := os.Stat(snapshotPath); statErr != nil {
				t.Fatalf("readiness refusal removed snapshot: %v", statErr)
			}
		})
	}
}

func TestExecuteDeAdoptT8RoutedSecretPrefilterSharedSkipAndClose(t *testing.T) {
	name := "deadopt-execute-secrets"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const (
		deleteKey = "DEADOPT_DELETE_KEY"
		goneKey   = "DEADOPT_ALREADY_GONE_KEY"
		sharedKey = "DEADOPT_SHARED_KEY"
	)
	rec.RoutedSecretKeys = []string{goneKey, sharedKey, deleteKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{
		deleteKey: "delete-secret-value",
		sharedKey: "shared-secret-value",
	})
	sharedManifestDir := filepath.Join(manifestRoot, "shared-secret-owner")
	if err := os.MkdirAll(sharedManifestDir, 0o700); err != nil {
		t.Fatalf("mkdir shared manifest: %v", err)
	}
	sharedManifest := "name: shared-secret-owner\nkind: global\ntransport: stdio-bridge\ncommand: test-command\nenv:\n  TOKEN: secret:" + sharedKey + "\ndaemons:\n  - name: default\n    port: 31991\n"
	if err := os.WriteFile(filepath.Join(sharedManifestDir, "manifest.yaml"), []byte(sharedManifest), 0o600); err != nil {
		t.Fatalf("write shared manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt secrets: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) {
		t.Fatalf("secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want only shared key", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
	logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if !bytes.Contains(logBytes, []byte(sharedKey)) || bytes.Contains(logBytes, []byte("shared-secret-value")) || bytes.Contains(logBytes, []byte("delete-secret-value")) {
		t.Fatalf("shared-key event missing or leaked a value:\n%s", logBytes)
	}
}

func TestExecuteDeAdoptPreservesRoutedSecretReferencedThroughExpandedEnv(t *testing.T) {
	name := "deadopt-execute-expanded-env-secret"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const sharedKey = "DEADOPT_EXPANDED_ENV_SHARED_KEY"
	rec.RoutedSecretKeys = []string{sharedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{sharedKey: "expanded-env-secret-value"})
	t.Setenv("DEADOPT_SHARED_SECRET_REF", "secret:"+sharedKey)

	sharedManifestDir := filepath.Join(manifestRoot, "expanded-env-secret-owner")
	t.Cleanup(func() { _ = os.RemoveAll(sharedManifestDir) })
	if err := os.MkdirAll(sharedManifestDir, 0o700); err != nil {
		t.Fatalf("mkdir shared manifest: %v", err)
	}
	sharedManifest := "name: expanded-env-secret-owner\nkind: global\ntransport: stdio-bridge\ncommand: test-command\nenv:\n  TOKEN: \"${DEADOPT_SHARED_SECRET_REF}\"\ndaemons:\n  - name: default\n    port: 31992\n"
	if err := os.WriteFile(filepath.Join(sharedManifestDir, "manifest.yaml"), []byte(sharedManifest), 0o600); err != nil {
		t.Fatalf("write shared manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt expanded-env secret: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) || strings.Contains(out.String(), "could not be read") {
		t.Fatalf("expanded-env secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want expanded env shared key preserved", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptPreservesRoutedSecretReferencedByEmbeddedManifest(t *testing.T) {
	name := "deadopt-execute-embedded-secret"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const sharedKey = "wolfram_app_id"
	rec.RoutedSecretKeys = []string{sharedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{sharedKey: "embedded-manifest-secret-value"})
	// Exercise the production manifest source: the wolfram manifest exists only
	// in the binary's embedded catalog for this test and references sharedKey.
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", "")

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt embedded-manifest secret: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) || strings.Contains(out.String(), "could not be read") {
		t.Fatalf("embedded-manifest secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want embedded-manifest shared key preserved", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptPreservesRoutedSecretReferencedByRemoteHeader(t *testing.T) {
	name := "deadopt-execute-header-secret"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const sharedKey = "DEADOPT_REMOTE_HEADER_SHARED_KEY"
	rec.RoutedSecretKeys = []string{sharedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{sharedKey: "remote-header-secret-value"})

	sharedManifestDir := filepath.Join(manifestRoot, "remote-header-secret-owner")
	if err := os.MkdirAll(sharedManifestDir, 0o700); err != nil {
		t.Fatalf("mkdir shared manifest: %v", err)
	}
	sharedManifest := "name: remote-header-secret-owner\nkind: global\ntransport: remote-http\nurl: https://example.com/mcp\nheaders:\n  Authorization: \"Bearer ${secret:" + sharedKey + "}\"\n"
	if err := os.WriteFile(filepath.Join(sharedManifestDir, "manifest.yaml"), []byte(sharedManifest), 0o600); err != nil {
		t.Fatalf("write shared manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt remote-header secret: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) {
		t.Fatalf("remote-header secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want remote-header shared key preserved", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptPreservesRoutedSecretWhenAnotherManifestIsUnreadable(t *testing.T) {
	name := "deadopt-execute-unreadable-secret-owner"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const routedKey = "DEADOPT_POSSIBLY_SHARED_KEY"
	rec.RoutedSecretKeys = []string{routedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{routedKey: "possibly-shared-secret-value"})

	const unreadableManifest = "unreadable-secret-owner"
	unreadableDir := filepath.Join(manifestRoot, unreadableManifest)
	t.Cleanup(func() { _ = os.RemoveAll(unreadableDir) })
	if err := os.MkdirAll(unreadableDir, 0o700); err != nil {
		t.Fatalf("mkdir unreadable manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(unreadableDir, "manifest.yaml"), []byte("name: [\n"), 0o600); err != nil {
		t.Fatalf("write unreadable manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt unreadable-manifest secret scan: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), "preserved 1 key(s)") || !strings.Contains(out.String(), unreadableManifest) {
		t.Fatalf("unreadable-manifest cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{routedKey}) {
		t.Fatalf("vault keys = %#v, want routed key preserved after unreadable manifest", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestDeAdoptManifestVaultReferenceKeysCoversEnvURLAndHeaders(t *testing.T) {
	got := deAdoptManifestVaultReferenceKeys(&config.ServerManifest{
		Env: map[string]string{
			"TOKEN": "secret:ENV_KEY",
		},
		URL: "https://example.com/${secret:URL_KEY}/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer ${secret:HEADER_KEY}",
			"X-Combined":    "${secret:FIRST_KEY}:${secret:SECOND_KEY}",
		},
	})
	want := map[string]bool{
		"ENV_KEY":    true,
		"URL_KEY":    true,
		"HEADER_KEY": true,
		"FIRST_KEY":  true,
		"SECOND_KEY": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest vault references = %#v, want %#v", got, want)
	}
}

func TestExecuteDeAdoptT9T10T14FailureBlocksWholeCloseAndRedacts(t *testing.T) {
	name := "deadopt-execute-blocked"
	const snapshotSecret = "SNAPSHOT_SECRET_MUST_NOT_LEAK"
	snapshot := []byte("[mcp_servers." + name + "]\ncommand = \"native-command\"\n[mcp_servers." + name + ".env]\nAPI_KEY = \"" + snapshotSecret + "\"\n")
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptNativeConfig(name, "operator-conflict"),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	const routedKey = "DEADOPT_BLOCKED_KEY"
	const routedValue = "ROUTED_SECRET_MUST_NOT_LEAK"
	rec.RoutedSecretKeys = []string{routedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{routedKey: routedValue})
	liveBefore := mustReadFileForAdoptTest(t, codexPath)
	snapshotPath := deAdoptSnapshotPathForTest(t, rec)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt partial failure returned setup error: %v", err)
	}
	if len(report.Failed) != 1 || report.Failed[0].Client != "codex-cli" || len(report.Restored) != 0 || len(report.Accepted) != 0 {
		t.Fatalf("blocked report = %+v", report)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, liveBefore) {
		t.Fatalf("conflicting live config was changed\nbefore:\n%s\nafter:\n%s", liveBefore, got)
	}
	assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, routedKey)

	logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	serializedReport, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatalf("marshal de-adopt report: %v", marshalErr)
	}
	joined := out.String() + string(logBytes) + string(serializedReport)
	for _, forbidden := range []string{snapshotSecret, routedValue, string(snapshot), string(liveBefore)} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("de-adopt output/event leaked forbidden content %q:\n%s", forbidden, joined)
		}
	}
	if !bytes.Contains(logBytes, []byte(`"event":"deadopt-close-ready-blocked"`)) {
		t.Fatalf("close-ready-blocked event missing:\n%s", logBytes)
	}
}

func TestExecuteDeAdoptCrashInsideCloseResumeWithoutManifestOrSnapshot(t *testing.T) {
	name := "deadopt-execute-crash-inside-close"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   false,
		manifestPresent: false,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt crash-inside-close resume: %v", err)
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
		t.Fatalf("crash-inside-close report = %+v", report)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptAcceptConflictCrashInsideCloseIsRestoreDone(t *testing.T) {
	name := "deadopt-accept-crash-inside-close"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   false,
		manifestPresent: false,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdoptWithOpts(name, &out, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
	if err != nil {
		t.Fatalf("ExecuteDeAdoptWithOpts crash-inside-close: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 || len(report.Accepted) != 0 {
		t.Fatalf("accept crash-inside-close report = %+v", report)
	}
	if !strings.Contains(out.String(), "already restored; --accept-conflict was not needed") {
		t.Fatalf("accept crash-inside-close did not narrate harmless no-op: %s", out.String())
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptPartialRoutedSecretDeleteRetry(t *testing.T) {
	name := "deadopt-execute-partial-secret-retry"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      "",
		manifestPresent: false,
	})
	const (
		alreadyDeletedKey = "DEADOPT_PARTIAL_ALREADY_DELETED"
		remainingKey      = "DEADOPT_PARTIAL_REMAINING"
	)
	rec.RoutedSecretKeys = []string{alreadyDeletedKey, remainingKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptVault(t, map[string]string{remainingKey: "remaining-secret-value"})

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt partial routed-secret retry: %v", err)
	}
	if len(report.Failed) != 0 || len(deAdoptVaultKeys(t)) != 0 {
		t.Fatalf("partial routed-secret retry report/vault = %+v / %#v", report, deAdoptVaultKeys(t))
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptResumeWithAbsentVaultCompletesRoutedSecretCleanup(t *testing.T) {
	name := "deadopt-execute-absent-vault-resume"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      "",
		manifestPresent: false,
	})
	rec.RoutedSecretKeys = []string{"DEADOPT_ABSENT_VAULT_KEY"}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptVault(t, map[string]string{"UNRELATED_KEY": "unrelated-value"})
	if err := os.Remove(secrets.DefaultVaultPath()); err != nil {
		t.Fatalf("remove vault before resume: %v", err)
	}

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt absent-vault resume: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("absent-vault resume report = %+v", report)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptResumeWithUnreadableVaultBlocksClose(t *testing.T) {
	name := "deadopt-execute-unreadable-vault-resume"
	_, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      "",
		manifestPresent: false,
	})
	rec.RoutedSecretKeys = []string{"DEADOPT_UNREADABLE_VAULT_KEY"}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptVault(t, map[string]string{"DEADOPT_UNREADABLE_VAULT_KEY": "secret-value"})
	if err := os.WriteFile(secrets.DefaultVaultPath(), []byte("not-an-age-vault"), 0o600); err != nil {
		t.Fatalf("corrupt existing vault: %v", err)
	}

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err == nil || !strings.Contains(err.Error(), "routed-secret prefilter") {
		t.Fatalf("ExecuteDeAdopt unreadable-vault resume = report=%+v err=%v", report, err)
	}
	if _, statErr := os.Stat(secrets.DefaultVaultPath()); statErr != nil {
		t.Fatalf("existing unreadable vault was removed: %v", statErr)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateDeAdopting {
		t.Fatalf("unreadable-vault refusal lost recovery row: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("resume manifest unexpectedly present after refusal: %v", statErr)
	}
}

func TestExecuteDeAdoptResumeRechecksRecreatedManifestAtE4(t *testing.T) {
	for _, tc := range []struct {
		name        string
		recreated   []byte
		wantSuccess bool
	}{
		{name: "matching adopt content is deleted", recreated: []byte("name: deadopt-recreate-matching\n"), wantSuccess: true},
		{name: "different content is refused", recreated: []byte("name: foreign-replacement\n"), wantSuccess: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "deadopt-recreate-matching"
			adoptContent := []byte("name: " + name + "\n")
			_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
				state:           AdoptOperationStateDeAdopting,
				originalState:   AdoptOriginalStateAbsent,
				liveConfig:      "",
				manifestPresent: false,
				manifestBytes:   adoptContent,
			})
			seedDeAdoptSupervisorIntent(t, stateRoot, rec)
			manifestPath := filepath.Join(manifestRoot, name, "manifest.yaml")
			deAdoptBeforeManifestDeleteHook = func() {
				if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
					t.Fatalf("recreate manifest dir before E4: %v", err)
				}
				if err := os.WriteFile(manifestPath, tc.recreated, 0o600); err != nil {
					t.Fatalf("recreate manifest before E4: %v", err)
				}
			}
			t.Cleanup(func() { deAdoptBeforeManifestDeleteHook = nil })

			report, err := NewAPI().ExecuteDeAdopt(name, nil)
			if tc.wantSuccess {
				if err != nil || len(report.Failed) != 0 {
					t.Fatalf("ExecuteDeAdopt matching recreation = report=%+v err=%v", report, err)
				}
				assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
				return
			}
			if err == nil || !strings.Contains(err.Error(), "content hash changed") {
				t.Fatalf("ExecuteDeAdopt foreign recreation = report=%+v err=%v", report, err)
			}
			if got := mustReadFileForAdoptTest(t, manifestPath); !bytes.Equal(got, tc.recreated) {
				t.Fatalf("foreign manifest changed\ngot: %q\nwant: %q", got, tc.recreated)
			}
			assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, "", "")
		})
	}
}

func TestExecuteDeAdoptSnapshotDeletedManifestPresentRetryBlocksClose(t *testing.T) {
	name := "deadopt-execute-snapshot-deleted-manifest-present"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   false,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt T14 retry: %v", err)
	}
	if len(report.Failed) != 1 || !strings.Contains(report.Failed[0].Reason, "snapshot") {
		t.Fatalf("T14 retry report = %+v", report)
	}
	assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, "", "")
}

func TestExecuteDeAdoptErrorAndReportSerializationRedactsSensitiveState(t *testing.T) {
	name := "deadopt-execute-error-serialization"
	const snapshotSecret = "SERIALIZED_SNAPSHOT_SECRET_MUST_NOT_LEAK"
	snapshot := []byte("[mcp_servers." + name + "]\ncommand = \"native-command\"\n[mcp_servers." + name + ".env]\nAPI_KEY = \"" + snapshotSecret + "\"\n")
	_, _, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:                AdoptOperationStateAdopted,
		originalState:        AdoptOriginalStatePresent,
		liveConfig:           deAdoptHubConfig(name),
		snapshotBytes:        snapshot,
		writeSnapshot:        true,
		manifestPresent:      true,
		expectedHashOverride: strings.Repeat("a", 64),
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err == nil {
		t.Fatalf("ExecuteDeAdopt hash mismatch = report=%+v err=nil", report)
	}
	wire, marshalErr := json.Marshal(struct {
		Report *DeAdoptReport `json:"report"`
		Error  string         `json:"error"`
	}{Report: report, Error: err.Error()})
	if marshalErr != nil {
		t.Fatalf("marshal error/report envelope: %v", marshalErr)
	}
	if bytes.Contains(wire, []byte(snapshotSecret)) || bytes.Contains(wire, snapshot) {
		t.Fatalf("serialized error/report leaked snapshot content: %s", wire)
	}
}

func TestExecuteDeAdoptT13AcceptConflictRequiresMutationPointProof(t *testing.T) {
	t.Run("genuine conflict accepted and snapshot destroyed at close", func(t *testing.T) {
		name := "deadopt-accept-genuine"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      deAdoptNativeConfig(name, "operator-owned"),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		liveBefore := mustReadFileForAdoptTest(t, codexPath)

		var out bytes.Buffer
		report, err := NewAPI().ExecuteDeAdoptWithOpts(name, &out, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err != nil {
			t.Fatalf("ExecuteDeAdoptWithOpts: %v\n%s", err, out.String())
		}
		if !reflect.DeepEqual(report.Accepted, []string{"codex-cli"}) || len(report.Restored) != 0 || len(report.Failed) != 0 {
			t.Fatalf("accepted report = %+v", report)
		}
		if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, liveBefore) {
			t.Fatalf("accepted client was mutated\nbefore:\n%s\nafter:\n%s", liveBefore, got)
		}
		assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
		if !strings.Contains(out.String(), deAdoptAcceptConflictWarning) {
			t.Fatalf("mandatory snapshot-destruction warning missing: %s", out.String())
		}
		logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
		if !bytes.Contains(logBytes, []byte(`"event":"deadopt-client-accepted"`)) || !bytes.Contains(logBytes, []byte(deAdoptAcceptConflictWarning)) {
			t.Fatalf("accepted warning event missing:\n%s", logBytes)
		}
	})

	t.Run("still hub rejects acceptance and preserves snapshot", func(t *testing.T) {
		name := "deadopt-accept-still-hub"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      deAdoptHubConfig(name),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().ExecuteDeAdoptWithOpts(name, nil, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err != nil {
			t.Fatalf("ExecuteDeAdoptWithOpts still-hub: %v", err)
		}
		if len(report.Failed) != 1 || !strings.Contains(report.Failed[0].Reason, "still the hub entry") || len(report.Accepted) != 0 {
			t.Fatalf("still-hub report = %+v", report)
		}
		assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, "")
	})

	t.Run("unverified snapshot rejects before acceptance", func(t *testing.T) {
		name := "deadopt-accept-bad-snapshot"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:                AdoptOperationStateAdopted,
			originalState:        AdoptOriginalStatePresent,
			liveConfig:           deAdoptNativeConfig(name, "operator-owned"),
			snapshotBytes:        snapshot,
			writeSnapshot:        true,
			snapshotHashOverride: strings.Repeat("0", 64),
			manifestPresent:      true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().ExecuteDeAdoptWithOpts(name, nil, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err != nil {
			t.Fatalf("ExecuteDeAdoptWithOpts bad snapshot: %v", err)
		}
		if len(report.Failed) != 1 || !strings.Contains(report.Failed[0].Reason, "snapshot hash does not match") || len(report.Accepted) != 0 {
			t.Fatalf("bad-snapshot report = %+v", report)
		}
		assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, "")
	})
}

func writeDeAdoptExecutorRecord(t *testing.T, rec AdoptProvenanceRecord) {
	t.Helper()
	if err := writeAdoptedEntries(&AdoptedEntries{
		Version: adoptedEntriesSchemaVersion,
		Records: []AdoptProvenanceRecord{rec},
	}); err != nil {
		t.Fatalf("write de-adopt executor record: %v", err)
	}
}

func seedDeAdoptSupervisorIntent(t *testing.T, stateRoot string, rec AdoptProvenanceRecord) {
	t.Helper()
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-` + rec.ManifestName + `-` + adoptDefaultDaemonName,
			Server:   rec.ManifestName,
			Daemon:   adoptDefaultDaemonName,
			Port:     rec.Port,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}
}

func seedDeAdoptVault(t *testing.T, values map[string]string) {
	t.Helper()
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	for key, value := range values {
		if err := vault.Set(key, value); err != nil {
			t.Fatalf("vault.Set(%s): %v", key, err)
		}
	}
}

func deAdoptVaultKeys(t *testing.T) []string {
	t.Helper()
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	return vault.List()
}

func deAdoptSnapshotPathForTest(t *testing.T, rec AdoptProvenanceRecord) string {
	t.Helper()
	dir, err := adoptSnapshotDir(rec.ManifestName)
	if err != nil {
		t.Fatalf("adoptSnapshotDir: %v", err)
	}
	return filepath.Join(dir, rec.Clients[0].Client+adoptSnapshotFileSuffix)
}

func assertDeAdoptClosed(t *testing.T, manifestRoot, stateRoot string, rec AdoptProvenanceRecord) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest still present after close: %v", err)
	}
	if persisted, found, err := ReadAdoptProvenance(rec.ManifestName); err != nil || found || persisted != nil {
		t.Fatalf("provenance not closed: found=%v rec=%+v err=%v", found, persisted, err)
	}
	snapshotDir, err := adoptSnapshotDir(rec.ManifestName)
	if err != nil {
		t.Fatalf("adoptSnapshotDir: %v", err)
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir still present after close: %v", err)
	}
	_ = stateRoot
}

func assertDeAdoptRecoveryStateIntact(t *testing.T, manifestRoot, stateRoot string, rec AdoptProvenanceRecord, snapshotPath, routedKey string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); err != nil {
		t.Fatalf("manifest was not preserved: %v", err)
	}
	persisted, found, err := ReadAdoptProvenance(rec.ManifestName)
	if err != nil || !found || persisted == nil || persisted.OperationState != AdoptOperationStateDeAdopting {
		t.Fatalf("recoverable row missing: found=%v rec=%+v err=%v", found, persisted, err)
	}
	if snapshotPath != "" {
		if _, err := os.Stat(snapshotPath); err != nil {
			t.Fatalf("snapshot was not preserved: %v", err)
		}
	}
	intent, err := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(intent.Daemons) != 1 || intent.Daemons[0].Server != rec.ManifestName {
		t.Fatalf("supervisor intent was not preserved: %+v", intent.Daemons)
	}
	if routedKey != "" {
		keys := deAdoptVaultKeys(t)
		if !reflect.DeepEqual(keys, []string{routedKey}) {
			t.Fatalf("routed secret key was not preserved: %#v", keys)
		}
	}
}
