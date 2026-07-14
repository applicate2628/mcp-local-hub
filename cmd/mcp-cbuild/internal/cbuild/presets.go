package cbuild

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	Environment    map[string]*string         `json:"environment"`
	// Condition is the raw preset `condition` (a bool shorthand, null, or a
	// condition object). It is inheritable, so it is merged through the inherits
	// chain and evaluated per host to drop presets disabled on this machine.
	Condition json.RawMessage `json:"condition"`
	fileDir   string
}

type rawNamedPreset struct {
	Name            string          `json:"name"`
	DisplayName     string          `json:"displayName"`
	Description     string          `json:"description"`
	Inherits        inheritsField   `json:"inherits"`
	Hidden          bool            `json:"hidden"`
	ConfigurePreset string          `json:"configurePreset"`
	Condition       json.RawMessage `json:"condition"`
}

type rawPresetsFile struct {
	Version          int                  `json:"version"`
	Include          []string             `json:"include"`
	ConfigurePresets []rawConfigurePreset `json:"configurePresets"`
	BuildPresets     []rawNamedPreset     `json:"buildPresets"`
	TestPresets      []rawNamedPreset     `json:"testPresets"`
	WorkflowPresets  []rawNamedPreset     `json:"workflowPresets"`
}

type duplicatePresetError struct {
	Kind          string
	Name          string
	FirstFile     string
	DuplicateFile string
}

func (e *duplicatePresetError) Error() string {
	return fmt.Sprintf("duplicate %s preset %q in %s (already declared in %s)", e.Kind, e.Name, e.DuplicateFile, e.FirstFile)
}

// Presets is a loaded, merged CMakePresets.json (+ CMakeUserPresets.json +
// their includes). It is read-only.
type Presets struct {
	Version        int
	Files          []string
	sourceDir      string // project dir (holds the top-level presets file); for macro expansion
	configure      map[string]rawConfigurePreset
	configureOrder []string
	configureFiles map[string]string
	build          []rawNamedPreset
	buildFiles     map[string]string
	test           []rawNamedPreset
	testFiles      map[string]string
	workflow       []rawNamedPreset
}

// LoadPresets reads CMakePresets.json and CMakeUserPresets.json from dir,
// resolving `include` recursively. At least one of the two files must exist.
func LoadPresets(dir string) (*Presets, error) {
	p := &Presets{
		sourceDir:      dir,
		configure:      map[string]rawConfigurePreset{},
		configureFiles: map[string]string{},
		buildFiles:     map[string]string{},
		testFiles:      map[string]string{},
	}
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
	// included base. For preset files version 7+, an include entry may itself
	// carry macros (e.g. "$penv{PRESET_DIR}/base.json" or
	// "${sourceDir}/cmake/base.json"); expand them via the shared helper (which
	// is fail-closed on an unset $env{}/$penv{}) BEFORE resolving the path, else
	// the raw macro string would be opened literally and the load would fail.
	for _, inc := range f.Include {
		expandedInc, err := expandPresetMacros(inc, presetMacroContext{
			sourceDir: p.sourceDir,
			fileDir:   filepath.Dir(abs),
		})
		if err != nil {
			return fmt.Errorf("expand include %q in %s: %w", inc, path, err)
		}
		incPath := expandedInc
		if !filepath.IsAbs(incPath) {
			incPath = filepath.Join(filepath.Dir(path), incPath)
		}
		if err := p.loadFile(incPath, visited); err != nil {
			return err
		}
	}

	for _, cp := range f.ConfigurePresets {
		if cp.Name == "" {
			continue
		}
		cp.fileDir = filepath.Dir(abs)
		if firstFile, exists := p.configureFiles[cp.Name]; exists {
			return &duplicatePresetError{Kind: "configure", Name: cp.Name, FirstFile: firstFile, DuplicateFile: abs}
		}
		p.configureFiles[cp.Name] = abs
		p.configureOrder = append(p.configureOrder, cp.Name)
		p.configure[cp.Name] = cp
	}
	for _, bp := range f.BuildPresets {
		if bp.Name == "" {
			continue
		}
		if firstFile, exists := p.buildFiles[bp.Name]; exists {
			return &duplicatePresetError{Kind: "build", Name: bp.Name, FirstFile: firstFile, DuplicateFile: abs}
		}
		p.buildFiles[bp.Name] = abs
	}
	for _, tp := range f.TestPresets {
		if tp.Name == "" {
			continue
		}
		if firstFile, exists := p.testFiles[tp.Name]; exists {
			return &duplicatePresetError{Kind: "test", Name: tp.Name, FirstFile: firstFile, DuplicateFile: abs}
		}
		p.testFiles[tp.Name] = abs
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
		if cp.Hidden {
			continue
		}
		merged := p.mergeConfigure(name, nil, cache)
		// Drop a preset disabled on this host by its (inherits-merged) condition,
		// matching `cmake --list-presets`, which omits condition-disabled presets.
		// A preset we cannot positively evaluate to false is kept (see evalCondition).
		if !evalCondition(merged.Condition, p.sourceDir, name) {
			continue
		}
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
	res.BuildPresets = resolveNamed(p.build, p.sourceDir)
	res.TestPresets = resolveNamed(p.test, p.sourceDir)
	// Workflow presets carry no `condition` field in CMake, so none are filtered;
	// resolveNamed still evaluates the (always-empty → enabled) condition uniformly.
	res.WorkflowPresets = resolveNamed(p.workflow, p.sourceDir)
	return res
}

// resolveNamed surfaces each build/test/workflow preset with its inherited
// fields resolved (currently configurePreset), preserving declaration order and
// de-duplicating by name (first declaration wins, matching configure presets).
func resolveNamed(list []rawNamedPreset, sourceDir string) []PresetInfo {
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
		if n.Hidden {
			continue
		}
		merged := mergeNamed(byName, n.Name, nil, resolved)
		// Drop a build/test preset disabled on this host by its merged condition
		// (workflow presets have no condition, so merged.Condition is empty →
		// enabled). Matches `cmake --list-presets`.
		if !evalCondition(merged.Condition, sourceDir, n.Name) {
			continue
		}
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
// O(V+E) rather than O(2^depth). A merged value's set fields are the scalar
// ConfigurePreset and the read-only Condition byte slice (Inherits is never
// populated on a merge); neither is mutated in place after caching, so the cached
// value carries no shared MUTABLE reference and is safe to return directly.
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
	if len(src.Condition) > 0 && !isJSONNull(src.Condition) {
		dst.Condition = src.Condition
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
	merged := rawConfigurePreset{
		CacheVariables: map[string]json.RawMessage{},
		Environment:    map[string]*string{},
	}
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
	if src.Environment != nil {
		m := make(map[string]*string, len(src.Environment))
		for k, v := range src.Environment {
			m[k] = v
		}
		dst.Environment = m
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
	if len(src.Condition) > 0 && !isJSONNull(src.Condition) {
		dst.Condition = src.Condition
	}
	if dst.CacheVariables == nil {
		dst.CacheVariables = map[string]json.RawMessage{}
	}
	for k, v := range src.CacheVariables {
		dst.CacheVariables[k] = v
	}
	if dst.Environment == nil {
		dst.Environment = map[string]*string{}
	}
	for k, v := range src.Environment {
		dst.Environment[k] = v
	}
}

// ResolvedBinaryDir returns the merged (inherits-resolved) binaryDir for a
// configure preset, before macro expansion. Errors when the preset is unknown
// or declares no binaryDir.
func (p *Presets) ResolvedBinaryDir(name string) (string, error) {
	binaryDir, _, err := p.resolvedBinaryDir(name)
	return binaryDir, err
}

// resolvedBinaryDir returns the merged binaryDir together with every standard
// preset value needed to expand it exactly where local consumers (clean and
// cache-summary reads) need the concrete path.
func (p *Presets) resolvedBinaryDir(name string) (string, presetMacroContext, error) {
	if _, ok := p.configure[name]; !ok {
		return "", presetMacroContext{}, fmt.Errorf("configure preset %q not found", name)
	}
	merged := p.mergedConfigure(name)
	if merged.BinaryDir == "" {
		return "", presetMacroContext{}, fmt.Errorf("configure preset %q has no binaryDir", name)
	}
	definition := p.configure[name]
	return merged.BinaryDir, presetMacroContext{
		sourceDir:   p.sourceDir,
		presetName:  name,
		generator:   merged.Generator,
		fileDir:     definition.fileDir,
		environment: merged.Environment,
	}, nil
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

// presetMacroContext is the resolved configure-preset context used by CMake's
// standard macro expansion in binaryDir.
type presetMacroContext struct {
	sourceDir   string
	presetName  string
	generator   string
	fileDir     string
	environment map[string]*string
}

const escapedDollarMarker = "\x00mcp-cbuild-escaped-dollar\x00"

// expandPresetMacros expands the standard CMake preset macros that can be
// resolved locally. Unknown/vendor namespaces remain intact so path consumers
// can reject them before filesystem cleaning. An unset $env{}/$penv{} or a
// context-dependent macro with no value is a fail-closed error.
func expandPresetMacros(s string, ctx presetMacroContext) (string, error) {
	expanded, err := expandPresetMacrosProtected(s, ctx)
	if err != nil {
		return "", err
	}
	return restoreEscapedDollars(expanded), nil
}

// expandPresetMacrosProtected retains ${dollar} as a private marker while
// expanding environment macros. Path consumers can therefore reject genuinely
// unresolved macros before restoring the escaped literal dollar.
func expandPresetMacrosProtected(s string, ctx presetMacroContext) (string, error) {
	sourceDir := filepath.Clean(ctx.sourceDir)
	if strings.Contains(s, "${generator}") && ctx.generator == "" {
		return "", errors.New("preset macro ${generator} cannot be resolved because the configure preset has no generator")
	}
	if strings.Contains(s, "${fileDir}") && ctx.fileDir == "" {
		return "", errors.New("preset macro ${fileDir} cannot be resolved because the presets-file directory is unknown")
	}
	// Protect escaped dollars before looking for $env{} / $penv{} so a literal
	// `${dollar}env{VAR}` segment is never expanded as an environment macro.
	s = strings.ReplaceAll(s, "${dollar}", escapedDollarMarker)
	repl := strings.NewReplacer(
		"${sourceDir}", sourceDir,
		"${sourceParentDir}", filepath.Dir(sourceDir),
		"${sourceDirName}", filepath.Base(sourceDir),
		"${presetName}", ctx.presetName,
		"${generator}", ctx.generator,
		"${hostSystemName}", cmakeHostSystemName(),
		"${fileDir}", filepath.Clean(ctx.fileDir),
		"${pathListSep}", string(os.PathListSeparator),
	)
	s = repl.Replace(s)
	// $env{VAR} / $penv{VAR}
	s, err := expandEnvMacroWithLookup(s, "$env{", func(name string) (string, bool) {
		if value, ok := ctx.environment[name]; ok && value != nil {
			return *value, true
		}
		return os.LookupEnv(name)
	})
	if err != nil {
		return "", err
	}
	s, err = expandEnvMacroWithLookup(s, "$penv{", os.LookupEnv)
	if err != nil {
		return "", err
	}
	return s, nil
}

func restoreEscapedDollars(s string) string {
	return strings.ReplaceAll(s, escapedDollarMarker, "$")
}

// expandEnvMacro substitutes every prefix{NAME} occurrence with the value of the
// NAME environment variable. An UNSET variable is a fail-closed error. The scan
// cursor advances past each substituted value so a self-referential value (one
// that itself contains the prefix) cannot trigger an infinite rescan.
func expandEnvMacro(s, prefix string) (string, error) {
	return expandEnvMacroWithLookup(s, prefix, os.LookupEnv)
}

func expandEnvMacroWithLookup(s, prefix string, lookup func(string) (string, bool)) (string, error) {
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
		val, ok := lookup(name)
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

// isJSONNull reports whether b is the JSON literal null (or empty).
func isJSONNull(b json.RawMessage) bool {
	s := string(trimSpaceBytes(b))
	return s == "" || s == "null"
}

// --- preset condition evaluation --------------------------------------------

// cmakeCondition is a decoded preset `condition` object. Only the fields
// relevant to the active `type` are populated by any given condition.
type cmakeCondition struct {
	Type       string            `json:"type"`
	Value      *bool             `json:"value"`      // const
	Lhs        string            `json:"lhs"`        // equals / notEquals
	Rhs        string            `json:"rhs"`        // equals / notEquals
	String     string            `json:"string"`     // inList / notInList / matches / notMatches
	List       []string          `json:"list"`       // inList / notInList
	Regex      string            `json:"regex"`      // matches / notMatches
	Condition  json.RawMessage   `json:"condition"`  // not
	Conditions []json.RawMessage `json:"conditions"` // anyOf / allOf
}

// evalCondition reports whether a preset carrying condition raw is ENABLED on the
// current host. It is FAIL-OPEN: an absent/null condition, a boolean shorthand, an
// unknown/future condition type, an unresolvable macro (unset env, unknown
// namespace), or a regex that will not compile all resolve to ENABLED.
// cmake_list_presets must never HIDE a preset it merely failed to understand —
// over-listing a preset the agent can still attempt is safer than silently
// dropping a usable one. Only a condition we POSITIVELY evaluate to false disables
// (and thus excludes) a preset, matching `cmake --list-presets`.
func evalCondition(raw json.RawMessage, sourceDir, presetName string) bool {
	enabled, _ := evalConditionResolved(raw, sourceDir, presetName)
	return enabled
}

// evalConditionResolved returns (enabled, resolved). resolved reports whether the
// condition was positively evaluated; resolved==false means "could not determine"
// and the caller treats it as enabled. The three-valued logic lets anyOf/allOf
// combine unknown sub-conditions correctly (unknown OR true == true; unknown AND
// false == false).
func evalConditionResolved(raw json.RawMessage, sourceDir, presetName string) (enabled, resolved bool) {
	raw = trimSpaceBytes(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return true, true // no condition → enabled
	}
	// Boolean shorthand: `"condition": true|false`.
	switch string(raw) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	if raw[0] != '{' {
		return true, false // unexpected shape → treat as enabled
	}
	var c cmakeCondition
	if err := json.Unmarshal(raw, &c); err != nil {
		return true, false
	}
	switch c.Type {
	case "const":
		if c.Value == nil {
			return true, false
		}
		return *c.Value, true
	case "equals", "notEquals":
		lhs, ok1 := expandConditionString(c.Lhs, sourceDir, presetName)
		rhs, ok2 := expandConditionString(c.Rhs, sourceDir, presetName)
		if !ok1 || !ok2 {
			return true, false
		}
		eq := lhs == rhs
		if c.Type == "notEquals" {
			eq = !eq
		}
		return eq, true
	case "inList", "notInList":
		str, ok := expandConditionString(c.String, sourceDir, presetName)
		if !ok {
			return true, false
		}
		found := false
		for _, item := range c.List {
			it, okItem := expandConditionString(item, sourceDir, presetName)
			if !okItem {
				return true, false
			}
			if it == str {
				found = true
				break
			}
		}
		if c.Type == "notInList" {
			found = !found
		}
		return found, true
	case "matches", "notMatches":
		str, ok1 := expandConditionString(c.String, sourceDir, presetName)
		rx, ok2 := expandConditionString(c.Regex, sourceDir, presetName)
		if !ok1 || !ok2 {
			return true, false
		}
		// CMake uses ECMAScript regex; Go's RE2 differs on exotic constructs. A
		// pattern RE2 cannot compile is treated as unresolvable → enabled.
		re, err := regexp.Compile(rx)
		if err != nil {
			return true, false
		}
		m := re.MatchString(str)
		if c.Type == "notMatches" {
			m = !m
		}
		return m, true
	case "anyOf":
		anyTrue, anyUnknown := false, false
		for _, sub := range c.Conditions {
			en, res := evalConditionResolved(sub, sourceDir, presetName)
			if !res {
				anyUnknown = true
				continue
			}
			if en {
				anyTrue = true
			}
		}
		if anyTrue {
			return true, true // OR short-circuits on any true, even with unknowns
		}
		if anyUnknown {
			return true, false // no true, some unknown → cannot resolve → enabled
		}
		return false, true // all sub-conditions positively false
	case "allOf":
		anyFalse, anyUnknown := false, false
		for _, sub := range c.Conditions {
			en, res := evalConditionResolved(sub, sourceDir, presetName)
			if !res {
				anyUnknown = true
				continue
			}
			if !en {
				anyFalse = true
			}
		}
		if anyFalse {
			return false, true // AND short-circuits on any false, even with unknowns
		}
		if anyUnknown {
			return true, false // no false, some unknown → cannot resolve → enabled
		}
		return true, true // all sub-conditions positively true
	case "not":
		en, res := evalConditionResolved(c.Condition, sourceDir, presetName)
		if !res {
			return true, false
		}
		return !en, true
	default:
		return true, false // unknown/future type → enabled (fail-open)
	}
}

// expandConditionString expands the macros CMake allows inside a condition's
// string operands. It returns ok=false when it meets a macro it cannot resolve
// (an unset $env{}/$penv{}, or any remaining ${...} / $<namespace>{...} token);
// the caller then treats the whole condition as unresolvable → enabled. Unlike
// expandPresetMacros (which hard-errors on an unset env so the purge guard fails
// closed), a condition must NOT abort listing on an unset env — it just becomes
// unresolvable.
func expandConditionString(s, sourceDir, presetName string) (string, bool) {
	sourceDir = filepath.Clean(sourceDir)
	repl := map[string]string{
		"${sourceDir}":       sourceDir,
		"${sourceParentDir}": filepath.Dir(sourceDir),
		"${sourceDirName}":   filepath.Base(sourceDir),
		"${presetName}":      presetName,
		"${hostSystemName}":  cmakeHostSystemName(),
		"${dollar}":          "$",
	}
	for k, v := range repl {
		s = strings.ReplaceAll(s, k, v)
	}
	var ok bool
	if s, ok = expandConditionEnv(s, "$env{"); !ok {
		return s, false
	}
	if s, ok = expandConditionEnv(s, "$penv{"); !ok {
		return s, false
	}
	// Any remaining macro token (unknown ${...} or $<namespace>{...}, or a
	// malformed $env{ with no closing brace) → unresolvable.
	if containsUnexpandedMacro(s) {
		return s, false
	}
	return s, true
}

// expandConditionEnv substitutes every prefix{NAME} with the NAME env value,
// returning ok=false on the first UNSET variable (the caller treats the condition
// as unresolvable rather than aborting the whole listing). The scan cursor
// advances past each substituted value so a self-referential value cannot loop.
func expandConditionEnv(s, prefix string) (string, bool) {
	searchFrom := 0
	for {
		rel := strings.Index(s[searchFrom:], prefix)
		if rel < 0 {
			return s, true
		}
		idx := searchFrom + rel
		end := strings.Index(s[idx:], "}")
		if end < 0 {
			// Malformed (no closing brace): leave literal; containsUnexpandedMacro
			// in the caller then flags it as unresolvable.
			return s, true
		}
		end += idx
		name := s[idx+len(prefix) : end]
		val, found := os.LookupEnv(name)
		if !found {
			return s, false
		}
		s = s[:idx] + val + s[end+1:]
		searchFrom = idx + len(val)
	}
}

// cmakeHostSystemName maps the Go runtime to CMake's ${hostSystemName}
// (CMAKE_HOST_SYSTEM_NAME): Windows / Linux / Darwin for the common hosts. For a
// less-common GOOS the CMake uname-style name is approximated by capitalizing the
// GOOS; a mismatch there only over-lists a preset (the fail-open direction).
func cmakeHostSystemName() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "darwin":
		return "Darwin"
	default:
		if runtime.GOOS == "" {
			return ""
		}
		return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
	}
}
