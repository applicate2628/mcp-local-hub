package cbuild

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PresetInfo is the surfaced view of a single preset (configure/build/test/
// workflow). Resolved* fields are computed by walking the inherits chain.
type PresetInfo struct {
	Name              string   `json:"name"`
	DisplayName       string   `json:"displayName,omitempty"`
	Description       string   `json:"description,omitempty"`
	Inherits          []string `json:"inherits,omitempty"`
	Hidden            bool     `json:"hidden,omitempty"`
	ConfigurePreset   string   `json:"configurePreset,omitempty"` // build/test presets only
	ResolvedGenerator string   `json:"resolvedGenerator,omitempty"`
	ResolvedToolchain string   `json:"resolvedToolchain,omitempty"`
	ResolvedCompiler  string   `json:"resolvedCompiler,omitempty"`
	BinaryDir         string   `json:"binaryDir,omitempty"`
}

// PresetsResult is the cmake_list_presets tool payload.
type PresetsResult struct {
	Version          int          `json:"version"`
	Files            []string     `json:"files"`
	ConfigurePresets []PresetInfo `json:"configurePresets"`
	BuildPresets     []PresetInfo `json:"buildPresets"`
	TestPresets      []PresetInfo `json:"testPresets"`
	WorkflowPresets  []PresetInfo `json:"workflowPresets"`
}

// inheritsField decodes CMake's `inherits`, which is either a single string or
// an array of strings.
type inheritsField []string

func (f *inheritsField) UnmarshalJSON(b []byte) error {
	b = trimSpaceBytes(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*f = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*f = []string{s}
	return nil
}

type rawConfigurePreset struct {
	Name           string                     `json:"name"`
	DisplayName    string                     `json:"displayName"`
	Description    string                     `json:"description"`
	Inherits       inheritsField              `json:"inherits"`
	Hidden         bool                       `json:"hidden"`
	Generator      string                     `json:"generator"`
	BinaryDir      string                     `json:"binaryDir"`
	ToolchainFile  string                     `json:"toolchainFile"`
	CacheVariables map[string]json.RawMessage `json:"cacheVariables"`
}

type rawNamedPreset struct {
	Name            string        `json:"name"`
	DisplayName     string        `json:"displayName"`
	Description     string        `json:"description"`
	Inherits        inheritsField `json:"inherits"`
	Hidden          bool          `json:"hidden"`
	ConfigurePreset string        `json:"configurePreset"`
}

type rawPresetsFile struct {
	Version          int                  `json:"version"`
	Include          []string             `json:"include"`
	ConfigurePresets []rawConfigurePreset `json:"configurePresets"`
	BuildPresets     []rawNamedPreset     `json:"buildPresets"`
	TestPresets      []rawNamedPreset     `json:"testPresets"`
	WorkflowPresets  []rawNamedPreset     `json:"workflowPresets"`
}

// Presets is a loaded, merged CMakePresets.json (+ CMakeUserPresets.json +
// their includes). It is read-only.
type Presets struct {
	Version        int
	Files          []string
	configure      map[string]rawConfigurePreset
	configureOrder []string
	build          []rawNamedPreset
	test           []rawNamedPreset
	workflow       []rawNamedPreset
}

// LoadPresets reads CMakePresets.json and CMakeUserPresets.json from dir,
// resolving `include` recursively. At least one of the two files must exist.
func LoadPresets(dir string) (*Presets, error) {
	p := &Presets{configure: map[string]rawConfigurePreset{}}
	visited := map[string]bool{}
	found := false
	for _, base := range []string{"CMakePresets.json", "CMakeUserPresets.json"} {
		path := filepath.Join(dir, base)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found = true
		if err := p.loadFile(path, visited); err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, fmt.Errorf("no CMakePresets.json or CMakeUserPresets.json in %s", dir)
	}
	return p, nil
}

// loadFile parses one presets file and its includes (depth-first). The visited
// set breaks include cycles and de-duplicates shared includes.
func (p *Presets) loadFile(path string, visited map[string]bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if visited[abs] {
		return nil
	}
	visited[abs] = true

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var f rawPresetsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Version > p.Version {
		p.Version = f.Version
	}
	p.Files = append(p.Files, path)

	// Includes are resolved relative to THIS file's directory, and are merged
	// BEFORE this file's own presets so a local preset can inherit from an
	// included base.
	for _, inc := range f.Include {
		incPath := inc
		if !filepath.IsAbs(incPath) {
			incPath = filepath.Join(filepath.Dir(path), inc)
		}
		if err := p.loadFile(incPath, visited); err != nil {
			return err
		}
	}

	for _, cp := range f.ConfigurePresets {
		if cp.Name == "" {
			continue
		}
		if _, exists := p.configure[cp.Name]; !exists {
			p.configureOrder = append(p.configureOrder, cp.Name)
		}
		p.configure[cp.Name] = cp
	}
	p.build = append(p.build, f.BuildPresets...)
	p.test = append(p.test, f.TestPresets...)
	p.workflow = append(p.workflow, f.WorkflowPresets...)
	return nil
}

// Result renders the merged presets into the tool payload, resolving inherited
// fields for each configure preset.
func (p *Presets) Result() PresetsResult {
	res := PresetsResult{Version: p.Version, Files: p.Files}
	res.ConfigurePresets = []PresetInfo{}
	res.BuildPresets = []PresetInfo{}
	res.TestPresets = []PresetInfo{}
	res.WorkflowPresets = []PresetInfo{}

	for _, name := range p.configureOrder {
		cp := p.configure[name]
		merged := p.mergedConfigure(name)
		info := PresetInfo{
			Name:              cp.Name,
			DisplayName:       cp.DisplayName,
			Description:       cp.Description,
			Inherits:          []string(cp.Inherits),
			Hidden:            cp.Hidden,
			ResolvedGenerator: merged.Generator,
			BinaryDir:         merged.BinaryDir,
		}
		info.ResolvedToolchain = resolvedToolchain(merged)
		info.ResolvedCompiler = cacheString(merged.CacheVariables, "CMAKE_CXX_COMPILER")
		if info.ResolvedCompiler == "" {
			info.ResolvedCompiler = cacheString(merged.CacheVariables, "CMAKE_C_COMPILER")
		}
		res.ConfigurePresets = append(res.ConfigurePresets, info)
	}
	for _, bp := range p.build {
		res.BuildPresets = append(res.BuildPresets, namedInfo(bp))
	}
	for _, tp := range p.test {
		res.TestPresets = append(res.TestPresets, namedInfo(tp))
	}
	for _, wp := range p.workflow {
		res.WorkflowPresets = append(res.WorkflowPresets, namedInfo(wp))
	}
	return res
}

func namedInfo(n rawNamedPreset) PresetInfo {
	return PresetInfo{
		Name:            n.Name,
		DisplayName:     n.DisplayName,
		Description:     n.Description,
		Inherits:        []string(n.Inherits),
		Hidden:          n.Hidden,
		ConfigurePreset: n.ConfigurePreset,
	}
}

// mergedConfigure resolves a configure preset against its inherits chain.
// A preset's own fields win over inherited ones; earlier entries in `inherits`
// win over later ones (CMake precedence). cacheVariables are merged key-wise.
func (p *Presets) mergedConfigure(name string) rawConfigurePreset {
	return p.mergeConfigure(name, map[string]bool{})
}

func (p *Presets) mergeConfigure(name string, seen map[string]bool) rawConfigurePreset {
	cp, ok := p.configure[name]
	if !ok || seen[name] {
		return rawConfigurePreset{}
	}
	seen[name] = true

	// Start from the resolved parents (first parent has highest precedence
	// among parents), then overlay this preset's own fields.
	merged := rawConfigurePreset{CacheVariables: map[string]json.RawMessage{}}
	for i := len(cp.Inherits) - 1; i >= 0; i-- {
		parent := p.mergeConfigure(cp.Inherits[i], seen)
		overlay(&merged, parent)
	}
	overlay(&merged, cp)
	return merged
}

// overlay copies non-empty scalar fields from src over dst and merges
// cacheVariables key-wise (src wins).
func overlay(dst *rawConfigurePreset, src rawConfigurePreset) {
	if src.Generator != "" {
		dst.Generator = src.Generator
	}
	if src.BinaryDir != "" {
		dst.BinaryDir = src.BinaryDir
	}
	if src.ToolchainFile != "" {
		dst.ToolchainFile = src.ToolchainFile
	}
	if dst.CacheVariables == nil {
		dst.CacheVariables = map[string]json.RawMessage{}
	}
	for k, v := range src.CacheVariables {
		dst.CacheVariables[k] = v
	}
}

// ResolvedBinaryDir returns the merged (inherits-resolved) binaryDir for a
// configure preset, before macro expansion. Errors when the preset is unknown
// or declares no binaryDir.
func (p *Presets) ResolvedBinaryDir(name string) (string, error) {
	if _, ok := p.configure[name]; !ok {
		return "", fmt.Errorf("configure preset %q not found", name)
	}
	merged := p.mergedConfigure(name)
	if merged.BinaryDir == "" {
		return "", fmt.Errorf("configure preset %q has no binaryDir", name)
	}
	return merged.BinaryDir, nil
}

func resolvedToolchain(cp rawConfigurePreset) string {
	if cp.ToolchainFile != "" {
		return cp.ToolchainFile
	}
	return cacheString(cp.CacheVariables, "CMAKE_TOOLCHAIN_FILE")
}

// cacheString extracts a string cache-variable value. CMake allows either a
// plain string or an object {"type": ..., "value": ...}.
func cacheString(vars map[string]json.RawMessage, key string) string {
	raw, ok := vars[key]
	if !ok {
		return ""
	}
	raw = trimSpaceBytes(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	if raw[0] == '{' {
		var obj struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			return obj.Value
		}
	}
	return ""
}

// expandPresetMacros expands the subset of CMake preset macros needed to
// resolve a binaryDir to a concrete path for the cmake_clean safety guard.
// sourceDir is the project (working) directory. Unresolved macros are left
// intact so the caller can refuse a path it could not fully resolve.
func expandPresetMacros(s, sourceDir, presetName string) string {
	sourceDir = filepath.Clean(sourceDir)
	repl := map[string]string{
		"${sourceDir}":       sourceDir,
		"${sourceParentDir}": filepath.Dir(sourceDir),
		"${sourceDirName}":   filepath.Base(sourceDir),
		"${presetName}":      presetName,
		"${dollar}":          "$",
	}
	for k, v := range repl {
		s = strings.ReplaceAll(s, k, v)
	}
	// $env{VAR} / $penv{VAR}
	s = expandEnvMacro(s, "$env{")
	s = expandEnvMacro(s, "$penv{")
	return s
}

func expandEnvMacro(s, prefix string) string {
	for {
		idx := strings.Index(s, prefix)
		if idx < 0 {
			return s
		}
		end := strings.Index(s[idx:], "}")
		if end < 0 {
			return s
		}
		end += idx
		name := s[idx+len(prefix) : end]
		s = s[:idx] + os.Getenv(name) + s[end+1:]
	}
}

func trimSpaceBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
