// internal/api/lsp_client_router_multilayer_test.go
//
// The MULTI-LAYER ADAPTER contract for the LSP router plan/apply family.
//
// mimocode is the one adapter in this repo whose GetEntry returns a MERGED
// multi-layer view (its own write target, the layers above it, the operator's
// config.json below it, and the ~/.claude.json MCP compatibility import) while
// its mutations touch ONLY the write target. clients.MCPEntry carries
// SourceBelowWriteTarget to mark exactly that split, and this family — which
// guards every write with a compare-and-swap on a captured pre-state and
// records that pre-state as the thing `--rollback` restores — is the family
// that MUST honour it.
//
// The defect these tests pin: `mcphub install --reconcile-mcp-front` on a host
// with mimocode AND claude-code installed failed with one `precondition`
// failure per manifest language, every run. Clients are applied in sorted
// order, so claude-code's rewrite of ~/.claude.json landed first and changed
// mimocode's merged view — the reconcile invalidated its own plan.
package api

import (
	"testing"

	"mcp-local-hub/internal/clients"
)

// The fake's belowWriteTarget map (lsp_client_router_test.go) stamps
// clients.MCPEntry.SourceBelowWriteTarget the way mimocode stamps an entry that
// resolves only from a layer the hub never writes. AddEntry stores the readback
// projection WITHOUT the flag, exactly like a real write landing in the write
// target.

// TestLSPRouterPlan_BelowWriteTargetEntryIsPlannedAsAbsentAndSurvivesASiblingWrite
// is the regression guard for the reconcile invalidating its own plan.
//
// Both halves matter and both fail without the fix:
//
//   - the recorded pre-state must be ABSENT, because the write target holds
//     nothing. Recording the merged URL would make `--rollback` WRITE that URL
//     into mimocode's own config, creating an entry that never existed there
//     and permanently shadowing the operator's lower/import layer.
//   - the compare-and-swap must still hold after the merged view MOVES, because
//     a sibling client's write inside the same run moves it. Comparing an object
//     the mutation does not write is the bug.
func TestLSPRouterPlan_BelowWriteTargetEntryIsPlannedAsAbsentAndSurvivesASiblingWrite(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})

	const language = "go"
	entryName := LSPRouterEntryName(language)
	const oldPort = 9125
	const newPort = 9137

	fake := newLSPRouterFakeClient(t, "mimocode", true)
	// The entry the hub can SEE but never wrote: it resolves from a layer below
	// the write target (for the real adapter, ~/.claude.json).
	fake.entries[entryName] = clients.MCPEntry{Name: entryName, URL: LSPRouterURL(oldPort, language)}
	fake.belowWriteTarget = map[string]bool{entryName: true}

	a := NewAPI()
	plan, err := a.PlanLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort:         newPort,
		Clients:         map[string]clients.Client{"mimocode": fake},
		ForceClientName: "mimocode",
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.CaptureFailures) != 0 {
		t.Fatalf("plan capture failures: %+v", plan.CaptureFailures)
	}
	if len(plan.Operations) != 1 {
		t.Fatalf("plan operations = %d, want 1: %+v", len(plan.Operations), plan.Operations)
	}
	op := plan.Operations[0]
	if op.Operation != "add" {
		t.Fatalf("planned operation = %q, want add", op.Operation)
	}
	if op.PreState.Present {
		t.Fatalf("pre-state recorded a below-write-target entry as this adapter's OWN state (%+v). The write target holds nothing, so rollback would CREATE an entry that never existed there and shadow the operator's lower layer forever", op.PreState)
	}

	// A sibling client's write inside the SAME run moves the merged view. In
	// production that is claude-code rewriting ~/.claude.json before mimocode's
	// turn (clients are applied in sorted order).
	fake.entries[entryName] = clients.MCPEntry{Name: entryName, URL: LSPRouterURL(newPort, language)}

	report, err := a.ApplyLSPRouterClientPlan(plan, LSPRouterApplyCallbacks{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("apply reported %d failure(s) after a sibling client moved the MERGED view: %+v — the compare-and-swap is reading an object the mutation does not write", len(report.Failed), report.Failed)
	}
	if len(report.Applied) != 1 {
		t.Fatalf("applied = %d, want 1: %+v", len(report.Applied), report.Applied)
	}
	if got := fake.entries[entryName].URL; got != LSPRouterURL(newPort, language) {
		t.Fatalf("write target entry URL = %q, want %q", got, LSPRouterURL(newPort, language))
	}
}

// TestLSPRouterLegacyCollection_SkipsCandidatesBelowTheWriteTarget pins the
// other half of the same contract. collectLegacyLSPEntriesToMigrate exists so
// the CAPTURED surface equals the MUTATED surface by construction; RemoveEntry
// only ever deletes the write target's own key, so a candidate that resolves
// from below it is not this reconcile's to migrate away.
func TestLSPRouterLegacyCollection_SkipsCandidatesBelowTheWriteTarget(t *testing.T) {
	const language = "go"
	const legacyName = "mcp-language-server-go-legacy"
	const legacyPort = 9410
	targetName := LSPRouterEntryName(language)

	regEntries := []WorkspaceEntry{{
		Language:      language,
		Port:          legacyPort,
		WorkspaceKey:  "abcd1234",
		ClientEntries: map[string]string{"mimocode": legacyName},
	}}
	legacyPorts := map[int]bool{legacyPort: true}
	legacyEntry := clients.MCPEntry{Name: legacyName, URL: "http://127.0.0.1:9410/mcp"}

	// Control: the same entry IN the write target is still collected, so the
	// test cannot pass by collecting nothing at all.
	own := newLSPRouterFakeClient(t, "mimocode", true)
	own.entries[legacyName] = legacyEntry
	found, readErrs := collectLegacyLSPEntriesToMigrate(own, regEntries, legacyPorts, language, "mimocode", targetName)
	if len(readErrs) != 0 {
		t.Fatalf("control read errors: %+v", readErrs)
	}
	if len(found) != 1 {
		t.Fatalf("control: collected %d legacy entries, want 1 (a write-target-owned legacy entry IS migrated away)", len(found))
	}

	below := newLSPRouterFakeClient(t, "mimocode", true)
	below.entries[legacyName] = legacyEntry
	below.belowWriteTarget = map[string]bool{legacyName: true}
	found, readErrs = collectLegacyLSPEntriesToMigrate(below, regEntries, legacyPorts, language, "mimocode", targetName)
	if len(readErrs) != 0 {
		t.Fatalf("read errors: %+v", readErrs)
	}
	if len(found) != 0 {
		t.Fatalf("collected %d below-write-target legacy entries, want 0: %+v — RemoveEntry cannot delete a layer the hub never writes, so planning it emits an operation that cannot take effect and records a pre-state whose inverse rollback cannot honour", len(found), found)
	}
}
