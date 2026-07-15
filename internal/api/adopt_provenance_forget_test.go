package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errStubManifestStat = errors.New("stub: manifest stat failed")

// forceAdoptManifestAbsent makes adoptRowProvablyUnmutated's manifest signal deterministic
// (no manifest on disk) so the warning logic depends only on the row shape, and restores it.
func forceAdoptManifestExists(t *testing.T, exists bool) {
	t.Helper()
	prior := adoptManifestExistsFn
	adoptManifestExistsFn = func(string) (bool, error) { return exists, nil }
	t.Cleanup(func() { adoptManifestExistsFn = prior })
}

// seedForgetRow seeds a BARE provenance row (no clients) — a clients-less `adopting` row is
// vacuously provably-unmutated, so with no manifest present it is the safe crash orphan that
// forget must NOT warn about.
func seedForgetRow(t *testing.T, manifest string, state AdoptOperationState, at time.Time, withSnapshot bool) {
	t.Helper()
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:   manifest,
		OperationState: state,
		UpdatedAt:      at,
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if withSnapshot {
		seedForgetSnapshotDir(t, manifest)
	}
}

func seedForgetRowWithClient(t *testing.T, manifest string, state AdoptOperationState, keys []string) {
	t.Helper()
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:     manifest,
		OperationState:   state,
		UpdatedAt:        time.Now().UTC(),
		RoutedSecretKeys: keys,
		Clients: []AdoptClientProvenance{
			{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent},
		},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	seedForgetSnapshotDir(t, manifest)
}

func seedForgetSnapshotDir(t *testing.T, manifest string) {
	t.Helper()
	dir, err := adoptSnapshotDir(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex-cli.snapshot"), []byte("SECRET-LITERAL"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// F1: the safe crash orphan (clients-less `adopting`, no manifest) forgets WITHOUT a warning.
func TestForgetAdoptProvenance_SafeCrashOrphanNoWarning(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetorphan"
	seedForgetRow(t, m, AdoptOperationStateAdopting, time.Now().UTC(), true)

	a := NewAPI()
	plan, err := a.ForgetAdoptProvenance(m, ForgetAdoptProvenanceOpts{Yes: true})
	if err != nil {
		t.Fatalf("ForgetAdoptProvenance: %v", err)
	}
	if !plan.HasRow || !plan.HasSnapshotDir {
		t.Errorf("plan = %+v, want HasRow && HasSnapshotDir", plan)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("a provably-safe crash orphan must carry no warning, got %v", plan.Warnings)
	}
	if _, found, _ := ReadAdoptProvenance(m); found {
		t.Errorf("row survived forget")
	}
	dir, _ := adoptSnapshotDir(m)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("snapshot dir survived forget: stat err = %v", err)
	}
}

// F1: a COMMITTED-but-`adopting` row (manifest still present) forgets WITH a warning.
func TestForgetAdoptProvenance_CommittedAdoptingWarns(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, true) // manifest present => the adopt may have committed
	m := "forgetcommitted"
	seedForgetRow(t, m, AdoptOperationStateAdopting, time.Now().UTC(), true)

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if !containsSubstr(plan.Warnings, "may have (partly) committed") {
		t.Errorf("a committed-but-adopting row (manifest present) must warn; got %v", plan.Warnings)
	}
}

// F1: a `de_adopting` row (mid-restore) forgets WITH a warning.
func TestForgetAdoptProvenance_DeAdoptingWarns(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetdeadopting"
	seedForgetRow(t, m, AdoptOperationStateDeAdopting, time.Now().UTC(), true)

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if !containsSubstr(plan.Warnings, "roll-forward recovery") {
		t.Errorf("a de_adopting row must warn about abandoned roll-forward recovery; got %v", plan.Warnings)
	}
}

func TestForgetAdoptProvenance_AdoptedRowWarnsAndBuildIsDryRun(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetadopted"
	seedForgetRow(t, m, AdoptOperationStateAdopted, time.Now().UTC(), true)

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if !containsSubstr(plan.Warnings, "de-adopt") {
		t.Errorf("an `adopted` row must warn and name de-adopt; got %v", plan.Warnings)
	}
	// BuildPlan must NOT remove anything (dry-run).
	if _, found, _ := ReadAdoptProvenance(m); !found {
		t.Errorf("BuildForgetAdoptProvenancePlan removed the row; it must be a dry read")
	}
	dir, _ := adoptSnapshotDir(m)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("BuildForgetAdoptProvenancePlan removed the snapshot dir; it must be a dry read: %v", err)
	}
	// Now actually forget it.
	if _, err := a.ForgetAdoptProvenance(m, ForgetAdoptProvenanceOpts{Yes: true}); err != nil {
		t.Fatalf("ForgetAdoptProvenance: %v", err)
	}
	if _, found, _ := ReadAdoptProvenance(m); found {
		t.Errorf("adopted row survived forget")
	}
}

// F3: routed vault key NAMES are surfaced in the plan (forget does not delete them).
func TestForgetAdoptProvenance_SurfacesRoutedKeyNames(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetkeys"
	seedForgetRowWithClient(t, m, AdoptOperationStateAdopted, []string{"FORGETKEYS_API_KEY", "FORGETKEYS_TOKEN"})

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if len(plan.RoutedSecretKeys) != 2 {
		t.Errorf("plan.RoutedSecretKeys = %v, want the 2 seeded key names", plan.RoutedSecretKeys)
	}
}

// F2: the identity gate refuses to destroy a row that changed since it was reviewed.
func TestForgetAdoptProvenance_IdentityGateRejectsChangedRow(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetchanged"
	t1 := time.Now().Add(-time.Hour).UTC()
	seedForgetRow(t, m, AdoptOperationStateAdopting, t1, true)

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}

	// Simulate a same-manifest commit in the gap: rewrite the row with a new state + updated_at.
	seedForgetRow(t, m, AdoptOperationStateAdopted, time.Now().UTC(), true)

	if _, err := a.ForgetAdoptProvenance(m, ForgetAdoptProvenanceOpts{
		Yes:               true,
		ConfirmIdentity:   true,
		ExpectedHasRow:    plan.HasRow,
		ExpectedUpdatedAt: plan.UpdatedAt,
		ExpectedRowState:  plan.RowState,
	}); err == nil {
		t.Errorf("identity gate must reject a row that changed since it was reviewed")
	}
	// The changed row must survive (not destroyed).
	if _, found, _ := ReadAdoptProvenance(m); !found {
		t.Errorf("identity gate must not destroy the changed row")
	}
}

// P2 (bot r2): a forget reviewed as rowless must refuse if a row appeared in the gap.
func TestForgetAdoptProvenance_IdentityGateRejectsRowAppearedWhereRowless(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetrowappeared"
	seedForgetSnapshotDir(t, m) // rowless: only a snapshot dir

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if plan.HasRow {
		t.Fatalf("precondition: plan must be rowless")
	}
	// A same-manifest adopt commits in the gap: a row appears.
	seedForgetRow(t, m, AdoptOperationStateAdopted, time.Now().UTC(), true)

	if _, err := a.ForgetAdoptProvenance(m, ForgetAdoptProvenanceOpts{
		Yes:             true,
		ConfirmIdentity: true,
		ExpectedHasRow:  plan.HasRow, // false
	}); err == nil {
		t.Errorf("identity gate must reject a row that appeared where 'row: none' was reviewed")
	}
	if _, found, _ := ReadAdoptProvenance(m); !found {
		t.Errorf("identity gate must not destroy the newly-appeared row")
	}
}

// P3 (bot r2): an unrecognized persisted state warns (not treated as the safe orphan).
func TestForgetRowWarnings_UnknownStateWarns(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetunknown"
	seedForgetRow(t, m, AdoptOperationState("some-future-state"), time.Now().UTC(), true)

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if !containsSubstr(plan.Warnings, "unrecognized state") {
		t.Errorf("an unknown row state must warn; got %v", plan.Warnings)
	}
}

// P2 (bot r2): an unverifiable manifest-existence check warns (fail-closed).
func TestForgetRowWarnings_ManifestErrorWarns(t *testing.T) {
	isolateStateDir(t)
	prior := adoptManifestExistsFn
	adoptManifestExistsFn = func(string) (bool, error) { return false, errStubManifestStat }
	t.Cleanup(func() { adoptManifestExistsFn = prior })

	m := "forgetmaniferr"
	seedForgetRow(t, m, AdoptOperationStateAdopting, time.Now().UTC(), true)

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if !containsSubstr(plan.Warnings, "may have (partly) committed") {
		t.Errorf("an unverifiable manifest existence must warn (fail-closed); got %v", plan.Warnings)
	}
}

func TestForgetAdoptProvenance_RowlessSnapshotDir(t *testing.T) {
	isolateStateDir(t)
	m := "forgetrowless"
	seedForgetSnapshotDir(t, m) // snapshot dir, NO row

	a := NewAPI()
	plan, err := a.ForgetAdoptProvenance(m, ForgetAdoptProvenanceOpts{Yes: true})
	if err != nil {
		t.Fatalf("ForgetAdoptProvenance: %v", err)
	}
	if plan.HasRow {
		t.Errorf("plan.HasRow = true, want false (rowless)")
	}
	if !plan.HasSnapshotDir {
		t.Errorf("plan.HasSnapshotDir = false, want true")
	}
	dir, _ := adoptSnapshotDir(m)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("rowless snapshot dir survived forget: stat err = %v", err)
	}
}

func TestForgetAdoptProvenance_RowOnlyNoSnapshot(t *testing.T) {
	isolateStateDir(t)
	forceAdoptManifestExists(t, false)
	m := "forgetrowonly"
	seedForgetRow(t, m, AdoptOperationStateAdopting, time.Now().UTC(), false) // row, NO snapshot dir

	a := NewAPI()
	plan, err := a.ForgetAdoptProvenance(m, ForgetAdoptProvenanceOpts{Yes: true})
	if err != nil {
		t.Fatalf("ForgetAdoptProvenance: %v", err)
	}
	if !plan.HasRow || plan.HasSnapshotDir {
		t.Errorf("plan = %+v, want row-only", plan)
	}
	if _, found, _ := ReadAdoptProvenance(m); found {
		t.Errorf("row-only survived forget")
	}
}

func TestForgetAdoptProvenance_NothingToForget(t *testing.T) {
	isolateStateDir(t)
	a := NewAPI()
	if _, err := a.BuildForgetAdoptProvenancePlan("nosuchmanifest"); err == nil {
		t.Errorf("BuildForgetAdoptProvenancePlan: expected error when nothing to forget")
	}
	if _, err := a.ForgetAdoptProvenance("nosuchmanifest", ForgetAdoptProvenanceOpts{Yes: true}); err == nil {
		t.Errorf("ForgetAdoptProvenance: expected error when nothing to forget")
	}
}

func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
