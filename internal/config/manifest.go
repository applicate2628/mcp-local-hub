package config

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mcp-local-hub/internal/secrets"
	"mcp-local-hub/internal/urlredact"

	"gopkg.in/yaml.v3"
)

// Kind enumerates daemon types. Only these values are valid in manifest.kind.
const (
	KindGlobal          = "global"
	KindWorkspaceScoped = "workspace-scoped"
	// KindCompanion is a hub-MANAGED but NON-MCP process (e.g. the excalidraw
	// canvas Express server). It is supervised like a daemon — Job-Object
	// orphan-protection, restart policy, autostart-on-boot, GUI status — but is
	// EXCLUDED from MCP routing, scan/classify, the Servers matrix, and
	// client-config writes (it has no client_bindings, so routing — which is
	// keyed off client_bindings — never sees it; the scan source-filter and the
	// routing/install/migrate sink-guards make the exclusion explicit). Paired
	// with transport=process. Decision:
	// work-items/decisions/2026-06-19-companion-process-manifest-kind.md
	KindCompanion = "companion"
)

// Transport enumerates how the server speaks MCP. Only these are valid.
const (
	TransportNativeHTTP  = "native-http"
	TransportStdioBridge = "stdio-bridge"
	// TransportRemoteHTTP is the G6 remote-HTTPS-endpoint transport.
	// No local daemon spawned; no per-daemon port. The manifest carries
	// `url:` + `headers:` and install writes those directly into client
	// configs after expanding ${secret:KEY} placeholders.
	// Spec: docs/superpowers/specs/2026-05-12-g6-remote-mcp-manifests-design.md
	TransportRemoteHTTP = "remote-http"
	// TransportProcess runs the manifest's command as a raw supervised process
	// with NO MCP host, NO HTTP listener, and NO port bind — the daemon layer
	// just execs it with cmd.Dir = daemons[0].cwd. Companion-only (kind=companion).
	TransportProcess = "process"
)

// Availability is the D-3 catalog-row lifecycle gate enum (Tier-0). Empty/absent
// == AvailabilityReady, the universal current behavior, so every existing
// manifest is unchanged. A "watch" / "disabled-until-probe" row MUST NOT spawn a
// daemon nor write a client config until its InstallProbe passes — enforced in
// the host-state readiness gate (internal/api.AdmissionCheck), NOT here.
//
// Decision: work-items/decisions/2026-06-23-d3-availability-probe.md
const (
	AvailabilityReady              = "ready"                // default; spawns + writes normally (== empty)
	AvailabilityWatch              = "watch"                // inert until probe passes; GUI greys with 'probe to enable'
	AvailabilityDisabledUntilProbe = "disabled-until-probe" // synonym-strength: same inert behavior, stronger label
)

// LicenseStatus enum for VendoredSource.LicenseStatus (D-2, Tier-0). Free-form-
// tolerant at parse, but Validate() rejects an unknown NON-EMPTY value so a typo
// cannot pass the D-4 vetting record silently. Empty is allowed (treated as
// unknown for gate purposes).
//
// Decision: work-items/decisions/2026-06-23-d2-vendored-source.md
const (
	LicenseStatusConfirmed = "confirmed" // LICENSE present + vetted on the real repo (gh API license != null)
	LicenseStatusPending   = "pending"   // not yet vetted — admission gate WARNS but does not block (advisory)
	LicenseStatusUnknown   = "unknown"   // explicitly recorded as unverifiable
)

// NativeHTTPInternalPortOffset is the fixed delta between a native-http
// daemon's external (client-facing) port and the internal port its
// upstream subprocess binds. Lives here so the two independent readers
// — api.Preflight (port-free check at install) and cli/daemon.go
// (subprocess --port flag at runtime) — share a single source of truth.
const NativeHTTPInternalPortOffset = 10000

// Valid LanguageSpec.Transport values. Kept in manifest alongside language so
// the launcher can dispatch on per-language transport without re-probing the
// upstream binary.
const (
	LanguageTransportStdio      = "stdio"       // v1 default: subprocess stdin/stdout wrapped by daemon.NewStdioHost
	LanguageTransportHTTPListen = "http_listen" // reserved (gopls -listen variant)
	LanguageTransportNativeHTTP = "native_http" // reserved
)

// ServerManifest is the parsed form of a `servers/<name>/manifest.yaml` file.
type ServerManifest struct {
	Name string `yaml:"name"`

	// Description is a one-line human-readable summary of what the server
	// does, surfaced in the GUI Catalog ("store") screen. ADDITIVE and
	// OPTIONAL: manifests without it parse and validate unchanged
	// (`omitempty` keeps it out of round-tripped YAML when empty), and
	// Validate()/ValidateStrict() never require it. Free-form metadata,
	// no enum or shape constraint.
	Description string `yaml:"description,omitempty"`

	Kind             string            `yaml:"kind"`
	Transport        string            `yaml:"transport"`
	Command          string            `yaml:"command"`
	BaseArgs         []string          `yaml:"base_args"`
	BaseArgsTemplate []string          `yaml:"base_args_template"`
	Env              map[string]string `yaml:"env"`
	Daemons          []DaemonSpec      `yaml:"daemons"`
	Languages        []LanguageSpec    `yaml:"languages"`
	PortPool         *PortPool         `yaml:"port_pool"`
	IdleTimeoutMin   int               `yaml:"idle_timeout_min"`
	ClientBindings   []ClientBinding   `yaml:"client_bindings"`
	WeeklyRefresh    bool              `yaml:"weekly_refresh"`

	// URL is the remote HTTPS endpoint for TransportRemoteHTTP servers.
	// REQUIRED for transport="remote-http"; REJECTED if set with any
	// other transport. Must parse as https:// with a non-empty host and
	// no URL-embedded credentials (G6 §"Validation rules": plain
	// http:// rejected — plaintext credentials over the wire are out of
	// scope).
	URL string `yaml:"url"`

	// Headers carries HTTP headers sent on every request to the remote
	// endpoint. Values may contain ${secret:KEY} placeholders which
	// are resolved at INSTALL time from the encrypted vault — the
	// manifest stays cleartext-free on disk. REJECTED if set with any
	// transport other than "remote-http".
	Headers map[string]string `yaml:"headers"`

	// RequiredBinaries is free-form metadata listing the external
	// binaries this server expects to find on PATH (e.g. "gdb",
	// "clangd"). Consumed by the Servers-matrix LSP-bridge recognition
	// surface; no Validate() logic is applied here so manifests can
	// declare unrecognized binaries without breaking startup.
	//
	// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md §"Manifest schema additions".
	RequiredBinaries []string `yaml:"required_binaries,omitempty"`

	// RequiredSecrets is the OPT-IN list of vault KEYS that MUST be resolvable
	// before this server installs. Unlike the default "secrets are optional"
	// posture (an unset `secret:` env ref is omitted at spawn so the server
	// reports its own missing-key — see secrets.ResolveMapBestEffort), a key
	// listed here turns into a BLOCKING admission finding when it is not set in
	// the vault, so the install is REFUSED rather than spawning a daemon that
	// hard-exits on startup for the missing credential. ADDITIVE + OPTIONAL:
	// every existing manifest omits it (nil slice → no change). The keys are the
	// vault keys behind the `secret:<key>` env refs, e.g.
	// `required_secrets: [acedata_api_token]` for env
	// ACEDATACLOUD_API_TOKEN: secret:acedata_api_token.
	//
	// Decision: work-items/decisions/2026-06-24-required-secret-install-gate.md
	RequiredSecrets []string `yaml:"required_secrets,omitempty"`

	// DaemonTemplate is the per-workspace dynamic-pool spawn template.
	// REQUIRES kind=workspace-scoped AND transport != remote-http
	// (cross-branch validator gate rejects other combinations). Mutually
	// exclusive with the legacy top-level Daemons list — manifests that
	// migrate to dynamic-pool drop daemons[] and add daemon_template.
	//
	// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.1.
	DaemonTemplate *DaemonTemplate `yaml:"daemon_template,omitempty"`

	// VendoredSource (D-2, Tier-0), when non-nil, declares that this manifest's
	// command/run path comes from a vendored or community-fork source pinned at
	// PinnedRef. ADDITIVE + OPTIONAL: every existing manifest (which omits it)
	// parses and validates byte-identically because the field is a pointer with
	// omitempty — a host with no desktop rows is unaffected. Enforcement (pin
	// presence + non-moving ref + license-status enum): Validate() D-2 gate via
	// validateVendoredAndAvailability.
	VendoredSource *VendoredSource `yaml:"vendored_source,omitempty"`

	// Availability (D-3, Tier-0) is the catalog-row lifecycle gate. Empty/absent
	// == AvailabilityReady (the universal current behavior, so every existing
	// manifest is unchanged). "watch" / "disabled-until-probe" mark a row that
	// MUST NOT spawn a daemon nor write a client config until InstallProbe passes
	// — that inert behavior is enforced by the host-state readiness gate
	// (internal/api.AdmissionCheck), not by this schema gate.
	Availability string `yaml:"availability,omitempty"`

	// InstallProbe (D-3, Tier-0) is the install-detector consulted when
	// Availability != ready. It is metadata describing WHAT the host must have;
	// the probe is EVALUATED by reusing the readiness gate primitives as a
	// dry-run (internal/api.availabilityProbePasses composing binaryAvailable +
	// entryScriptStatus). It carries NO new detection logic here.
	InstallProbe *AvailabilityProbe `yaml:"install_probe,omitempty"`
}

// VendoredSource is the D-2 vendored/community-fork provenance + pin descriptor.
// It is metadata-only: nothing here spawns a process or writes a client config.
// PinnedRef is the load-bearing safety field — a vendored source MUST be pinned
// to an immutable ref (full 40-hex SHA preferred; an annotated tag accepted),
// never a moving branch like main/master/HEAD.
//
// Decision: work-items/decisions/2026-06-23-d2-vendored-source.md
type VendoredSource struct {
	Repo          string `yaml:"repo,omitempty"`           // upstream/fork repo URL or owner/name slug (free-form metadata)
	PinnedRef     string `yaml:"pinned_ref,omitempty"`     // immutable git ref — 40-hex SHA (preferred) or tag; REQUIRED when VendoredSource is declared
	InstallCmd    string `yaml:"install_cmd,omitempty"`    // human-readable vendor/build command (e.g. "uv pip install ."); documentation only — NOT executed by mcphub
	RunCmd        string `yaml:"run_cmd,omitempty"`        // human-readable run hint; documentation only — the real launcher stays command/base_args
	LicenseStatus string `yaml:"license_status,omitempty"` // one of LicenseStatus* constants; vetting outcome recorded by D-4 at admission
}

// AvailabilityProbe is the D-3 install-detector descriptor for a watch /
// disabled-until-probe row. It declares the host signal that flips the row to
// enabled. It carries NO new detection logic — each field maps onto an EXISTING
// readiness probe primitive (binaryAvailable / entryScriptStatus / a glob over
// entryScriptStatus). The probe is satisfied iff EVERY declared signal is present
// (AND semantics across all three fields).
//
// files[] vs file_globs[] is an EXPLICIT-INTENT split (codex catalog finding,
// glob-vs-literal ambiguity): a files[] entry is a LITERAL path stat'd verbatim
// and NEVER globbed (so a real install path containing a glob metacharacter —
// "/opt/Foo*/marker", "Foo [Beta]" — resolves to itself and a sibling like
// "/opt/FooBeta/marker" can never satisfy it). A file_globs[] entry is the
// OPT-IN pattern path: it is filepath.Glob-expanded and any matching regular file
// satisfies it (where Mathcad-Prime-*/Live-*/R*/v* version globs live). One field
// per intent removes the ambiguity of inferring glob-vs-literal from a single
// files[] value.
//
// Decision: work-items/decisions/2026-06-23-d3-availability-probe.md
type AvailabilityProbe struct {
	Binaries  []string `yaml:"binaries,omitempty"`   // each must resolve via the readiness binaryAvailable() owner (PATH/toolchain-dir) — e.g. ["matlab"]
	Files     []string `yaml:"files,omitempty"`      // each LITERAL path must exist as a regular file via the readiness entryScriptStatus() owner — NEVER globbed — e.g. an install marker
	FileGlobs []string `yaml:"file_globs,omitempty"` // each GLOB PATTERN must filepath.Glob-expand to at least one regular file — the opt-in version-agnostic path (e.g. "…\\Live *\\…\\Ableton Live *.exe")
}

type DaemonSpec struct {
	Name      string   `yaml:"name"`
	Context   string   `yaml:"context"`
	Port      int      `yaml:"port"`
	ExtraArgs []string `yaml:"extra_args"`

	// Cwd, when non-empty, is the working directory the supervisor sets
	// as the daemon subprocess's cwd (cmd.Dir) at spawn time. Empty means
	// inherit mcphub's own cwd (the prior behavior). MUST be an absolute
	// path — a relative cwd has no stable base across the scheduler /
	// supervisor / interactive launch surfaces, so Validate() rejects it.
	// ${ENV} / ${HOME} tokens are expanded at parse time (like base_args /
	// env) so a shipped manifest stays portable.
	Cwd string `yaml:"cwd,omitempty" json:"cwd,omitempty"`
}

// DaemonTemplate describes a per-workspace daemon spawn template for the
// dynamic-pool branch of kind=workspace-scoped. Mutually exclusive with
// the legacy ServerManifest.Daemons list (validator rejects both-present).
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.1.
type DaemonTemplate struct {
	Context           string    `yaml:"context"`
	PortPool          *PortPool `yaml:"port_pool"`           // reuse existing PortPool{Start,End}
	ExtraArgsTemplate []string  `yaml:"extra_args_template"` // each arg may contain ${workspace.path}
}

type LanguageSpec struct {
	Name           string   `yaml:"name"`
	Backend        string   `yaml:"backend"`   // "mcp-language-server" or "gopls-mcp"
	Transport      string   `yaml:"transport"` // "stdio" (default) | "http_listen" | "native_http"
	LspCommand     string   `yaml:"lsp_command"`
	ExtraFlags     []string `yaml:"extra_flags"`
	ProjectMarkers []string `yaml:"project_markers,omitempty"`

	// RequiredBinaries is free-form metadata listing the external
	// binaries this language backend expects to find on PATH (e.g.
	// "clangd", "pyright-langserver"). Symmetric with the server-level
	// field; consumed by the Servers-matrix LSP-bridge recognition
	// surface.
	RequiredBinaries []string `yaml:"required_binaries,omitempty"`
}

const maxTCPPort = 65535

type PortPool struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

func validatePortPool(label string, pool *PortPool) error {
	if pool == nil {
		return fmt.Errorf("%s is required (start/end)", label)
	}
	if pool.Start <= 0 || pool.End < pool.Start || pool.End > maxTCPPort {
		return fmt.Errorf("%s must have start>0, end>=start, and end<=%d (got {%d,%d})", label, maxTCPPort, pool.Start, pool.End)
	}
	return nil
}

type ClientBinding struct {
	Client  string `yaml:"client"`
	Daemon  string `yaml:"daemon"`
	URLPath string `yaml:"url_path"`
}

// ParseManifest reads YAML from r and returns a validated ServerManifest.
// Returns an error if required fields are missing or kind/transport values
// are unknown.
//
// Environment expansion: ${USERPROFILE}, ${HOME}, and other ${ENV} tokens
// in BaseArgs and Env values are expanded against the host environment
// at parse time (via os.ExpandEnv). This keeps shipped manifests portable
// — the user's home path doesn't need to be hard-coded in the YAML.
//
// codex bot r5 P2 closure (PR #169): we cannot distinguish "key
// absent" from "key explicit-empty/null" after decoding into a Go
// struct, but we CAN distinguish them by also decoding into a
// `map[string]any` and checking key presence. ParseManifest enforces
// the transport-scoped field gates (url/headers are remote-http
// only) BEFORE returning so an explicit `url:` or `headers: null`
// on a non-remote-http transport gets rejected even though Go's
// decoded value is the zero form.
func ParseManifest(r io.Reader) (*ServerManifest, error) {
	raw, readErr := io.ReadAll(r)
	if readErr != nil {
		return nil, fmt.Errorf("read manifest: %w", readErr)
	}
	var m ServerManifest
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}
	// Second pass to detect key presence (yaml.v3 collapses
	// `headers:` / `headers: null` / `headers: {}` into the same
	// Go value when decoded into a struct field).
	var keyed map[string]any
	if err := yaml.Unmarshal(raw, &keyed); err != nil {
		return nil, fmt.Errorf("yaml decode (keyed pass): %w", err)
	}
	_, urlMentioned := keyed["url"]
	_, headersMentioned := keyed["headers"]
	// Reject remote-http-only keys mentioned under non-remote
	// transports. Done HERE (parse time) rather than in Validate()
	// because Validate's input is a Go struct that has already
	// lost the key-presence info. The transport-scoped field-gate
	// errors from Validate() still cover programmatic callers that
	// construct a ServerManifest directly (with non-zero URL or
	// non-nil Headers); this guard adds the YAML-level signal.
	if m.Transport != TransportRemoteHTTP {
		if urlMentioned {
			return nil, fmt.Errorf("manifest %s: url is only valid with transport=remote-http (got transport=%q; remove the url: key)", m.Name, m.Transport)
		}
		if headersMentioned {
			return nil, fmt.Errorf("manifest %s: headers is only valid with transport=remote-http (got transport=%q; remove the headers: key)", m.Name, m.Transport)
		}
	}
	// Symmetric guard (codex bot r9 P2 closure on PR #169): reject
	// local-subprocess keys mentioned under transport=remote-http.
	// The Go-struct Validate() can only spot non-zero values, so
	// `command:` (bare) / `command: null` / `command: ""` would
	// otherwise pass; same for `base_args:` / `env:` / `daemons:`.
	// Key-presence detection requires the YAML-level second pass.
	if m.Transport == TransportRemoteHTTP {
		for _, k := range []string{
			"command", "base_args", "base_args_template", "env",
			"daemons", "languages", "port_pool", "idle_timeout_min",
		} {
			if _, mentioned := keyed[k]; mentioned {
				return nil, fmt.Errorf("manifest %s: transport=remote-http rejects %s: (no local subprocess / no per-daemon port; remove the %s key)", m.Name, k, k)
			}
		}
	}
	var missing []string
	for i, a := range m.BaseArgs {
		expanded, miss := expandEnvCrossPlatform(a)
		m.BaseArgs[i] = expanded
		missing = append(missing, miss...)
	}
	for k, v := range m.Env {
		expanded, miss := expandEnvCrossPlatform(v)
		m.Env[k] = expanded
		for _, name := range miss {
			missing = append(missing, k+":"+name)
		}
	}
	// Expand ${ENV} / ${HOME} tokens in each daemon's cwd at parse time so
	// a shipped manifest can carry e.g. cwd: "${HOME}/.cache/foo" without
	// hard-coding the operator's home. Mirrors the base_args / env handling
	// above; a referenced-but-unset variable is reported the same way.
	for i := range m.Daemons {
		if m.Daemons[i].Cwd == "" {
			continue
		}
		expanded, miss := expandEnvCrossPlatform(m.Daemons[i].Cwd)
		m.Daemons[i].Cwd = expanded
		for _, name := range miss {
			missing = append(missing, fmt.Sprintf("daemons[%d].cwd:%s", i, name))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("manifest references unresolved environment variable(s): %s (set them before invoking mcphub, or remove the ${...} reference from the manifest)",
			strings.Join(missing, ", "))
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// CatalogFields is the projection of a manifest used by the GUI Catalog
// ("store") screen: just the display-relevant top-level scalars. It
// deliberately does NOT carry the full ServerManifest so the catalog
// surface stays decoupled from the spawn/validation fields.
type CatalogFields struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Kind        string `yaml:"kind" json:"kind"`
}

// ParseCatalogFields reads YAML from r and extracts only the catalog
// display scalars (name/description/kind). Unlike ParseManifest it does
// NOT expand ${ENV} tokens, resolve secret: refs, or run Validate() — a
// catalog projection must succeed for every shipped manifest regardless
// of whether the host has the manifest's env/secrets set (e.g. memory's
// ${HOME} or wolfram's secret:wolfram_app_id). It also tolerates unknown
// keys so it never breaks as the manifest schema grows. name is the only
// required field; description/kind are returned empty when absent.
func ParseCatalogFields(r io.Reader) (CatalogFields, error) {
	raw, readErr := io.ReadAll(r)
	if readErr != nil {
		return CatalogFields{}, fmt.Errorf("read manifest: %w", readErr)
	}
	var c CatalogFields
	// No KnownFields(true): the catalog projection intentionally ignores
	// every field except these three, so unknown keys must not error.
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return CatalogFields{}, fmt.Errorf("yaml decode (catalog fields): %w", err)
	}
	if strings.TrimSpace(c.Name) == "" {
		return CatalogFields{}, fmt.Errorf("manifest: name is required")
	}
	return c, nil
}

// expandEnvCrossPlatform expands $VAR and ${VAR} tokens against the host
// environment. Returns the expanded string plus a list of variable
// names that were referenced but not set — callers can decide whether
// to treat empty expansion as an error or accept the empty value.
//
// Cross-platform niceness: ${HOME} on Windows (where HOME is typically
// unset) falls back to USERPROFILE, and vice-versa, so the same
// manifest works under bash, cmd.exe, and PowerShell without dual
// templating. Both unset → the name is reported as missing.
func expandEnvCrossPlatform(s string) (string, []string) {
	var missing []string
	expanded := os.Expand(s, func(name string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		if name == "HOME" {
			if v := os.Getenv("USERPROFILE"); v != "" {
				return v
			}
		}
		if name == "USERPROFILE" {
			if v := os.Getenv("HOME"); v != "" {
				return v
			}
		}
		missing = append(missing, name)
		return ""
	})
	return expanded, missing
}

// containsWorkspacePathTokenInArgs scans each element of args for the
// literal substring "${workspace.path}". Returns true on the first match.
// Substring-match (not exact-equality) so operators can write composite
// args like "--project=${workspace.path}/src". Internal helper, lowercase
// — only the validator uses it.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.1.
func containsWorkspacePathTokenInArgs(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, WorkspacePathToken) {
			return true
		}
	}
	return false
}

// ArgsContainContextFlag reports whether args carries a --context flag in any
// supported spelling (`--context value` or `--context=value`). A daemon_template
// manifest must NOT carry --context in base_args / extra_args_template: the
// context comes solely from daemon_template.context and is appended at spawn, so
// a token here would duplicate the flag (bot PR #246 r2 P2).
func ArgsContainContextFlag(args []string) bool {
	for _, a := range args {
		if a == "--context" || strings.HasPrefix(a, "--context=") {
			return true
		}
	}
	return false
}

// movingGitRefs is the conservative set of well-known MOVING branch names a
// vendored source MUST NOT pin to (D-2 rule A2). A bare branch name is the exact
// failure D-4 forbids — the fork content can change underneath the pin. A 40-hex
// SHA or a vX.Y.Z tag is immutable and passes. We deliberately do NOT attempt
// full SHA-shape validation (a short SHA or unusual tag is legitimate); only
// these known moving-branch names are rejected, conservative by design.
var movingGitRefs = map[string]struct{}{
	"main": {}, "master": {}, "head": {}, "latest": {},
	"trunk": {}, "develop": {}, "dev": {},
}

// IsMovingGitRef reports whether ref is a MOVING (non-immutable) git ref — one
// whose commit can change underneath the pin (D-2 rule A2). It is the single
// predicate the D-2 pin gate uses, shared by the manifest gate and the
// catalog-entry mirror so the two cannot diverge.
//
// Rather than enumerate the (open-ended) set of BAD ref forms — the approach
// that repeatedly let a new shape slip through (a non-listed branch name, then
// a degenerate prefix-only ref, then a bare "<remote>/<branch>" shorthand like
// "origin/main") — it states the COMPLETE INVERTIBLE rule and rejects everything
// else. A ref is IMMUTABLE (returns false) iff it matches EXACTLY ONE of:
//
//	(a) a hex object name (SHA): ^[0-9a-fA-F]{7,40}$ — no slash;
//	(b) a fully-qualified tag: starts with "refs/tags/" (slashes inside the tag
//	    are fine, e.g. "refs/tags/release/2026");
//	(c) a bare tag name: contains NO slash AND is NOT a well-known moving branch
//	    name (movingGitRefs, case-insensitive).
//
// Everything else is MOVING (returns true), specifically: empty/whitespace; a
// "refs/heads/" or "refs/remotes/" branch/remote-tracking ref (mutable regardless
// of the bare name); ANY slash-containing value that is not a "refs/tags/" tag
// (this is what catches "origin/main", "upstream/develop", any "<remote>/<branch>"
// shorthand, and any other non-tag slash form — operators must qualify a
// slash-containing tag as "refs/tags/<tag>" or use a SHA); and a bare name that
// IS a well-known moving branch. The rule keys on IMMUTABILITY (slash-presence +
// tag/SHA shape) directly, not on branch-name normalization, so no enumerate-bad
// helper is needed.
func IsMovingGitRef(ref string) bool {
	r := strings.TrimSpace(ref)
	if r == "" {
		return true
	}
	// (b) fully-qualified tag — immutable, but ONLY when the tag name after the
	// "refs/tags/" prefix is non-blank. A degenerate "refs/tags/" (or
	// "refs/tags/   ") carries no tag and is not an immutable pin, so it must fall
	// through to MOVING (return true) rather than be accepted as a tag.
	if strings.HasPrefix(r, "refs/tags/") {
		if strings.TrimSpace(strings.TrimPrefix(r, "refs/tags/")) != "" {
			return false
		}
		return true
	}
	if strings.ContainsRune(r, '/') {
		// Any other slash-containing ref (refs/heads/*, refs/remotes/*,
		// origin/main, upstream/develop, feature/x, …) is NOT an immutable pin.
		return true
	}
	// No slash from here on.
	// (a) hex SHA — immutable.
	if isHexObjectName(r) {
		return false
	}
	// (c) bare name: immutable iff NOT a well-known moving branch.
	_, moving := movingGitRefs[strings.ToLower(r)]
	return moving
}

// IsPathShaped reports whether token LEXICALLY looks like a filesystem path
// rather than a bare PATH-searchable command name. It is the single lexical
// owner of the "this is a path, not a command" taxonomy, shared by the
// install_probe validator (a path-shaped binaries[] entry is rejected — binaries
// are exec.LookPath'd, not stat'd) and the browse classifier (a path-shaped
// binary is deferred rather than LookPath'd). A token is path-shaped iff it:
//
//	(a) contains a forward slash '/' or a backslash '\\' (covers POSIX paths,
//	    Windows paths, AND UNC "\\\\host\\share"); OR
//	(b) starts with a Windows drive-letter prefix "<letter>:" (e.g. "C:tools");
//	    OR
//	(c) begins with '~' (a shell home-dir reference like "~/tool").
//
// It is PLATFORM-NEUTRAL and PURELY LEXICAL — it never touches filepath.* (whose
// separator set is GOOS-dependent) and never touches the filesystem, so a
// catalog authored on one OS classifies identically on every host. A bare name
// like "matlab" or "go" is NOT path-shaped.
func IsPathShaped(token string) bool {
	if token == "" {
		return false
	}
	if strings.ContainsAny(token, `/\`) {
		return true
	}
	if token[0] == '~' {
		return true
	}
	return hasDriveLetterPrefix(token)
}

// IsAbsolutePathShape reports whether token LEXICALLY looks like an ABSOLUTE
// filesystem path on SOME host OS, GOOS-independently. It is the single lexical
// owner used by the install_probe files[] gate so a catalog files[] entry that
// is absolute on the host it targets (e.g. "C:\\marker" for a Windows row, or
// "/opt/marker" for a POSIX row) is ACCEPTED at parse/validate time on ANY build
// platform — filepath.IsAbs would wrongly reject "C:\\marker" on a linux build
// and "/opt/marker" on a windows build, making a cross-platform registry
// host-OS-specific. A token is absolute-path-shaped iff it:
//
//	(a) starts with a Windows drive-letter ABSOLUTE prefix "<letter>:\\" or
//	    "<letter>:/" (drive letter + ':' + a separator); OR
//	(b) begins with '/' (POSIX absolute); OR
//	(c) begins with '\\' (Windows absolute or UNC "\\\\host\\share").
//
// The drive prefix MUST be followed by a separator: "C:\\marker" / "C:/marker"
// are absolute, but a bare "C:marker" (drive letter + ':' + no separator) is
// Windows DRIVE-RELATIVE — it resolves against the current directory ON THAT
// DRIVE, so os.Stat'ing it would depend on the process CWD exactly like a plain
// relative path. It is therefore NOT absolute-path-shaped and falls to the
// files[] relative-path reject in ValidateProbeValuesNonEmpty. (It IS still
// path-shaped per IsPathShaped, so a drive-relative binaries[] entry is rejected
// as a path, not LookPath'd.)
//
// Relative forms ("./marker", "marker", "sub/marker", "C:marker") and '~'-home
// references ("~/marker") are NOT absolute-path-shaped, so the validator still
// rejects them (preserving the CWD-protection invariant). Real host-OS
// existence/regular-file resolution is intentionally DEFERRED to the
// install/readiness probe (entryScriptStatus), which os.Stat's the path on the
// actual host.
func IsAbsolutePathShape(token string) bool {
	if token == "" {
		return false
	}
	if token[0] == '/' || token[0] == '\\' {
		return true
	}
	return hasDriveLetterAbsolutePrefix(token)
}

// hasDriveLetterPrefix reports whether token begins with a Windows drive-letter
// prefix "<ASCII-letter>:" (e.g. "C:", "d:tools"). Lexical and GOOS-independent.
// This is the PATH-SHAPE predicate (IsPathShaped): a bare "C:marker" still IS a
// path (drive-relative), so a binaries[] entry of that shape is rejected as a
// path rather than LookPath'd. The ABSOLUTE classification is stricter — see
// hasDriveLetterAbsolutePrefix.
func hasDriveLetterPrefix(token string) bool {
	if len(token) < 2 || token[1] != ':' {
		return false
	}
	c := token[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// hasDriveLetterAbsolutePrefix reports whether token begins with a Windows
// drive-letter ABSOLUTE prefix "<ASCII-letter>:\\" or "<ASCII-letter>:/" — a
// drive letter, a colon, AND a path separator (e.g. "C:\\tools", "d:/rel"). It
// is STRICTER than hasDriveLetterPrefix: a bare "C:marker" (no separator) is
// Windows drive-RELATIVE (resolved against the CWD on that drive), so it is NOT
// absolute. Lexical and GOOS-independent.
func hasDriveLetterAbsolutePrefix(token string) bool {
	if len(token) < 3 || token[1] != ':' {
		return false
	}
	c := token[0]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
		return false
	}
	return token[2] == '\\' || token[2] == '/'
}

// isHexObjectName reports whether s is a git object-name (SHA) shape: 7..40 hex
// digits and nothing else. A full SHA-1 is 40 hex; git accepts unambiguous
// abbreviations down to ~7, so we admit that range. Callers guarantee s has no
// slash before reaching here.
func isHexObjectName(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// validateVendoredAndAvailability is the D-2 + D-3 schema/cross-field gate
// (Tier-0). It is PURE: it inspects only the manifest struct, never PATH / ports
// / vault / host state. Host-install detection (the D-3 probe) is evaluated
// separately in internal/api.AdmissionCheck as a readiness dry-run. This helper
// is additive — it touches no existing companion/remote-http/workspace-scoped
// branch — and short-circuits on the nil/empty fields every existing manifest
// carries, so a host with no desktop rows is unaffected.
func (m *ServerManifest) validateVendoredAndAvailability() error {
	// D-2: vendored/community-fork provenance + pin.
	if m.VendoredSource != nil {
		vs := m.VendoredSource
		// A1 (load-bearing): a vendored source MUST be pinned to a non-empty ref.
		ref := strings.TrimSpace(vs.PinnedRef)
		if ref == "" {
			return fmt.Errorf("manifest %s: vendored_source requires a non-empty pinned_ref (pin to a 40-hex SHA or tag; a moving branch like main/HEAD is rejected)", m.Name)
		}
		// A2: a well-known moving branch name is not an immutable pin. Both a
		// bare branch name ("main") and a branch-qualified ref
		// ("refs/heads/main", "refs/remotes/origin/main") are rejected;
		// "refs/tags/<tag>" is an immutable tag and passes.
		if IsMovingGitRef(ref) {
			return fmt.Errorf("manifest %s: vendored_source requires a non-empty pinned_ref (pin to a 40-hex SHA or tag; a moving branch like main/HEAD is rejected)", m.Name)
		}
		// A3: license_status enum (empty allowed == unknown for gate purposes).
		switch vs.LicenseStatus {
		case "", LicenseStatusConfirmed, LicenseStatusPending, LicenseStatusUnknown:
		default:
			return fmt.Errorf("manifest %s: vendored_source.license_status %q is not one of %q|%q|%q", m.Name, vs.LicenseStatus, LicenseStatusConfirmed, LicenseStatusPending, LicenseStatusUnknown)
		}
	}

	// D-3: availability lifecycle gate + install probe.
	switch m.Availability {
	case "", AvailabilityReady, AvailabilityWatch, AvailabilityDisabledUntilProbe:
	default:
		return fmt.Errorf("manifest %s: availability %q must be %q|%q|%q", m.Name, m.Availability, AvailabilityReady, AvailabilityWatch, AvailabilityDisabledUntilProbe)
	}
	inert := m.Availability == AvailabilityWatch || m.Availability == AvailabilityDisabledUntilProbe
	// A5: a probe on a ready/empty row is dead config (symmetric to the existing
	// remote-http field-gate idiom that rejects fields meaningless for the mode).
	if m.InstallProbe != nil && !inert {
		return fmt.Errorf("manifest %s: install_probe is only meaningful with availability=watch|disabled-until-probe", m.Name)
	}
	// A6: a watch/disabled row needs a NON-EMPTY probe (binaries, files, or
	// file_globs) — a watch row with no probe can never become ready.
	if inert {
		if m.InstallProbe == nil || (len(m.InstallProbe.Binaries) == 0 && len(m.InstallProbe.Files) == 0 && len(m.InstallProbe.FileGlobs) == 0) {
			return fmt.Errorf("manifest %s: availability=%q requires a non-empty install_probe (binaries, files, or file_globs) — a watch row with no probe can never become ready", m.Name, m.Availability)
		}
		// A7: each DECLARED probe value must be a non-empty (trimmed) token. A
		// blank binary/file slot passes the length check above but the runtime
		// probe then looks up an empty name and can never pass — a permanently
		// disabled row with a confusing error. Reject it up front instead.
		if err := ValidateProbeValuesNonEmpty(m.InstallProbe, "manifest "+m.Name); err != nil {
			return err
		}
	}
	return nil
}

// validateRequiredSecretsBackEnv enforces the required_secrets integrity rule on
// a manifest: each key in RequiredSecrets MUST appear as a `secret:<key>` value
// in Env. It is the manifest-side mirror of the catalog authoring guard
// (validateCatalogVendoredAndAvailability), so a persisted or hand-edited
// manifest gets the SAME protection a catalog-drafted one does — a typo or stale
// key in required_secrets can neither block on a phantom credential the daemon
// never reads, nor silently pass the install gate while the REAL env secret stays
// unset (which would crash-loop the daemon on startup). Empty entries are rejected
// so a stray "" cannot become a permanently-unblockable phantom requirement.
// PURE: it inspects only the manifest struct (no PATH / vault / host state). A
// manifest with no required_secrets is a no-op.
func (m *ServerManifest) validateRequiredSecretsBackEnv() error {
	if len(m.RequiredSecrets) == 0 {
		return nil
	}
	envSecretKeys := map[string]bool{}
	for _, v := range m.Env {
		if strings.HasPrefix(v, "secret:") {
			envSecretKeys[strings.TrimPrefix(v, "secret:")] = true
		}
	}
	for _, key := range m.RequiredSecrets {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("manifest %s: required_secrets contains an empty key", m.Name)
		}
		if !envSecretKeys[key] {
			return fmt.Errorf("manifest %s: required_secrets key %q has no matching secret:%s env ref (a required secret must back an env value the server actually reads)", m.Name, key, key)
		}
	}
	return nil
}

// ValidateProbeValuesNonEmpty is the single-owner validator for install_probe
// values, shared by the manifest gate and the catalog-entry mirror (via
// catalogProbeToConfig) so both produce the same diagnostic shape. label
// prefixes the error. A nil probe is a no-op (the caller's A6 check owns the
// "needs a probe" rule). It enforces three rules per declared value, in this
// order (validate/normalize first, then the path-shape check on files):
//
//  1. NON-EMPTY: a blank (empty / whitespace-only) entry is rejected — the
//     runtime probe would look up an empty name and can never pass (a confusing
//     permanently-disabled row).
//  2. NO SURROUNDING WHITESPACE: a value with leading/trailing whitespace (e.g.
//     "go ") is rejected. The non-empty check above trims before testing, so a
//     padded-but-non-blank value would otherwise slip past, yet the runtime
//     probe passes the ORIGINAL padded token to exec.LookPath / os.Stat and the
//     row never enables even when the tool is installed (invisible-whitespace
//     permanent-disable). Fail loud on the malformed value instead.
//  3. BARE binaries[] (not a path): each binaries[] entry must be a bare
//     PATH-searchable command name, NOT a path. binaries[] are resolved via
//     exec.LookPath (PATH search), so a path-shaped value (e.g. "/net/slow/tool",
//     "C:\\tools\\x.exe", a UNC "\\\\host\\share\\x", "./tool", "bin/tool",
//     "~/tool") is a category error: LookPath would treat the slash/drive form as
//     a literal path or never resolve it, so the row silently never enables. Use
//     files[] for a fixed path instead. The path-shape taxonomy is the single
//     lexical owner config.IsPathShaped (platform-neutral).
//  4. ABSOLUTE files[] PATHS: each files[] entry must be an absolute-path SHAPE on
//     SOME host OS (IsAbsolutePathShape — drive-letter "C:\\…", leading "/", or
//     leading "\\" incl UNC). A relative file-probe path is os.Stat'd as-is by the
//     runtime probe, so the gate would depend on the GUI/CLI process CWD — an
//     unrelated "./marker" in one dir would enable the row while the same manifest
//     stays blocked from another dir. We use the GOOS-INDEPENDENT lexical shape
//     (NOT filepath.IsAbs, whose result is host-OS-specific) so a cross-platform
//     registry that declares "C:\\marker" or "/opt/marker" parses identically on
//     every build platform; the real host-OS stat (existence + regular-file) is
//     intentionally deferred to the runtime entryScriptStatus owner at
//     install/readiness.
//
//     A files[] entry is a LITERAL path stat'd VERBATIM by the runtime probe
//     (entryScriptStatus) — it is NEVER globbed. Metacharacters (`*` / `?` / `[`)
//     in a files[] value are therefore treated literally (a directory really named
//     "Foo*" or "Foo [Beta]"); they are NOT rejected here (IsAbsolutePathShape keys
//     only on the absolute PREFIX), but they do NOT make the path a pattern. Use
//     file_globs[] for an intentional version-agnostic pattern.
//
//  5. ABSOLUTE file_globs[] PATTERNS: each file_globs[] entry is the OPT-IN
//     version-agnostic glob (e.g. "C:\\…\\Live *\\…\\Ableton Live *.exe" matching
//     Live 11/12). The runtime probe (globProbeMatches) filepath.Glob-expands it, so
//     a SHARED cross-host catalog can declare ONE pattern instead of a frozen host
//     path. A glob pattern is STILL an absolute path (with wildcards in the leaf
//     segment(s)), so it must satisfy the SAME non-empty / no-surrounding-whitespace
//     / absolute-path-shape rules as a files[] literal — only the EXPANSION at the
//     runtime probe differs. The metacharacters are explicitly ALLOWED here (that is
//     the field's purpose); they share the absolute PREFIX with the literal they
//     generalize.
func ValidateProbeValuesNonEmpty(p *AvailabilityProbe, label string) error {
	if p == nil {
		return nil
	}
	for i, bin := range p.Binaries {
		if strings.TrimSpace(bin) == "" {
			return fmt.Errorf("%s: install_probe.binaries[%d] is empty — every declared probe value must be a non-empty name", label, i)
		}
		if strings.TrimSpace(bin) != bin {
			return fmt.Errorf("%s: install_probe.binaries[%d] %q has leading/trailing whitespace — the runtime probe looks up the value verbatim, so a padded name never resolves on PATH; remove the surrounding whitespace", label, i, bin)
		}
		if IsPathShaped(bin) {
			return fmt.Errorf("%s: install_probe.binaries[%d] %q is a path, not a PATH-searchable name; a binary probe must be a bare command name (use files[] for a fixed path)", label, i, bin)
		}
	}
	for i, f := range p.Files {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("%s: install_probe.files[%d] is empty — every declared probe value must be a non-empty path", label, i)
		}
		if strings.TrimSpace(f) != f {
			return fmt.Errorf("%s: install_probe.files[%d] %q has leading/trailing whitespace — the runtime probe stats the value verbatim, so a padded path never resolves; remove the surrounding whitespace", label, i, f)
		}
		if !IsAbsolutePathShape(f) {
			return fmt.Errorf("%s: install_probe.files[%d] %q must be an absolute path — a relative file probe is stat'd against the process working directory, so the gate would depend on which directory mcphub runs from", label, i, f)
		}
	}
	for i, g := range p.FileGlobs {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("%s: install_probe.file_globs[%d] is empty — every declared probe value must be a non-empty glob pattern", label, i)
		}
		if strings.TrimSpace(g) != g {
			return fmt.Errorf("%s: install_probe.file_globs[%d] %q has leading/trailing whitespace — the runtime probe globs the value verbatim, so a padded pattern never matches; remove the surrounding whitespace", label, i, g)
		}
		if !IsAbsolutePathShape(g) {
			return fmt.Errorf("%s: install_probe.file_globs[%d] %q must be an absolute path pattern — a relative glob is expanded against the process working directory, so the gate would depend on which directory mcphub runs from", label, i, g)
		}
	}
	return nil
}

// Validate checks required fields and enum values. Called automatically by ParseManifest.
//
// Validate is COMPAT mode for the '__'-in-name policy: structural fields
// are enforced but '__'-substring names are accepted silently. The
// compat path is used by startup inventory + manifest listing reads so
// legacy '__'-named manifests stay readable. See ValidateStrict for the
// mutation-path gate that rejects them.
//
// G4 §"Pre-gate" (docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md).
// ReservedGUIPort is the mcphub GUI/router listener's default port. A server
// manifest daemon must NEVER declare it: the GUI binds it, so a daemon sharing
// the port collides with the GUI listener. This is the static "disjoint
// GUI-vs-daemon" planning guard enforced at manifest validation (DM-2);
// runtime collisions against a non-default operator-configured GUI port are
// separately caught by install Preflight. Keep in sync with the api band map
// in internal/api/global_port_alloc.go (which documents 9125 as the GUI port
// carved OUTSIDE every daemon band) and the settings default in
// internal/api/settings_registry.go.
const ReservedGUIPort = 9125

// validateCompanion validates a kind=companion / transport=process manifest: a
// hub-managed NON-MCP process. It REQUIRES command + exactly one daemons[] entry
// carrying the working directory (cwd — the bug the companion feature fixes: the
// process must run from its package dir), and REJECTS every MCP-shaped field
// (client_bindings, daemon_template, languages, port_pool, url, headers) so a
// companion can never be mistaken for — or routed as — an MCP server.
func (m *ServerManifest) validateCompanion() error {
	if m.Command == "" {
		return fmt.Errorf("manifest %s: kind=companion requires command (the process to run)", m.Name)
	}
	if len(m.ClientBindings) != 0 {
		return fmt.Errorf("manifest %s: kind=companion rejects client_bindings (a companion is a non-MCP process, never routed to clients)", m.Name)
	}
	if m.DaemonTemplate != nil {
		return fmt.Errorf("manifest %s: kind=companion rejects daemon_template (no dynamic pool; declare one fixed daemons[] entry)", m.Name)
	}
	if len(m.Languages) != 0 {
		return fmt.Errorf("manifest %s: kind=companion rejects languages[] (not a workspace LSP proxy)", m.Name)
	}
	if m.PortPool != nil {
		return fmt.Errorf("manifest %s: kind=companion rejects port_pool (no MCP ports allocated)", m.Name)
	}
	if m.URL != "" {
		return fmt.Errorf("manifest %s: kind=companion rejects url (only valid with transport=remote-http)", m.Name)
	}
	if m.Headers != nil {
		return fmt.Errorf("manifest %s: kind=companion rejects headers (only valid with transport=remote-http)", m.Name)
	}
	// A companion has NO per-workspace materialization — the launch path
	// (cli/daemon.go) builds childArgs from base_args + the daemon's extra_args
	// VERBATIM and never expands a *_template. Accepting a template would let the
	// manifest install successfully while silently dropping those args at spawn
	// (Codex #381). Reject both template surfaces.
	if len(m.BaseArgsTemplate) != 0 {
		return fmt.Errorf("manifest %s: kind=companion rejects base_args_template (no per-workspace materialization; use base_args — a template is silently dropped at spawn)", m.Name)
	}
	if len(m.Daemons) != 1 {
		return fmt.Errorf("manifest %s: kind=companion requires exactly one daemons[] entry carrying the working directory (got %d)", m.Name, len(m.Daemons))
	}
	d := m.Daemons[0]
	if d.Name == "" {
		return fmt.Errorf("manifest %s: kind=companion daemons[0].name is required", m.Name)
	}
	// cwd is REQUIRED for a companion — the canvas process must run from its
	// package directory (it writes cwd-relative files + serves cwd-relative
	// assets). Parse already ${ENV}-expanded it; the companion returns before the
	// global-branch absolute-cwd check, so enforce both presence AND absoluteness
	// here (a relative cwd has no stable base across supervisor / autostart).
	if d.Cwd == "" {
		return fmt.Errorf("manifest %s: kind=companion daemons[0].cwd is required (the package working directory the process must run from)", m.Name)
	}
	if !filepath.IsAbs(d.Cwd) {
		return fmt.Errorf("manifest %s: kind=companion daemons[0].cwd %q must be an absolute path", m.Name, d.Cwd)
	}
	// A companion daemon must carry NO port (Port==0). The companion process binds
	// its own listener directly (e.g. the excalidraw canvas on its own port); it is
	// NOT an mcphub MCP port. If an operator records the companion's own port here,
	// supervisorDaemonsFromPlan copies it into SupervisorDaemon.Port and the liveness
	// sweep treats it as a port the `mcphub daemon` wrapper owns — but the real
	// listener belongs to the raw child, so supervisorDaemonEntryLiveWithProbe
	// returns port_owner_mismatch and restarts an otherwise-healthy companion
	// (Codex #381). Reject a non-zero port so that can never be recorded.
	if d.Port != 0 {
		return fmt.Errorf("manifest %s: kind=companion daemons[0].port must be 0 (the companion binds its own port directly; a non-zero value is mis-owned by the liveness probe and would restart a healthy companion)", m.Name)
	}
	return nil
}

// ValidateRemoteHTTPURL validates the remote-http endpoint URL shape shared by
// manifests and marketplace catalog http entries.
func ValidateRemoteHTTPURL(raw string) error {
	parseRaw := raw
	if remoteHTTPURLHasSecretPlaceholderHost(raw) {
		parseRaw = remoteHTTPURLPlaceholderHostShapeURL(raw)
	}
	u, err := url.Parse(parseRaw)
	if err != nil {
		// net/url's *url.Error embeds parseRaw verbatim. For the
		// non-placeholder path parseRaw == raw, so a credentialed input
		// like https://user:pass@example.com:abc/mcp would otherwise
		// leak the credential through %w even though the outer caller
		// redacts its own "got" value (bot PR #388 r10: manifest.go:489).
		return fmt.Errorf("parse url: %w", urlredact.ScrubParseError(err))
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must use https:// (got scheme %q)", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("must not embed credentials")
	}
	return nil
}

func remoteHTTPURLPlaceholderHostShapeURL(raw string) string {
	const prefix = "https://"
	rest := raw[len(prefix):]
	end := strings.IndexAny(rest, "/?#")
	authority := rest
	suffix := ""
	if end >= 0 {
		authority = rest[:end]
		suffix = rest[end:]
	}
	matches := secrets.SecretPlaceholderRE.FindStringIndex(authority)
	tail := authority[matches[1]:]
	return prefix + "placeholder.example" + tail + suffix
}

// RemoteHTTPURLHasSecretPlaceholderHost reports whether raw has a remote-http
// URL authority made only from a ${secret:KEY} placeholder plus optional port.
func RemoteHTTPURLHasSecretPlaceholderHost(raw string) bool {
	return remoteHTTPURLHasSecretPlaceholderHost(raw)
}

func remoteHTTPURLHasSecretPlaceholderHost(raw string) bool {
	const prefix = "https://"
	if !strings.HasPrefix(raw, prefix) {
		return false
	}
	rest := raw[len(prefix):]
	end := strings.IndexAny(rest, "/?#")
	authority := rest
	if end >= 0 {
		authority = rest[:end]
	}
	if authority == "" || strings.Contains(authority, "@") {
		return false
	}
	matches := secrets.SecretPlaceholderRE.FindAllStringIndex(authority, -1)
	if len(matches) != 1 || matches[0][0] != 0 {
		return false
	}
	tail := authority[matches[0][1]:]
	if tail == "" {
		return true
	}
	if !strings.HasPrefix(tail, ":") || len(tail) == 1 {
		return false
	}
	portText := tail[1:]
	for _, r := range portText {
		if r < '0' || r > '9' {
			return false
		}
	}
	// Range-check the port. url.Parse accepts the shaped form
	// (https://placeholder.example:99999/mcp) without complaint, and the
	// later expanded-secret-host validation only checks the secret value,
	// so an out-of-range port like :99999 would be persisted as a
	// placeholder-host URL no client can dial (bot PR #388 r10:
	// manifest.go:554). A leading-zero port (e.g. :080) is also
	// non-canonical and rejected.
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	if len(portText) > 1 && portText[0] == '0' {
		return false
	}
	return true
}

func (m *ServerManifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest: name is required")
	}
	if m.Kind != KindGlobal && m.Kind != KindWorkspaceScoped && m.Kind != KindCompanion {
		return fmt.Errorf("manifest %s: kind must be %q, %q, or %q (got %q)", m.Name, KindGlobal, KindWorkspaceScoped, KindCompanion, m.Kind)
	}
	if m.Transport != TransportNativeHTTP && m.Transport != TransportStdioBridge && m.Transport != TransportRemoteHTTP && m.Transport != TransportProcess {
		return fmt.Errorf("manifest %s: transport must be %q, %q, %q, or %q (got %q)", m.Name, TransportNativeHTTP, TransportStdioBridge, TransportRemoteHTTP, TransportProcess, m.Transport)
	}

	// D-2 + D-3 schema/cross-field gates (Tier-0). Placed here — AFTER the
	// kind/transport enum checks and BEFORE the companion early-return below — so
	// it applies to EVERY kind/transport uniformly (a vendored COM server is
	// kind=global; a watch row could be any kind). Pin-presence and enum-shape
	// are decidable from the manifest struct alone, so they belong in this pure,
	// host-state-free gate that runs at parse time (ManifestCreate AND Install).
	// Host-install detection (the D-3 probe) is a host/runtime fact and lives in
	// internal/api.AdmissionCheck instead.
	if err := m.validateVendoredAndAvailability(); err != nil {
		return err
	}

	// required_secrets authoring/integrity gate. Each declared key MUST back a
	// `secret:<key>` value in this manifest's Env — otherwise the required-secret
	// install gate (api.AdmissionCheck) would block the install on a credential the
	// daemon never actually reads from its env, OR a typo key (one that happens to
	// exist in the vault) would pass the gate while the REAL env secret stays
	// missing → crash-loop on startup (codex finding 3). The catalog authoring
	// guard (validateCatalogVendoredAndAvailability) enforces the SAME rule on
	// MarketplaceEntry, but a persisted / hand-edited manifest gets no such check
	// without this. Placed BEFORE the companion / remote-http early-returns so it
	// applies to EVERY kind/transport uniformly: required_secrets is a local-env
	// concern, so a key with no backing env secret: ref is rejected regardless of
	// transport (a remote-http endpoint carries credentials via url/headers
	// ${secret:KEY} placeholders, not required_secrets). The reverse direction is
	// intentionally NOT required — a `secret:` env ref WITHOUT a required_secrets
	// entry stays the default optional-secret posture, which is the whole point of
	// the opt-in gate. ADDITIVE: a manifest without required_secrets (every existing
	// manifest) skips this block entirely.
	if err := m.validateRequiredSecretsBackEnv(); err != nil {
		return err
	}

	// kind=companion and transport=process are a matched pair: each is invalid
	// without the other. A companion is a supervised NON-MCP process; transport
	// =process has no MCP host to belong to under any other kind.
	if (m.Kind == KindCompanion) != (m.Transport == TransportProcess) {
		return fmt.Errorf("manifest %s: kind=%q and transport=%q must be used together (got kind=%q transport=%q)", m.Name, KindCompanion, TransportProcess, m.Kind, m.Transport)
	}
	if m.Kind == KindCompanion {
		return m.validateCompanion()
	}

	// Cross-branch gate (closes v6 codex BLOCKER "daemon_template silently
	// accepted under kind=global / transport=remote-http"). The legacy
	// remote-http and kind=global branches each `return nil` before
	// reaching the workspace-scoped block, so a manifest carrying
	// daemon_template under those modes would otherwise pass Validate
	// as accepted-but-nonfunctional. Reject here, before either branch.
	//
	// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.1.
	if m.DaemonTemplate != nil && m.Kind != KindWorkspaceScoped {
		return fmt.Errorf("manifest %s: daemon_template requires kind=workspace-scoped (got kind=%q); dynamic-pool spawn is incompatible with kind=global", m.Name, m.Kind)
	}
	if m.DaemonTemplate != nil && m.Transport == TransportRemoteHTTP {
		return fmt.Errorf("manifest %s: daemon_template is incompatible with transport=remote-http (no local subprocess to spawn from the template)", m.Name)
	}

	// G6 remote-http branch (spec §"Validation rules"):
	//   - URL required and must parse as https:// with host and no
	//     embedded credentials
	//   - Command / BaseArgs / BaseArgsTemplate / Env / Daemons /
	//     PortPool / Languages / IdleTimeoutMin REJECTED if non-zero
	//     (silent ignore would let malformed manifests slip through
	//     — codex bot P2 r1 on PR #152 closure).
	//   - Headers may carry ${secret:KEY} placeholders (expanded at
	//     install time; not validated here).
	//   - WeeklyRefresh:true emits a startup warning but does not
	//     fail validation; false is silently accepted (G6 spec
	//     §"Validation rules" — accepted-but-no-op semantic for the
	//     YAML-bool-can't-distinguish-absent-vs-false edge).
	if m.Transport == TransportRemoteHTTP {
		// codex bot r8 P2 closure (PR #169): workspace-scoped is
		// per-(workspace, language) lazy-proxy. That model
		// requires local LSP backends + port_pool — none of which
		// remote-http can express. Reject the combination
		// explicitly so the manifest doesn't pass Validate as an
		// accepted-but-nonfunctional shape.
		if m.Kind == KindWorkspaceScoped {
			return fmt.Errorf("manifest %s: transport=remote-http is incompatible with kind=workspace-scoped (no local LSP per-language proxy; use kind=global)", m.Name)
		}
		if m.URL == "" {
			return fmt.Errorf("manifest %s: transport=remote-http requires url:", m.Name)
		}
		if err := ValidateRemoteHTTPURL(m.URL); err != nil {
			return fmt.Errorf("manifest %s: transport=remote-http url must be valid https:// without embedded credentials (got %q; plaintext rejected — operator must TLS-terminate): %w", m.Name, urlredact.MarketplaceURLForError(m.URL), err)
		}
		if m.Command != "" {
			return fmt.Errorf("manifest %s: transport=remote-http rejects command (no local subprocess; remove the field)", m.Name)
		}
		if len(m.BaseArgs) != 0 {
			return fmt.Errorf("manifest %s: transport=remote-http rejects base_args (no local subprocess)", m.Name)
		}
		if len(m.BaseArgsTemplate) != 0 {
			return fmt.Errorf("manifest %s: transport=remote-http rejects base_args_template (no local subprocess)", m.Name)
		}
		if len(m.Env) != 0 {
			return fmt.Errorf("manifest %s: transport=remote-http rejects env (no local subprocess; remote endpoint manages its own env)", m.Name)
		}
		if len(m.Daemons) != 0 {
			return fmt.Errorf("manifest %s: transport=remote-http rejects daemons[] (no per-daemon-port model; clients connect directly to the remote URL)", m.Name)
		}
		if len(m.Languages) != 0 {
			return fmt.Errorf("manifest %s: transport=remote-http rejects languages[] (workspace-scoped LSP backends incompatible with remote-only model)", m.Name)
		}
		if m.PortPool != nil {
			return fmt.Errorf("manifest %s: transport=remote-http rejects port_pool (no local ports allocated)", m.Name)
		}
		if m.IdleTimeoutMin != 0 {
			return fmt.Errorf("manifest %s: transport=remote-http rejects idle_timeout_min (no local daemon to idle-out)", m.Name)
		}
		return nil
	}

	// Non-remote-http branches reject URL and Headers — those fields
	// are exclusive to remote-http.
	//
	// codex bot r4 P2 closure (PR #169): use Headers != nil rather
	// than len(Headers) != 0 so an explicit `headers: {}` (decoded
	// as a non-nil empty map) is also rejected. YAML doesn't
	// distinguish `headers:` (absent) from `headers: {}` for slices
	// — both decode to non-nil zero-length — so the only signal we
	// have is the nil-vs-non-nil bit set by the decoder when the
	// key is mentioned in the YAML at all.
	if m.URL != "" {
		return fmt.Errorf("manifest %s: url is only valid with transport=remote-http (got transport=%q)", m.Name, m.Transport)
	}
	if m.Headers != nil {
		return fmt.Errorf("manifest %s: headers is only valid with transport=remote-http (got transport=%q; remove the headers: key entirely if you meant to declare none)", m.Name, m.Transport)
	}

	if m.Command == "" {
		return fmt.Errorf("manifest %s: command is required", m.Name)
	}
	// Port-planning gate (DM-2): a fixed daemon port must not squat the GUI
	// listener port. Applies to any non-remote-http manifest with declared
	// daemons (remote-http returned above; workspace-scoped pools declare a
	// range, not a fixed port, and are unaffected since a pool start of 9125
	// is itself outside every reserved pool band).
	for i := range m.Daemons {
		if m.Daemons[i].Port == ReservedGUIPort {
			return fmt.Errorf("manifest %s: daemons[%d] (%q) declares port %d, the reserved GUI listener port; choose a port outside the GUI/infra range (hand-assigned globals use 9121–9149 per configs/ports.yaml)",
				m.Name, i, m.Daemons[i].Name, ReservedGUIPort)
		}
		// A daemon cwd, when set, becomes the subprocess's cmd.Dir at spawn
		// time. It MUST be absolute: the daemon is launched from the
		// supervisor / scheduler / an interactive shell whose own cwd is not
		// stable, so a relative cwd would resolve against an unpredictable
		// base. Reject it at validation so the dead-end never reaches spawn.
		// (${ENV}/${HOME} tokens were already expanded at parse time, so by
		// here Cwd is a literal path.)
		if c := m.Daemons[i].Cwd; c != "" && !filepath.IsAbs(c) {
			return fmt.Errorf("manifest %s: daemons[%d] (%q) cwd %q must be an absolute path (a relative cwd has no stable base across the supervisor / scheduler / interactive launch surfaces)",
				m.Name, i, m.Daemons[i].Name, c)
		}
	}
	if m.Kind == KindWorkspaceScoped {
		// Dynamic-pool branch (Phase D.1): per-workspace daemon spawned
		// from daemon_template. Mutually exclusive with the legacy
		// LSP-bridge daemons[]+languages[]+top-level port_pool shape.
		//
		// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.1.
		if m.DaemonTemplate != nil {
			if m.PortPool != nil {
				return fmt.Errorf("manifest %s: kind=workspace-scoped with daemon_template must NOT set top-level port_pool (move start/end into daemon_template.port_pool)", m.Name)
			}
			if len(m.Languages) > 0 {
				return fmt.Errorf("manifest %s: kind=workspace-scoped with daemon_template rejects top-level languages[] (dynamic-pool serena is multi-language per .serena/project.yml)", m.Name)
			}
			if len(m.Daemons) > 0 {
				return fmt.Errorf("manifest %s: kind=workspace-scoped with daemon_template is mutually exclusive with daemons[] (dynamic-pool migration requires removing the legacy daemons[] block)", m.Name)
			}
			if err := validatePortPool("daemon_template.port_pool", m.DaemonTemplate.PortPool); err != nil {
				return fmt.Errorf("manifest %s: %w", m.Name, err)
			}
			if len(m.DaemonTemplate.ExtraArgsTemplate) == 0 {
				return fmt.Errorf("manifest %s: daemon_template.extra_args_template must be non-empty", m.Name)
			}
			if !containsWorkspacePathTokenInArgs(m.DaemonTemplate.ExtraArgsTemplate) {
				return fmt.Errorf("manifest %s: daemon_template.extra_args_template must contain ${workspace.path} token somewhere (else workspace context is lost on spawn)", m.Name)
			}
			// daemon_template.context is the single authoritative context value;
			// the materializer APPENDS `--context <Context>` to every per-workspace
			// child argv (design §5). A --context token already present in base_args
			// or extra_args_template would materialize a SECOND --context flag,
			// which the child CLI either rejects (duplicate) or silently resolves to
			// the wrong value when the two differ (bot PR #246 r2 P2). Reject the
			// malformed shape here so it never reaches RuntimeSpec materialization.
			if ArgsContainContextFlag(m.BaseArgs) || ArgsContainContextFlag(m.DaemonTemplate.ExtraArgsTemplate) {
				return fmt.Errorf("manifest %s: daemon_template manifests must not place --context in base_args or extra_args_template; the context comes solely from daemon_template.context and is appended at spawn", m.Name)
			}
			return nil
		}
		// Legacy LSP-bridge branch (unchanged: preserves current
		// mcp-language-server / gopls-mcp manifest shape).
		if err := validatePortPool("port_pool", m.PortPool); err != nil {
			return fmt.Errorf("manifest %s: %w", m.Name, err)
		}
		if len(m.Languages) == 0 {
			return fmt.Errorf("manifest %s: languages[] must be non-empty for kind=workspace-scoped", m.Name)
		}
		for i := range m.Languages {
			l := &m.Languages[i]
			if l.Name == "" {
				return fmt.Errorf("manifest %s: languages[%d].name is required", m.Name, i)
			}
			// B.1 dual-gate defense (plan §B.1): refuse '@' prefix on
			// LanguageSpec.Name to keep the @serena sentinel
			// collision-free. The registry write paths (PutLSP /
			// PutSerena) carry the runtime gate; this manifest-time
			// gate stops a malformed YAML from reaching the registry
			// path at all.
			//
			// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §B.1.
			if strings.HasPrefix(l.Name, "@") {
				return fmt.Errorf("manifest %s: languages[%d].name must not start with '@' (reserved for sentinel rows)", m.Name, i)
			}
			if l.Backend != "mcp-language-server" && l.Backend != "gopls-mcp" {
				return fmt.Errorf("manifest %s: languages[%d].backend must be \"mcp-language-server\" or \"gopls-mcp\" (got %q)", m.Name, i, l.Backend)
			}
			if l.Transport == "" {
				l.Transport = LanguageTransportStdio
			}
			if l.Transport != LanguageTransportStdio && l.Transport != LanguageTransportHTTPListen && l.Transport != LanguageTransportNativeHTTP {
				return fmt.Errorf("manifest %s: languages[%d].transport must be %q | %q | %q (got %q)", m.Name, i,
					LanguageTransportStdio, LanguageTransportHTTPListen, LanguageTransportNativeHTTP, l.Transport)
			}
			if l.LspCommand == "" {
				return fmt.Errorf("manifest %s: languages[%d].lsp_command is required", m.Name, i)
			}
		}
	}
	return nil
}

// ValidateStrict runs the standard Validate() checks PLUS the strict
// '__'-substring rejection on the server name. Used by manifest
// mutation surfaces (create / edit / install) and by the hub bind-time
// gate when gui_server.hub_endpoint_enabled=true. Legacy manifests that
// still carry '__' in their name continue to load through Validate()
// (compat mode) so the v0.3.0 upgrade path doesn't break first-start
// inventory reads.
//
// G4 §"Pre-gate" (docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md).
func (m *ServerManifest) ValidateStrict() error {
	if err := m.Validate(); err != nil {
		return err
	}
	if strings.Contains(m.Name, "__") {
		return fmt.Errorf("manifest %s: server name contains '__' (reserved for hub-mode tool-name namespacing; rename via `mcphub manifest edit`)", m.Name)
	}
	return nil
}
