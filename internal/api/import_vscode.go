// Package api — G7 VS Code workspace import (Phase 3B-II backlog G7
// "VS Code workspace + JSON5 import compatibility").
//
// Reads a workspace's .vscode/mcp.json and projects it into a draft
// mcp-local-hub manifest. The output is a YAML string that operators
// can inspect, edit, and pass to `mcphub manifest create` (or to the
// GUI Add server screen via Paste YAML). NO write side effects — this
// step honors the inspect → validate → dry-run → backup → apply
// contract documented in `docs/superpowers/plans/2026-05-04-ravitemer-mcp-hub-adoption-proposals.md`.
//
// Schema handling:
//   - Accepts both `servers` (new VS Code schema) and `mcpServers`
//     (legacy / Claude Desktop schema). If both keys are present, they
//     are merged with `servers` taking precedence on name collision.
//   - Each server's `type` is honored: "stdio" → stdio backend with
//     command/args/env; "http" / "sse" → native_http transport with
//     URL + headers. Unknown types are surfaced as warnings, not
//     fatal errors (operator can edit the draft).
//   - JSON5 quirks: tolerates `// line comments`, `/* block comments */`,
//     and trailing commas via a pre-parse strip. This is intentionally
//     conservative — strict JSON5 spec includes more features (single-
//     quoted strings, unquoted keys) that we do NOT implement; if the
//     workspace file uses them, the operator must clean up first.
//
// Placeholder expansion (applied to string values in env / args / url):
//   - ${env:VAR}           → os.Getenv("VAR")
//   - ${workspaceFolder}   → the workspace path argument
//   - ${userHome}          → os.UserHomeDir() result
//   - ${pathSeparator}     → string(os.PathSeparator)
//
// Empty env-var expansions surface as a warning in the returned
// VSCodeImportResult; the substitution still happens so the operator
// sees the literal empty string and can decide whether to keep going.

package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// VSCodeImportResult is the structured output of ImportVSCodeWorkspace.
//
// YAML is the projected mcp-local-hub manifest in pasteable form.
// Servers lists each entry that contributed to YAML, in alphabetical
// order, so the operator can match draft entries to source rows.
// Warnings carry non-fatal observations: unknown server types,
// undefined env-var placeholders, name collisions across servers
// and mcpServers. EmptyResult is true when the source file contained
// no server entries at all (operator probably pointed at the wrong
// path).
type VSCodeImportResult struct {
	YAML        string
	Servers     []string
	Warnings    []string
	EmptyResult bool
}

// VSCodeImportOpts carries override hooks for ImportVSCodeWorkspace.
// The exported variant lets tests inject deterministic env / home
// resolution without setting real environment variables. When unset,
// production defaults are used (os.Getenv, os.UserHomeDir, runtime
// path separator).
type VSCodeImportOpts struct {
	Getenv        func(string) string // nil → os.Getenv
	UserHome      func() (string, error)
	PathSeparator string // "" → string(os.PathSeparator)
}

// ImportVSCodeWorkspace reads <workspacePath>/.vscode/mcp.json and
// projects it onto a draft manifest. Returns an error only on read /
// parse failures; non-fatal issues populate result.Warnings.
//
// The caller is expected to pass an absolute workspacePath. Relative
// paths are accepted but resolved against the process cwd (not
// validated for existence).
func (a *API) ImportVSCodeWorkspace(workspacePath string, opts VSCodeImportOpts) (*VSCodeImportResult, error) {
	if workspacePath == "" {
		return nil, fmt.Errorf("workspace path is required")
	}
	srcPath := filepath.Join(workspacePath, ".vscode", "mcp.json")
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", srcPath, err)
	}

	cleaned := stripJSONCommentsAndTrailingCommas(raw)
	var root map[string]any
	if err := json.Unmarshal(cleaned, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", srcPath, err)
	}

	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	homeFn := opts.UserHome
	if homeFn == nil {
		homeFn = os.UserHomeDir
	}
	pathSep := opts.PathSeparator
	if pathSep == "" {
		pathSep = string(os.PathSeparator)
	}
	home, _ := homeFn() // empty on failure is acceptable; expansion stays literal

	result := &VSCodeImportResult{}

	// Collect both schemas and merge with `servers` precedence.
	merged, schemaWarnings := mergeVSCodeServerSchemas(root)
	result.Warnings = append(result.Warnings, schemaWarnings...)
	if len(merged) == 0 {
		result.EmptyResult = true
		result.Warnings = append(result.Warnings, fmt.Sprintf("no server entries found in %s", srcPath))
		return result, nil
	}

	exp := PlaceholderExpander{
		Workspace:     workspacePath,
		UserHome:      home,
		PathSeparator: pathSep,
		Getenv:        getenv,
		UndefinedEnv:  map[string]struct{}{},
	}

	var entries []vscodeProjected
	for _, name := range sortedKeys(merged) {
		entry := merged[name]
		projected, projWarnings := projectVSCodeServer(name, entry, &exp)
		result.Warnings = append(result.Warnings, projWarnings...)
		if projected != nil {
			entries = append(entries, *projected)
			result.Servers = append(result.Servers, name)
		}
	}

	for _, name := range sortedKeys(exp.UndefinedEnv) {
		result.Warnings = append(result.Warnings, fmt.Sprintf("placeholder ${env:%s} expanded to empty string (variable not set)", name))
	}

	// EmptyResult fires when NO entries projected, even if the source
	// file had entries that were all skipped (e.g., all http/sse types
	// deferred to G6). Tests + CLI use EmptyResult as the "no YAML to
	// emit" signal.
	if len(entries) == 0 {
		result.EmptyResult = true
	}
	result.YAML = renderVSCodeProjectedYAML(entries)
	return result, nil
}

// vscodeProjected captures one server's projection between schemas
// and YAML rendering. Stored intermediately so the renderer can
// alphabetize and apply consistent quoting.
type vscodeProjected struct {
	Name      string
	Transport string            // "stdio-bridge" or "native-http"
	Command   string            // stdio only
	Args      []string          // stdio only
	Env       map[string]string // stdio only
	URL       string            // http/sse only
	Headers   map[string]string // http/sse only
}

// mergeVSCodeServerSchemas reads both `servers` (preferred) and
// `mcpServers` (legacy) top-level keys. Returns a merged map keyed by
// server name; `servers` entries take precedence on collision.
// Warnings list any collision resolutions so the operator can audit.
func mergeVSCodeServerSchemas(root map[string]any) (map[string]map[string]any, []string) {
	var warnings []string
	out := map[string]map[string]any{}
	for _, key := range []string{"mcpServers", "servers"} {
		section, ok := root[key].(map[string]any)
		if !ok {
			continue
		}
		for name, raw := range section {
			entry, ok := raw.(map[string]any)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("%s/%s: skipped (entry is not a JSON object)", key, name))
				continue
			}
			if existing, dup := out[name]; dup && key == "servers" {
				warnings = append(warnings, fmt.Sprintf("server %q present in both servers and mcpServers — using servers entry", name))
				_ = existing
			} else if dup {
				warnings = append(warnings, fmt.Sprintf("server %q in mcpServers shadowed by earlier servers entry", name))
				continue
			}
			out[name] = entry
		}
	}
	return out, warnings
}

// projectVSCodeServer maps one VS Code entry onto our manifest shape.
// Returns nil + warnings when the entry is unusable (unknown type
// missing both stdio and url fields). Non-fatal issues are surfaced as
// warnings; the entry is still projected as best-effort so the
// operator sees the draft.
func projectVSCodeServer(name string, entry map[string]any, exp *PlaceholderExpander) (*vscodeProjected, []string) {
	var warnings []string
	serverType, _ := entry["type"].(string)
	cmd, _ := entry["command"].(string)
	url, _ := entry["url"].(string)
	// VS Code allows omitting type. Infer from command vs url.
	if serverType == "" {
		switch {
		case cmd != "":
			serverType = "stdio"
		case url != "":
			serverType = "http"
		default:
			warnings = append(warnings, fmt.Sprintf("server %q: missing both command and url — skipped", name))
			return nil, warnings
		}
	}
	switch strings.ToLower(serverType) {
	case "stdio":
		if cmd == "" {
			warnings = append(warnings, fmt.Sprintf("server %q: type=stdio but no command — skipped", name))
			return nil, warnings
		}
		args, _ := entry["args"].([]any)
		env, _ := entry["env"].(map[string]any)
		projected := &vscodeProjected{
			Name:      name,
			Transport: "stdio-bridge",
			Command:   exp.Expand(cmd),
			Args:      expandStringSlice(args, exp),
			Env:       expandStringMap(env, exp),
		}
		return projected, warnings
	case "http", "sse":
		// VS Code's `type: http` and `type: sse` describe a REMOTE
		// MCP server addressed by URL — the client opens an HTTP
		// connection directly. Project onto manifest
		// transport=remote-http (G6 sub-PR 4) so the operator gets a
		// valid draft instead of the prior skip-with-warning. Note:
		// `native-http` is reserved for LOCALLY-spawned daemons that
		// expose an HTTP endpoint (servers/serena/manifest.yaml); the
		// two transports look similar but native-http requires a
		// `command:` and remote-http forbids it.
		//
		// Bot r1 P2 closure (PR #172): check emptiness AFTER expansion.
		// A workspace file using `url: "${env:MCP_URL}"` with the env
		// var unset would otherwise project a draft with an empty url
		// (since the raw value `${env:MCP_URL}` is non-empty), which
		// manifest validation later rejects. Skip with a clear
		// post-expansion warning instead.
		expandedURL := exp.Expand(url)
		if expandedURL == "" {
			if url == "" {
				warnings = append(warnings, fmt.Sprintf("server %q: type=%s but no url — skipped", name, serverType))
			} else {
				warnings = append(warnings, fmt.Sprintf("server %q: url %q expanded to empty (placeholder unset?) — skipped", name, url))
			}
			return nil, warnings
		}
		// Bot r3 P2 closure (PR #172): schema requires https:// for
		// remote-http. A plaintext http://localhost workspace entry
		// would project here and then fail manifest validation with
		// the wrong diagnostic ("remote-http requires https://"). Skip
		// upfront with a clear cause so the operator knows the
		// workspace url itself is the issue, not the projection.
		if !strings.HasPrefix(strings.ToLower(expandedURL), "https://") {
			warnings = append(warnings, fmt.Sprintf("server %q: url %q is not https:// — remote-http manifests require TLS; skipped", name, expandedURL))
			return nil, warnings
		}
		hdrs, _ := entry["headers"].(map[string]any)
		projected := &vscodeProjected{
			Name:      name,
			Transport: "remote-http",
			URL:       expandedURL,
			Headers:   expandStringMap(hdrs, exp),
		}
		return projected, warnings
	default:
		warnings = append(warnings, fmt.Sprintf("server %q: unknown type %q (expected stdio/http/sse) — skipped", name, serverType))
		return nil, warnings
	}
}

// renderVSCodeProjectedYAML renders projections as a multi-document
// YAML stream — one document per server, separated by `---`. This
// matches the shape the GUI's Paste YAML accepts and lets the
// operator drop the whole stream into a single screen.
//
// Each document follows the canonical manifest layout: name, kind,
// transport, command/base_args, env, daemons[default+claude+codex].
// The default daemon binding is left blank so the operator can edit
// it (manifest validation will surface the missing-binding gate).
func renderVSCodeProjectedYAML(entries []vscodeProjected) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, e := range entries {
		if i > 0 {
			sb.WriteString("---\n")
		}
		sb.WriteString(fmt.Sprintf("name: %s\n", yamlEscape(e.Name)))
		sb.WriteString("kind: global\n")
		sb.WriteString(fmt.Sprintf("transport: %s\n", e.Transport))
		switch e.Transport {
		case "stdio-bridge":
			sb.WriteString(fmt.Sprintf("command: %s\n", yamlEscape(e.Command)))
			if len(e.Args) > 0 {
				sb.WriteString("base_args:\n")
				for _, a := range e.Args {
					sb.WriteString(fmt.Sprintf("  - %s\n", yamlEscape(a)))
				}
			}
			if len(e.Env) > 0 {
				sb.WriteString("env:\n")
				for _, k := range sortedStringKeys(e.Env) {
					sb.WriteString(fmt.Sprintf("  %s: %s\n", yamlEscape(k), yamlEscape(e.Env[k])))
				}
			}
		case "native-http":
			sb.WriteString(fmt.Sprintf("url: %s\n", yamlEscape(e.URL)))
			if len(e.Headers) > 0 {
				sb.WriteString("headers:\n")
				for _, k := range sortedStringKeys(e.Headers) {
					sb.WriteString(fmt.Sprintf("  %s: %s\n", yamlEscape(k), yamlEscape(e.Headers[k])))
				}
			}
		case "remote-http":
			sb.WriteString(fmt.Sprintf("url: %s\n", yamlEscape(e.URL)))
			if len(e.Headers) > 0 {
				sb.WriteString("headers:\n")
				for _, k := range sortedStringKeys(e.Headers) {
					sb.WriteString(fmt.Sprintf("  %s: %s\n", yamlEscape(k), yamlEscape(e.Headers[k])))
				}
			}
		}
		switch e.Transport {
		case "remote-http":
			// Remote-http manifests have no local daemons (schema
			// rejects daemons: with this transport). Prefill bindings
			// for the adapters that support remote-http per the
			// adapter compatibility matrix; operator removes the ones
			// they don't want. Antigravity is excluded — stdio-relay
			// only — and uninstall handles it at install time.
			sb.WriteString("client_bindings:\n")
			for _, c := range []string{"claude-code", "codex-cli", "cursor", "gemini-cli", "vscode"} {
				sb.WriteString(fmt.Sprintf("  - client: %s\n", c))
			}
		default:
			// Local-daemon transports: leave the daemons block as a
			// placeholder so manifest validation surfaces the
			// port-assignment gate before install.
			sb.WriteString("daemons:\n  - name: default\n    port: 0  # TODO: assign\n")
			sb.WriteString("client_bindings: []\n")
		}
	}
	return sb.String()
}

// PlaceholderExpander holds resolved environment + workspace path.
// Shared between G7 (VS Code import) and G5 (marketplace generate).
//
// G7 callers leave SkipSensitiveEnv at its default (false): the
// local-trusted VS Code file is expanded as before, and undefined
// env vars are collected via UndefinedEnv for the existing
// "placeholder ${env:%s} expanded to empty string" warning surface.
//
// G5 callers set SkipSensitiveEnv: true. Sensitive env names stay
// verbatim in the projected draft, and SensitiveSkipped accumulates
// the names for `WarningsForSensitive()`.
type PlaceholderExpander struct {
	Workspace     string              // ${workspaceFolder}
	UserHome      string              // ${userHome}
	PathSeparator string              // ${pathSeparator}
	Getenv        func(string) string // injection point for tests

	// UndefinedEnv is the existing G7 surface (was vscodeExpander.undefinedW).
	// Empty-resolution env names are collected here. G7 reads this
	// AFTER expansion to emit "placeholder ${env:%s} expanded to
	// empty string" warnings. Caller initializes to non-nil; the
	// expander writes via map-set semantics (no replacement of the
	// map reference, so caller can keep a reference to read later).
	UndefinedEnv map[string]struct{}

	// SkipSensitiveEnv, when true, leaves ${env:NAME} tokens
	// VERBATIM in the output whenever IsSensitiveEnvName(NAME) is
	// true. G5 (catalog generate) opts in; G7 (local VS Code import)
	// keeps the default false.
	SkipSensitiveEnv bool

	// SensitiveSkipped collects env-var names left verbatim under
	// SkipSensitiveEnv. G5 reads this AFTER all Expand calls to
	// emit one stderr warning per name. Same expander is reused
	// across many Expand calls; the slice accumulates with dedup.
	SensitiveSkipped []string
}

// PlaceholderRE matches ${name} or ${env:VAR}. The capture groups are:
// 1=full placeholder body, 2=env var name (only set for the env: form).
var PlaceholderRE = regexp.MustCompile(`\$\{(env:([^}]+)|workspaceFolder|userHome|pathSeparator)\}`)

// Sensitive-env name policy (G5 catalog placeholder redaction).
//
// Bias: prefer overflagging — a false positive emits an extra
// "${env:NAME} left verbatim — edit before saving" warning, which the
// operator dismisses if it's a legitimate non-secret. A false
// negative writes a real secret value into a YAML the operator might
// commit, which is the failure mode we are guarding against. G7's
// trusted-local-file import keeps SkipSensitiveEnv=false and is not
// affected by this policy.
//
// codex deep-sec PR #163 lane 2 closure: predicate expanded from
// suffix-only to suffix + prefix + substring + exact-name shapes so
// names like DATABASE_URL, CONNECTION_STRING, AUTHORIZATION,
// GOOGLE_APPLICATION_CREDENTIALS, BEARER_TOKEN, and MY_TOKEN_VALUE
// (infix TOKEN) are flagged instead of expanded into the draft YAML.
var sensitiveEnvNameSuffixes = []string{
	"_TOKEN", "_SECRET", "_PASSWORD", "_PASSWD",
	"_KEY", "_API_KEY", "_AUTH", "_DSN",
}
var sensitiveEnvNamePrefixes = []string{
	"AWS_", "AZURE_", "GCP_", "GITHUB_", "GOOGLE_", "OAUTH_",
}
var sensitiveEnvNameSubstrings = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD",
	"CREDENTIAL", "BEARER", "PRIVATE_KEY",
}
var sensitiveEnvNameExact = []string{
	"DATABASE_URL", "CONNECTION_STRING", "DSN",
	"AUTHORIZATION", "OAUTH",
	"GOOGLE_APPLICATION_CREDENTIALS",
}

// IsSensitiveEnvName returns true if the env-var name matches any of
// the sensitive-name shapes used by G5's catalog placeholder policy.
// Match is case-insensitive against the ASCII uppercase form.
//
// The four match families:
//   - exact: `DATABASE_URL`, `CONNECTION_STRING`, `DSN`,
//     `AUTHORIZATION`, `OAUTH`, `GOOGLE_APPLICATION_CREDENTIALS`.
//   - prefix: cloud-provider env namespaces (`AWS_*`, `AZURE_*`,
//     `GCP_*`, `GITHUB_*`, `GOOGLE_*`, `OAUTH_*`).
//   - suffix: classic name-shapes (`*_TOKEN`, `*_SECRET`,
//     `*_PASSWORD`, `*_PASSWD`, `*_KEY`, `*_API_KEY`, `*_AUTH`,
//     `*_DSN`).
//   - substring (anywhere in the name): `TOKEN`, `SECRET`,
//     `PASSWORD`, `PASSWD`, `CREDENTIAL`, `BEARER`, `PRIVATE_KEY`.
func IsSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, exact := range sensitiveEnvNameExact {
		if upper == exact {
			return true
		}
	}
	for _, suf := range sensitiveEnvNameSuffixes {
		if strings.HasSuffix(upper, suf) {
			return true
		}
	}
	for _, pre := range sensitiveEnvNamePrefixes {
		if strings.HasPrefix(upper, pre) {
			return true
		}
	}
	for _, sub := range sensitiveEnvNameSubstrings {
		if strings.Contains(upper, sub) {
			return true
		}
	}
	return false
}

// Expand replaces every ${...} placeholder in s and returns the
// expanded string. Mutates UndefinedEnv (when an env: form resolves
// empty) and SensitiveSkipped (when SkipSensitiveEnv && the env name
// is sensitive). Byte-equivalent to G7's previous vscodeExpander.expand
// for the default (SkipSensitiveEnv=false) case.
func (e *PlaceholderExpander) Expand(s string) string {
	return PlaceholderRE.ReplaceAllStringFunc(s, func(match string) string {
		sub := PlaceholderRE.FindStringSubmatch(match)
		// sub[1] is the body, sub[2] is the env var name when "env:VAR" form.
		body := sub[1]
		envName := sub[2]
		switch {
		case envName != "":
			if e.SkipSensitiveEnv && IsSensitiveEnvName(envName) {
				// G5 catalog policy: leave the placeholder VERBATIM so
				// the value is never written into the draft YAML.
				// Dedup the name into SensitiveSkipped for a single
				// stderr warning at the caller.
				already := false
				for _, n := range e.SensitiveSkipped {
					if n == envName {
						already = true
						break
					}
				}
				if !already {
					e.SensitiveSkipped = append(e.SensitiveSkipped, envName)
				}
				return match
			}
			val := e.Getenv(envName)
			if val == "" {
				e.UndefinedEnv[envName] = struct{}{}
			}
			return val
		case body == "workspaceFolder":
			return e.Workspace
		case body == "userHome":
			return e.UserHome
		case body == "pathSeparator":
			return e.PathSeparator
		default:
			return match
		}
	})
}

// WarningsForSensitive returns one stderr-friendly warning per
// distinct sensitive-env name seen during Expand calls. Empty when
// SkipSensitiveEnv was false or no sensitive names matched.
func (e *PlaceholderExpander) WarningsForSensitive() []string {
	if len(e.SensitiveSkipped) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.SensitiveSkipped))
	for _, name := range e.SensitiveSkipped {
		out = append(out, fmt.Sprintf("catalog references ${env:%s} — left verbatim in the draft so the value is never written to the YAML you'll commit; edit before saving", name))
	}
	return out
}

func expandStringSlice(raw []any, exp *PlaceholderExpander) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		out = append(out, exp.Expand(s))
	}
	return out
}

func expandStringMap(raw map[string]any, exp *PlaceholderExpander) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = exp.Expand(s)
	}
	return out
}

// stripJSONCommentsAndTrailingCommas removes // line comments,
// /* block comments */, and trailing commas before }/]. Operates on
// bytes — does NOT understand strings, so a `//` inside a string
// would be eaten. VS Code's mcp.json is small and rarely contains
// strings with // tokens; the conservative approach is to document
// this limitation rather than implement a proper JSON5 parser.
func stripJSONCommentsAndTrailingCommas(raw []byte) []byte {
	// First pass: strip block comments.
	out := make([]byte, 0, len(raw))
	inString := false
	var stringDelim byte
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if inString {
			if b == '\\' && i+1 < len(raw) {
				out = append(out, b, raw[i+1])
				i++
				continue
			}
			out = append(out, b)
			if b == stringDelim {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			stringDelim = b
			out = append(out, b)
			continue
		}
		if b == '/' && i+1 < len(raw) {
			next := raw[i+1]
			if next == '/' {
				// Line comment — skip to end of line.
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
				if i < len(raw) {
					out = append(out, raw[i])
				}
				continue
			}
			if next == '*' {
				// Block comment — skip until */.
				i += 2
				for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
					i++
				}
				i++ // consume the */ closing pair
				continue
			}
		}
		out = append(out, b)
	}
	// Second pass: drop trailing commas before } or ].
	return stripTrailingCommas(out)
}

func stripTrailingCommas(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	inString := false
	var stringDelim byte
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if inString {
			out = append(out, b)
			if b == '\\' && i+1 < len(raw) {
				out = append(out, raw[i+1])
				i++
				continue
			}
			if b == stringDelim {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			stringDelim = b
			out = append(out, b)
			continue
		}
		if b == ',' {
			// Peek ahead skipping whitespace; if next non-ws is ] or }, drop the comma.
			j := i + 1
			for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
				j++
			}
			if j < len(raw) && (raw[j] == '}' || raw[j] == ']') {
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// yamlEscape minimally quotes strings that contain characters
// requiring YAML escaping. Plain identifiers and ASCII values are
// emitted bare. The output is not a full YAML serializer — it's
// scoped to the limited shape of mcp.json-derived values.
func yamlEscape(s string) string {
	if s == "" {
		return "\"\""
	}
	// Heuristic: quote if the string contains characters that YAML
	// would otherwise interpret (#, :, &, *, !, |, >, etc.) OR if
	// it starts with a digit/dash, OR if it has leading/trailing
	// whitespace.
	if needsQuoting(s) {
		// Use double-quoted form and escape backslashes + quotes.
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return "\"" + s + "\""
	}
	return s
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	if strings.ContainsAny(s, "#:&*!|>'\"\t\n%@`") {
		return true
	}
	if s[0] == '-' || s[0] == '?' || s[0] == ',' || s[0] == '[' || s[0] == ']' || s[0] == '{' || s[0] == '}' {
		return true
	}
	if s != strings.TrimSpace(s) {
		return true
	}
	return false
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
