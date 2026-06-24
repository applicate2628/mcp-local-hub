package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// availability_admission_paths_test.go — per-path D-3 (+ D-2) admission
// regressions for the Tier-0 catalog cross-cut findings. The unifying defect:
// the D-2 (vendored_source.pinned_ref) and D-3 (watch / disabled-until-probe)
// gates were consulted only on the happy install path and bypassed on the
// marketplace / InstallParsedManifest / serena-projection / register /
// lsp-auto-register paths. Each test below proves an inert (or unpinned)
// manifest is BLOCKED on ONE path, plus the additive guarantee that an
// absent-fields manifest behaves byte-identically.
//
// inertProbeBinary is a definitely-absent binary so availabilityProbePasses can
// never be satisfied on any host (CI or dev), making the inert gate fire
// deterministically.
const inertProbeBinary = "definitely-not-on-path-xyz"

// --- Finding 2: InstallParsedManifest now runs the canonical schema Validate ---

// TestInstallParsedManifest_RejectsUnpinnedVendoredSource is the finding-2
// regression: InstallParsedManifest accepts an ALREADY-PARSED manifest, so an
// in-process manifest declaring a vendored_source with an EMPTY pinned_ref would
// previously skip the D-2 pin gate (which lives in config.Validate, not in
// Preflight/AdmissionCheck). The added m.Validate() call rejects it BEFORE any
// supervisor-intent mutation.
func TestInstallParsedManifest_RejectsUnpinnedVendoredSource(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest()
	m.VendoredSource = &config.VendoredSource{Repo: "https://github.com/x/y"} // PinnedRef empty → D-2 violation

	a := NewAPI()
	var buf bytes.Buffer
	_, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{Writer: &buf, Workspaces: []WorkspaceEntry{}})
	if err == nil {
		t.Fatal("InstallParsedManifest(unpinned vendored_source): want Validate error, got nil")
	}
	if !strings.Contains(err.Error(), "pinned_ref") {
		t.Errorf("error = %q, want it to name the missing pinned_ref (D-2)", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("no intent must be written on D-2 reject; stat err = %v", statErr)
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("no scheduler mutation on reject: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_BlocksInertManifest proves the D-3 host gate also
// blocks here (Preflight → AdmissionCheck), and that the schema Validate added
// for D-2 does not break the inert case (a watch row with a valid probe shape
// passes Validate, then Preflight blocks on the host probe).
func TestInstallParsedManifest_BlocksInertManifest(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest()
	m.Availability = config.AvailabilityWatch
	m.InstallProbe = &config.AvailabilityProbe{Binaries: []string{inertProbeBinary}}

	a := NewAPI()
	var buf bytes.Buffer
	_, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{Writer: &buf, Workspaces: []WorkspaceEntry{}})
	if err == nil {
		t.Fatal("InstallParsedManifest(inert): want availability-probe block, got nil")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) || ae.ID != "availability-probe" {
		t.Errorf("error = %v, want *AdmissionError{ID: availability-probe}", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("no intent must be written on inert reject; stat err = %v", statErr)
	}
}

// TestInstallParsedManifest_AbsentFieldsByteIdentical confirms a manifest with
// no D-2/D-3 fields still installs exactly as before (the canonical
// serenaTemplateManifest is the absent-fields case; the existing happy-path
// tests cover the full install, so here we only assert it is NOT rejected by the
// new Validate/availability gate via the dry-run path which mutates nothing).
func TestInstallParsedManifest_AbsentFieldsByteIdentical(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest() // no VendoredSource / Availability / InstallProbe
	a := NewAPI()
	var buf bytes.Buffer
	// DryRun mutates nothing but runs the full Validate + Preflight gate, so a
	// spurious new rejection would surface here.
	if _, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{Writer: &buf, DryRun: true}); err != nil {
		t.Fatalf("absent-fields manifest must pass the new gate (byte-identical): %v", err)
	}
}

// --- Finding 3: serena dynamic-pool projection carries D-2/D-3 through ---

// TestSerenaProjection_PreservesInertAvailability proves the synthesized
// dynamic-pool manifest keeps an inert availability from the embed, so an inert
// serena source manifest stays blocked downstream (BuildInMemorySerenaDynamicPoolManifest
// previously DROPPED Availability/InstallProbe, making the projection look ready).
func TestSerenaProjection_PreservesInertAvailability(t *testing.T) {
	m := globalEmbedSerenaManifest()
	m.Availability = config.AvailabilityDisabledUntilProbe
	m.InstallProbe = &config.AvailabilityProbe{Binaries: []string{inertProbeBinary}}

	out, err := BuildInMemorySerenaDynamicPoolManifest(m)
	if err != nil {
		t.Fatalf("BuildInMemorySerenaDynamicPoolManifest: %v", err)
	}
	if out.Availability != config.AvailabilityDisabledUntilProbe {
		t.Fatalf("projected Availability = %q, want it carried through %q", out.Availability, config.AvailabilityDisabledUntilProbe)
	}
	if out.InstallProbe == nil || len(out.InstallProbe.Binaries) != 1 || out.InstallProbe.Binaries[0] != inertProbeBinary {
		t.Fatalf("projected InstallProbe = %+v, want the embed's probe carried through", out.InstallProbe)
	}
	// The downstream gate (the one InstallParsedManifest runs) must now see it as
	// inert+blocked.
	if err := AvailabilityAdmission(out); err == nil {
		t.Fatal("projected inert manifest passed AvailabilityAdmission; D-3 fields were dropped")
	}
	// And the projected probe slice must be an independent copy (deep-copy), not
	// an alias of the embed's slice.
	m.InstallProbe.Binaries[0] = "mutated"
	if out.InstallProbe.Binaries[0] != inertProbeBinary {
		t.Fatalf("projected InstallProbe aliases the embed slice (got %q after embed mutation)", out.InstallProbe.Binaries[0])
	}
}

// TestSerenaProjection_RejectsUnpinnedVendoredSource proves the projection now
// carries vendored_source through and out.Validate() catches an unpinned source
// at build time (D-2), instead of silently dropping it.
func TestSerenaProjection_RejectsUnpinnedVendoredSource(t *testing.T) {
	m := globalEmbedSerenaManifest()
	m.VendoredSource = &config.VendoredSource{Repo: "https://github.com/x/y"} // no PinnedRef

	if _, err := BuildInMemorySerenaDynamicPoolManifest(m); err == nil {
		t.Fatal("unpinned vendored_source embed: want build-time Validate error, got nil")
	} else if !strings.Contains(err.Error(), "pinned_ref") {
		t.Errorf("error = %q, want it to name the missing pinned_ref (D-2)", err.Error())
	}
}

// TestSerenaProjection_AbsentFieldsByteIdentical confirms an embed with no
// D-2/D-3 fields produces a projection with none (additive: nil in → nil out).
func TestSerenaProjection_AbsentFieldsByteIdentical(t *testing.T) {
	out, err := BuildInMemorySerenaDynamicPoolManifest(globalEmbedSerenaManifest())
	if err != nil {
		t.Fatalf("BuildInMemorySerenaDynamicPoolManifest: %v", err)
	}
	if out.Availability != "" || out.InstallProbe != nil || out.VendoredSource != nil {
		t.Fatalf("absent-fields embed projected non-empty D-2/D-3: availability=%q probe=%+v vendored=%+v",
			out.Availability, out.InstallProbe, out.VendoredSource)
	}
}

// --- Finding 4: register consults the D-3 gate before any side effect ---

// TestRegisterWithManifest_BlocksInertManifest is the finding-4 regression: an
// inert mcp-language-server manifest must be refused at the START of
// registerWithManifest, before EnsureWeeklyRefreshTask or any per-language
// scheduler/registry/client side effect.
func TestRegisterWithManifest_BlocksInertManifest(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()

	m := nineLanguageManifest()
	m.Availability = config.AvailabilityWatch
	m.InstallProbe = &config.AvailabilityProbe{Binaries: []string{inertProbeBinary}}

	_, err := mustNewAPI(t).registerWithManifest(m, ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("registerWithManifest(inert): want availability-probe block, got nil")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) || ae.ID != "availability-probe" {
		t.Errorf("error = %v, want *AdmissionError{ID: availability-probe}", err)
	}
	// No scheduler side effect: not the per-language task, not the shared
	// weekly-refresh task — the gate ran before either.
	if len(h.fakeSch.createdSpecs) != 0 {
		t.Errorf("scheduler Create fired %d time(s) for an inert manifest; gate must run first", len(h.fakeSch.createdSpecs))
	}
	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err == nil && len(reg.Workspaces) != 0 {
		t.Errorf("registry has %d row(s) for an inert manifest; gate must run before any registry write", len(reg.Workspaces))
	}
}

// TestRegisterWithManifest_AbsentFieldsByteIdentical confirms the shipped
// (no-availability) manifest still registers — the gate is a no-op for it.
func TestRegisterWithManifest_AbsentFieldsByteIdentical(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	ws := t.TempDir()

	rpt, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("absent-fields manifest must register byte-identically: %v", err)
	}
	if len(rpt.Entries) != 1 {
		t.Fatalf("report entries = %d, want 1", len(rpt.Entries))
	}
}

// --- Finding 5: lsp-auto-register consults the D-3 gate after manifest load ---

// TestEnsureLSPRegistered_BlocksInertManifest is the finding-5 regression: the
// first-touch LSP auto-register path must run the shared availability gate
// immediately after loadLSPRegisterManifest, before any registry write,
// supervisor-intent upsert, reconcile, or spawn. The loadLSPRegisterManifestFn
// seam injects an inert manifest (the embedded one has no availability).
func TestEnsureLSPRegistered_BlocksInertManifest(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(hardenedTempDir(t))
	defer restoreState()

	// Fail loud if the path reaches reconcile/spawn — the gate must short-circuit
	// before either.
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatal("reconcile reached for an inert LSP manifest; D-3 gate bypassed")
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origLoad := loadLSPRegisterManifestFn
	loadLSPRegisterManifestFn = func(language string) (*config.ServerManifest, config.LanguageSpec, error) {
		m := nineLanguageManifest()
		m.Availability = config.AvailabilityWatch
		m.InstallProbe = &config.AvailabilityProbe{Binaries: []string{inertProbeBinary}}
		for _, spec := range m.Languages {
			if spec.Name == language {
				return m, spec, nil
			}
		}
		return nil, config.LanguageSpec{}, errors.New("unknown language in test seam")
	}
	defer func() { loadLSPRegisterManifestFn = origLoad }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)

	_, err = NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err == nil {
		t.Fatal("EnsureLSPRegistered(inert): want availability-probe block, got nil")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) || ae.ID != "availability-probe" {
		t.Errorf("error = %v, want *AdmissionError{ID: availability-probe}", err)
	}
	// No registry row written.
	reg := NewRegistry(h.regPath)
	if lerr := reg.Load(); lerr == nil && len(reg.Workspaces) != 0 {
		t.Errorf("registry has %d row(s) for an inert LSP manifest; gate must run before any registry write", len(reg.Workspaces))
	}
}

// --- Finding 6: readiness surfaces the dropped availability finding ---

// TestCheckServerReadiness_InertSurfacesRequirement is the finding-6 regression:
// when AdmissionCheck blocks an inert row, the readiness report previously set
// Ready=false but dropped the availability-probe finding, so the GUI surface was
// false with no explanation. The report must now carry a visible requirement row
// naming the availability blocker.
func TestCheckServerReadiness_InertSurfacesRequirement(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:         "inert-readiness",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go",
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityWatch,
		InstallProbe: &config.AvailabilityProbe{Binaries: []string{inertProbeBinary}},
	}
	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatal("inert row reported Ready=true")
	}
	var found *ReadinessRequirement
	for i := range rep.Requirements {
		if strings.HasPrefix(rep.Requirements[i].Name, "availability:") {
			found = &rep.Requirements[i]
		}
	}
	if found == nil {
		t.Fatalf("no availability requirement row surfaced; requirements=%+v", rep.Requirements)
	}
	if found.OK {
		t.Errorf("availability requirement row OK=true; must be false to explain Ready=false")
	}
	if found.Reason == "" || found.Fix == "" {
		t.Errorf("availability requirement row missing Reason/Fix: %+v", *found)
	}
}

// TestCheckServerReadiness_ReadyRowByteIdentical confirms the short-circuit does
// NOT fire for a ready/empty manifest — the requirement list still contains the
// normal launcher/port/mcphub rows, not the availability row.
func TestCheckServerReadiness_ReadyRowByteIdentical(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:      "ready-readiness",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 9999}},
		// Availability empty == ready.
	}
	rep := CheckServerReadiness(m)
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "availability:") {
			t.Fatalf("ready row produced an availability requirement row: %+v", r)
		}
	}
	// The normal rows must still be present (the short-circuit did not fire).
	if len(rep.Requirements) == 0 {
		t.Fatal("ready row produced no requirement rows; short-circuit fired incorrectly")
	}
}
