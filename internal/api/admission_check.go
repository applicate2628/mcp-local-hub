package api

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/lldb"
	"mcp-local-hub/internal/secrets"
)

type AdmissionFinding struct {
	ID       string
	Name     string
	Reason   string
	Fix      string
	Optional bool
}

// AdmissionError carries the first BLOCKING admission finding as a typed error
// so callers (the CLI install render) can surface the actionable Fix instead of
// only the cryptic Reason. Error() preserves the bare Reason as its prefix so
// existing callers that match on a Reason substring keep working; the guided
// Fix is appended on its own line when present. The richer Fix/ID fields stay
// available to callers that type-assert *AdmissionError.
//
// SEAM-B (install-and-it-works Area 2): the GATE DECISION — which findings block
// — is unchanged (still keyed on AdmissionFinding.Optional in Preflight); only
// the error VALUE returned for a blocking finding is now structured.
type AdmissionError struct {
	ID     string
	Reason string
	Fix    string
}

func (e *AdmissionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Fix == "" {
		return e.Reason
	}
	return e.Reason + "\n  Fix: " + e.Fix
}

// admissionErrorFromFinding builds the typed error for a single blocking finding.
func admissionErrorFromFinding(f AdmissionFinding) *AdmissionError {
	return &AdmissionError{ID: f.ID, Reason: f.Reason, Fix: f.Fix}
}

type AdmissionScope struct {
	DaemonFilter string
}

func AdmissionCheck(m *config.ServerManifest, scope AdmissionScope) []AdmissionFinding {
	if m == nil {
		return nil
	}

	var findings []AdmissionFinding
	add := func(id, name, reason, fix string, optional bool) {
		findings = append(findings, AdmissionFinding{
			ID:       id,
			Name:     name,
			Reason:   reason,
			Fix:      fix,
			Optional: optional,
		})
	}

	// D-3 inert gate (Tier-0), evaluated FIRST and for EVERY transport. A
	// watch / disabled-until-probe row whose install-probe has NOT passed gets a
	// NON-OPTIONAL finding, which makes Preflight return the AdmissionError and
	// abort Install BEFORE installPlanCore writes any supervisor-intent row or
	// client config — the single chokepoint guaranteeing "an inert row NEVER
	// spawns a daemon nor writes a client config until the probe passes". The
	// probe is the readiness gate reused as a dry-run (availabilityProbePasses
	// composes the existing binaryAvailable + entryScriptStatus owners), so it
	// cannot drift from the install gate. When the probe PASSES the row falls
	// through and behaves exactly like a ready row.
	//
	// The finding itself is emitted by availabilityProbeFinding — the SINGLE
	// OWNER reused by every non-Preflight install/register/spawn path through
	// AvailabilityAdmission / AvailabilityAdmissionFields, so the D-3 gate is one
	// decision shared across all paths (architecture law: one owner per
	// cross-cutting invariant).
	if f, inertBlock := availabilityProbeFinding(m.Availability, m.InstallProbe, m.Name); inertBlock {
		findings = append(findings, f)
		// Short-circuit: an inert un-probed row needs no further port/binary
		// findings — it is not going to install.
		return findings
	}
	// Probe passed (or row is ready) → fall through to the normal admission
	// checks; the row now behaves exactly like a ready row (spawn + write proceed).

	// D-2 advisory (Tier-0): a vendored/community-fork server whose license has
	// not been vetted (license_status pending / empty / unknown) gets an OPTIONAL
	// finding. Optional == does NOT block install (the operator may knowingly
	// install a pending-license fork on their own host); it surfaces in the GUI
	// readiness panel as a yellow advisory row. The HARD pin-presence enforcement
	// lives in config.Validate() (Gate A); only the soft license vetting is
	// advisory here, matching the epic's "confirm LICENSE" being a D-4 protocol
	// step, not a schema invariant. Network-fact license vetting (gh API) is out
	// of scope for this PRE-SPAWN/MCPHUB-FIXABLE gate.
	if vs := m.VendoredSource; vs != nil && vs.LicenseStatus != config.LicenseStatusConfirmed {
		shown := vs.LicenseStatus
		if shown == "" {
			shown = "unset"
		}
		add("vendored-license-unvetted", "vendored source: "+m.Name, fmt.Sprintf("server %s is vendored from %s but its license_status is %q (not confirmed) — vet LICENSE on the real repo before relying on it", m.Name, vendoredRepoForFinding(vs.Repo), shown), "Vet the LICENSE on the upstream/fork repo, then set vendored_source.license_status: confirmed in the manifest.", true)
	}

	if m.Transport == config.TransportRemoteHTTP {
		if _, err := ensureCanonicalMcphubPresent(); err != nil {
			_, fix := LauncherGuidance("mcphub")
			add("canonical-mcphub", "mcphub binary", err.Error(), fix, false)
		}
		if _, err := expandRemoteHTTPURLSecrets(m.URL, nil); err != nil {
			add("remote-url-secret", "remote URL secrets", fmt.Sprintf("install remote-http manifest %s: expand url: %v", m.Name, err), "Set the missing remote URL secret or fix the malformed ${secret:KEY} placeholder.", false)
		}
		if _, err := ExpandSecretsMap(m.Headers, nil); err != nil {
			add("remote-headers-secret", "remote header secrets", fmt.Sprintf("install remote-http manifest %s: expand headers: %v", m.Name, err), "Set the missing remote header secret or fix the malformed ${secret:KEY} placeholder.", false)
		}
		return findings
	}

	launcherOptional := m.Kind == config.KindWorkspaceScoped && m.DaemonTemplate == nil
	if _, err := exec.LookPath(m.Command); err != nil {
		_, fix := LauncherGuidance(m.Command)
		add("command-on-path", "launcher: "+filepath.Base(m.Command), fmt.Sprintf("command %q not found on PATH — %s: %v", m.Command, fix, err), fix, launcherOptional)
	} else if rt := runtimeBehindLauncher(m.Command); rt != "" {
		if _, err := exec.LookPath(rt); err != nil {
			_, fix := LauncherGuidance(rt)
			add("runtime-behind-launcher", "runtime: "+rt, fmt.Sprintf("runtime %q (needed by %q) not found on PATH — %s: %v", rt, m.Command, fix, err), fix, launcherOptional)
		}
	}

	for _, bin := range m.RequiredBinaries {
		if !binaryAvailable(bin) {
			_, fix := LauncherGuidance(bin)
			add("required-binary", "binary: "+filepath.Base(bin), fmt.Sprintf("required binary %q not found — %s", filepath.Base(bin), fix), fix, false)
		}
	}
	seenLanguageBin := map[string]bool{}
	for _, lang := range m.Languages {
		for _, bin := range lang.RequiredBinaries {
			if bin == "" || seenLanguageBin[bin] {
				continue
			}
			seenLanguageBin[bin] = true
			if !binaryAvailable(bin) {
				_, fix := LauncherGuidance(bin)
				add("language-required-binary", "binary: "+filepath.Base(bin), fmt.Sprintf("required binary %q not found — %s", filepath.Base(bin), fix), fix, true)
			}
		}
	}
	if manifestNeedsGit(m) && !binaryAvailable("git") {
		_, fix := LauncherGuidance("git")
		add("git-for-uvx-git-source", "binary: git", fmt.Sprintf("git is required to fetch the uvx git+ source but is not on PATH — %s", fix), fix, launcherOptional)
	}

	if m.Transport == config.TransportStdioBridge && len(m.BaseArgs) >= 2 && m.BaseArgs[0] == "lldb-bridge" {
		addr := m.BaseArgs[1]
		if _, _, err := lldb.ParseHostPort(addr); err != nil {
			add("lldb-bridge-address", "debugger: lldb", fmt.Sprintf("lldb-bridge address %q is not a valid host:port (e.g. localhost:47000): %v", addr, err), "Set base_args[1] to a valid host:port (e.g. localhost:47000) in the manifest.", false)
		} else if !bridgeListenerUp(addr) && !binaryAvailable("lldb") {
			_, fix := LauncherGuidance("lldb")
			add("lldb-bridge-listener-or-binary", "debugger: lldb", fmt.Sprintf("lldb-bridge: no MCP listener on %s and no lldb binary found — %s, or start an lldb MCP listener on %s first", addr, fix, addr), fix+" — OR start an lldb MCP listener on "+addr+" first, then re-run install.", false)
		}
	}

	if !launcherOptional {
		for _, c := range entryScriptCheckTargets(m) {
			if scope.DaemonFilter != "" && c.daemon != "" && c.daemon != scope.DaemonFilter {
				continue
			}
			if !c.resolvable {
				add("entry-script-unresolvable", "script: "+c.label, "relative entry script with no absolute daemon cwd — the daemon inherits an unpredictable working directory, so the script cannot be verified here", "Make base_args[0] absolute, or set an absolute daemon cwd, so readiness can verify the entry script exists.", true)
				continue
			}
			if ok, reason := entryScriptStatus(c.path); !ok {
				add("entry-script-present", "script: "+c.label, fmt.Sprintf("entry script %q for %q %s — install/clone the server so base_args[0] points at the file, then re-run install", filepath.Base(c.path), normalizeLauncher(m.Command), reason), "Install/clone the server so the manifest's base_args[0] script path exists and points at a file (not a directory), then re-run install.", false)
			}
		}
	}

	if _, err := ensureCanonicalMcphubPresent(); err != nil {
		_, fix := LauncherGuidance("mcphub")
		add("canonical-mcphub", "mcphub binary", err.Error(), fix, false)
	}

	if err := validateDynamicPoolManifest(m); err != nil {
		add("dynamic-pool", "dynamic-pool", err.Error(), "Fix the daemon_template manifest: native-http transport, a non-empty daemon_template.context, and no --context token in base_args/extra_args_template.", false)
	}

	for _, d := range m.Daemons {
		if scope.DaemonFilter != "" && d.Name != scope.DaemonFilter {
			continue
		}
		// A kind=companion daemon binds NO mcphub MCP port (Port==0 is valid — it
		// listens on its own port directly). Skip the MCP port range + collision
		// checks (Codex #381; preserved through the AdmissionCheck refactor so the
		// merged companion + AdmissionCheck features stay consistent).
		if m.Kind == config.KindCompanion {
			continue
		}
		if d.Port < 1 || d.Port > 65535 {
			add("external-port-range", fmt.Sprintf("port %d (%s)", d.Port, d.Name), fmt.Sprintf("daemon %s/%s: port %d is outside the valid range 1..65535", m.Name, d.Name, d.Port), "Set a valid free fixed port (1..65535) for this daemon in the manifest.", false)
			continue
		}
		if preflightPortInUse(d.Port) && !portHeldByOurDaemonForPortArm(d.Port, m.Name, d.Name, false) {
			add("external-port-free", fmt.Sprintf("port %d (%s)", d.Port, d.Name), fmt.Sprintf("port %d already in use (needed for daemon %s/%s)", d.Port, m.Name, d.Name), "Free the port or change the daemon port in the manifest.", false)
		}
		if m.Transport != config.TransportNativeHTTP {
			continue
		}
		internal := d.Port + config.NativeHTTPInternalPortOffset
		if internal < 1 || internal > 65535 {
			add("native-http-internal-range", fmt.Sprintf("internal port %d (%s)", internal, d.Name), fmt.Sprintf("daemon %s/%s native-http upstream port %d is outside the valid range 1..65535 (external=%d, internal=external+%d)", m.Name, d.Name, internal, d.Port, config.NativeHTTPInternalPortOffset), "Free the port or change the daemon port (internal = external + offset, both must be 1..65535).", false)
			continue
		}
		if preflightPortInUse(internal) && !portHeldByOurDaemonForPortArm(internal, m.Name, d.Name, true) {
			add("native-http-internal-free", fmt.Sprintf("internal port %d (%s)", internal, d.Name), fmt.Sprintf("internal port %d already in use (needed for native-http upstream of %s/%s; external=%d, internal=external+%d)", internal, m.Name, d.Name, d.Port, config.NativeHTTPInternalPortOffset), "Free the port or change the daemon port (internal = external + offset, both must be 1..65535).", false)
		}
	}

	findings = append(findings, admissionPortPoolFindings(m)...)

	if secrets.HasSecretRef(m.Env) {
		if _, err := secrets.OpenVaultOptional(secrets.DefaultKeyPath(), secrets.DefaultVaultPath()); err != nil {
			add("secrets-vault-readable", "secrets vault", fmt.Sprintf("manifest %s uses secret refs but the vault is unreadable: %v", m.Name, err), "Fix or remove the corrupt vault — a secret-using server fails to start when it cannot be read.", false)
		}
	}
	// REQUIRED secrets (opt-in install gate). A key declared in
	// m.RequiredSecrets is a NON-OPTIONAL (blocking) finding when it is not
	// resolvable in the vault — the SERVER hard-exits on startup without it
	// (e.g. mcp-suno exits 1 when ACEDATACLOUD_API_TOKEN is unset), so a
	// one-click install with no token would crash-loop instead of failing
	// loud here. This is the SIBLING of the default optional-secret posture: an
	// UNMARKED `secret:` ref stays optional (omitted at spawn, server reports
	// its own missing-key); only a marked key blocks. The blocking finding makes
	// containsNonOptional() true, so Preflight returns an AdmissionError and
	// Install aborts BEFORE any manifest/intent/client-config write.
	//
	// The vault-UNREADABLE case is NOT re-reported here — the
	// "secrets-vault-readable" finding above already blocks on it. This loop
	// fires ONLY on a resolvable vault (absent or readable) where the specific
	// key is genuinely missing, so the operator sees the actionable per-key
	// "set <key>" fix, not a duplicate corrupt-vault row. The Reason names the
	// KEY ONLY, never a value (redaction posture — readiness.go's secret rows).
	if reqSecrets := requiredSecretSet(m); len(reqSecrets) > 0 {
		vault, verr := secrets.OpenVaultOptional(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
		// verr != nil → vault EXISTS but is unreadable; the vault-readable finding
		// above already blocks (when the manifest has secret refs, which a
		// required-secret manifest does). Skip the per-key loop so we do not stack
		// a confusing "<key> not set" on top of "vault unreadable".
		if verr == nil {
			for _, key := range sortedRequiredSecretKeys(reqSecrets) {
				resolvable := false
				if vault != nil {
					// vault.Get returns ("", nil) when the key EXISTS but holds an
					// empty string — a present-but-blank token. That is NOT resolved:
					// a required server (e.g. mcp-suno) still hard-exits on startup
					// with an empty ACEDATACLOUD_API_TOKEN, so a gate that passed on
					// gerr==nil alone would let a crash-looping install through (codex
					// finding 2). Treat a whitespace-only value as unresolved so the
					// blocking finding still fires and the operator is told to set a
					// real value. The Reason names the KEY only (redaction posture),
					// never the value.
					if v, gerr := vault.Get(key); gerr == nil && strings.TrimSpace(v) != "" {
						resolvable = true
					}
				}
				if !resolvable {
					add("required-secret", "secret: "+key, key+" is REQUIRED — the server exits on startup when it is unset", "Set it on the Secrets screen or `mcphub secrets set "+key+"` before installing.", false)
				}
			}
		}
	}
	for k, v := range m.Env {
		if strings.HasPrefix(v, "file:") {
			add("file-env-ref", "env: "+k, fmt.Sprintf("manifest %s env[%s] uses a file: ref, which the daemon launch path cannot resolve (mcphub has no local config map); replace it with a secret: ref or a literal value", m.Name, k), "Replace the file: env ref with a secret: ref (vault) or a literal value in the manifest.", false)
		}
	}

	return findings
}

func containsNonOptional(findings []AdmissionFinding) bool {
	for _, f := range findings {
		if !f.Optional {
			return true
		}
	}
	return false
}

// requiredSecretSet is the SINGLE OWNER of the manifest's opt-in required-secret
// key set, consumed by BOTH the AdmissionCheck blocking finding (above) and the
// readiness per-key Optional classification (readiness.go) so the two can never
// drift on which secrets block vs. stay advisory — there is exactly one predicate
// for "is this vault key a REQUIRED install gate". Returns nil for a manifest with
// no required_secrets (every existing manifest), keeping the gate additive. Blank
// entries are skipped defensively so a stray "" cannot become a permanently
// unblockable phantom key.
func requiredSecretSet(m *config.ServerManifest) map[string]bool {
	if m == nil || len(m.RequiredSecrets) == 0 {
		return nil
	}
	set := make(map[string]bool, len(m.RequiredSecrets))
	for _, k := range m.RequiredSecrets {
		if k == "" {
			continue
		}
		set[k] = true
	}
	return set
}

// RequiredSecretAdmission runs the SAME AdmissionCheck owner and returns the
// blocking required-secret finding (as the typed *AdmissionError) when a key in
// m.RequiredSecrets is unset in the vault, or nil otherwise. It is the
// pre-persist gate seam for callers that hold a PARSED-but-not-yet-written
// manifest and must refuse the install BEFORE writing it to disk — chiefly the
// GUI one-click hub-install handler, which calls ManifestCreate (a disk write)
// before the production Install→Preflight gate runs, so without this a blocked
// install would leave a manifest file behind.
//
// It deliberately surfaces ONLY the SECRET-related blocking findings, NOT the
// full Preflight set: ports / binaries / launchers are re-checked loud at the
// real Install→Preflight gate (which loads the persisted manifest), and a parsed
// in-memory draft may legitimately not yet satisfy a host port/binary check that
// Install handles. Reusing AdmissionCheck + requiredSecretSet keeps this on the
// single required-secret owner (no second predicate). Returns nil for a manifest
// with no required_secrets and a readable/absent vault, so it is additive.
//
// It surfaces TWO secret-block findings, both non-optional:
//   - "required-secret"        — a declared key is unset in a readable vault.
//   - "secrets-vault-readable" — the vault FILE exists but cannot be opened.
//
// The second is load-bearing: when the vault is unreadable, AdmissionCheck
// emits "secrets-vault-readable" and DELIBERATELY SKIPS the per-key
// "required-secret" loop (it cannot tell which keys are present). Filtering to
// "required-secret" ALONE would then see no blocking finding and let
// ManifestCreate write the manifest BEFORE the real Install→Preflight gate
// re-detects the unreadable vault — leaving a manifest behind. Surfacing the
// secret-vault-readable block here keeps the pre-persist gate symmetric with the
// AdmissionCheck secret block. It stays scoped to the SECRET findings: ports /
// binaries / launchers remain at the real Install→Preflight gate.
func RequiredSecretAdmission(m *config.ServerManifest) error {
	if m == nil {
		return nil
	}
	for _, f := range AdmissionCheck(m, AdmissionScope{}) {
		if f.Optional {
			continue
		}
		if f.ID == "required-secret" || f.ID == "secrets-vault-readable" {
			return admissionErrorFromFinding(f)
		}
	}
	return nil
}

// sortedRequiredSecretKeys returns the required-secret keys in a deterministic
// order so the emitted findings are stable across runs (Go map iteration order
// is randomized).
func sortedRequiredSecretKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// optionalFindingByID returns the FIRST advisory (Optional) finding with the
// given ID from an AdmissionCheck result, so CheckServerReadiness can surface a
// specific non-blocking advisory (e.g. the D-2 "vendored-license-unvetted" row)
// as a visible requirement WITHOUT re-deriving its predicate or text — the
// AdmissionCheck finding stays the single owner. It matches only Optional
// findings by design: a blocking (non-optional) finding is already surfaced
// through its own readiness path / the install-plan blocker, so this never
// reclassifies a blocker as advisory.
func optionalFindingByID(findings []AdmissionFinding, id string) (AdmissionFinding, bool) {
	for _, f := range findings {
		if f.Optional && f.ID == id {
			return f, true
		}
	}
	return AdmissionFinding{}, false
}

// findingByID returns the FIRST finding with the given ID from an AdmissionCheck
// result, regardless of its Optional flag. CheckServerReadinessWithScope uses it
// to REUSE the blocking "availability-probe" finding produced by the SINGLE
// AdmissionCheck call instead of re-evaluating the file-based probe a second time
// (which would risk an intra-request TOCTOU between the Ready seed and the
// surfaced advisory row). It deliberately does NOT filter on Optional — the
// availability-probe finding is non-optional — so it is the general single-call
// reuse helper, while optionalFindingByID stays the advisory-only variant.
func findingByID(findings []AdmissionFinding, id string) (AdmissionFinding, bool) {
	for _, f := range findings {
		if f.ID == id {
			return f, true
		}
	}
	return AdmissionFinding{}, false
}

// availabilityProbeFinding is the SINGLE OWNER of the D-3 inert-gate finding.
// Given a manifest/entry's availability string + install probe, it returns the
// NON-OPTIONAL availability-probe finding (and inertBlock=true) when the row is
// inert (watch/disabled-until-probe) AND its probe has NOT passed; otherwise it
// returns inertBlock=false (a ready/empty row, or an inert row whose probe is
// satisfied, falls through to normal admission). AdmissionCheck composes it for
// the Preflight path; AvailabilityAdmission / AvailabilityAdmissionFields wrap
// it into a typed error for the 4 install/register/spawn paths that do NOT
// route through full AdmissionCheck/Preflight. Defining the finding text +
// inert predicate + probe dry-run in this one place is what keeps the D-3 gate
// from drifting across paths (architecture law: one owner per cross-cutting
// invariant). availabilityInert + availabilityProbePasses are the same readiness
// owners the gate has always used — no second detector is introduced here.
func availabilityProbeFinding(availability string, probe *config.AvailabilityProbe, name string) (AdmissionFinding, bool) {
	if availability != config.AvailabilityWatch && availability != config.AvailabilityDisabledUntilProbe {
		return AdmissionFinding{}, false
	}
	ok, why := availabilityProbePasses(probe)
	if ok {
		return AdmissionFinding{}, false
	}
	return AdmissionFinding{
		ID:       "availability-probe",
		Name:     "availability: " + availability,
		Reason:   fmt.Sprintf("server %s is %s and its install-probe has not passed (%s); it will not spawn or write client configs until the probe is satisfied", name, availability, why),
		Fix:      "Install the host app/tool the probe detects, then re-run install / re-check readiness; the row is greyed until then.",
		Optional: false,
	}, true
}

// AvailabilityAdmission is the shared D-3-only admission gate every
// install/register/spawn path that does NOT already run the full
// AdmissionCheck/Preflight gate must call so an inert (watch /
// disabled-until-probe) manifest NEVER spawns a daemon nor writes a client
// config until its install-probe passes. It returns the same typed
// *AdmissionError the Preflight chokepoint returns for the availability-probe
// finding, or nil when the row is ready / the probe is satisfied. ADDITIVE: a
// manifest with empty availability + no probe (every existing manifest) returns
// nil immediately, so the path behaves byte-identically to before Tier-0.
//
// It is intentionally D-3-ONLY (not the full AdmissionCheck): the register /
// LSP-auto-register / serena-projection paths have their own port/binary/
// scheduler verification and only the cross-cutting availability gate was
// being bypassed; running the full port/secret/dynamic-pool gate here would
// change their behavior. The D-2 pin-presence gate for those paths lives in
// config.ServerManifest.Validate (its own single owner).
func AvailabilityAdmission(m *config.ServerManifest) error {
	if m == nil {
		return nil
	}
	return AvailabilityAdmissionFields(m.Availability, m.InstallProbe, m.Name)
}

// AvailabilityAdmissionFields is the field-level form of AvailabilityAdmission
// for callers that hold the availability string + probe directly rather than a
// *config.ServerManifest — chiefly the marketplace one-click install handler,
// whose catalog MarketplaceEntry mirrors the manifest's availability/install_probe
// in JSON-tagged types. It maps onto the SAME availabilityProbeFinding owner, so
// the marketplace entry gate and the manifest gate are one decision.
func AvailabilityAdmissionFields(availability string, probe *config.AvailabilityProbe, name string) error {
	if f, inertBlock := availabilityProbeFinding(availability, probe, name); inertBlock {
		return admissionErrorFromFinding(f)
	}
	return nil
}

// MarketplaceEntryProbePasses reports whether a catalog entry's install probe
// is satisfied on THIS host RIGHT NOW — the LIVE readiness dry-run, not the
// static availability field. It composes the SAME availabilityProbeFinding owner
// the install gate (AvailabilityAdmissionFields / AdmissionCheck) uses, so the
// GUI browse DTO reflects the EXACT gate verdict instead of re-deriving a
// stricter one: a ready/empty row is always installable, and an inert (watch /
// disabled-until-probe) row is installable iff its probe passes (the host app is
// detected). This is the mirror-gate seam for the Catalog screen — the GUI keys
// its install-button suppression on "inert AND NOT this", matching the backend
// gate exactly, so a now-ready inert row can be installed once detected. A nil
// entry, or an entry whose availability is empty/ready, returns true.
func MarketplaceEntryProbePasses(e *MarketplaceEntry) bool {
	if e == nil {
		return true
	}
	_, inertBlock := availabilityProbeFinding(e.Availability, catalogProbeToConfig(e.InstallProbe), e.ID)
	return !inertBlock
}

// ProbeBrowseState is the TRI-STATE browse-time verdict for one catalog row,
// emitted by the read-only GET /api/marketplace projection. It distinguishes the
// THREE states a single bool conflated — "definitely installable", "definitely
// not yet (host app missing)", and "cannot tell without an install-time probe" —
// so the frontend can offer install on the last (the probe runs at install) yet
// keep greying the genuinely-blocked one. The three values:
//
//	ProbeBrowseReady        — ready/empty row, OR an inert binary-only row whose
//	                          bare binaries ALL resolve on PATH. Installable now.
//	ProbeBrowseInertBlocked — inert row that is provably NOT installable yet: a
//	                          nil/empty probe (fail-closed), or a bare binary that
//	                          is absent from PATH. The GUI greys it "probe to
//	                          enable".
//	ProbeBrowseInertUnknown — inert row whose readiness cannot be decided WITHOUT
//	                          touching the filesystem/an external location: it
//	                          carries a files[] probe, OR a path-shaped binary, OR
//	                          a mix. The browse path NEVER os.Stats a file and
//	                          NEVER LookPaths a path, so it defers — the GUI still
//	                          offers install (the real probe runs at the
//	                          install-time AvailabilityAdmissionEntry gate, which
//	                          DOES stat).
type ProbeBrowseState string

const (
	ProbeBrowseReady        ProbeBrowseState = "ready"
	ProbeBrowseInertBlocked ProbeBrowseState = "inert-blocked"
	ProbeBrowseInertUnknown ProbeBrowseState = "inert-unknown"
)

// MarketplaceEntryBrowseProbeState is the PASSIVE browse-time TRI-STATE
// classifier for the read-only GET /api/marketplace projection. The full gate
// (MarketplaceEntryProbePasses / AvailabilityAdmissionEntry) runs
// availabilityProbePasses, which os.Stats every files[] target — fine on the
// operator-initiated install path, but a per-row os.Stat while merely SERVING
// the browse list lets a catalog-provided file-probe path (a slow automount, a
// Windows UNC share) stall opening/refreshing the Catalog and touches an
// external location before the operator ever chooses to install. So this
// classifier NEVER os.Stats a files[] entry and NEVER exec.LookPaths a
// path-shaped token; it evaluates ONLY the bounded bare-binary probe (the SAME
// binaryAvailable owner the install gate uses) and DEFERS everything else.
//
// Algorithm (in order):
//   - non-inert (ready/empty, incl. a nil entry) → ProbeBrowseReady.
//   - nil/empty probe → ProbeBrowseInertBlocked (fail-closed; A6 forbids an inert
//     row without a probe, so this is defensive).
//   - BARE binaries FIRST: ANY bare binary absent from PATH → ProbeBrowseInertBlocked.
//     With the install gate's AND semantics a missing bare binary already proves
//     the probe cannot pass, even on a MIXED row that also carries files[]; the
//     check is bounded (exec.LookPath, no os.Stat), so it is safe on the browse
//     path. Path-shaped binaries are skipped here and deferred below (codex r6
//     finding 4).
//   - then any files[] or file_globs[] present → ProbeBrowseInertUnknown (DEFERRED;
//     never os.Stat / filepath.Glob — the no-touch-on-browse invariant holds for
//     a glob pattern exactly as for a literal path).
//   - then any path-shaped binary (config.IsPathShaped) → ProbeBrowseInertUnknown
//     (DEFERRED; never LookPath a path — defense-in-depth for a manifest that
//     bypassed the strict ValidateProbeValuesNonEmpty path gate).
//   - otherwise (all bare binaries resolved, no files[]/file_globs[], no path-shaped
//     binary) → ProbeBrowseReady.
//
// A mixed binaries+files probe whose bare binaries ALL resolve lands in
// ProbeBrowseInertUnknown via the files[]/file_globs[] rule; if a bare binary is MISSING it is
// ProbeBrowseInertBlocked (the row is definitely not installable yet, so the GUI
// greys it instead of offering an install that immediately 412s). The install-time
// gate still runs the FULL file probe, so an inert-unknown row is still installable
// once the operator clicks install.
func MarketplaceEntryBrowseProbeState(e *MarketplaceEntry) ProbeBrowseState {
	if e == nil {
		return ProbeBrowseReady
	}
	if e.Availability != config.AvailabilityWatch && e.Availability != config.AvailabilityDisabledUntilProbe {
		return ProbeBrowseReady
	}
	p := catalogProbeToConfig(e.InstallProbe)
	// A6 guarantees an inert row declares a non-empty probe; defensively a nil /
	// empty probe is fail-closed (provably not installable yet).
	if p == nil || (len(p.Binaries) == 0 && len(p.Files) == 0 && len(p.FileGlobs) == 0) {
		return ProbeBrowseInertBlocked
	}
	// BARE binaries FIRST (codex r6 finding 4): the install-time probe is AND
	// semantics — EVERY declared binary must resolve AND every file must exist — so a
	// MISSING bare binary already PROVES the probe cannot pass, regardless of any
	// files[] alongside it. Checking it is bounded (exec.LookPath / PATH search, no
	// os.Stat, no external location touched), so it is safe on the browse path. Doing
	// it before the files[]/path-shaped deferral means a mixed row whose bare binary
	// is absent is correctly greyed (inert-blocked) instead of offered as
	// inert-unknown and then immediately 412'd at install. A path-shaped binary is
	// SKIPPED here (it must NOT be LookPath'd as a bare name) and handled by the
	// deferral below.
	for _, bin := range p.Binaries {
		if config.IsPathShaped(bin) {
			continue
		}
		if !binaryAvailable(bin) {
			return ProbeBrowseInertBlocked
		}
	}
	// All BARE binaries resolved. Anything that cannot be decided WITHOUT touching
	// the filesystem / an external location is now DEFERRED to the install gate: a
	// declared files[] LITERAL or file_globs[] PATTERN probe (never os.Stat'd /
	// filepath.Glob'd here — the no-stat/no-glob-on-browse invariant holds for BOTH
	// fields), or a path-shaped binary (never exec.LookPath'd as a bare name —
	// defense-in-depth for a manifest that bypassed the strict
	// ValidateProbeValuesNonEmpty path gate).
	if len(p.Files) > 0 || len(p.FileGlobs) > 0 {
		return ProbeBrowseInertUnknown
	}
	for _, bin := range p.Binaries {
		if config.IsPathShaped(bin) {
			return ProbeBrowseInertUnknown
		}
	}
	return ProbeBrowseReady
}

// vendoredRepoForFinding renders the D-2 advisory's repo reference, falling back
// to a neutral phrase when the manifest omits the (free-form, optional) repo.
func vendoredRepoForFinding(repo string) string {
	if strings.TrimSpace(repo) == "" {
		return "an unspecified source"
	}
	return repo
}

func scopeForPreflight(daemonFilter string) AdmissionScope {
	return AdmissionScope{DaemonFilter: daemonFilter}
}

type manifestPortPool struct {
	pool           *config.PortPool
	daemonTemplate bool
}

func manifestPortPools(m *config.ServerManifest) []manifestPortPool {
	var pools []manifestPortPool
	if m.Kind == config.KindWorkspaceScoped && m.PortPool != nil {
		pools = append(pools, manifestPortPool{pool: m.PortPool})
	}
	if m.DaemonTemplate != nil && m.DaemonTemplate.PortPool != nil {
		pools = append(pools, manifestPortPool{pool: m.DaemonTemplate.PortPool, daemonTemplate: true})
	}
	return pools
}

func portPoolName(p *config.PortPool) string {
	return fmt.Sprintf("port pool %d-%d", p.Start, p.End)
}

func nativeHTTPPoolExternalCeiling() int {
	return 65535 - config.NativeHTTPInternalPortOffset
}

func nativeHTTPPoolOverflows(p *config.PortPool, nativeHTTP bool) bool {
	return nativeHTTP && p != nil && p.End >= p.Start && p.End > nativeHTTPPoolExternalCeiling()
}

func nativeHTTPPoolOverflowReason(p *config.PortPool) string {
	highestUpstream := int64(p.End) + int64(config.NativeHTTPInternalPortOffset)
	return fmt.Sprintf("native-http port pool %d-%d exceeds the external port ceiling %d; the highest upstream port would be %d (external+%d) outside 1..65535", p.Start, p.End, nativeHTTPPoolExternalCeiling(), highestUpstream, config.NativeHTTPInternalPortOffset)
}

func nativeHTTPPoolOverflowFix() string {
	return fmt.Sprintf("Keep the pool's ports at or below %d so external+%d stays within 1..65535.", nativeHTTPPoolExternalCeiling(), config.NativeHTTPInternalPortOffset)
}

func loadRegistryAllocatedPorts() (map[int]bool, error) {
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return nil, fmt.Errorf("resolve registry path: %w", err)
	}
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	return reg.AllocatedPorts(), nil
}

func portPoolFullyAllocatedByRegistry(p *config.PortPool, allocatedPorts map[int]bool) bool {
	if p == nil || p.End < p.Start {
		return false
	}
	for port := p.Start; port <= p.End; port++ {
		if !allocatedPorts[port] {
			return false
		}
	}
	return true
}

func admissionPortPoolFindings(m *config.ServerManifest) []AdmissionFinding {
	pools := manifestPortPools(m)
	if len(pools) == 0 {
		return nil
	}

	var findings []AdmissionFinding
	add := func(id, name, reason, fix string, optional bool) {
		findings = append(findings, AdmissionFinding{ID: id, Name: name, Reason: reason, Fix: fix, Optional: optional})
	}

	nativeHTTP := m.Transport == config.TransportNativeHTTP
	for _, pp := range pools {
		p := pp.pool
		if p == nil || p.End < p.Start {
			continue
		}
		if nativeHTTPPoolOverflows(p, nativeHTTP) {
			add("port-pool-native-overflow", portPoolName(p), nativeHTTPPoolOverflowReason(p), nativeHTTPPoolOverflowFix(), false)
		}
	}

	allocatedPorts, registryErr := loadRegistryAllocatedPorts()
	if registryErr != nil {
		for _, pp := range pools {
			p := pp.pool
			if p == nil || p.End < p.Start {
				continue
			}
			add("port-pool-registry", portPoolName(p), "the workspace registry could not be read or resolved (register reads it before allocating a pool port)", "Fix or remove the corrupt workspaces.yaml registry (register reads it before allocating a pool port).", false)
		}
		return findings
	}

	for _, pp := range pools {
		p := pp.pool
		if p == nil || p.End < p.Start {
			continue
		}
		if nativeHTTPPoolOverflows(p, nativeHTTP) {
			continue
		}
		if portPoolFullyAllocatedByRegistry(p, allocatedPorts) {
			add("port-pool-free", portPoolName(p), "no port in the workspace pool is free for a NEW workspace (registry-allocated by existing workspaces); existing workspaces and reinstall are unaffected", "Free a pool port or widen the pool in the manifest before registering a new workspace.", true)
		}
	}
	return findings
}
