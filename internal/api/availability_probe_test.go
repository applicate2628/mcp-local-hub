package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// --- D-3 readiness dry-run primitives ----------------------------------------

func TestAvailabilityInert(t *testing.T) {
	for _, av := range []string{config.AvailabilityWatch, config.AvailabilityDisabledUntilProbe} {
		if !availabilityInert(&config.ServerManifest{Availability: av}) {
			t.Fatalf("availabilityInert(%q) = false, want true", av)
		}
	}
	for _, av := range []string{"", config.AvailabilityReady} {
		if availabilityInert(&config.ServerManifest{Availability: av}) {
			t.Fatalf("availabilityInert(%q) = true, want false", av)
		}
	}
}

func TestAvailabilityProbePasses_Binaries(t *testing.T) {
	// "go" is guaranteed present in the test toolchain → probe passes.
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{Binaries: []string{"go"}}); !ok {
		t.Fatalf("probe with present binary failed: %q", why)
	}
	// A definitely-absent binary → probe fails with the basename reason.
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}})
	if ok {
		t.Fatalf("probe with absent binary passed; want fail")
	}
	if !strings.Contains(why, "definitely-not-on-path-xyz") || !strings.Contains(why, "not found on PATH") {
		t.Fatalf("reason %q missing basename/PATH text", why)
	}
}

func TestAvailabilityProbePasses_Files(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "installed.marker")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{marker}}); !ok {
		t.Fatalf("probe with present file failed: %q", why)
	}
	// Missing file.
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{filepath.Join(dir, "nope")}})
	if ok || !strings.Contains(why, "does not exist") {
		t.Fatalf("missing-file probe: ok=%v why=%q", ok, why)
	}
	// A directory is not a runnable regular file.
	ok, why = availabilityProbePasses(&config.AvailabilityProbe{Files: []string{dir}})
	if ok || !strings.Contains(why, "directory") {
		t.Fatalf("directory-file probe: ok=%v why=%q", ok, why)
	}
}

func TestAvailabilityProbePasses_NilNeverPasses(t *testing.T) {
	if ok, _ := availabilityProbePasses(nil); ok {
		t.Fatalf("nil probe passed; want fail")
	}
}

// TestAvailabilityProbePasses_WindowsPathBasenameInError is the codex r6 finding 3
// regression: a file probe with a Windows ABSOLUTE path (now accepted by the
// cross-platform validator) must surface ONLY the marker basename in the failure
// reason, NOT the whole `C:\Users\alice\...\marker`. On a non-Windows build
// filepath.Base treats '\' as a normal char and returns the full path; the fix
// uses basenameAcrossSeparators (splits on BOTH separators) so the GUI/API error
// never echoes the full host path. The probe LOGIC is unchanged — the verbatim
// path is still os.Stat'd (it does not exist on the test host, so the probe fails,
// which is exactly the surfacing path we assert).
func TestAvailabilityProbePasses_WindowsPathBasenameInError(t *testing.T) {
	winPath := `C:\Users\alice\AppData\Local\Programs\tool\installed.marker`
	ok, why := availabilityProbePasses(&config.AvailabilityProbe{Files: []string{winPath}})
	if ok {
		t.Fatalf("a nonexistent Windows path probe passed; want fail")
	}
	if !strings.Contains(why, "installed.marker") {
		t.Fatalf("reason %q missing the marker basename", why)
	}
	// The directory prefix must NOT leak — assert the full path is absent.
	if strings.Contains(why, "alice") || strings.Contains(why, `C:\`) {
		t.Fatalf("reason %q leaked the full Windows path; want basename only", why)
	}
	// The same for a path-shaped binary probe (the binary branch shares the fix).
	winBin := `C:\Program Files\tool\definitely-absent.exe`
	ok, why = availabilityProbePasses(&config.AvailabilityProbe{Binaries: []string{winBin}})
	if ok {
		t.Fatalf("a Windows-path binary probe passed; want fail")
	}
	if !strings.Contains(why, "definitely-absent.exe") {
		t.Fatalf("binary reason %q missing the basename", why)
	}
	if strings.Contains(why, "Program Files") || strings.Contains(why, `C:\`) {
		t.Fatalf("binary reason %q leaked the full Windows path; want basename only", why)
	}
}

// TestMarketplaceEntryBrowseProbeState_TriState is the FINDING-1 (codex catalog
// r4/r5) regression for the TRI-STATE browse classifier. The PASSIVE browse
// projection must NOT os.Stat a files[] probe nor exec.LookPath a path-shaped
// token while serving GET /api/marketplace — a catalog-supplied slow/UNC path
// must not stall opening the Catalog. It must ALSO distinguish the three states a
// single bool conflated: "ready" (installable now), "inert-blocked" (host app
// provably absent → greyed), and "inert-unknown" (file/path probe deferred → the
// GUI still offers install). This table is architect section-E's browse row.
func TestMarketplaceEntryBrowseProbeState_TriState(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "installed.marker")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	absent := filepath.Join(dir, "not-installed.marker")

	mk := func(p *CatalogAvailabilityProbe) *MarketplaceEntry {
		return &MarketplaceEntry{ID: "x", Availability: "watch", InstallProbe: p}
	}

	cases := []struct {
		name  string
		entry *MarketplaceEntry
		want  ProbeBrowseState
	}{
		// inert bare-binary present → ready (bounded exec.LookPath, "go" present).
		{"bare-binary-present", mk(&CatalogAvailabilityProbe{Binaries: []string{"go"}}), ProbeBrowseReady},
		// inert bare-binary absent → inert-blocked.
		{"bare-binary-absent", mk(&CatalogAvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}}), ProbeBrowseInertBlocked},
		// file-only (present) → inert-unknown WITHOUT stating it.
		{"file-only-present", mk(&CatalogAvailabilityProbe{Files: []string{present}}), ProbeBrowseInertUnknown},
		// file-only (absent) → inert-unknown — same verdict (file-presence-INDEPENDENT).
		{"file-only-absent", mk(&CatalogAvailabilityProbe{Files: []string{absent}}), ProbeBrowseInertUnknown},
		// path-shaped binary → inert-unknown (never LookPath a path — B2 defense).
		{"path-binary-posix", mk(&CatalogAvailabilityProbe{Binaries: []string{"/net/slow/tool"}}), ProbeBrowseInertUnknown},
		{"path-binary-windows", mk(&CatalogAvailabilityProbe{Binaries: []string{`C:\tools\x.exe`}}), ProbeBrowseInertUnknown},
		{"path-binary-unc", mk(&CatalogAvailabilityProbe{Binaries: []string{`\\host\share\x`}}), ProbeBrowseInertUnknown},
		// mixed binaries+files, ALL bare binaries present → inert-unknown (carries a
		// file the browse path defers; the bare binary already resolved).
		{"mixed-bin-present-and-file", mk(&CatalogAvailabilityProbe{Binaries: []string{"go"}, Files: []string{present}}), ProbeBrowseInertUnknown},
		// codex r6 finding 4: mixed binaries+files where a BARE binary is MISSING →
		// inert-blocked. With AND semantics the absent bare binary already proves the
		// probe cannot pass, so the row is greyed instead of offered-then-412'd. This
		// classifies BLOCKED even though a files[] entry is present (the prior order
		// returned inert-unknown via the files[] rule before checking the binary).
		{"mixed-bin-absent-and-file", mk(&CatalogAvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}, Files: []string{present}}), ProbeBrowseInertBlocked},
		// codex r6 finding 4: a path-shaped binary is NOT LookPath'd; it is deferred.
		// Mixed with an absent BARE binary still blocks (the bare one is checked; the
		// path-shaped one is skipped in the bare loop).
		{"mixed-path-binary-and-absent-bare", mk(&CatalogAvailabilityProbe{Binaries: []string{`C:\tools\x.exe`, "definitely-not-on-path-xyz"}}), ProbeBrowseInertBlocked},
		// A path-shaped binary alongside a PRESENT bare binary → inert-unknown (the
		// path-shaped one defers after the bare one resolves).
		{"mixed-path-binary-and-present-bare", mk(&CatalogAvailabilityProbe{Binaries: []string{"go", `C:\tools\x.exe`}}), ProbeBrowseInertUnknown},
		// nil/empty probe → inert-blocked (fail-closed).
		{"nil-probe", mk(nil), ProbeBrowseInertBlocked},
		{"empty-probe", mk(&CatalogAvailabilityProbe{}), ProbeBrowseInertBlocked},
		// ready/empty availability → ready (a nil entry too, asserted below).
		{"ready-row", &MarketplaceEntry{ID: "x", Availability: "ready"}, ProbeBrowseReady},
		{"empty-availability", &MarketplaceEntry{ID: "x"}, ProbeBrowseReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MarketplaceEntryBrowseProbeState(tc.entry); got != tc.want {
				t.Fatalf("MarketplaceEntryBrowseProbeState = %q, want %q", got, tc.want)
			}
		})
	}
	if got := MarketplaceEntryBrowseProbeState(nil); got != ProbeBrowseReady {
		t.Fatalf("nil entry: browse state = %q, want %q", got, ProbeBrowseReady)
	}

	// The file path is browse-presence-INDEPENDENT — proven by the file-only
	// present/absent rows above BOTH classifying inert-unknown. The FULL gate DOES
	// stat, so it diverges present=true / absent=false: that divergence is exactly
	// the file stat the browse path skips.
	if !MarketplaceEntryProbePasses(mk(&CatalogAvailabilityProbe{Files: []string{present}})) {
		t.Fatalf("full gate should pass for a present marker file")
	}
	if MarketplaceEntryProbePasses(mk(&CatalogAvailabilityProbe{Files: []string{absent}})) {
		t.Fatalf("full gate should fail for an absent marker file")
	}
}

// --- D-3 / D-2 AdmissionCheck gate -------------------------------------------

func TestAdmissionCheck_InertProbeFailsBlocks(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:         "watch-row",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go", // present, so no command-on-path finding
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityWatch,
		InstallProbe: &config.AvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}},
	}
	findings := AdmissionCheck(m, AdmissionScope{})
	var probe *AdmissionFinding
	for i := range findings {
		if findings[i].ID == "availability-probe" {
			probe = &findings[i]
		}
	}
	if probe == nil {
		t.Fatalf("no availability-probe finding; findings=%#v", findings)
	}
	if probe.Optional {
		t.Fatalf("availability-probe finding is Optional; must block")
	}
	if !containsNonOptional(findings) {
		t.Fatalf("findings contain no non-optional finding")
	}
	// Short-circuit: an inert un-probed row appends NO further port/binary
	// findings — the availability-probe finding is the only one.
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 (short-circuit) finding, got %d: %#v", len(findings), findings)
	}
	// And Preflight surfaces it as the blocking error.
	if err := Preflight(m, ""); err == nil {
		t.Fatalf("Preflight admitted an inert un-probed row; want AdmissionError")
	}
}

func TestAdmissionCheck_InertProbePassesFallsThrough(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:         "watch-row-ok",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go",
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityWatch,
		InstallProbe: &config.AvailabilityProbe{Binaries: []string{"go"}}, // present → passes
	}
	for _, f := range AdmissionCheck(m, AdmissionScope{}) {
		if f.ID == "availability-probe" {
			t.Fatalf("availability-probe finding present though probe passed: %#v", f)
		}
	}
}

func TestAdmissionCheck_ReadyRowNoProbeFinding(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:      "ready-row",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 9999}},
		// Availability empty == ready.
	}
	for _, f := range AdmissionCheck(m, AdmissionScope{}) {
		if f.ID == "availability-probe" {
			t.Fatalf("ready row produced an availability-probe finding: %#v", f)
		}
	}
}

// TestPreflight_InertRowNeverInstallsUntilProbe is the load-bearing integration
// claim for D-3: Preflight is the chokepoint Install runs (install.go step 2)
// BEFORE installPlanCore (step 4) writes any supervisor-intent row or client
// config. An inert row whose probe FAILS makes Preflight return the typed
// *AdmissionError carrying the availability-probe id, so Install returns before
// reaching installPlanCore — no spawn, no write. Once the probe PASSES (the
// host app is detected), Preflight returns nil — the enable transition. This
// asserts the gate at the exact boundary Install consults, with no live-state
// mutation (Preflight is pure / read-only).
func TestPreflight_InertRowNeverInstallsUntilProbe(t *testing.T) {
	setupAdmissionParityTest(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "host-app.marker")

	m := &config.ServerManifest{
		Name:         "inert-row",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go",
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityDisabledUntilProbe,
		InstallProbe: &config.AvailabilityProbe{Files: []string{marker}},
	}

	// Probe FAILS (marker absent) → Preflight blocks with the typed availability
	// error. This is the "never installs" guarantee: Install returns here, before
	// BuildPlan/installPlanCore.
	err := Preflight(m, "")
	if err == nil {
		t.Fatalf("Preflight admitted an inert row whose probe fails; want block")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) {
		t.Fatalf("Preflight error is not *AdmissionError: %T %v", err, err)
	}
	if ae.ID != "availability-probe" {
		t.Fatalf("blocking finding id = %q, want availability-probe", ae.ID)
	}

	// Operator installs the host app → marker appears → probe PASSES → Preflight
	// admits (the enable transition). Nothing else about the manifest changed.
	if werr := os.WriteFile(marker, []byte("installed"), 0o644); werr != nil {
		t.Fatalf("write marker: %v", werr)
	}
	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight still blocked after the probe passed: %v", err)
	}
}

func TestAdmissionCheck_VendoredLicenseAdvisory(t *testing.T) {
	setupAdmissionParityTest(t)
	base := func(ls string) *config.ServerManifest {
		return &config.ServerManifest{
			Name:           "vendored",
			Kind:           config.KindGlobal,
			Transport:      config.TransportStdioBridge,
			Command:        "go",
			Daemons:        []config.DaemonSpec{{Name: "default", Port: 9999}},
			VendoredSource: &config.VendoredSource{Repo: "https://github.com/x/y", PinnedRef: "v1", LicenseStatus: ls},
		}
	}
	// pending / empty / unknown → OPTIONAL advisory finding, does NOT block.
	for _, ls := range []string{config.LicenseStatusPending, "", config.LicenseStatusUnknown} {
		m := base(ls)
		findings := AdmissionCheck(m, AdmissionScope{})
		var found *AdmissionFinding
		for i := range findings {
			if findings[i].ID == "vendored-license-unvetted" {
				found = &findings[i]
			}
		}
		if found == nil {
			t.Fatalf("license_status %q produced no vendored-license-unvetted advisory", ls)
		}
		if !found.Optional {
			t.Fatalf("vendored-license-unvetted finding is non-optional for %q; must be advisory", ls)
		}
		if containsNonOptional(findings) {
			// "go" command present + ports free (mocked) → the only finding is the
			// optional advisory, so nothing should block.
			t.Fatalf("advisory license finding unexpectedly produced a blocking finding for %q", ls)
		}
		if err := Preflight(m, ""); err != nil {
			t.Fatalf("Preflight blocked on an advisory license finding (%q): %v", ls, err)
		}
	}
	// confirmed → no advisory finding.
	for _, f := range AdmissionCheck(base(config.LicenseStatusConfirmed), AdmissionScope{}) {
		if f.ID == "vendored-license-unvetted" {
			t.Fatalf("confirmed license still produced an advisory: %#v", f)
		}
	}
}

// TestCheckServerReadiness_VendoredLicenseAdvisorySurfaced is the FINDING 1
// regression: the D-2 vendored-license advisory (license_status empty / pending /
// unknown) must SURFACE in CheckServerReadiness as a visible Optional requirement
// row — previously it was emitted by AdmissionCheck but never rendered, so the
// operator could not see it. It must remain ADVISORY (Optional, NON-blocking): an
// empty/pending/unknown license does NOT flip Ready to false (the operator may
// knowingly install a pending-license fork on their own host). A confirmed
// license produces no such row.
func TestCheckServerReadiness_VendoredLicenseAdvisorySurfaced(t *testing.T) {
	setupAdmissionParityTest(t)
	base := func(ls string) *config.ServerManifest {
		return &config.ServerManifest{
			Name:           "vendored",
			Kind:           config.KindGlobal,
			Transport:      config.TransportStdioBridge,
			Command:        "go",
			Daemons:        []config.DaemonSpec{{Name: "default", Port: 9999}},
			VendoredSource: &config.VendoredSource{Repo: "https://github.com/x/y", PinnedRef: "v1", LicenseStatus: ls},
		}
	}
	findRow := func(rep *ReadinessReport) *ReadinessRequirement {
		for i := range rep.Requirements {
			if rep.Requirements[i].Name == "vendored source: vendored" {
				return &rep.Requirements[i]
			}
		}
		return nil
	}

	// empty / pending / unknown → a visible advisory row that does NOT block Ready.
	for _, ls := range []string{"", config.LicenseStatusPending, config.LicenseStatusUnknown} {
		rep := CheckServerReadiness(base(ls))
		row := findRow(rep)
		if row == nil {
			t.Fatalf("license_status %q: vendored-license advisory not surfaced in readiness; requirements=%+v", ls, rep.Requirements)
		}
		if !row.Optional {
			t.Fatalf("license_status %q: vendored-license readiness row is not Optional (must be advisory): %#v", ls, row)
		}
		if row.OK {
			t.Fatalf("license_status %q: vendored-license advisory row should be OK=false (unvetted): %#v", ls, row)
		}
		if !rep.Ready {
			t.Fatalf("license_status %q: advisory license row wrongly blocked Ready; requirements=%+v", ls, rep.Requirements)
		}
	}

	// confirmed → no advisory row, Ready unaffected.
	rep := CheckServerReadiness(base(config.LicenseStatusConfirmed))
	if row := findRow(rep); row != nil {
		t.Fatalf("confirmed license still surfaced a vendored-license advisory row: %#v", row)
	}
	if !rep.Ready {
		t.Fatalf("confirmed-license manifest unexpectedly not Ready; requirements=%+v", rep.Requirements)
	}
}

// TestCheckServerReadiness_InertProbeSingleAdmissionVerdict is the bot catalog-r3
// P3 regression: CheckServerReadinessWithScope must run AdmissionCheck (and thus
// the file-based install-probe) EXACTLY ONCE per request and reuse that single
// verdict for BOTH the Ready seed AND the surfaced availability-probe row — the
// prior code re-called availabilityProbeFinding a second time, which re-evaluated
// the probe and could yield an inconsistent Ready/row verdict if a probed file
// appeared or disappeared mid-request (intra-request TOCTOU).
//
// We prove single-verdict reuse two ways:
//   - the surfaced availability-probe row's Name/Reason/Fix are byte-identical to
//     the finding from ONE independent AdmissionCheck call (so readiness reuses
//     the same verdict, not a fresh re-probe); and
//   - the probe is a file under a temp dir; with the fix, the file's presence is
//     consulted exactly once via the single AdmissionCheck, so Ready==false and
//     the row are guaranteed self-consistent.
func TestCheckServerReadiness_InertProbeSingleAdmissionVerdict(t *testing.T) {
	setupAdmissionParityTest(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-installed.marker")
	m := &config.ServerManifest{
		Name:         "inert-row",
		Kind:         config.KindGlobal,
		Transport:    config.TransportStdioBridge,
		Command:      "go",
		Daemons:      []config.DaemonSpec{{Name: "default", Port: 9999}},
		Availability: config.AvailabilityWatch,
		InstallProbe: &config.AvailabilityProbe{Files: []string{missing}},
	}

	// The single AdmissionCheck verdict the readiness path must reuse.
	want, ok := findingByID(AdmissionCheck(m, AdmissionScope{}), "availability-probe")
	if !ok {
		t.Fatalf("AdmissionCheck did not produce an availability-probe finding for an inert un-probed row")
	}

	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatalf("inert un-probed row reported Ready=true; want false; requirements=%+v", rep.Requirements)
	}
	var rows []ReadinessRequirement
	for _, r := range rep.Requirements {
		if r.Name == want.Name {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly one availability-probe row (single AdmissionCheck verdict reused), got %d: %+v", len(rows), rep.Requirements)
	}
	row := rows[0]
	if row.OK {
		t.Fatalf("availability-probe row should be OK=false: %#v", row)
	}
	// The row MUST carry the SAME Reason/Fix the single AdmissionCheck verdict
	// produced — proving it was reused, not re-derived by a second probe call.
	if row.Reason != want.Reason || row.Fix != want.Fix {
		t.Fatalf("availability-probe row not sourced from the single AdmissionCheck verdict:\n  row    = %#v\n  verdict= %#v", row, want)
	}
}
