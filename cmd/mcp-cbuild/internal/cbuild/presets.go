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

	// One resolution cache shared across every configure preset: a merged value
	// is path-independent, so resolving the whole set against a single cache keeps
	// the pass O(V+E) instead of re-walking shared subtrees once per preset.
	cache := map[string]rawConfigurePreset{}
	for _, name := range p.configureOrder {
		cp := p.configure[name]
		merged := p.mergeConfigure(name, nil, cache)
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
	res.BuildPresets = resolveNamed(p.build)
	res.TestPresets = resolveNamed(p.test)
	res.WorkflowPresets = resolveNamed(p.workflow)
	return res
}

// resolveNamed surfaces each build/test/workflow preset with its inherited
// fields resolved (currently configurePreset), preserving declaration order and
// de-duplicating by name (first declaration wins, matching configure presets).
func resolveNamed(list []rawNamedPreset) []PresetInfo {
	byName := make(map[string]rawNamedPreset, len(list))
	for _, n := range list {
		if n.Name == "" {
			continue
		}
		if _, exists := byName[n.Name]; !exists {
			byName[n.Name] = n
		}
	}
	out := []PresetInfo{}
	emitted := map[string]bool{}
	// Shared resolution cache (see mergeNamed): keeps the whole list O(V+E)
	// instead of O(2^depth) on diamond-inheritance named presets.
	resolved := map[string]rawNamedPreset{}
	for _, n := range list {
		if n.Name == "" || emitted[n.Name] {
			continue
		}
		emitted[n.Name] = true
		merged := mergeNamed(byName, n.Name, nil, resolved)
		out = append(out, PresetInfo{
			Name:        n.Name,
			DisplayName: n.DisplayName,
			Description: n.Description,
			Inherits:    []string(n.Inherits),
			Hidden:      n.Hidden,
			// configurePreset is the one inheritable field surfaced here: a
			// build/test preset that omits it inherits it from its chain.
			ConfigurePreset: merged.ConfigurePreset,
		})
	}
	return out
}

// mergeNamed resolves a named preset against its inherits chain. Precedence
// matches CMake and mergeConfigure: the preset's own value wins over inherited
// ones, and earlier entries in `inherits` win over later ones. chain carries the
// current resolution path (per-branch) for cycle detection only, so a preset
// shared across sibling branches contributes at each branch's correct precedence.
//
// resolved memoizes each node's merged value BY NAME (path-independent, same
// rationale as mergeConfigure) so diamond-inheritance named presets resolve in
// O(V+E) rather than O(2^depth). A merged value's only set field is the scalar
// ConfigurePreset (Inherits is never populated on a merge), so the cached value
// carries no shared mutable reference and is safe to return directly.
func mergeNamed(byName map[string]rawNamedPreset, name string, chain []string, resolved map[string]rawNamedPreset) rawNamedPreset {
	cur, ok := byName[name]
	if !ok {
		return rawNamedPreset{}
	}
	for _, n := range chain {
		if n == name {
			return rawNamedPreset{} // cycle on this path
		}
	}
	if cached, ok := resolved[name]; ok {
		return cached // off-path node; value-type fields only
	}
	childChain := append(append([]string{}, chain...), name)

	merged := rawNamedPreset{}
	for i := len(cur.Inherits) - 1; i >= 0; i-- {
		parent := mergeNamed(byName, cur.Inherits[i], childChain, resolved)
		overlayNamed(&merged, parent)
	}
	overlayNamed(&merged, cur)
	resolved[name] = merged
	return merged
}

// overlayNamed copies the inheritable configurePreset field from src over dst
// when set. Name/DisplayName/Description/Hidden/Inherits are a preset's own
// identity and are never inherited.
func overlayNamed(dst *rawNamedPreset, src rawNamedPreset) {
	if src.ConfigurePreset != "" {
		dst.ConfigurePreset = src.ConfigurePreset
	}
}

// mergedConfigure resolves a configure preset against its inherits chain.
// A preset's own fields win over inherited ones; earlier entries in `inherits`
// win over later ones (CMake precedence). cacheVariables are merged key-wise.
func (p *Presets) mergedConfigure(name string) rawConfigurePreset {
	return p.mergeConfigure(name, nil, map[string]rawConfigurePreset{})
}

// mergeConfigure resolves a configure preset against its inherits chain. chain
// is the set of preset names on the CURRENT resolution path, used only for
// cycle detection; each parent is resolved with its own chain copy. A shared
// visited-set (the earlier design) wrongly skipped a parent the first time it
// was reached on any branch, which let a lower-precedence sibling branch's
// binaryDir win — a correctness AND safety bug, since the purge guard resolves
// against the winning binaryDir.
//
// resolved memoizes each node's fully-merged value BY NAME. A merged value is
// path-independent (own-fields-win / earlier-inherit-wins holds regardless of
// which caller reached the node), so a node NOT currently on the resolution path
// may reuse its cached result. This keeps resolution O(V+E) for
// diamond-inheritance DAGs instead of O(2^depth): the per-path chain alone (with
// no memoization) re-resolves a node's whole subtree once per inheritance path,
// which a crafted CMakePresets.json turns into an uninterruptible CPU DoS. The
// chain still owns cycle detection; the cache only short-circuits off-path nodes,
// so a cyclic path is always broken and never served a stale cache entry.
func (p *Presets) mergeConfigure(name string, chain []string, resolved map[string]rawConfigurePreset) rawConfigurePreset {
	cp, ok := p.configure[name]
	if !ok {
		return rawConfigurePreset{}
	}
	for _, n := range chain {
		if n == name {
			return rawConfigurePreset{} // cycle on this path
		}
	}
	// Cache hit for an off-path node: return a copy so a caller that overlays
	// onto the result cannot mutate the shared cache entry.
	if cached, ok := resolved[name]; ok {
		return copyConfigure(cached)
	}
	childChain := append(append([]string{}, chain...), name)

	// Start from the resolved parents (first parent has highest precedence
	// among parents), then overlay this preset's own fields.
	merged := rawConfigurePreset{CacheVariables: map[string]json.RawMessage{}}
	for i := len(cp.Inherits) - 1; i >= 0; i-- {
		parent := p.mergeConfigure(cp.Inherits[i], childChain, resolved)
		overlay(&merged, parent)
	}
	overlay(&merged, cp)
	// The stored entry is only ever read (overlaid as a parent, or read scalar-
	// wise by callers); every later reuse goes through copyConfigure, so the
	// cache map is never mutated after insertion.
	resolved[name] = merged
	return merged
}

// copyConfigure returns a clone of a cached resolved preset that is safe to
// overlay onto a caller's merge: scalar fields copy by value and CacheVariables
// gets a fresh map. The json.RawMessage values are never mutated in place (only
// re-pointed by overlay / read by cacheString), so sharing the byte slices is
// safe; only the map itself must be private per copy.
func copyConfigure(src rawConfigurePreset) rawConfigurePreset {
	dst := src
	if src.CacheVariables != nil {
		m := make(map[string]json.RawMessage, len(src.CacheVariables))
		for k, v := range src.CacheVariables {
			m[k] = v
		}
		dst.CacheVariables = m
	}
	return dst
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
// sourceDir is the project (working) directory. Non-env unresolved macros are
// left intact so the caller can refuse a path it could not fully resolve; an
// UNSET $env{}/$penv{} macro is a fail-closed error (never silently collapsed to
// an empty string, which would let the purge guard trust a wrong directory).
func expandPresetMacros(s, sourceDir, presetName string) (string, error) {
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
	s, err := expandEnvMacro(s, "$env{")
	if err != nil {
		return "", err
	}
	s, err = expandEnvMacro(s, "$penv{")
	if err != nil {
		return "", err
	}
	return s, nil
}

// expandEnvMacro substitutes every prefix{NAME} occurrence with the value of the
// NAME environment variable. An UNSET variable is a fail-closed error. The scan
// cursor advances past each substituted value so a self-referential value (one
// that itself contains the prefix) cannot trigger an infinite rescan.
func expandEnvMacro(s, prefix string) (string, error) {
	searchFrom := 0
	for {
		rel := strings.Index(s[searchFrom:], prefix)
		if rel < 0 {
			return s, nil
		}
		idx := searchFrom + rel
		end := strings.Index(s[idx:], "}")
		if end < 0 {
			// Malformed (no closing brace): leave the literal in place so the
			// caller's unresolved-macro check refuses the path.
			return s, nil
		}
		end += idx
		name := s[idx+len(prefix) : end]
		val, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("preset env macro %s%s} is unset — refusing to resolve binaryDir to an empty path", prefix, name)
		}
		s = s[:idx] + val + s[end+1:]
		// Resume AFTER the substituted value: a value that happens to contain
		// the prefix is treated as a literal, not re-expanded.
		searchFrom = idx + len(val)
	}
}

func trimSpaceBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
