package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedForgetRow(t *testing.T, manifest string, state AdoptOperationState, withSnapshot bool) {
	t.Helper()
	seed := &AdoptedEntries{Version: 1, Records: []AdoptProvenanceRecord{{
		ManifestName:   manifest,
		OperationState: state,
		UpdatedAt:      time.Now().UTC(),
		Clients: []AdoptClientProvenance{
			{Client: "codex-cli", OriginalState: AdoptOriginalStatePresent},
		},
	}}}
	if err := writeAdoptedEntries(seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if withSnapshot {
		seedForgetSnapshotDir(t, manifest)
	}
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

func TestForgetAdoptProvenance_AdoptingRowAndSnapshotBothRemoved(t *testing.T) {
	isolateStateDir(t)
	m := "forgetadopting"
	seedForgetRow(t, m, AdoptOperationStateAdopting, true)

	a := NewAPI()
	plan, err := a.ForgetAdoptProvenance(m, ForgetAdoptProvenanceOpts{Yes: true})
	if err != nil {
		t.Fatalf("ForgetAdoptProvenance: %v", err)
	}
	if !plan.HasRow || !plan.HasSnapshotDir {
		t.Errorf("plan = %+v, want HasRow && HasSnapshotDir", plan)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("an `adopting` (crash-orphan) row must carry no warning, got %v", plan.Warnings)
	}
	if _, found, err := ReadAdoptProvenance(m); err != nil {
		t.Fatalf("ReadAdoptProvenance: %v", err)
	} else if found {
		t.Errorf("row survived forget")
	}
	dir, _ := adoptSnapshotDir(m)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("snapshot dir survived forget: stat err = %v", err)
	}
}

func TestForgetAdoptProvenance_AdoptedRowWarnsAndBuildIsDryRun(t *testing.T) {
	isolateStateDir(t)
	m := "forgetadopted"
	seedForgetRow(t, m, AdoptOperationStateAdopted, true)

	a := NewAPI()
	plan, err := a.BuildForgetAdoptProvenancePlan(m)
	if err != nil {
		t.Fatalf("BuildForgetAdoptProvenancePlan: %v", err)
	}
	if len(plan.Warnings) == 0 {
		t.Errorf("an `adopted` (committed) row must carry a de-adopt-capability-loss warning")
	}
	if !containsSubstr(plan.Warnings, "de-adopt") {
		t.Errorf("warning should name de-adopt; got %v", plan.Warnings)
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
	m := "forgetrowonly"
	seedForgetRow(t, m, AdoptOperationStateAdopting, false) // row, NO snapshot dir

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
