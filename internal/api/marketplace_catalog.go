// internal/api/marketplace_catalog.go — G5 Marketplace catalog parser.
//
// Spec: docs/superpowers/specs/2026-05-12-g5-marketplace-draft-import-design.md
// §"Registry source" + §"Threat model" + §"Acceptance criteria".

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/urlredact"
)

// MarketplaceCatalogSchemaVersion is the BASELINE catalog schema version a
// freshly-drafted/written catalog declares (the minimum this build understands).
// MarketplaceCatalogSchemaVersionV2 is the version that gained the additive D-2/D-3
// entry metadata (vendored_source / availability / install_probe). Both are
// ACCEPTED on read (marketplaceSchemaVersionAccepted), so an OLDER released client
// reading a v1 catalog and a newer client reading a v2 catalog both parse — but the
// new entry fields are gated to v2 (newCatalogFieldsRequireV2) so a v1 catalog can
// NEVER carry them, which is what keeps the additive metadata rollout from breaking
// older v1-only clients (codex r6 finding 2).
const (
	MarketplaceCatalogSchemaVersion   = "1"
	MarketplaceCatalogSchemaVersionV2 = "2"
)

// marketplaceSchemaVersionAccepted is the SINGLE OWNER of "which catalog
// schema_version values this build parses", consumed by both the catalog parser
// (validateMarketplaceCatalog) and the on-disk cache validator (readMarketplaceCache)
// so the two read paths can never diverge on the accepted set. It accepts the
// baseline v1 AND the additive-metadata v2.
func marketplaceSchemaVersionAccepted(v string) bool {
	return v == MarketplaceCatalogSchemaVersion || v == MarketplaceCatalogSchemaVersionV2
}

type MarketplaceCatalog struct {
	SchemaVersion string             `json:"schema_version"`
	GeneratedAt   string             `json:"generated_at,omitempty"`
	Entries       []MarketplaceEntry `json:"entries"`
}

type MarketplaceEntry struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Summary    string            `json:"summary,omitempty"`
	Homepage   string            `json:"homepage,omitempty"`
	ReadmeURL  string            `json:"readme_url,omitempty"`
	Transport  string            `json:"transport"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Categories []string          `json:"categories,omitempty"`
	License    string            `json:"license,omitempty"`

	// VendoredSource (D-2, Tier-0) mirrors config.VendoredSource but JSON-tagged
	// for catalog.json. ADDITIVE + OPTIONAL: the current catalog entries omit it,
	// so they parse byte-identically (pointer + omitempty). It is projected into
	// the drafted hub-install manifest by generateCommandDraft so the persisted
	// manifest's Validate()/readiness gate can see the pin post-install.
	VendoredSource *CatalogVendoredSource `json:"vendored_source,omitempty"`

	// Availability (D-3, Tier-0) mirrors config.ServerManifest.Availability. The
	// GUI Catalog DTO surfaces it read-only so the frontend can grey a watch row.
	Availability string `json:"availability,omitempty"`

	// InstallProbe (D-3, Tier-0) mirrors config.AvailabilityProbe, JSON-tagged
	// for catalog.json. Projected into the drafted manifest alongside
	// Availability so the post-install readiness gate can re-evaluate the probe.
	InstallProbe *CatalogAvailabilityProbe `json:"install_probe,omitempty"`

	// RequiredSecrets mirrors config.ServerManifest.RequiredSecrets, JSON-tagged
	// for catalog.json. The OPT-IN list of vault KEYS that MUST be set before this
	// row installs (the server hard-exits on startup without them). Each key MUST
	// appear as a `secret:<key>` value in Env (catalog-authoring guard in
	// validateCatalogVendoredAndAvailability — a typo key can't silently un-gate).
	// Gated to schema_version 2 (newCatalogFieldKeys), and projected into the
	// drafted stdio manifest by generateCommandDraft so the persisted manifest's
	// AdmissionCheck install gate sees it.
	//
	// Decision: work-items/decisions/2026-06-24-required-secret-install-gate.md
	RequiredSecrets []string `json:"required_secrets,omitempty"`
}

// CatalogVendoredSource is the catalog-entry (JSON) mirror of
// config.VendoredSource (D-2, Tier-0). Metadata-only; see the config type for
// field semantics. Decision: work-items/decisions/2026-06-23-d2-vendored-source.md
type CatalogVendoredSource struct {
	Repo          string `json:"repo,omitempty"`
	PinnedRef     string `json:"pinned_ref,omitempty"`
	InstallCmd    string `json:"install_cmd,omitempty"`
	RunCmd        string `json:"run_cmd,omitempty"`
	LicenseStatus string `json:"license_status,omitempty"`
}

// CatalogAvailabilityProbe is the catalog-entry (JSON) mirror of
// config.AvailabilityProbe (D-3, Tier-0). file_globs[] is the OPT-IN glob-pattern
// field; files[] is the LITERAL-path field (stat'd verbatim, never globbed) — see
// the config type for the split. Decision:
// work-items/decisions/2026-06-23-d3-availability-probe.md
type CatalogAvailabilityProbe struct {
	Binaries  []string `json:"binaries,omitempty"`
	Files     []string `json:"files,omitempty"`
	FileGlobs []string `json:"file_globs,omitempty"`
}

// AvailabilityAdmissionEntry runs the shared D-3 availability admission gate
// over a marketplace catalog entry, mapping the entry's JSON-mirror
// availability / install_probe onto the SAME availabilityProbeFinding owner the
// manifest gate uses. The marketplace one-click install handler calls this once,
// immediately after LoadEntry and BEFORE both the hub-mode and direct-mode
// dispatch, so an inert (watch / disabled-until-probe) entry whose host-app
// install-probe has not passed is refused with a typed error BEFORE any client
// config write or hub daemon install — the direct path never reaches the
// manifest AdmissionCheck, and gating at the entry keeps both modes consistent.
// ADDITIVE: an entry with empty availability + no install_probe (every current
// catalog row) returns nil immediately. Returns nil for a nil entry.
func AvailabilityAdmissionEntry(e *MarketplaceEntry) error {
	if e == nil {
		return nil
	}
	return AvailabilityAdmissionFields(e.Availability, catalogProbeToConfig(e.InstallProbe), e.ID)
}

// catalogProbeToConfig maps the JSON-mirror CatalogAvailabilityProbe onto the
// config.AvailabilityProbe the readiness dry-run consumes, so the marketplace
// entry gate evaluates the SAME probe primitives (binaryAvailable /
// entryScriptStatus) as the post-install manifest gate — one detector, no drift.
func catalogProbeToConfig(p *CatalogAvailabilityProbe) *config.AvailabilityProbe {
	if p == nil {
		return nil
	}
	return &config.AvailabilityProbe{
		Binaries:  append([]string(nil), p.Binaries...),
		Files:     append([]string(nil), p.Files...),
		FileGlobs: append([]string(nil), p.FileGlobs...),
	}
}

// ParseMarketplaceCatalog decodes raw JSON. Returns the first error
// per spec §"Threat model" (malformed catalogs reject wholesale,
// never partial-accept).
//
// codex r5 P2 closure: rejects trailing bytes after the top-level
// JSON object so a valid catalog appended with garbage (or a second
// object) cannot be silently accepted. Mirrors the registry-source
// "single canonical document" contract from §"Threat model".
func ParseMarketplaceCatalog(raw []byte) (*MarketplaceCatalog, error) {
	var cat MarketplaceCatalog
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode catalog: trailing bytes after top-level JSON object")
		}
		return nil, fmt.Errorf("decode catalog: trailing bytes after top-level JSON object: %w", err)
	}
	// Re-decode the SAME bytes into a parallel raw key map per entry so the
	// forward-compat gate can reject on KEY PRESENCE (codex r7 P2) — a typed
	// decode of `availability:""` / `vendored_source:null` leaves the field
	// empty/nil and is indistinguishable from the key being absent.
	presence, err := catalogEntryNewKeyPresence(raw)
	if err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if err := validateMarketplaceCatalog(&cat, presence); err != nil {
		return nil, err
	}
	return &cat, nil
}

// newCatalogFieldKeys are the additive D-2/D-3 entry keys gated to
// schema_version 2. The forward-compat gate rejects any of them present in a
// schema_version < 2 catalog, regardless of value (present-empty, present-null,
// or populated all count) — so an older v1-only client whose
// DisallowUnknownFields decoder rejects the WHOLE catalog on the bare KEY never
// has to face a v1 catalog carrying it.
var newCatalogFieldKeys = []string{"vendored_source", "availability", "install_probe", "required_secrets"}

// catalogEntryNewKeyPresence re-decodes the catalog body's `entries` array into
// a parallel per-entry map[string]json.RawMessage from the SAME bytes the typed
// MarketplaceCatalog decode consumed, so key-PRESENCE (not decoded value) drives
// the forward-compat gate. `encoding/json` preserves array element order, so
// presence[i] aligns with cat.Entries[i]. Returns a nil-length slice when the
// catalog has no entries. A nil entry map (entry was not a JSON object) leaves
// the gate to fall back to its value-based inner guard.
func catalogEntryNewKeyPresence(raw []byte) ([]map[string]json.RawMessage, error) {
	var shell struct {
		Entries []json.RawMessage `json:"entries"`
	}
	// No DisallowUnknownFields here: the typed decode already enforced the
	// no-unknown-keys contract; this pass only needs the raw entry objects.
	if err := json.Unmarshal(raw, &shell); err != nil {
		return nil, fmt.Errorf("re-decode entries for key-presence check: %w", err)
	}
	presence := make([]map[string]json.RawMessage, len(shell.Entries))
	for i, rawEntry := range shell.Entries {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rawEntry, &m); err != nil {
			// A non-object entry leaves m nil; the typed decode would have
			// already failed in that case, so this is defensive. Leave nil and
			// let the value-based inner guard run.
			continue
		}
		presence[i] = m
	}
	return presence, nil
}

// entryNewKeyPresence returns the raw key map for entry index i, or nil when no
// presence info is available (cache path that pre-parsed the typed struct, or a
// non-object entry). A nil map makes newCatalogFieldsRequireV2 fall back to its
// value-based inner guard.
func entryNewKeyPresence(presence []map[string]json.RawMessage, i int) map[string]json.RawMessage {
	if i < 0 || i >= len(presence) {
		return nil
	}
	return presence[i]
}

func validateMarketplaceCatalog(cat *MarketplaceCatalog, presence []map[string]json.RawMessage) error {
	if !marketplaceSchemaVersionAccepted(cat.SchemaVersion) {
		return fmt.Errorf("schema_version %q: this build only accepts %q or %q",
			cat.SchemaVersion, MarketplaceCatalogSchemaVersion, MarketplaceCatalogSchemaVersionV2)
	}
	seen := map[string]bool{}
	for i := range cat.Entries {
		e := &cat.Entries[i]
		if err := validateMarketplaceEntry(e, cat.SchemaVersion, entryNewKeyPresence(presence, i)); err != nil {
			return fmt.Errorf("entry %d (id=%q): %w", i, e.ID, err)
		}
		if seen[e.ID] {
			return fmt.Errorf("entry %d: duplicate id %q", i, e.ID)
		}
		seen[e.ID] = true
	}
	return nil
}

// newCatalogFieldsRequireV2 is the forward-compat gate (codex r6 finding 2,
// strengthened r7 P2): the additive D-2/D-3 entry fields (vendored_source /
// availability / install_probe) were introduced at schema_version 2, so an entry
// that carries ANY of those KEYS inside a schema_version < 2 catalog is REJECTED
// — naming the offending key. This makes the metadata rollout truly additive: a
// v1 catalog can never carry the new keys, so an OLDER v1-only client (whose
// DisallowUnknownFields decoder would reject the WHOLE catalog on an unknown KEY,
// regardless of value) never has to face a v1 catalog with them. Tier-1 publishes
// the new fields under schema_version 2. A v2 catalog imposes no such restriction.
//
// The AUTHORITATIVE check is KEY PRESENCE in the raw entry JSON (`presence`),
// because a typed decode of `availability:""` / `vendored_source:null` /
// `install_probe:null` leaves the field empty/nil — indistinguishable from the
// key being absent — yet the bare key still breaks a v1-only DisallowUnknownFields
// client. The value-based check below is kept as a redundant inner guard for the
// cache path, which validates an already-typed struct with no raw bytes (presence
// nil): a populated field is still caught there.
func newCatalogFieldsRequireV2(e *MarketplaceEntry, schemaVersion string, presence map[string]json.RawMessage) error {
	if schemaVersion == MarketplaceCatalogSchemaVersionV2 {
		return nil
	}
	// Authoritative: reject on raw KEY PRESENCE (present-empty / present-null /
	// populated all count). presence is nil on the cache path; the value guard
	// below covers it.
	for _, key := range newCatalogFieldKeys {
		if _, ok := presence[key]; ok {
			return fmt.Errorf("%s requires catalog schema_version %q", key, MarketplaceCatalogSchemaVersionV2)
		}
	}
	// Redundant inner guard: a populated decoded value is also rejected (covers
	// the cache path where presence is nil).
	switch {
	case e.VendoredSource != nil:
		return fmt.Errorf("vendored_source requires catalog schema_version %q", MarketplaceCatalogSchemaVersionV2)
	case e.Availability != "":
		return fmt.Errorf("availability requires catalog schema_version %q", MarketplaceCatalogSchemaVersionV2)
	case e.InstallProbe != nil:
		return fmt.Errorf("install_probe requires catalog schema_version %q", MarketplaceCatalogSchemaVersionV2)
	case len(e.RequiredSecrets) > 0:
		return fmt.Errorf("required_secrets requires catalog schema_version %q", MarketplaceCatalogSchemaVersionV2)
	}
	return nil
}

func validateMarketplaceEntry(e *MarketplaceEntry, schemaVersion string, presence map[string]json.RawMessage) error {
	if e.ID == "" {
		return fmt.Errorf("missing id")
	}
	if e.Name == "" {
		return fmt.Errorf("missing name")
	}
	// codex r1 P2 closure: entry id must pass CheckManifestName so
	// the projected draft can be accepted by `mcphub manifest create`
	// later — including the reserved-aggregate-name guard from r15.
	if err := CheckManifestName(e.ID); err != nil {
		return fmt.Errorf("id %q fails manifest-name gate: %w", e.ID, err)
	}
	switch e.Transport {
	case "stdio", "native-http":
		if e.Command == "" {
			return fmt.Errorf("%s entry must declare command", e.Transport)
		}
	case "http":
		if e.URL == "" {
			return fmt.Errorf("http entry must declare url")
		}
		if _, err := parseMarketplacePublicHTTPSURL(e.URL); err != nil {
			return fmt.Errorf("http entry url must be valid public https:// without embedded credentials (got %q): %w", marketplaceCatalogURLForError(e.URL), err)
		}
	default:
		return fmt.Errorf("unknown transport %q (want stdio, native-http, or http)", e.Transport)
	}
	// Forward-compat: the additive D-2/D-3 metadata fields are gated to
	// schema_version 2 so a v1 catalog can never carry keys an older v1-only
	// client would choke on (codex r6 finding 2).
	if err := newCatalogFieldsRequireV2(e, schemaVersion, presence); err != nil {
		return err
	}
	if err := validateCatalogVendoredAndAvailability(e); err != nil {
		return err
	}
	return nil
}

// validateCatalogVendoredAndAvailability is the catalog-side defense-in-depth
// mirror of config.ServerManifest.validateVendoredAndAvailability (D-2 + D-3,
// Tier-0). A hostile or malformed registry must not be able to ship an unpinned
// vendored entry or an availability typo that would only be caught later, after
// projection, by the manifest gate. The manifest gate remains the authoritative
// one post-projection; this catches the same shape one layer earlier. ADDITIVE:
// the current catalog entries omit all the new fields, so this short-circuits.
func validateCatalogVendoredAndAvailability(e *MarketplaceEntry) error {
	if vs := e.VendoredSource; vs != nil {
		ref := strings.TrimSpace(vs.PinnedRef)
		if ref == "" {
			return fmt.Errorf("vendored_source requires a non-empty pinned_ref (pin to a 40-hex SHA or tag; a moving branch like main/HEAD is rejected)")
		}
		// config.IsMovingGitRef is the single owner of the moving-branch
		// predicate: it normalizes a branch-qualified ref ("refs/heads/main",
		// "refs/remotes/origin/main") to its bare name before the check, so a
		// fully-qualified branch pin cannot slip past. "refs/tags/<tag>" is an
		// immutable tag and passes. Reusing the config owner keeps this
		// defense-in-depth mirror from drifting from the manifest gate.
		if config.IsMovingGitRef(ref) {
			return fmt.Errorf("vendored_source requires a non-empty pinned_ref (pin to a 40-hex SHA or tag; a moving branch like main/HEAD is rejected)")
		}
		switch vs.LicenseStatus {
		case "", "confirmed", "pending", "unknown":
		default:
			return fmt.Errorf("vendored_source.license_status %q is not one of confirmed|pending|unknown", vs.LicenseStatus)
		}
	}
	switch e.Availability {
	case "", "ready", "watch", "disabled-until-probe":
	default:
		return fmt.Errorf("availability %q must be ready|watch|disabled-until-probe", e.Availability)
	}
	inert := e.Availability == "watch" || e.Availability == "disabled-until-probe"
	if e.InstallProbe != nil && !inert {
		return fmt.Errorf("install_probe is only meaningful with availability=watch|disabled-until-probe")
	}
	if inert {
		if e.InstallProbe == nil || (len(e.InstallProbe.Binaries) == 0 && len(e.InstallProbe.Files) == 0 && len(e.InstallProbe.FileGlobs) == 0) {
			return fmt.Errorf("availability=%q requires a non-empty install_probe (binaries, files, or file_globs)", e.Availability)
		}
		// Each declared probe value must be a non-empty token — same A7 rule the
		// manifest gate enforces, run through the SAME config owner (mapping the
		// JSON-mirror probe onto config.AvailabilityProbe) so a `binaries: [""]`
		// row is rejected at catalog parse rather than becoming a permanently
		// disabled row whose runtime probe looks up an empty name.
		if err := config.ValidateProbeValuesNonEmpty(catalogProbeToConfig(e.InstallProbe), "catalog entry"); err != nil {
			return err
		}
	}
	// required_secrets authoring guard: each named vault key MUST appear as a
	// `secret:<key>` value in the entry's Env. A required-secret key that has no
	// matching env ref would gate the install on a credential the projected
	// manifest never actually consumes — so a typo in required_secrets (or a stale
	// key left after an env rename) cannot silently UN-gate the row or block on a
	// phantom credential. The reverse direction is intentionally NOT required: a
	// `secret:` env ref WITHOUT a required_secrets entry stays the default
	// optional-secret posture (paper-search's unpaywall_email), which is the whole
	// point of the opt-in gate. Empty entries are rejected so a stray "" cannot
	// become a permanently-unblockable phantom requirement.
	if len(e.RequiredSecrets) > 0 {
		// required_secrets is a LOCAL-STDIO concern only (codex finding 3): the
		// install gate that blocks on a missing required secret runs on the
		// daemon-spawn path. BOTH http install paths ignore required_secrets —
		// generateRemoteHTTPDraft (marketplace_generate.go) projects only
		// name/kind/transport/url/client_bindings/availability, and the remote-http
		// install never gates on it — so required_secrets on a transport:"http"
		// entry is meaningless and misleading (it would imply a gate that does not
		// exist). Reject it at authoring rather than silently dropping it. A remote
		// endpoint's credentials live in its headers (Authorization / X-API-Key),
		// not in a required-secret env gate.
		if e.Transport == "http" {
			return fmt.Errorf("required_secrets is not supported on a transport:\"http\" entry (it is a local-stdio install gate; a remote endpoint's credentials belong in its headers)")
		}
		envSecretKeys := map[string]bool{}
		for _, v := range e.Env {
			if strings.HasPrefix(v, "secret:") {
				envSecretKeys[strings.TrimPrefix(v, "secret:")] = true
			}
		}
		for _, key := range e.RequiredSecrets {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("required_secrets contains an empty key")
			}
			if !envSecretKeys[key] {
				return fmt.Errorf("required_secrets key %q has no matching secret:%s env ref (a required secret must back an env value the server actually reads)", key, key)
			}
		}
	}
	return nil
}

func marketplaceCatalogURLForError(raw string) string {
	return sanitizeMarketplaceErrorText(urlredact.MarketplaceURLForError(raw))
}

func sanitizeMarketplaceErrorText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if IsUnsafeMarketplaceTextRune(r) {
			b.WriteRune('\uFFFD')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
