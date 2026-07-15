package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

const deAdoptTestPort = 7 // inert URL data only; these tests never bind a TCP port

type deAdoptPlannerFixture struct {
	state                AdoptOperationState
	originalState        AdoptOriginalState
	liveConfig           string
	snapshotBytes        []byte
	writeSnapshot        bool
	snapshotHashOverride string
	manifestPresent      bool
	manifestBytes        []byte
	expectedHashOverride string
}

func deAdoptHubConfig(name string) string {
	return fmt.Sprintf("[mcp_servers.%s]\nurl = \"http://127.0.0.1:%d/mcp\"\n", name, deAdoptTestPort)
}

func deAdoptNativeConfig(name, command string) string {
	return fmt.Sprintf("[mcp_servers.%s]\ncommand = %q\nargs = [\"version\"]\n", name, command)
}

func isolateDeAdoptPlannerAppData(t *testing.T, codexPath string) {
	t.Helper()
	root := filepath.Dir(filepath.Dir(filepath.Dir(codexPath)))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
}

func setupDeAdoptPlannerFixture(t *testing.T, name string, fixture deAdoptPlannerFixture) (codexPath, manifestRoot, stateRoot string, rec AdoptProvenanceRecord) {
	t.Helper()
	codexPath, manifestRoot, stateRoot = setupAdoptTestEnv(t, name, fixture.liveConfig)
	isolateDeAdoptPlannerAppData(t, codexPath)

	manifestBytes := fixture.manifestBytes
	if manifestBytes == nil {
		manifestBytes = []byte("name: " + name + "\n")
	}
	expectedHash := ManifestHashContent(manifestBytes)
	if fixture.expectedHashOverride != "" {
		expectedHash = fixture.expectedHashOverride
	}
	if fixture.manifestPresent {
		dir := filepath.Join(manifestRoot, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir manifest dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), manifestBytes, 0o600); err != nil {
			t.Fatalf("seed manifest: %v", err)
		}
	}

	clientRec := AdoptClientProvenance{
		Client:        "codex-cli",
		OriginalState: fixture.originalState,
		RestoreMode:   AdoptRestoreModeNA,
	}
	if fixture.originalState == AdoptOriginalStatePresent {
		clientRec.RestoreMode = AdoptRestoreModeFunctionalEquivalent
		clientRec.SnapshotRef = "../../untrusted-recorded-path.snapshot"
		clientRec.SnapshotSHA256 = ManifestHashContent(fixture.snapshotBytes)
		if fixture.writeSnapshot {
			if _, _, err := writeAdoptClientSnapshot(name, clientRec.Client, fixture.snapshotBytes); err != nil {
				t.Fatalf("seed canonical snapshot: %v", err)
			}
		}
		if fixture.snapshotHashOverride != "" {
			clientRec.SnapshotSHA256 = fixture.snapshotHashOverride
		}
	}

	rec = AdoptProvenanceRecord{
		ManifestName:         name,
		SourceClient:         "codex-cli",
		SourceEntryName:      name,
		Port:                 deAdoptTestPort,
		AdoptClients:         []string{"codex-cli"},
		AdoptManifestHash:    expectedHash,
		ExpectedManifestHash: expectedHash,
		OperationState:       fixture.state,
		Clients:              []AdoptClientProvenance{clientRec},
	}
	if err := writeAdoptedEntries(&AdoptedEntries{
		Version: adoptedEntriesSchemaVersion,
		Records: []AdoptProvenanceRecord{rec},
	}); err != nil {
		t.Fatalf("seed adopt provenance: %v", err)
	}
	return codexPath, manifestRoot, stateRoot, rec
}

func TestDeAdoptT11DispositionCompositionTable(t *testing.T) {
	tests := []struct {
		name                  string
		routing               DeAdoptRoutingVerdict
		original              AdoptOriginalState
		snapshot              deAdoptSnapshotState
		manifestAlreadyAbsent bool
		verdict               clients.EntryClassification
		want                  DeAdoptClientDisposition
		acceptEligible        bool
	}{
		{"resume present still-hub snapshot ready", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyStillHub, DeAdoptClientRestorePending, false},
		{"resume present still-hub snapshot unreadable", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, false, clients.ClassifyStillHub, DeAdoptClientFailed, false},
		{"resume present still-hub snapshot missing", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, true, clients.ClassifyStillHub, DeAdoptClientFailed, false},
		{"resume absent still-hub", DeAdoptRoutingResume, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyStillHub, DeAdoptClientRemovePending, false},
		{"resume merged-lower still-hub", DeAdoptRoutingResume, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyStillHub, DeAdoptClientRemovePending, false},
		{"resume present restore-done", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyRestoreDone, DeAdoptClientRestoreDone, false},
		{"resume absent restore-done", DeAdoptRoutingResume, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyRestoreDone, DeAdoptClientRestoreDone, false},
		{"resume merged-lower restore-done", DeAdoptRoutingResume, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyRestoreDone, DeAdoptClientRestoreDone, false},
		{"resume present genuine conflict", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, true},
		{"resume absent genuine conflict", DeAdoptRoutingResume, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, true},
		{"resume merged-lower genuine conflict", DeAdoptRoutingResume, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, true},
		{"resume present unreadable live", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"resume absent unreadable live", DeAdoptRoutingResume, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"resume merged-lower unreadable live", DeAdoptRoutingResume, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"resume unreadable snapshot cannot prove done", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, true, clients.ClassifyRestoreDone, DeAdoptClientFailed, false},
		{"resume unreadable snapshot cannot prove conflict", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, true, clients.ClassifyGenuineConflict, DeAdoptClientFailed, false},
		{"resume unreadable snapshot unreadable live", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"resume missing snapshot present manifest still-hub", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyStillHub, DeAdoptClientFailed, false},
		{"resume missing snapshot present manifest restore verdict", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyRestoreDone, DeAdoptClientFailed, false},
		{"resume missing snapshot present manifest conflict verdict", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, false},
		{"resume missing snapshot present manifest unreadable live", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"resume missing snapshot unreadable manifest conflict verdict", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, false},
		{"resume missing snapshot absent manifest restore verdict", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, true, clients.ClassifyRestoreDone, DeAdoptClientRestoreDone, false},
		{"resume missing snapshot absent manifest conflict verdict", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, true, clients.ClassifyGenuineConflict, DeAdoptClientRestoreDone, false},
		{"resume missing snapshot absent manifest unreadable live", DeAdoptRoutingResume, AdoptOriginalStatePresent, deAdoptSnapshotMissing, true, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"fresh present still-hub", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyStillHub, DeAdoptClientRestorePending, false},
		{"fresh present restore-done", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyRestoreDone, DeAdoptClientRestoreDone, false},
		{"fresh genuine conflict is accept-eligible", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, true},
		{"fresh present available unreadable live", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"fresh present missing still-hub", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyStillHub, DeAdoptClientFailed, false},
		{"fresh present missing restore-done", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyRestoreDone, DeAdoptClientFailed, false},
		{"fresh missing snapshot is not crash-done", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotMissing, true, clients.ClassifyGenuineConflict, DeAdoptClientFailed, false},
		{"fresh present missing unreadable live", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotMissing, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"fresh present unreadable still-hub", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, false, clients.ClassifyStillHub, DeAdoptClientFailed, false},
		{"fresh present unreadable restore-done", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, false, clients.ClassifyRestoreDone, DeAdoptClientFailed, false},
		{"fresh present unreadable genuine conflict", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, false},
		{"fresh present unreadable live", DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotUnreadable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"fresh absent still-hub", DeAdoptRoutingFresh, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyStillHub, DeAdoptClientRemovePending, false},
		{"fresh absent restore-done", DeAdoptRoutingFresh, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyRestoreDone, DeAdoptClientRestoreDone, false},
		{"fresh absent genuine conflict", DeAdoptRoutingFresh, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, true},
		{"fresh absent unreadable live", DeAdoptRoutingFresh, AdoptOriginalStateAbsent, deAdoptSnapshotNotApplicable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
		{"fresh merged-lower still-hub", DeAdoptRoutingFresh, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyStillHub, DeAdoptClientRemovePending, false},
		{"fresh merged-lower restore-done", DeAdoptRoutingFresh, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyRestoreDone, DeAdoptClientRestoreDone, false},
		{"fresh merged-lower genuine conflict", DeAdoptRoutingFresh, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyGenuineConflict, DeAdoptClientFailed, true},
		{"fresh merged-lower unreadable live", DeAdoptRoutingFresh, AdoptOriginalStatePresentMergedLower, deAdoptSnapshotNotApplicable, false, clients.ClassifyUnreadable, DeAdoptClientFailed, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, acceptEligible, _ := mapDeAdoptClientDisposition(tc.routing, tc.original, tc.snapshot, tc.manifestAlreadyAbsent, tc.verdict)
			if got != tc.want || acceptEligible != tc.acceptEligible {
				t.Fatalf("composition = (%q, accept=%v), want (%q, accept=%v)", got, acceptEligible, tc.want, tc.acceptEligible)
			}
		})
	}
}

func TestBuildDeAdoptPlanT11UsesAtomicClassifierAndRecomputedSnapshotPath(t *testing.T) {
	tests := []struct {
		name             string
		originalState    AdoptOriginalState
		live             func(string) string
		writeSnapshot    bool
		snapshotHash     string
		manifestPresent  bool
		wantDisposition  DeAdoptClientDisposition
		acceptEligible   bool
		wantManifestLive bool
	}{
		{"still-hub present restores", AdoptOriginalStatePresent, deAdoptHubConfig, true, "", true, DeAdoptClientRestorePending, false, true},
		{"restore-done present", AdoptOriginalStatePresent, func(n string) string { return deAdoptNativeConfig(n, "go") }, true, "", true, DeAdoptClientRestoreDone, false, true},
		{"genuine-conflict present", AdoptOriginalStatePresent, func(n string) string { return deAdoptNativeConfig(n, "operator-command") }, true, "", true, DeAdoptClientFailed, true, true},
		{"snapshot missing manifest present", AdoptOriginalStatePresent, func(n string) string { return deAdoptNativeConfig(n, "operator-command") }, false, "", true, DeAdoptClientFailed, false, true},
		{"snapshot missing after manifest delete", AdoptOriginalStatePresent, func(n string) string { return deAdoptNativeConfig(n, "operator-command") }, false, "", false, DeAdoptClientRestoreDone, false, false},
		{"still-hub snapshot missing after manifest delete", AdoptOriginalStatePresent, deAdoptHubConfig, false, "", false, DeAdoptClientFailed, false, false},
		{"snapshot sha mismatch cannot prove conflict", AdoptOriginalStatePresent, func(n string) string { return deAdoptNativeConfig(n, "operator-command") }, true, strings.Repeat("0", 64), true, DeAdoptClientFailed, false, true},
		{"absent original removes hub", AdoptOriginalStateAbsent, deAdoptHubConfig, false, "", true, DeAdoptClientRemovePending, false, true},
		{"absent original already removed", AdoptOriginalStateAbsent, func(string) string { return "model = \"gpt-5\"\n" }, false, "", true, DeAdoptClientRestoreDone, false, true},
		{"absent original genuine conflict", AdoptOriginalStateAbsent, func(n string) string { return deAdoptNativeConfig(n, "operator-command") }, false, "", true, DeAdoptClientFailed, true, true},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("deadoptt11%d", i)
			snapshot := []byte(deAdoptNativeConfig(name, "go"))
			codexPath, _, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
				state:                AdoptOperationStateDeAdopting,
				originalState:        tc.originalState,
				liveConfig:           tc.live(name),
				snapshotBytes:        snapshot,
				writeSnapshot:        tc.writeSnapshot,
				snapshotHashOverride: tc.snapshotHash,
				manifestPresent:      tc.manifestPresent,
			})
			before, err := os.ReadFile(codexPath)
			if err != nil {
				t.Fatalf("read live config before plan: %v", err)
			}

			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatalf("BuildDeAdoptPlan: %v", err)
			}
			if plan.Routing != DeAdoptRoutingResume {
				t.Fatalf("routing = %q, want RESUME", plan.Routing)
			}
			if len(plan.Clients) != 1 {
				t.Fatalf("clients = %#v, want one", plan.Clients)
			}
			got := plan.Clients[0]
			if got.Disposition != tc.wantDisposition || got.AcceptEligible != tc.acceptEligible {
				t.Fatalf("client plan = (%q, accept=%v, reason=%q), want (%q, accept=%v)", got.Disposition, got.AcceptEligible, got.Reason, tc.wantDisposition, tc.acceptEligible)
			}
			if plan.Manifest.Present != tc.wantManifestLive || !plan.Manifest.HashReady {
				t.Fatalf("manifest readiness = %+v, want present=%v ready=true", plan.Manifest, tc.wantManifestLive)
			}
			after, err := os.ReadFile(codexPath)
			if err != nil {
				t.Fatalf("read live config after plan: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("BuildDeAdoptPlan mutated client config\nbefore:\n%s\nafter:\n%s", before, after)
			}
			persisted, found, err := ReadAdoptProvenance(name)
			if err != nil || !found || persisted.OperationState != rec.OperationState {
				t.Fatalf("BuildDeAdoptPlan mutated provenance: found=%v row=%+v err=%v", found, persisted, err)
			}
		})
	}
}

func TestBuildDeAdoptPlanT12ResumeSkipsAbsentManifest(t *testing.T) {
	name := "deadoptt12resume"
	snapshot := []byte(deAdoptNativeConfig(name, "go"))
	_, _, _, _ = setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptNativeConfig(name, "go"),
		snapshotBytes:   snapshot,
		writeSnapshot:   false,
		manifestPresent: false,
	})

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatalf("BuildDeAdoptPlan: %v", err)
	}
	if plan.Routing != DeAdoptRoutingResume {
		t.Fatalf("routing = %q, want RESUME", plan.Routing)
	}
	if !plan.Manifest.AlreadyAbsent || !plan.Manifest.HashReady || plan.Manifest.Present {
		t.Fatalf("absent-manifest readiness = %+v, want already-absent ready skip", plan.Manifest)
	}
	if len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
		t.Fatalf("resume clients = %#v, want RESTORE-DONE", plan.Clients)
	}
}

func TestBuildDeAdoptPlanResumeMissingSnapshotUnreadableManifestFailsClosed(t *testing.T) {
	name := "deadoptunreadablemanifest"
	snapshot := []byte(deAdoptNativeConfig(name, "go"))
	_, manifestRoot, _, _ := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptNativeConfig(name, "operator-command"),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})

	snapshotDir, err := adoptSnapshotDir(name)
	if err != nil {
		t.Fatalf("resolve snapshot dir: %v", err)
	}
	if err := os.Remove(filepath.Join(snapshotDir, "codex-cli"+adoptSnapshotFileSuffix)); err != nil {
		t.Fatalf("externally delete snapshot: %v", err)
	}
	manifestPath := filepath.Join(manifestRoot, name, "manifest.yaml")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("remove readable manifest: %v", err)
	}
	if err := os.Mkdir(manifestPath, 0o700); err != nil {
		t.Fatalf("replace manifest file with unreadable directory: %v", err)
	}

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatalf("BuildDeAdoptPlan: %v", err)
	}
	if plan.Manifest.Present || plan.Manifest.AlreadyAbsent || plan.Manifest.HashReady {
		t.Fatalf("unreadable-manifest readiness = %+v, want present=false already-absent=false hash-ready=false", plan.Manifest)
	}
	if len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientFailed {
		t.Fatalf("unreadable-manifest clients = %#v, want FAILED", plan.Clients)
	}
}

func TestBuildDeAdoptPlanUnreadableGatePrecedesSnapshotClassification(t *testing.T) {
	name := "deadoptunreadablereason"
	_, _, _, _ = setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      "not = [valid",
		snapshotBytes:   []byte(deAdoptNativeConfig(name, "go")),
		writeSnapshot:   false,
		manifestPresent: true,
	})

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatalf("BuildDeAdoptPlan: %v", err)
	}
	if plan.Routing != DeAdoptRoutingRefuse || plan.Eligibility.Eligible ||
		!plan.Eligibility.AdoptOwned || plan.Eligibility.GateOn || len(plan.Clients) != 0 ||
		!strings.Contains(plan.RefusalReason, "cannot prove gate-OFF") ||
		!strings.Contains(plan.RefusalReason, "codex-cli") {
		t.Fatalf("unreadable-gate plan = %+v, want P0 REFUSE before snapshot classification", plan)
	}
}

func TestBuildDeAdoptPlanT2PartialGateAndStateRouting(t *testing.T) {
	t.Run("gate-on refuses before state routing", func(t *testing.T) {
		name := "deadoptt2gate"
		body := "[mcp_servers.mcphub-hub]\nurl = \"http://127.0.0.1:1/mcp\"\n"
		codexPath, _, _, _ := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStateAbsent,
			liveConfig:      body,
			manifestPresent: true,
		})
		before, err := os.ReadFile(codexPath)
		if err != nil {
			t.Fatal(err)
		}

		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil {
			t.Fatalf("BuildDeAdoptPlan: %v", err)
		}
		if plan.Routing != DeAdoptRoutingRefuse || !plan.Eligibility.GateOn ||
			!plan.Eligibility.AdoptOwned || plan.Eligibility.Eligible ||
			!strings.Contains(plan.RefusalReason, "gate OFF first") {
			t.Fatalf("gate-on plan = %+v, want actionable REFUSE", plan)
		}
		after, err := os.ReadFile(codexPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("gate-on plan mutated config: err=%v before=%q after=%q", err, before, after)
		}
	})

	t.Run("unreadable hub gate fails closed before state routing", func(t *testing.T) {
		name := "deadoptt2unreadablegate"
		body := "[mcp_servers.mcphub-hub]\nurl = \"http://127.0.0.1:1/mcp\"\ninvalid = [\n"
		codexPath, _, _, _ := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStateAbsent,
			liveConfig:      body,
			manifestPresent: true,
		})
		claudePath := filepath.Join(filepath.Dir(filepath.Dir(codexPath)), ".claude.json")
		if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{"ordinary":{"url":"http://127.0.0.1:2/mcp","type":"http"}}}`), 0o600); err != nil {
			t.Fatalf("seed cleanly gate-absent claude-code config: %v", err)
		}

		if gated := GatedOnClients(); slices.Contains(gated, "codex-cli") {
			t.Fatalf("GatedOnClients() = %v, want reset-port probe to keep skipping unreadable codex-cli", gated)
		}
		probe := ProbeHubGate()
		if len(probe.GatedOn) != 0 || !slices.Contains(probe.Unreadable, "codex-cli") ||
			slices.Contains(probe.Unreadable, "claude-code") {
			t.Fatalf("ProbeHubGate() = %+v, want codex-cli unreadable and cleanly gate-absent claude-code in neither set", probe)
		}

		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil {
			t.Fatalf("BuildDeAdoptPlan: %v", err)
		}
		if plan.Routing != DeAdoptRoutingRefuse || plan.Eligibility.Eligible ||
			!plan.Eligibility.AdoptOwned || plan.Eligibility.GateOn ||
			!strings.Contains(plan.RefusalReason, "unreadable") ||
			!strings.Contains(plan.RefusalReason, "codex-cli") {
			t.Fatalf("unreadable-gate plan = %+v, want fail-closed REFUSE naming codex-cli", plan)
		}
	})

	t.Run("un-stattable hub gate fails closed before state routing", func(t *testing.T) {
		name := "deadoptt2unstattablegate"
		codexPath, _, _, _ := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStateAbsent,
			liveConfig:      deAdoptHubConfig(name),
			manifestPresent: true,
		})

		root := filepath.Dir(filepath.Dir(filepath.Dir(codexPath)))
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))

		candidateHomes := []string{
			filepath.Join(root, "*"),
			filepath.Join(root, strings.Repeat("x", 300)),
		}
		loopHome := filepath.Join(root, "home-loop")
		if err := os.Symlink(loopHome, loopHome); err == nil {
			candidateHomes = append(candidateHomes, loopHome)
		}
		var unStattablePath string
		for _, candidateHome := range candidateHomes {
			t.Setenv("HOME", candidateHome)
			t.Setenv("USERPROFILE", candidateHome)
			candidatePath, err := clients.ConfigPathForName("codex-cli")
			if err != nil {
				t.Fatalf("resolve codex config path: %v", err)
			}
			if _, statErr := os.Stat(candidatePath); statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
				unStattablePath = candidatePath
				break
			}
		}
		if unStattablePath == "" {
			t.Fatal("no fixture config path produced a non-NotExist stat error")
		}
		absentPath, err := clients.ConfigPathForName("opencode")
		if err != nil {
			t.Fatalf("resolve opencode config path: %v", err)
		}
		if _, absentErr := os.Stat(absentPath); !errors.Is(absentErr, fs.ErrNotExist) {
			t.Fatalf("os.Stat(%q) error = %v, want NotExist", absentPath, absentErr)
		}

		probe := ProbeHubGate()
		if slices.Contains(probe.GatedOn, "codex-cli") ||
			!slices.Contains(probe.Unreadable, "codex-cli") ||
			slices.Contains(probe.GatedOn, "opencode") ||
			slices.Contains(probe.Unreadable, "opencode") {
			t.Fatalf("ProbeHubGate() = %+v, want un-stattable codex-cli unreadable and absent opencode in neither set", probe)
		}
		if gated := GatedOnClients(); slices.Contains(gated, "codex-cli") {
			t.Fatalf("GatedOnClients() = %v, want reset-port probe to keep excluding un-stattable codex-cli", gated)
		}

		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil {
			t.Fatalf("BuildDeAdoptPlan: %v", err)
		}
		if plan.Routing != DeAdoptRoutingRefuse || plan.Eligibility.Eligible ||
			!plan.Eligibility.AdoptOwned || plan.Eligibility.GateOn ||
			!strings.Contains(plan.RefusalReason, "unreadable") ||
			!strings.Contains(plan.RefusalReason, "codex-cli") {
			t.Fatalf("un-stattable-gate plan = %+v, want fail-closed REFUSE naming codex-cli", plan)
		}
	})

	t.Run("missing provenance refuses", func(t *testing.T) {
		name := "deadoptt2missing"
		codexPath, _, _ := setupAdoptTestEnv(t, name, "model = \"gpt-5\"\n")
		isolateDeAdoptPlannerAppData(t, codexPath)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil {
			t.Fatalf("BuildDeAdoptPlan: %v", err)
		}
		if plan.Routing != DeAdoptRoutingRefuse || plan.Eligibility.Eligible || !strings.Contains(plan.RefusalReason, "not adopt-owned") {
			t.Fatalf("missing-provenance plan = %+v, want REFUSE", plan)
		}
	})

	tests := []struct {
		name            string
		state           AdoptOperationState
		live            func(string) string
		manifestPresent bool
		wantRoute       DeAdoptRoutingVerdict
		wantReason      string
	}{
		{"adopted routes fresh", AdoptOperationStateAdopted, deAdoptHubConfig, true, DeAdoptRoutingFresh, ""},
		{"committed adopting live hub routes fresh", AdoptOperationStateAdopting, deAdoptHubConfig, true, DeAdoptRoutingFresh, ""},
		{"committed adopting manifest-present drift routes fresh", AdoptOperationStateAdopting, func(string) string { return "model = \"gpt-5\"\n" }, true, DeAdoptRoutingFresh, ""},
		{"pre-install crash orphan adopting refuses", AdoptOperationStateAdopting, func(string) string { return "model = \"gpt-5\"\n" }, false, DeAdoptRoutingRefuse, "orphan GC owns it"},
		{"closed refuses", AdoptOperationStateClosed, func(string) string { return "model = \"gpt-5\"\n" }, true, DeAdoptRoutingRefuse, "already de-adopted"},
		{"unknown state refuses", AdoptOperationState("unknown"), deAdoptHubConfig, true, DeAdoptRoutingRefuse, "unsupported"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("deadoptt2state%d", i)
			_, _, _, _ = setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
				state:           tc.state,
				originalState:   AdoptOriginalStateAbsent,
				liveConfig:      tc.live(name),
				manifestPresent: tc.manifestPresent,
			})
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatalf("BuildDeAdoptPlan: %v", err)
			}
			if plan.Routing != tc.wantRoute || (tc.wantReason != "" && !strings.Contains(plan.RefusalReason, tc.wantReason)) {
				t.Fatalf("routing plan = %+v, want route=%q reason containing %q", plan, tc.wantRoute, tc.wantReason)
			}
		})
	}
}

func TestBuildDeAdoptPlanManifestHashReadiness(t *testing.T) {
	name := "deadopthashready"
	_, _, _, _ = setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:                AdoptOperationStateAdopted,
		originalState:        AdoptOriginalStateAbsent,
		liveConfig:           deAdoptHubConfig(name),
		manifestPresent:      true,
		expectedHashOverride: strings.Repeat("f", 64),
	})
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatalf("BuildDeAdoptPlan: %v", err)
	}
	if plan.Routing != DeAdoptRoutingFresh || !plan.Manifest.Present || plan.Manifest.HashReady || plan.Manifest.ActualHash == "" {
		t.Fatalf("hash-mismatch readiness = %+v with route %q", plan.Manifest, plan.Routing)
	}
}

func TestDeAdoptPlanSecretFieldsAreNotSerialized(t *testing.T) {
	plan := &DeAdoptPlan{
		ManifestName: "wire-safe",
		provenance: &AdoptProvenanceRecord{
			RoutedSecretKeys: []string{"WIRE_UNSAFE_KEY_NAME"},
		},
		snapshotBytes: map[string][]byte{"codex-cli": []byte("literal-secret-value")},
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	for _, forbidden := range []string{"WIRE_UNSAFE_KEY_NAME", "literal-secret-value", "snapshotBytes", "provenance"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("serialized DeAdoptPlan leaked %q: %s", forbidden, raw)
		}
	}
}
