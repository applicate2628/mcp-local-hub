package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// required_secret_gate_test.go — the opt-in required-secret install gate.
//
// The default secret posture is OPTIONAL: an unset `secret:` env ref is omitted
// at spawn (secrets.ResolveMapBestEffort), so the server reports its own
// missing-key instead of mcphub failing the install. The EXCEPTION is a key
// listed in m.RequiredSecrets — that one is a BLOCKING admission finding when
// unset, so the install is REFUSED (the server hard-exits on startup without it).
// These tests pin the architect's six claims; they make the gate non-vacuous and
// guard the protected default-optional posture against regression.

// sunoStyleManifest is the in-process equivalent of the catalog suno row: a
// global stdio-bridge server whose ACEDATACLOUD_API_TOKEN env is a `secret:` ref
// and whose required_secrets opt-in lists that key. command "go" is on PATH under
// `go test` (mcphubShortName is stubbed to "go" by preparePreflightBinaryChecks),
// so the only blocking finding under test is the required-secret one.
func sunoStyleManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:            "suno",
		Kind:            config.KindGlobal,
		Transport:       config.TransportStdioBridge,
		Command:         "go",
		Daemons:         []config.DaemonSpec{{Name: "default", Port: 9314}},
		Env:             map[string]string{"ACEDATACLOUD_API_TOKEN": "secret:acedata_api_token"},
		RequiredSecrets: []string{"acedata_api_token"},
	}
}

// CLAIM 1 — paper-search with NO unpaywall_email + NO required_secrets stays
// READY (advisory unchanged). A `secret:` env ref WITHOUT a required_secrets
// opt-in is the default optional posture: it does NOT block install/readiness.
func TestRequiredSecretGate_OptionalSecretStaysReady(t *testing.T) {
	setupAdmissionParityTest(t) // LOCALAPPDATA/HOME → temp; vault genuinely absent

	m := &config.ServerManifest{
		Name:      "paper-search-mcp",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 9315}},
		Env:       map[string]string{"PAPER_SEARCH_MCP_UNPAYWALL_EMAIL": "secret:unpaywall_email"},
		// NO RequiredSecrets → optional.
	}

	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight blocked an OPTIONAL-secret manifest: %v (an unmarked secret must not block install)", err)
	}
	if !CheckServerReadiness(m).Ready {
		t.Fatal("CheckServerReadiness.Ready=false for an optional-secret manifest; an unset optional secret must stay advisory")
	}
	// The per-key secret row is present but advisory (Optional=true), and no
	// required-secret blocking finding is emitted.
	findings := AdmissionCheck(m, AdmissionScope{})
	if hasAdmissionFinding(findings, "required-secret") {
		t.Fatalf("optional-secret manifest produced a required-secret finding: %#v", findings)
	}
}

// CLAIM 2 — Preflight(suno) with an EMPTY vault returns a non-nil AdmissionError
// (ID required-secret) AND the gate writes NO supervisor-intent. Preflight runs
// BEFORE installPlanCore (install.go step 2 before step 4), so a non-nil
// AdmissionError structurally aborts the install before any manifest/intent/
// client-config write — asserted concretely here by a clean state dir after the
// gate evaluation.
func TestRequiredSecretGate_BlocksInstallWithEmptyVault(t *testing.T) {
	setupAdmissionParityTest(t) // vault genuinely absent
	stateDir := daemonIntentTestHelper(t)

	m := sunoStyleManifest()

	err := Preflight(m, "")
	if err == nil {
		t.Fatal("Preflight(suno, empty vault): want required-secret block, got nil")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) || ae.ID != "required-secret" {
		t.Fatalf("error = %v, want *AdmissionError{ID: required-secret}", err)
	}
	// The blocking gate must touch no state — no supervisor-intent committed.
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("no intent must be written when the required-secret gate blocks; stat err = %v", statErr)
	}
	// The blocking finding's Reason names the KEY ONLY, never a value (redaction).
	f, ok := admissionFindingByID(AdmissionCheck(m, AdmissionScope{}), "required-secret")
	if !ok {
		t.Fatal("required-secret finding missing from AdmissionCheck")
	}
	if f.Optional {
		t.Fatal("required-secret finding is Optional; it must be BLOCKING")
	}
	if f.Name != "secret: acedata_api_token" {
		t.Errorf("finding Name = %q, want %q", f.Name, "secret: acedata_api_token")
	}
}

// CLAIM 3 — a suno manifest WITH the secret SET emits no blocking finding and
// installs (Preflight passes / Ready=true). The vault is seeded with the
// required key, so the gate is satisfied.
func TestRequiredSecretGate_SecretSetNoBlock(t *testing.T) {
	// seedDefaultSecretForTest redirects LOCALAPPDATA/XDG_DATA_HOME to a temp dir
	// and seeds the vault with the key. preparePreflightBinaryChecks stubs the
	// canonical mcphub binary + mcphubShortName→"go" so the launcher checks pass.
	seedDefaultSecretForTest(t, "acedata_api_token", "live-token-value")
	preparePreflightBinaryChecks(t)

	m := sunoStyleManifest()

	if hasAdmissionFinding(AdmissionCheck(m, AdmissionScope{}), "required-secret") {
		t.Fatalf("required-secret finding emitted when the secret IS set: %#v", AdmissionCheck(m, AdmissionScope{}))
	}
	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight blocked a suno manifest whose required secret IS set: %v", err)
	}
}

// codex finding 2 — a required key PRESENT in the vault but holding an EMPTY
// STRING value must be treated as UNRESOLVED → the blocking required-secret
// finding still fires. vault.Get returns ("", nil) for a present-but-blank key,
// so a gate that trusted gerr==nil alone would let a crash-looping install
// through (the server still hard-exits on an empty ACEDATACLOUD_API_TOKEN). This
// pins the value-check, not just the error-check.
func TestRequiredSecretGate_EmptyValueBlocks(t *testing.T) {
	// Seed the required key with an EMPTY value (present-but-blank).
	seedDefaultSecretForTest(t, "acedata_api_token", "")
	preparePreflightBinaryChecks(t)

	m := sunoStyleManifest()

	findings := AdmissionCheck(m, AdmissionScope{})
	f, ok := admissionFindingByID(findings, "required-secret")
	if !ok {
		t.Fatalf("required-secret finding missing when the key is present-but-EMPTY: %#v", findings)
	}
	if f.Optional {
		t.Fatal("required-secret finding for an empty value is Optional; it must be BLOCKING")
	}
	if f.Name != "secret: acedata_api_token" {
		t.Errorf("finding Name = %q, want %q", f.Name, "secret: acedata_api_token")
	}
	if err := Preflight(m, ""); err == nil {
		t.Fatal("Preflight passed with a present-but-EMPTY required secret; an empty token must block (server crash-loops on it)")
	}
	// Parity: readiness must also see the empty value as blocking (Ready=false).
	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatal("readiness Ready=true with a present-but-EMPTY required secret; want false")
	}
	// Non-vacuous parity at the ROW level: assert the SPECIFIC required-secret row
	// is OK=false, not merely that overall Ready is false (which some other failing
	// requirement could satisfy by accident). Before the requiredSecretResolved
	// collapse, readiness used checkSecretRefs → resolver.Resolve → vault.Get, which
	// returns ("",nil) for a present-but-blank key, so this row was painted OK=true
	// (GREEN) while admission BLOCKED — the exact divergence (codex finding 2). Now
	// both call the single requiredSecretResolved owner, so a blank value blocks both.
	var secretRow *ReadinessRequirement
	for i := range rep.Requirements {
		if rep.Requirements[i].Name == "secret: acedata_api_token" {
			secretRow = &rep.Requirements[i]
			break
		}
	}
	if secretRow == nil {
		t.Fatalf("no secret row surfaced for the required key; requirements=%#v", rep.Requirements)
	}
	if secretRow.OK {
		t.Errorf("required-secret readiness row OK=true with a present-but-EMPTY value; admission blocks it, so readiness must too (parity)")
	}
	if secretRow.Optional {
		t.Errorf("required-secret readiness row Optional=true; a required secret must render blocking (RED)")
	}
}

// TestRequiredSecretGate_WhitespaceValueBlocks is the sibling: a whitespace-only
// value (spaces / tabs) is just as unusable as empty — strings.TrimSpace makes
// the gate treat it as unresolved too.
func TestRequiredSecretGate_WhitespaceValueBlocks(t *testing.T) {
	seedDefaultSecretForTest(t, "acedata_api_token", "   \t  ")
	preparePreflightBinaryChecks(t)

	m := sunoStyleManifest()
	if !hasAdmissionFinding(AdmissionCheck(m, AdmissionScope{}), "required-secret") {
		t.Fatalf("required-secret finding missing for a whitespace-only value: %#v", AdmissionCheck(m, AdmissionScope{}))
	}
	if err := Preflight(m, ""); err == nil {
		t.Fatal("Preflight passed with a whitespace-only required secret; want block")
	}
	// Readiness row parity: the whitespace-only value must paint the row OK=false
	// (the shared requiredSecretResolved owner TrimSpaces it to unresolved).
	rep := CheckServerReadiness(m)
	for i := range rep.Requirements {
		if rep.Requirements[i].Name == "secret: acedata_api_token" && rep.Requirements[i].OK {
			t.Errorf("required-secret readiness row OK=true for a whitespace-only value; want false (parity with admission block)")
		}
	}
}

// CLAIM 4 — the schema is ADDITIVE: a manifest WITH required_secrets round-trips
// through ParseManifest, and a manifest WITHOUT it parses byte-identically (nil
// slice). The existing manifest parse corpus is unaffected (covered by the
// config package tests); this pins the new field's round-trip directly.
func TestRequiredSecretGate_ManifestRoundTrip(t *testing.T) {
	withYAML := `
name: suno
kind: global
transport: stdio-bridge
command: uvx
required_secrets: [acedata_api_token]
env:
  ACEDATACLOUD_API_TOKEN: secret:acedata_api_token
daemons:
  - name: default
    port: 9314
`
	m, err := config.ParseManifest(strings.NewReader(withYAML))
	if err != nil {
		t.Fatalf("ParseManifest(with required_secrets): %v", err)
	}
	if len(m.RequiredSecrets) != 1 || m.RequiredSecrets[0] != "acedata_api_token" {
		t.Fatalf("RequiredSecrets = %v, want [acedata_api_token]", m.RequiredSecrets)
	}

	withoutYAML := `
name: plain
kind: global
transport: stdio-bridge
command: uvx
daemons:
  - name: default
    port: 9316
`
	m2, err := config.ParseManifest(strings.NewReader(withoutYAML))
	if err != nil {
		t.Fatalf("ParseManifest(without required_secrets): %v", err)
	}
	if m2.RequiredSecrets != nil {
		t.Fatalf("absent required_secrets parsed non-nil: %v", m2.RequiredSecrets)
	}
}

// CLAIM 5 — readiness's blocking classification derives from the SAME
// requiredSecretSet owner the admission finding consults. With an empty vault the
// suno manifest's secret row is BLOCKING (Optional=false, OK=false) and flips
// Ready=false — in PARITY with the AdmissionCheck required-secret finding. The
// admission↔readiness verdicts must AGREE (the project's parity guard).
func TestRequiredSecretGate_ReadinessBlockingParity(t *testing.T) {
	setupAdmissionParityTest(t) // vault genuinely absent

	m := sunoStyleManifest()

	// Admission: required-secret is a blocking finding → Preflight fails.
	preflightOK := Preflight(m, "") == nil
	// Readiness: the secret row is blocking → Ready=false.
	rep := CheckServerReadiness(m)
	if preflightOK != rep.Ready {
		t.Fatalf("admission↔readiness parity broken: Preflight==nil is %t, readiness.Ready is %t", preflightOK, rep.Ready)
	}
	if rep.Ready {
		t.Fatal("readiness Ready=true with an unset REQUIRED secret; the required-secret gate must block")
	}
	// The specific secret row must be BLOCKING (Optional=false), not advisory.
	var secretRow *ReadinessRequirement
	for i := range rep.Requirements {
		if rep.Requirements[i].Name == "secret: acedata_api_token" {
			secretRow = &rep.Requirements[i]
			break
		}
	}
	if secretRow == nil {
		t.Fatalf("no secret row surfaced for the required key; requirements=%#v", rep.Requirements)
	}
	if secretRow.Optional {
		t.Errorf("required-secret readiness row is Optional=true; a required secret must render blocking (RED)")
	}
	if secretRow.OK {
		t.Errorf("required-secret readiness row OK=true with an unset key; want false")
	}
}

// TestRequiredSecretGate_ReadinessReadyWhenSet is the parity companion: with the
// secret SET, the required row is OK and Ready=true (the gate is satisfied).
func TestRequiredSecretGate_ReadinessReadyWhenSet(t *testing.T) {
	seedDefaultSecretForTest(t, "acedata_api_token", "live-token-value")
	preparePreflightBinaryChecks(t)

	m := sunoStyleManifest()
	rep := CheckServerReadiness(m)
	if !rep.Ready {
		t.Fatalf("readiness Ready=false with the required secret SET; requirements=%#v", rep.Requirements)
	}
	for i := range rep.Requirements {
		if rep.Requirements[i].Name == "secret: acedata_api_token" && !rep.Requirements[i].OK {
			t.Errorf("required-secret row OK=false with the secret set: %#v", rep.Requirements[i])
		}
	}
}

// --- catalog-side authoring guard + forward-compat gate ---

// TestCatalogRequiredSecrets_AuthoringGuardRejectsUnbackedKey proves the
// catalog-authoring guard: a required_secrets key with NO matching secret:<key>
// env ref is rejected at parse, so a typo key (or a stale key after an env
// rename) cannot silently un-gate the row or block on a phantom credential.
func TestCatalogRequiredSecrets_AuthoringGuardRejectsUnbackedKey(t *testing.T) {
	raw := `{
  "schema_version": "2",
  "entries": [
    {
      "id": "suno",
      "name": "Suno",
      "transport": "stdio",
      "command": "uvx",
      "args": ["--from", "mcp-suno", "mcp-suno"],
      "env": {"ACEDATACLOUD_API_TOKEN": "secret:acedata_api_token"},
      "required_secrets": ["typo_key"]
    }
  ]
}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil {
		t.Fatal("catalog with an unbacked required_secrets key parsed; want authoring-guard rejection")
	}
	if !strings.Contains(err.Error(), "typo_key") || !strings.Contains(err.Error(), "no matching secret:") {
		t.Fatalf("error = %q, want it to name the unbacked key + the missing secret: ref", err.Error())
	}
}

// TestCatalogRequiredSecrets_AuthoringGuardAcceptsBackedKey proves the matched
// case parses cleanly (the suno shape).
func TestCatalogRequiredSecrets_AuthoringGuardAcceptsBackedKey(t *testing.T) {
	raw := `{
  "schema_version": "2",
  "entries": [
    {
      "id": "suno",
      "name": "Suno",
      "transport": "stdio",
      "command": "uvx",
      "args": ["--from", "mcp-suno", "mcp-suno"],
      "env": {"ACEDATACLOUD_API_TOKEN": "secret:acedata_api_token"},
      "required_secrets": ["acedata_api_token"]
    }
  ]
}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("catalog with a backed required_secrets key failed to parse: %v", err)
	}
	if len(cat.Entries) != 1 || len(cat.Entries[0].RequiredSecrets) != 1 || cat.Entries[0].RequiredSecrets[0] != "acedata_api_token" {
		t.Fatalf("required_secrets not parsed: %#v", cat.Entries)
	}
}

// codex finding 3 — the catalog authoring guard must reject a required_secrets
// key whose NAME is not settable (the same shape `mcphub secrets set` / POST
// /api/secrets accepts, owned by secrets.ValidateSettableKeyName). A key the
// operator literally cannot `set` (e.g. `bad-key` — hyphen — or a digit-leading
// name) would gate the projected install FOREVER. The env secret: ref backs the
// key so the back-env check passes, isolating the settable-key-name guard as the
// rejector. The r2 manifest gate already reuses the SAME owner; this proves the
// catalog author guard rejects one layer earlier, at catalog-author time.
func TestCatalogRequiredSecrets_AuthoringGuardRejectsUnsettableKeyName(t *testing.T) {
	raw := `{
  "schema_version": "2",
  "entries": [
    {
      "id": "suno",
      "name": "Suno",
      "transport": "stdio",
      "command": "uvx",
      "args": ["--from", "mcp-suno", "mcp-suno"],
      "env": {"ACEDATACLOUD_API_TOKEN": "secret:bad-key"},
      "required_secrets": ["bad-key"]
    }
  ]
}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil {
		t.Fatal("catalog with an un-settable required_secrets key name parsed; want settable-key-name rejection")
	}
	if !strings.Contains(err.Error(), "bad-key") || !strings.Contains(err.Error(), "settable") {
		t.Fatalf("error = %q, want it to name the un-settable key + the settable-key-name restriction", err.Error())
	}
}

// TestCatalogRequiredSecrets_HTTPEntryRejected proves the codex finding-3 guard:
// required_secrets on a transport:"http" entry is REJECTED at catalog parse.
// BOTH http install paths ignore required_secrets (generateRemoteHTTPDraft does
// not project it; the remote-http install never gates on it), so declaring it on
// an http entry is meaningless/misleading — a gate that does not exist. The env
// secret: ref backs the key (so the back-env check passes), isolating the
// transport guard as the rejector.
func TestCatalogRequiredSecrets_HTTPEntryRejected(t *testing.T) {
	raw := `{
  "schema_version": "2",
  "entries": [
    {
      "id": "remote-thing",
      "name": "Remote Thing",
      "transport": "http",
      "url": "https://mcp.example.com/mcp",
      "env": {"SOME_TOKEN": "secret:some_token"},
      "required_secrets": ["some_token"]
    }
  ]
}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil {
		t.Fatal("catalog with required_secrets on an http entry parsed; want transport-guard rejection")
	}
	if !strings.Contains(err.Error(), "required_secrets") || !strings.Contains(err.Error(), "http") {
		t.Fatalf("error = %q, want it to name required_secrets + the http-transport restriction", err.Error())
	}
}

// TestCatalogRequiredSecrets_V1Rejects proves required_secrets is gated to
// schema_version 2 (newCatalogFieldKeys): a v1 catalog carrying the key is
// rejected by the forward-compat gate, so a frozen v1 catalog can never carry it.
func TestCatalogRequiredSecrets_V1Rejects(t *testing.T) {
	raw := `{
  "schema_version": "1",
  "entries": [
    {
      "id": "suno",
      "name": "Suno",
      "transport": "stdio",
      "command": "uvx",
      "args": ["x"],
      "env": {"ACEDATACLOUD_API_TOKEN": "secret:acedata_api_token"},
      "required_secrets": ["acedata_api_token"]
    }
  ]
}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil {
		t.Fatal("v1 catalog with required_secrets parsed; want forward-compat rejection")
	}
	if !strings.Contains(err.Error(), "required_secrets") || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("error = %q, want it to name required_secrets + the schema_version requirement", err.Error())
	}
}
