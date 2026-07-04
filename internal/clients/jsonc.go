// jsonc.go — shared JSONC (JSON-with-comments) parse + comment-preserving
// write helpers for the JSON-family client adapters.
//
// Several real MCP clients store their config as JSONC / JWCC ("JSON With
// Commas and Comments"): Zed's settings.json ships with a `// Zed settings`
// header, VS Code / Cursor settings.json allow `//` and `/* */` comments and
// trailing commas, and operators routinely hand-edit these files. The strict
// encoding/json parser rejects all of that ("invalid character '/' looking
// for beginning of value"), which previously broke BOTH migrate and the
// Initialize button for every JSONC-config client.
//
// READ side (parseJSONCBytes): strip comments + trailing commas via
// github.com/tailscale/hujson, then json.Unmarshal the standardized bytes
// into a map[string]any — exactly the shape the adapters already operate on.
//
// WRITE side (applyJSONCObjectMember): the adapters used to re-serialize the
// whole parsed map with json.MarshalIndent, which DESTROYS the operator's
// comments and reorders keys. Instead, this helper takes the ORIGINAL on-disk
// bytes and expresses the intended mutation (set or delete one member under a
// top-level object key) as an RFC-6902 JSON Patch applied to the hujson AST,
// then Pack()s the AST back out — preserving every comment, the operator's
// unrelated keys, and the surrounding formatting. The resulting bytes still
// flow through the UNCHANGED WriteConfigFile / SecureWriteClientConfig
// pipeline; only the byte computation changes here.
package clients

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/tailscale/hujson"
)

// readRawConfig returns the raw on-disk config bytes for path, or nil when the
// file is absent (os.IsNotExist). The single owner of the "absent file reads as
// no bytes" convention shared by the JSONC read + comment-preserving write
// seams. Any other read error propagates.
func readRawConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// parseJSONCBytes parses data as JSONC (JSON with `//` + `/* */` comments and
// trailing commas) into a map[string]any. Empty / whitespace-only input
// returns an empty map (preserving the prior json.Unmarshal-on-non-empty
// behavior where an absent or empty file yields an empty map, not an error).
// A nil top-level (literal JSON `null`) is also coerced to an empty map so
// callers never have to nil-check the result.
//
// SAFETY (input is NOT mutated): hujson.Standardize MUTATES its input slice IN
// PLACE (it overwrites comment bytes with spaces while standardizing). To make
// this helper safe for EVERY caller — including a read-then-comment-preserving-
// write path that parses to inspect, then later Pack()s the ORIGINAL bytes to
// keep comments — parseJSONCBytes standardizes a DEFENSIVE COPY, so the `data`
// slice the caller passes is never clobbered. This is the single-owner fix
// (deep-review P4, closing work-items/backlog/2026-06-25-jsonc-parsebytes-
// defensive-copy.md): no caller needs to copy at the call site. An existing
// belt-and-suspenders copy at a call site (e.g. currentClaudeToggleArrays
// passing copyBytes(original)) is now redundant but harmless.
func parseJSONCBytes(data []byte) (map[string]any, error) {
	if len(data) == 0 || len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	std, err := StandardizeJSONC(data)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(std, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// StandardizeJSONC converts a JWCC/JSONC document (JSON with `//` and `/* */`
// comments plus trailing commas) into strict JSON bytes using hujson — the
// SAME parser the client-config adapters use. It is the single owner of
// "how mcphub standardizes a JSONC client config", so the read-only scan and
// manifest-extraction paths (internal/api) accept exactly what the read/write
// adapters accept: a malformed document (e.g. an unterminated `/*` comment, or
// a `//` inside a string literal that a naive byte-stripper would mishandle) is
// REJECTED with an error here, rather than silently transformed into something
// the adapter would later fail to read.
//
// SAFETY (input is NOT mutated): hujson.Standardize overwrites comment bytes
// with spaces IN PLACE, so this standardizes a DEFENSIVE COPY (via
// jsoncTrailingNewline) — the caller's slice is never clobbered.
func StandardizeJSONC(data []byte) ([]byte, error) {
	return hujson.Standardize(jsoncTrailingNewline(data))
}

// jsoncTrailingNewline returns a COPY of data with a single trailing '\n'
// appended when data is not already newline-terminated, so a config ending in a
// `// line comment` with no final newline is parseable by hujson — which
// otherwise reports "parsing comment: unexpected EOF" for `{...} // note<EOF>`
// (a real hand-edited shape for VS Code / Zed / OpenCode configs). The single
// owner of that normalization for BOTH the read path (StandardizeJSONC /
// parseJSONCBytes) AND the comment-preserving WRITE path
// (applyJSONCObjectMemberPath's hujson.Parse), so an operator's newline-less
// trailing-comment config is BOTH scannable and writable — no read-accepts /
// write-rejects divergence. Always a copy (hujson.Standardize/Parse alias +
// mutate their input in place). The extra newline is invariant for valid JSON
// (trailing whitespace is ignored) and only terminates an otherwise-unterminated
// FINAL LINE comment; a truly unterminated `/* block */` comment still
// (correctly) errors. The conditional append avoids adding a redundant blank
// line to an already-terminated file on every comment-preserving write.
func jsoncTrailingNewline(data []byte) []byte {
	if n := len(data); n > 0 && data[n-1] == '\n' {
		buf := make([]byte, n)
		copy(buf, data)
		return buf
	}
	buf := make([]byte, len(data)+1)
	copy(buf, data)
	buf[len(data)] = '\n'
	return buf
}

// jsonPointerEscape escapes a single JSON object key for use as one reference
// token in an RFC-6901 JSON Pointer: `~` -> `~0`, `/` -> `~1`. Order matters
// (`~` first) so a key already containing `~1` is not corrupted. Applied to
// BOTH the section key and the entry name so adapter-supplied names with
// slashes (none expected today, but defense-in-depth) build a valid pointer.
func jsonPointerEscape(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	return token
}

// applyJSONCObjectMember computes the new file bytes for setting or deleting a
// single member `<sectionKey>.<name>` while preserving every comment, unrelated
// key, and formatting detail of `original`.
//
//   - When delete is false: `value` is set at `<sectionKey>.<name>` (added if
//     absent, replaced if present). The top-level `<sectionKey>` object is
//     created first when it does not already exist.
//   - When delete is true: the member at `<sectionKey>.<name>` is removed if
//     present; absence is a no-op (idempotent — mirrors RemoveEntry's
//     delete-of-absent-is-nil contract). `value` is ignored.
//
// `original` is the exact current on-disk content (may be empty/absent). When
// it has no JSON content the caller should NOT reach here — see the adapter
// write seams, which fall back to a plain marshal for the empty/stub case so a
// brand-new file is born as clean indented JSON rather than a single-line pack.
//
// The mutation is applied via hujson's RFC-6902 Patch on the parsed AST, then
// Format()+Pack() serialize it back out. Format is the documented
// post-Patch step; it preserves all comments and never reorders or drops the
// operator's keys (it only normalizes whitespace and indents the newly-added
// member to match its siblings).
func applyJSONCObjectMember(original []byte, sectionKey, name string, value any, del bool) ([]byte, error) {
	return applyJSONCObjectMemberPath(original, []string{sectionKey}, name, value, del)
}

// applyJSONCObjectMemberPath is the multi-level generalization of
// applyJSONCObjectMember: `sectionPath` is the chain of nested object keys under
// which the member `<name>` lives (e.g. {"mcpServers"} for the JSON family,
// {"mcp", "servers"} for OpenClaw). Every intermediate object along the path is
// created (in order) when absent, so a set into a fresh `{ }` builds the whole
// `{"mcp": {"servers": {...}}}` nesting. Comments, unrelated keys, and the
// formatting of `original` are preserved exactly as in the single-key case.
// sectionPath must be non-empty.
func applyJSONCObjectMemberPath(original []byte, sectionPath []string, name string, value any, del bool) ([]byte, error) {
	// Normalize a newline-less trailing `// comment` the SAME way the read path
	// does (jsoncTrailingNewline), so a config the scan/read side now accepts is
	// also writable here — otherwise AddEntry/RemoveEntry/migrate would fail with
	// "parsing comment: unexpected EOF" on a file Scan just reported as valid.
	v, err := hujson.Parse(jsoncTrailingNewline(original))
	if err != nil {
		return nil, fmt.Errorf("parse JSONC: %w", err)
	}

	// Build the cumulative pointer for each path segment so missing
	// intermediates can be created top-down before the member is set.
	sectionPtrs := make([]string, len(sectionPath))
	ptr := ""
	for i, seg := range sectionPath {
		ptr += "/" + jsonPointerEscape(seg)
		sectionPtrs[i] = ptr
	}
	sectionPtr := sectionPtrs[len(sectionPtrs)-1]
	memberPtr := sectionPtr + "/" + jsonPointerEscape(name)

	if del {
		// Removing a member that (or whose parent that) does not exist is a
		// no-op: only emit the remove op when the member is actually present,
		// so a delete-of-absent neither errors nor needlessly rewrites the
		// file's whitespace.
		if v.Find(memberPtr) == nil {
			return v.Pack(), nil
		}
		if err := patchValue(&v, []map[string]any{
			{"op": "remove", "path": memberPtr},
		}); err != nil {
			return nil, err
		}
		v.Format()
		return v.Pack(), nil
	}

	// Set path: ensure each section object along the path exists (top-down),
	// then add/replace the member.
	var ops []map[string]any
	for _, ptr := range sectionPtrs {
		if v.Find(ptr) == nil {
			ops = append(ops, map[string]any{"op": "add", "path": ptr, "value": map[string]any{}})
		}
	}
	ops = append(ops, map[string]any{"op": "add", "path": memberPtr, "value": value})
	if err := patchValue(&v, ops); err != nil {
		return nil, err
	}
	v.Format()
	return v.Pack(), nil
}

// mutateJSONObjectMember reads the live config at path, sets (or deletes) the
// single member `<sectionKey>.<name>`, and writes the result back through the
// shared WriteConfigFile pipeline — preserving comments + unrelated keys when
// the file already has JSONC content.
//
// The empty/absent fallback (no JSON content on disk) builds a fresh
// `{ "<sectionKey>": { ... } }` and writes it with json.MarshalIndent so a
// brand-new file is clean indented JSON. A delete against an empty/absent file
// is a no-op that writes nothing (mirrors the prior delete-of-absent contract,
// where readJSON returned an empty map and writeJSON of the unchanged map was
// a harmless rewrite — here we skip the rewrite entirely).
//
// The byte computation is the ONLY thing that changes; WriteConfigFile (which
// production swaps to api.SecureWriteClientConfig: atomic temp+rename, DACL
// allowlist, flock) is invoked exactly as before.
func mutateJSONObjectMember(path, sectionKey, name string, value any, del bool) error {
	return mutateJSONObjectMemberPath(path, []string{sectionKey}, name, value, del)
}

// mutateJSONObjectMemberPath is the multi-level generalization of
// mutateJSONObjectMember: `sectionPath` is the chain of nested object keys under
// which the member `<name>` lives (single-element {"mcpServers"} for the JSON
// family, two-element {"mcp", "servers"} for OpenClaw). The empty/absent-file
// fallback builds the full fresh nesting via json.MarshalIndent so a brand-new
// file is clean indented JSON; the comment-preserving path goes through
// applyJSONCObjectMemberPath. The byte computation is the ONLY thing that
// differs from the single-key form; WriteConfigFile is invoked exactly as
// before. sectionPath must be non-empty.
func mutateJSONObjectMemberPath(path string, sectionPath []string, name string, value any, del bool) error {
	original, err := readRawConfig(path)
	if err != nil {
		return err
	}

	if len(strings.TrimSpace(string(original))) == 0 {
		// Empty / absent file: no comments or unrelated keys to preserve.
		if del {
			return nil
		}
		// Build `{<seg0>: {<seg1>: {... {<name>: value}}}}` for the path.
		fresh := map[string]any{name: value}
		for i := len(sectionPath) - 1; i >= 0; i-- {
			fresh = map[string]any{sectionPath[i]: fresh}
		}
		out, mErr := json.MarshalIndent(fresh, "", "  ")
		if mErr != nil {
			return mErr
		}
		return WriteConfigFile(path, append(out, '\n'))
	}

	out, err := applyJSONCObjectMemberPath(original, sectionPath, name, value, del)
	if err != nil {
		return fmt.Errorf("rewrite %s: %w", path, err)
	}
	return WriteConfigFile(path, out)
}

// patchValue marshals ops to an RFC-6902 patch document and applies it to v.
// Split out so the set and delete paths share one marshal+apply body. Values
// are marshalled with encoding/json so backslashes, quotes, and unicode in
// adapter-supplied paths (e.g. a Windows `C:\mcphub.exe` command) are escaped
// correctly before hujson re-parses them as patch literals.
func patchValue(v *hujson.Value, ops []map[string]any) error {
	patch, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshal JSONC patch: %w", err)
	}
	if err := v.Patch(patch); err != nil {
		return fmt.Errorf("apply JSONC patch: %w", err)
	}
	return nil
}
