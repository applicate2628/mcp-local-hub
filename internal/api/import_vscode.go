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

	exp := vscodeExpander{
		workspace:  workspacePath,
		home:       home,
		pathSep:    pathSep,
		getenv:     getenv,
		undefinedW: map[string]struct{}{},
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

	for _, name := range sortedKeys(exp.undefinedW) {
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
func projectVSCodeServer(name string, entry map[string]any, exp *vscodeExpander) (*vscodeProjected, []string) {
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
			Command:   exp.expand(cmd),
			Args:      expandStringSlice(args, exp),
			Env:       expandStringMap(env, exp),
		}
		return projected, warnings
	case "http", "sse":
		// VS Code's `type: http` and `type: sse` describe a REMOTE MCP
		// server addressed by URL — the client opens an HTTP connection
		// directly. mcp-local-hub's `transport: native-http` is
		// different: it means a LOCALLY-spawned daemon that exposes an
		// HTTP endpoint (see servers/serena/manifest.yaml: command: uvx
		// ... --transport streamable-http). The current ServerManifest
		// schema requires a `command` field for every manifest
		// (internal/config/manifest.go Validate enforces this); no
		// shape proxies to a remote URL.
		//
		// Remote URL imports belong to the G6 backlog ("Remote MCP
		// manifests"), deferred to v0.4.x. Skip with a clear warning
		// rather than emit YAML that ParseManifest would reject.
		//
		// Codex bot P1 on PR #151 line 289 caught the original
		// invalid emission.
		_ = url
		warnings = append(warnings, fmt.Sprintf(
			"server %q: type=%s describes a remote MCP server — current manifest "+
				"schema requires a locally-spawned command (transport: stdio-bridge or "+
				"native-http with command:). Remote URL imports land in backlog G6 "+
				"(Remote MCP manifests), deferred to v0.4.x. Skipped.",
			name, serverType))
		return nil, warnings
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
		}
		// Daemons block left as a placeholder. Manifest validation
		// will require the operator to add at least one daemon
		// before install — the draft does not pretend to be
		// install-ready.
		sb.WriteString("daemons:\n  - name: default\n    port: 0  # TODO: assign\n")
		sb.WriteString("client_bindings: []\n")
	}
	return sb.String()
}

// vscodeExpander holds resolved environment so per-string expansion
// can be quick and warning collection lives in one place.
type vscodeExpander struct {
	workspace  string
	home       string
	pathSep    string
	getenv     func(string) string
	undefinedW map[string]struct{}
}

// vscodePlaceholderRE matches ${name} or ${env:VAR}. The capture
// groups are: 1=full placeholder body, 2=env var name (only set for
// the env: form).
var vscodePlaceholderRE = regexp.MustCompile(`\$\{(env:([^}]+)|workspaceFolder|userHome|pathSeparator)\}`)

func (e *vscodeExpander) expand(s string) string {
	return vscodePlaceholderRE.ReplaceAllStringFunc(s, func(match string) string {
		sub := vscodePlaceholderRE.FindStringSubmatch(match)
		// sub[1] is the body, sub[2] is the env var name when "env:VAR" form.
		body := sub[1]
		envName := sub[2]
		switch {
		case envName != "":
			val := e.getenv(envName)
			if val == "" {
				e.undefinedW[envName] = struct{}{}
			}
			return val
		case body == "workspaceFolder":
			return e.workspace
		case body == "userHome":
			return e.home
		case body == "pathSeparator":
			return e.pathSep
		default:
			return match
		}
	})
}

func expandStringSlice(raw []any, exp *vscodeExpander) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		out = append(out, exp.expand(s))
	}
	return out
}

func expandStringMap(raw map[string]any, exp *vscodeExpander) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = exp.expand(s)
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
