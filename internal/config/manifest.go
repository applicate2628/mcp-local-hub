package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind enumerates daemon types. Only these two values are valid in manifest.kind.
const (
	KindGlobal          = "global"
	KindWorkspaceScoped = "workspace-scoped"
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
	// other transport. Must start with "https://" (G6 §"Validation
	// rules": plain http:// rejected — plaintext credentials over the
	// wire are out of scope).
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

	// DaemonTemplate is the per-workspace dynamic-pool spawn template.
	// REQUIRES kind=workspace-scoped AND transport != remote-http
	// (cross-branch validator gate rejects other combinations). Mutually
	// exclusive with the legacy top-level Daemons list — manifests that
	// migrate to dynamic-pool drop daemons[] and add daemon_template.
	//
	// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.1.
	DaemonTemplate *DaemonTemplate `yaml:"daemon_template,omitempty"`
}

type DaemonSpec struct {
	Name      string   `yaml:"name"`
	Context   string   `yaml:"context"`
	Port      int      `yaml:"port"`
	ExtraArgs []string `yaml:"extra_args"`
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

type PortPool struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
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

// Validate checks required fields and enum values. Called automatically by ParseManifest.
//
// Validate is COMPAT mode for the '__'-in-name policy: structural fields
// are enforced but '__'-substring names are accepted silently. The
// compat path is used by startup inventory + manifest listing reads so
// legacy '__'-named manifests stay readable. See ValidateStrict for the
// mutation-path gate that rejects them.
//
// G4 §"Pre-gate" (docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md).
func (m *ServerManifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest: name is required")
	}
	if m.Kind != KindGlobal && m.Kind != KindWorkspaceScoped {
		return fmt.Errorf("manifest %s: kind must be %q or %q (got %q)", m.Name, KindGlobal, KindWorkspaceScoped, m.Kind)
	}
	if m.Transport != TransportNativeHTTP && m.Transport != TransportStdioBridge && m.Transport != TransportRemoteHTTP {
		return fmt.Errorf("manifest %s: transport must be %q, %q, or %q (got %q)", m.Name, TransportNativeHTTP, TransportStdioBridge, TransportRemoteHTTP, m.Transport)
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
	//   - URL required and must be https://
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
		if !strings.HasPrefix(m.URL, "https://") {
			return fmt.Errorf("manifest %s: transport=remote-http url must start with https:// (got %q; plaintext rejected — operator must TLS-terminate)", m.Name, m.URL)
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
			if m.DaemonTemplate.PortPool == nil {
				return fmt.Errorf("manifest %s: daemon_template.port_pool is required (start/end)", m.Name)
			}
			if m.DaemonTemplate.PortPool.Start <= 0 || m.DaemonTemplate.PortPool.End < m.DaemonTemplate.PortPool.Start {
				return fmt.Errorf("manifest %s: daemon_template.port_pool must have start>0 and end>=start (got {%d,%d})", m.Name, m.DaemonTemplate.PortPool.Start, m.DaemonTemplate.PortPool.End)
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
		if m.PortPool == nil {
			return fmt.Errorf("manifest %s: port_pool is required for kind=workspace-scoped", m.Name)
		}
		if m.PortPool.Start <= 0 || m.PortPool.End < m.PortPool.Start {
			return fmt.Errorf("manifest %s: port_pool must have start>0 and end>=start (got {%d,%d})", m.Name, m.PortPool.Start, m.PortPool.End)
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
