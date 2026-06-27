package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
)

// validManifestName bounds acceptable server names to lower-case
// letters, digits, dot, underscore, hyphen. Any other character would
// either (a) change how defaultManifestDir's filepath.Join resolves —
// ".." escapes the parent, absolute paths ignore the root, leading
// slashes change the meaning — or (b) collide with OS-specific path
// semantics (colon-drive on Windows, control chars). Restricting the
// charset means we never need to revalidate the joined path, which
// eliminates the class of bugs where name parsing and path resolution
// disagree.
var validManifestName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// CheckManifestName is the exported gate for callers outside this
// package (cli/daemon.go disk fallback, cli/relay.go disk fallback)
// that need the same input validation as internal API entry points.
// Keeping a single implementation prevents the gate from drifting
// across surfaces.
func CheckManifestName(name string) error {
	return checkManifestName(name)
}

// reservedWinNames is the set of legacy DOS device names that Windows
// resolves specially regardless of working directory or extension.
// Stored lower-case for case-insensitive matching against ToLower(name).
//
// Reference: https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file
//
// "Do not use the following reserved names for the name of a file:
//
//	CON, PRN, AUX, NUL, COM0, COM1, COM2, COM3, COM4, COM5, COM6,
//	COM7, COM8, COM9, COM¹, COM², COM³, LPT0, LPT1, LPT2, LPT3,
//	LPT4, LPT5, LPT6, LPT7, LPT8, LPT9, LPT¹, LPT², and LPT³.
//	Also avoid these names followed immediately by an extension; for
//	example, NUL.txt and NUL.tar.gz are both equivalent to NUL."
//
// We omit the COM0/LPT0 and superscript variants because the manifest
// regex (`[a-z0-9][a-z0-9._-]*`) already rejects superscripts via
// charset; COM0/LPT0 are vanishingly unlikely to be a typo'd manifest
// name but are listed below for completeness.
var reservedWinNames = map[string]struct{}{
	"con":  {},
	"prn":  {},
	"aux":  {},
	"nul":  {},
	"com0": {}, "com1": {}, "com2": {}, "com3": {}, "com4": {},
	"com5": {}, "com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt0": {}, "lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {},
	"lpt5": {}, "lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// rejectWindowsReservedManifestName implements the reserved-name half
// of checkManifestName's documented contract. The validManifestName
// regex blocks every form of path-separator and capital ASCII, so by
// the time we get here `name` is lower-case ASCII, but it does NOT
// stop names like "con", "nul.yaml", "aux.json", or trailing-dot
// aliases from passing — Windows will rewrite all of these to the
// underlying device, leaving manifest CRUD with ambiguous on-disk
// targets and (for delete) potentially device-handle behavior.
//
// Rules enforced:
//   - bare reserved device names (CON, PRN, AUX, NUL, COMn, LPTn)
//   - reserved-with-extension forms (`con.txt`, `nul.yaml`, ...) —
//     Windows treats `<RESERVED>.<EXT>` as the device too
//   - trailing '.' or ' ' — Windows trims these on file create, which
//     means two distinct manifest names could resolve to the same
//     on-disk file.
func rejectWindowsReservedManifestName(name string) error {
	if name == "" {
		return nil // checkManifestName handles empty separately
	}
	// Trailing dot or space. Cheap check first; doesn't depend on
	// any allocation.
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("manifest name %q: trailing '.' or ' ' is reserved on Windows (filesystem rewrites the path, leading to ambiguous on-disk targets)", name)
	}
	// The manifest regex already lower-cases the input, but call
	// ToLower defensively — the helper is exposed unit-tested so a
	// caller bypassing the regex still gets the right answer.
	lower := strings.ToLower(name)
	// Take everything up to the first '.': Windows treats any extension
	// suffix as immaterial (`NUL.tar.gz` == `NUL`).
	base := lower
	if i := strings.Index(lower, "."); i >= 0 {
		base = lower[:i]
	}
	if _, ok := reservedWinNames[base]; ok {
		return fmt.Errorf("manifest name %q: reserved Windows device name %q (with or without extension) is not allowed", name, base)
	}
	return nil
}

// hubReconcileAggregateEntryName is the reserved client-config entry
// name the gate-ON reconciler emits per spec §"Hub MCP endpoint
// contract". A user-created manifest with this exact server name
// would deterministically collide with the gate-ON AddReplace +
// per-server Remove pair (codex bot phase5 r14 P2 closure on PR #160):
// the apply order ("adds before removes") would write the aggregate
// then delete it via the per-server Remove with the same EntryName.
// Reserve the name at validation time so the broken state can't be
// created.
const hubReconcileAggregateEntryName = "mcphub-hub"

// checkManifestName rejects names that could escape the manifest
// directory via path traversal, contain absolute-path semantics, or
// land on reserved Windows filenames. Returns a descriptive error so
// the CLI/API surface the reason rather than a generic "bad name".
func checkManifestName(name string) error {
	if name == "" {
		return fmt.Errorf("manifest name: must be non-empty")
	}
	if !validManifestName.MatchString(name) {
		return fmt.Errorf("manifest name %q: must match [a-z0-9][a-z0-9._-]* (lowercase ASCII, digits, '.', '_', '-', and must not start with '.' or '-')", name)
	}
	// Reject any name that resolves to '.' or '..' after clean. The
	// regex already blocks '..' literally, but Clean catches chained
	// forms like '.../..' that a future looser regex might miss.
	if clean := filepath.Clean(name); clean != name || clean == "." || clean == ".." {
		return fmt.Errorf("manifest name %q: resolves to non-canonical path %q", name, clean)
	}
	// Final layer: reserved Windows device names + trailing-dot/space
	// aliases. The regex above enforces lower-case ASCII so this only
	// has to handle the device-name semantics, not casing.
	if err := rejectWindowsReservedManifestName(name); err != nil {
		return err
	}
	// codex bot phase5 r14 P2 closure on PR #160: reserve the
	// aggregate entry name used by the gate-ON reconciler.
	if name == hubReconcileAggregateEntryName {
		return fmt.Errorf("manifest name %q: reserved (clashes with the gate-ON hub aggregate entry name; pick a different server name)", name)
	}
	return nil
}

// ManifestList returns the sorted list of server names that have a
// manifest, unioning the embed FS (shipped with the binary, source of
// truth in production) with the on-disk defaultManifestDir (used by
// dev flows where a new manifest hasn't been compiled in yet).
//
// Before this changed, ManifestList ONLY looked at disk — so a canonical
// ~/.local/bin/mcphub.exe invoked from %TEMP% reported 0 servers even
// though 10 were baked into the binary. That was split-brain with the
// daemon (which always reads from embed).
func (a *API) ManifestList() ([]string, error) {
	return listManifestNamesEmbedFirst()
}

// RoutableServerNames returns the sorted subset of ManifestList() whose
// manifest declares at least one LOCAL daemon (a non-empty daemons[] block)
// — i.e. the servers a group can actually route to. It is the same locality
// the resolver snapshot builder honors: a group binds a member server by
// folding each of that server's daemons into the snapshot, so a server with
// NO local daemon ref (transport=remote-http, where clients connect straight
// to the remote URL; or a daemonless / dynamic-pool-only manifest with no
// static daemons[]) contributes ZERO bindings and is unroutable as a group
// member.
//
// The Groups GUI sources its server picker from this (not ManifestList) so it
// never offers an unroutable member that would persist as a dead row and then
// re-trip GROUPS_UNKNOWN_SERVER on the next save. A manifest that fails to
// load or parse is SKIPPED (best-effort enrichment, never blanks the picker),
// mirroring CatalogList.
//
// R4-1 (bot R4): a per-session server (perSessionServers — scan.go marks it
// CanMigrate=false because its sessions MUST stay 1-per-local-client) is also
// EXCLUDED. Such a server can never be folded into a shared /g/<group>/mcp
// route without breaking per-session isolation, so the group picker must not
// offer it AND the groupsUpsert known-server save-gate (which sources its set
// here) must reject it — the snapshot builder's matching skip is the
// defense-in-depth backstop for a hand-edited groups.yaml.
func (a *API) RoutableServerNames() ([]string, error) {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		// Per-session servers must stay 1-per-local-client; they are never
		// routable as a shared group member (see doc comment + the matching
		// skip in BuildResolverSnapshotFromManifestsAndGroups).
		if perSessionServers[name] {
			continue
		}
		data, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			// Unreadable manifest — skip (it still appears in ManifestList,
			// but without a parsed body we cannot assert local routability).
			continue
		}
		m, err := config.ParseManifest(bytes.NewReader(data))
		if err != nil {
			continue
		}
		// Local daemon ref == a non-empty static daemons[] block. This is
		// exactly what BuildResolverSnapshotFromManifestsAndGroups indexes
		// per server, so a member with no daemons[] binds nothing.
		// transport=remote-http manifests are rejected from declaring
		// daemons[] at parse time, so they fall out here by construction.
		if len(m.Daemons) == 0 {
			continue
		}
		// kind=companion has daemons[] (so it is supervised) but is a NON-MCP
		// process with NO client routing — exclude it from the routable /
		// group-authoring set so the Groups GUI never offers it as a member (the
		// hub publish scan-filter already drops it from snapshots; excluding it
		// here stops it being selectable in the first place) (Codex #381).
		if m.Kind == config.KindCompanion {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// CatalogList returns the catalog projection ({name, description, kind})
// of every available server, sorted by name. It reuses the same
// embed-first name set as ManifestList, then loads + projects each
// manifest's display scalars via config.ParseCatalogFields.
//
// A manifest that fails to load or project is SKIPPED (logged at the
// caller's discretion via the returned-from-ManifestList name still
// appearing in ManifestList but not here) rather than failing the whole
// catalog — a single malformed dev-added manifest must not blank the
// store. The projection deliberately does NOT expand env / resolve
// secrets / run Validate(), so it succeeds for shipped manifests whose
// env (memory's ${HOME}) or secrets (wolfram) are unset on this host.
func (a *API) CatalogList() ([]config.CatalogFields, error) {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return nil, err
	}
	out := make([]config.CatalogFields, 0, len(names))
	for _, name := range names {
		data, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			// Skip an unreadable manifest; it still appears in the
			// name-only ManifestList, so the catalog is a best-effort
			// enrichment, never the source of truth for availability.
			continue
		}
		fields, err := config.ParseCatalogFields(bytes.NewReader(data))
		if err != nil {
			continue
		}
		out = append(out, fields)
	}
	return out, nil
}

// ManifestListIn is the tempdir-capable form of ManifestList.
func (a *API) ManifestListIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ManifestGet returns the raw YAML of the named server's manifest,
// reading from the embed FS first (production) with disk fallback for
// dev flow. See listManifestNamesEmbedFirst for the rationale.
func (a *API) ManifestGet(name string) (string, error) {
	if err := checkManifestName(name); err != nil {
		return "", err
	}
	data, err := loadManifestYAMLEmbedFirst(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ErrManifestNotEmbedded is returned by CatalogManifestGet when the
// requested server name is NOT in the binary's embedded manifest set
// (embeddedManifestNames). The D2 cold-re-enable Re-add flow maps it to
// the "isn't in the catalog" 404 → name-only seed. It is the explicit
// membership-gate signal: a name that is only on disk (a dev-checkout
// manifest, or — the security case — a hand-planted disk manifest whose
// env carries literal secrets) is excluded BEFORE the loader runs, so the
// disk read in loadManifestYAMLEmbedFirst is unreachable for this path.
var ErrManifestNotEmbedded = errors.New("manifest not in the embedded catalog set")

// CatalogManifestGet returns the raw YAML of the named server's manifest
// SOURCED ONLY FROM THE BINARY'S EMBED — never disk. It backs the D2
// cold-re-enable Re-add prefill, whose only secret-safe value source is
// the shipped manifest (its env carries `secret:`/`${env:}` placeholders,
// never a resolved literal). It is deliberately distinct from ManifestGet
// (embed-first WITH disk fallback) and ManifestGetWithHash (disk-only edit
// contract): the prefill must NOT echo a disk manifest, because a
// hand-planted on-disk manifest could carry a literal secret in env.
//
// SECURITY CORE — the membership gate MUST run BEFORE the loader:
//  1. checkManifestName(name) — the same path-traversal / reserved-name gate
//     every manifest entry point applies, so a bad name cannot drive a
//     pre-validation filesystem probe.
//  2. MEMBERSHIP GATE — name ∈ embeddedManifestNames()? If NO, return
//     ErrManifestNotEmbedded immediately. embeddedManifestNames reads the
//     embed FS directly (it does NOT consult MCPHUB_MANIFEST_DIR_OVERRIDE),
//     so a name present only on disk is excluded here regardless of any
//     test override, and the loader below is never reached for it.
//  3. loadManifestYAMLEmbedFirst(name) — for a name that PASSED the gate
//     (so it IS embedded), the embed branch (manifest_source.go:81) returns
//     before the disk fallback (:84-86) ever executes. A disk manifest with
//     literal secrets is therefore never sourced by this path.
func (a *API) CatalogManifestGet(name string) (string, error) {
	if err := checkManifestName(name); err != nil {
		return "", err
	}
	// Membership gate BEFORE the loader. A name that is not in the embed
	// set (disk-only dev manifest, or a hand-planted disk manifest) is
	// refused here, so loadManifestYAMLEmbedFirst's disk fallback is
	// structurally unreachable for the catalog-prefill contract.
	embedded := false
	for _, n := range embeddedManifestNames() {
		if n == name {
			embedded = true
			break
		}
	}
	if !embedded {
		return "", ErrManifestNotEmbedded
	}
	// name is embedded → loadManifestYAMLEmbedFirst hits the embed branch
	// and returns before any disk read.
	data, err := loadManifestYAMLEmbedFirst(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ManifestGetIn is the tempdir-capable form of ManifestGet.
//
// checkManifestName must run at entry: production wrappers gate on it,
// but ManifestGetIn is also reachable directly (tests, future callers,
// embedded toolchain code) and the path is built by raw-joining dir +
// name. Without the check, a name like "../escape" would let a caller
// read any manifest.yaml under dir's parent.
func (a *API) ManifestGetIn(dir, name string) (string, error) {
	if err := checkManifestName(name); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name, "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ManifestGetInWithHash reads the manifest YAML and returns both the
// text and its SHA-256 content hash. Used by the GUI edit flow so
// ManifestEdit can detect external writes that occurred between Load
// and Save (A2b D3 stale-file detection).
//
// Like ManifestGetIn, checkManifestName runs at entry so this entry
// point cannot be confused-deputy'd into reading manifests outside
// dir even when called directly (bypassing ManifestGetWithHash).
func (a *API) ManifestGetInWithHash(dir, name string) (string, string, error) {
	if err := checkManifestName(name); err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, name, "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return string(data), ManifestHashContent(data), nil
}

// ManifestGetWithHash is the default-dir convenience wrapper, used by
// GUI handlers which always read from defaultManifestDir().
func (a *API) ManifestGetWithHash(name string) (string, string, error) {
	if err := checkManifestName(name); err != nil {
		return "", "", err
	}
	// Read from disk (not embed) because edit flow only makes sense
	// for user-created / on-disk manifests — you cannot edit embedded
	// shipped manifests in-place.
	return a.ManifestGetInWithHash(defaultManifestDir(), name)
}

// ManifestCreate writes a new manifest under the default servers dir.
// Rejects if the server name already has a manifest — use ManifestEdit
// to change existing ones.
func (a *API) ManifestCreate(name, yaml string) error {
	return a.ManifestCreateIn(defaultManifestDir(), name, yaml)
}

// ManifestExists reports whether the default manifest dir already has the
// target manifest file. It mirrors ManifestCreateIn's pre-create existence
// gate without reading the file contents.
func (a *API) ManifestExists(name string) (bool, error) {
	if err := checkManifestName(name); err != nil {
		return false, err
	}
	target := filepath.Join(defaultManifestDir(), name, "manifest.yaml")
	if _, err := os.Stat(target); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

// ManifestCreateIn is the tempdir-capable form of ManifestCreate.
func (a *API) ManifestCreateIn(dir, name, yaml string) error {
	if err := checkManifestName(name); err != nil {
		return err
	}
	target := filepath.Join(dir, name, "manifest.yaml")
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("manifest %q already exists at %s; use edit instead", name, target)
	}
	if warnings := a.validateManifestForStorageName(name, yaml); len(warnings) > 0 {
		return fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	// Strict-mode gate (codex bot r5 P1 closure): mutation surfaces
	// must reject '__'-in-server-name per the spec's "Pre-gate" section.
	// validateManifestForStorageName only emits warnings; strict mode
	// is what produces a hard error.
	if _, err := a.ManifestValidateMode(yaml, ValidateModeStrict); err != nil {
		return fmt.Errorf("manifest rejected by strict validation: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(yaml), 0644)
}

// ManifestEdit replaces an existing manifest after validation. Fails if
// the manifest doesn't exist; use ManifestCreate for new entries.
func (a *API) ManifestEdit(name, yaml string) error {
	return a.ManifestEditIn(defaultManifestDir(), name, yaml)
}

// ManifestEditIn is the tempdir-capable form of ManifestEdit.
func (a *API) ManifestEditIn(dir, name, yaml string) error {
	if err := checkManifestName(name); err != nil {
		return err
	}
	target := filepath.Join(dir, name, "manifest.yaml")
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("manifest %q does not exist; use create instead", name)
	}
	if warnings := a.validateManifestForStorageName(name, yaml); len(warnings) > 0 {
		return fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	// Strict-mode gate (codex bot r5 P1 closure): same as ManifestCreateIn.
	// Edit paths must enforce '__' rejection for the same reason as
	// create paths — both are spec-mandated "mutation surfaces."
	if _, err := a.ManifestValidateMode(yaml, ValidateModeStrict); err != nil {
		return fmt.Errorf("manifest rejected by strict validation: %w", err)
	}
	return os.WriteFile(target, []byte(yaml), 0644)
}

// ValidateMode discriminates the '__'-substring policy in server names.
// Strict mode is applied at manifest mutation surfaces (create / edit /
// install + hub binding setup); compat mode at startup inventory + GUI
// manifest reads so legacy '__'-named manifests stay readable.
//
// G4 §"Pre-gate" (docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md).
type ValidateMode int

const (
	// ValidateModeCompat warns on '__' substring in server names but
	// accepts the manifest. This is the default / existing-caller
	// behavior preserved by ManifestValidate.
	ValidateModeCompat ValidateMode = iota
	// ValidateModeStrict rejects '__' substring in server names. Used
	// by the hub bind-time gate and mutation surfaces.
	ValidateModeStrict
)

// ManifestValidate parses a manifest YAML and returns any structural
// issues (missing required fields, unknown kind/transport values). Empty
// slice means the manifest passes basic validation. Does NOT check that
// referenced binaries, ports, or secrets actually exist — that's caller
// responsibility at install time.
//
// Existing callers receive COMPAT-mode semantics (warns on '__' but
// accepts). New callers that need strict-mode rejection use
// ManifestValidateMode or ManifestValidateForHubBind.
func (a *API) ManifestValidate(yaml string) []string {
	warnings, _ := a.ManifestValidateMode(yaml, ValidateModeCompat)
	return warnings
}

// ManifestValidateMode is ManifestValidate with explicit mode. Returns
// (warnings, err). In COMPAT mode, err is always nil — structural parse
// errors are reported via warnings[0] for backward compatibility with
// existing ManifestValidate callers that ignored the (returned but
// unused) error channel. In STRICT mode, both parse failures AND hard
// rule violations ('__' in server name) return a hard error;
// admission gates that discard warnings (e.g. ManifestValidateForHubBind)
// rely on this strict-mode error path being authoritative (codex bot
// r1 P1 closure — earlier wording reported parse failures as warnings
// only, so a malformed manifest passed the strict hub-bind gate as
// valid).
//
// G4 §"Pre-gate".
func (a *API) ManifestValidateMode(yaml string, mode ValidateMode) ([]string, error) {
	reader := strings.NewReader(yaml)
	m, err := config.ParseManifest(reader)
	if err != nil {
		if mode == ValidateModeStrict {
			return nil, fmt.Errorf("manifest parse failed: %w", err)
		}
		return []string{err.Error()}, nil
	}
	warnings := manifestValidationWarnings(m)
	if strings.Contains(m.Name, "__") {
		switch mode {
		case ValidateModeStrict:
			return warnings, fmt.Errorf("manifest name %q: '__' substring rejected in strict mode (reserved for hub-mode tool-name namespacing)", m.Name)
		case ValidateModeCompat:
			warnings = append(warnings, fmt.Sprintf("manifest name %q contains '__' (deprecated; will be rejected in strict mode)", m.Name))
		}
	}
	return warnings, nil
}

// ManifestValidateForHubBind wraps ManifestValidateMode in strict mode
// and returns only the hard error (warnings dropped). Phase 4's hub
// listener bring-up uses this from gui/server.go to gate on the
// participating manifest set when gui_server.hub_endpoint_enabled=true.
//
// G4 §"Pre-gate".
func (a *API) ManifestValidateForHubBind(yaml string) error {
	_, err := a.ManifestValidateMode(yaml, ValidateModeStrict)
	return err
}

func (a *API) validateManifestForStorageName(name, yaml string) []string {
	m, err := parseManifestForName(name, []byte(yaml))
	if err != nil {
		return []string{err.Error()}
	}
	// Storage paths (create / edit) gate on BLOCKING warnings only.
	// Advisory warnings are returned by ManifestValidate (for the
	// GUI/CLI to surface) but do not refuse the write.
	return manifestBlockingWarnings(m)
}

// manifestValidationWarnings returns the BLOCKING warnings only.
// Draft readiness currently mirrors the write gate by calling
// ManifestValidateMode(Strict), so this alias to manifestBlockingWarnings is a
// temporary drift-sensitive dependency, not an incidental implementation detail
// (see work-items/decisions/2026-06-20-draft-readiness-mirrors-write-gate-follow-up.md).
// Pre-r10 it returned blocking + advisory combined; codex bot r10
// P2 closure (PR #169) flagged that the GUI save flow
// (AddServer.tsx) treats ANY warnings.length > 0 as fatal, so
// surfacing advisories through this path effectively blocks the
// GUI save of accepted-but-no-op manifests like remote-http +
// weekly_refresh:true.
//
// Advisories are surfaced separately via manifestAdvisoryWarnings
// (consumed at install / daemon-launch time by sub-PR 2+ of G6).
// The current API surface intentionally does NOT expose advisories
// to ManifestValidate callers — that gate is for "this manifest
// will fail to install/run", not "you might want to know".
//
// Daemons-empty exemptions:
//   - Workspace-scoped manifests legitimately have no daemons
//     (PR #108) — lazy-proxy per-(workspace, language).
//   - Remote-http manifests have no local daemon at all (G6) — the
//     client connects directly to the remote URL.
//
// codex bot r3 P1 closure (PR #169): pre-fix, valid remote-http
// manifests couldn't be created/edited because the daemon-empty
// warning was treated as a hard error.
func manifestValidationWarnings(m *config.ServerManifest) []string {
	return manifestBlockingWarnings(m)
}

// manifestBlockingWarnings returns warnings that ManifestCreateIn /
// ManifestEditIn / ManifestEditInWithHash treat as hard errors.
// These are structural issues that would produce a non-functional
// manifest on disk.
func manifestBlockingWarnings(m *config.ServerManifest) []string {
	var warnings []string
	if m.Kind != config.KindWorkspaceScoped &&
		m.Transport != config.TransportRemoteHTTP &&
		len(m.Daemons) == 0 {
		warnings = append(warnings, "no daemons declared")
	}
	// A kind=companion daemon is a NON-MCP process — it binds no mcphub MCP port
	// (the companion, e.g. the excalidraw canvas, listens on its own port
	// directly), so port=0 is VALID for it and must not be flagged as a structural
	// error that blocks the manifest write / install (Codex #381).
	if m.Kind != config.KindCompanion {
		for _, d := range m.Daemons {
			if d.Port == 0 {
				warnings = append(warnings, fmt.Sprintf("daemon %q has port=0", d.Name))
			}
		}
	}
	return warnings
}

// manifestAdvisoryWarnings returns non-fatal observations: spec-
// defined conditions that are accepted-but-no-op or otherwise
// deserve operator attention without blocking the write.
//
// G6 spec §"Validation rules": weekly_refresh:true on remote-http
// is accepted but emits a warning (no local daemon to refresh).
func manifestAdvisoryWarnings(m *config.ServerManifest) []string {
	var warnings []string
	if m.Transport == config.TransportRemoteHTTP && m.WeeklyRefresh {
		warnings = append(warnings, "weekly_refresh has no effect on remote-http manifests (no local daemon to refresh) — remove the line")
	}
	return warnings
}

func parseManifestForName(name string, data []byte) (*config.ServerManifest, error) {
	if err := checkManifestName(name); err != nil {
		return nil, err
	}
	m, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if err := checkManifestName(m.Name); err != nil {
		return nil, fmt.Errorf("manifest yaml name: %w", err)
	}
	if m.Name != name {
		return nil, fmt.Errorf("manifest yaml name %q must match requested server %q", m.Name, name)
	}
	return m, nil
}

// ErrManifestHashMismatch is returned by ManifestEditInWithHash when
// the on-disk manifest's current content hash does not match the
// client-supplied expectedHash. The GUI maps this to the stale-file
// banner (A2b D3). Passing an empty expectedHash skips the check —
// used by Force Save which re-reads at save-time.
var ErrManifestHashMismatch = errors.New("manifest hash mismatch: file changed on disk since it was loaded")

// ManifestEditInWithHash replaces an existing manifest atomically via
// tmp-file-plus-rename. If the on-disk content hash diverged from
// expectedHash (non-empty), returns ErrManifestHashMismatch without
// writing. Empty expectedHash skips the check (Force Save path which
// re-reads at save-time). Returns the new post-write content hash so
// callers can update their loadedHash cache in one pass — avoids an
// extra GET round-trip AND the stale-hash-after-force-save race.
func (a *API) ManifestEditInWithHash(dir, name, yaml, expectedHash string) (newHash string, err error) {
	if err := checkManifestName(name); err != nil {
		return "", err
	}
	target := filepath.Join(dir, name, "manifest.yaml")
	current, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("manifest %q does not exist; use create instead", name)
	}
	if expectedHash != "" {
		if got := ManifestHashContent(current); got != expectedHash {
			return "", ErrManifestHashMismatch
		}
	}
	if warnings := a.validateManifestForStorageName(name, yaml); len(warnings) > 0 {
		return "", fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	// Strict-mode gate (codex bot r7 P1 closure): the hash-based edit
	// path is ALSO a mutation surface and must reject '__' in server
	// names per the spec's Pre-gate. Earlier wording wired strict mode
	// only into ManifestEditIn; this is the second of the two edit
	// paths (the GUI save uses the hash-based one).
	if _, err := a.ManifestValidateMode(yaml, ValidateModeStrict); err != nil {
		return "", fmt.Errorf("manifest rejected by strict validation: %w", err)
	}
	// Atomic write: unique tmp in the same directory, defer cleanup,
	// os.Rename on success. Test-only hook manifestEditFailWriteHook
	// lets tests inject a write/rename failure without relying on
	// read-only-dir tricks (brittle on Windows).
	tmp, err := os.CreateTemp(filepath.Dir(target), "manifest-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	// Always attempt to remove tmp; harmless if rename already moved it.
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write([]byte(yaml)); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close tmp: %w", err)
	}
	if manifestEditFailWriteHook != nil && manifestEditFailWriteHook() {
		return "", fmt.Errorf("injected write failure")
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", fmt.Errorf("atomic rename: %w", err)
	}
	return ManifestHashContent([]byte(yaml)), nil
}

// manifestEditFailWriteHook is a package-internal test hook that forces
// a simulated write-failure to verify atomic-write crash-safety. Tests
// set it via ManifestSetFailWriteHook, run the operation, then reset.
var manifestEditFailWriteHook func() bool

// ManifestSetFailWriteHook is test-only; callers in production code MUST
// NOT set this. Exported only so internal/gui tests can reuse the hook.
func ManifestSetFailWriteHook(h func() bool) { manifestEditFailWriteHook = h }

// ManifestEditWithHash is the default-dir convenience wrapper.
func (a *API) ManifestEditWithHash(name, yaml, expectedHash string) (string, error) {
	return a.ManifestEditInWithHash(defaultManifestDir(), name, yaml, expectedHash)
}

// ManifestDelete removes the named server's manifest directory. Does NOT
// uninstall the server — caller should run Uninstall first for a clean
// teardown.
func (a *API) ManifestDelete(name string) error {
	return a.ManifestDeleteIn(defaultManifestDir(), name)
}

// ManifestDeleteIn is the tempdir-capable form of ManifestDelete.
//
// The regex-guarded name lets us use filepath.Join safely: validManifestName
// cannot contain separators, so the resulting target is always a direct
// child of dir. We still compare Dir(target) to Clean(dir) as a defense in
// depth in case some future edit relaxes the guard or introduces a new
// separator quirk.
func (a *API) ManifestDeleteIn(dir, name string) error {
	if err := checkManifestName(name); err != nil {
		return err
	}
	target := filepath.Join(dir, name)
	cleanDir := filepath.Clean(dir)
	if parent := filepath.Dir(target); parent != cleanDir {
		return fmt.Errorf("manifest delete: resolved path %q escapes manifest dir %q", target, cleanDir)
	}
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("manifest %q does not exist", name)
	}
	return os.RemoveAll(target)
}
