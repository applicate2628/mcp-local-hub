// Package cbuild implements the mcp-cbuild tool set: CMake configure/build/test
// and vcpkg dependency management, plus the single multi-format build-output
// diagnostics parser. It is fully self-contained and imports no mcphub
// internal packages.
package cbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/cmd/mcp-cbuild/internal/mcp"
)

// Default per-call exec timeouts (each clamped to the hard cap in exec.go).
const (
	defaultConfigureTimeout = 5 * time.Minute
	defaultBuildTimeout     = 10 * time.Minute
	defaultTestTimeout      = 30 * time.Minute
	defaultWorkflowTimeout  = 40 * time.Minute
	defaultCleanTimeout     = 5 * time.Minute
	defaultVcpkgTimeout     = 30 * time.Minute
	defaultQueryTimeout     = 2 * time.Minute
)

// builder carries the launch-time default working directory shared by every
// tool.
type builder struct {
	defaultDir string
}

// Tools constructs the 10 mcp-cbuild tools bound to defaultDir (used when a
// tool call omits working_dir).
func Tools(defaultDir string) []mcp.Tool {
	b := &builder{defaultDir: defaultDir}
	return []mcp.Tool{
		b.cmakeListPresets(),
		b.cmakeConfigure(),
		b.cmakeBuild(),
		b.cmakeTest(),
		b.cmakeWorkflow(),
		b.cmakeClean(),
		b.vcpkgInstall(),
		b.vcpkgList(),
		b.vcpkgManifest(),
		b.vcpkgSearch(),
	}
}

// funcTool is a small adapter from a handler function to the mcp.Tool
// interface.
type funcTool struct {
	name        string
	title       string
	description string
	schema      map[string]any
	handler     func(ctx context.Context, args json.RawMessage) (any, error)
}

func (t *funcTool) Name() string                { return t.name }
func (t *funcTool) Title() string               { return t.title }
func (t *funcTool) Description() string         { return t.description }
func (t *funcTool) InputSchema() map[string]any { return t.schema }
func (t *funcTool) Call(ctx context.Context, args json.RawMessage) (any, error) {
	return t.handler(ctx, args)
}

// workingDir resolves the effective working directory for a call. An empty
// arg falls back to the launch default; a provided value must be an absolute,
// existing directory.
func (b *builder) workingDir(raw string) (string, error) {
	dir := raw
	if dir == "" {
		dir = b.defaultDir
	}
	if dir == "" {
		if cwd, err := os.Getwd(); err == nil {
			dir = cwd
		}
	}
	if raw != "" && !filepath.IsAbs(raw) {
		return "", mcp.NewParamError("working_dir must be an absolute path: %q", raw)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", mcp.NewParamError("working_dir does not exist or is not a directory: %q", dir)
	}
	return dir, nil
}

// decodeArgs unmarshals the tools/call arguments into v, treating an
// absent/null arguments object as an empty struct. A decode failure is a
// caller (param) error.
func decodeArgs(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return mcp.NewParamError("invalid arguments: %v", err)
	}
	return nil
}

// timeoutOrDefault converts an optional per-call timeout (seconds) to a
// duration, falling back to def when unset. Zero/negative means "use default".
func timeoutOrDefault(sec int, def time.Duration) time.Duration {
	if sec <= 0 {
		return def
	}
	return time.Duration(sec) * time.Second
}

// --- JSON Schema builders ----------------------------------------------------

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func strArrayProp(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func strMapProp(desc string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
		"description":          desc,
	}
}

// workingDirProp is the shared working_dir property description.
func workingDirProp() map[string]any {
	return strProp("Absolute path to the project directory (holds CMakePresets.json / vcpkg.json). Defaults to the server launch directory.")
}
