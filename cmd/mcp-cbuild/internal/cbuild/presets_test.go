package cbuild

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// writeFile is a test helper that writes content to dir/name.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// realDir resolves symlinks in dir so tests can compare against the canonical
// (symlink-free) path resolveBuildDirWithinSource returns for RemoveAll (on
// e.g. macOS, t.TempDir() sits under the /var -> /private/var symlink).
func realDir(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return real
}

// findPreset returns the PresetInfo with the given name, or fails.
func findPreset(t *testing.T, ps []PresetInfo, name string) PresetInfo {
	t.Helper()
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("preset %q not found in %+v", name, ps)
	return PresetInfo{}
}

func TestLoadPresetsIncludeAndInherits(t *testing.T) {
	dir := t.TempDir()

	// An included base file supplies two hidden base presets.
	writeFile(t, dir, "common.json", `{
      "version": 3,
      "configurePresets": [
        {
          "name": "base",
          "hidden": true,
          "generator": "Ninja",
          "binaryDir": "${sourceDir}/build/${presetName}",
          "cacheVariables": { "CMAKE_CXX_COMPILER": "clang++" }
        },
        {
          "name": "base2",
          "hidden": true,
          "generator": "Unix Makefiles",
          "cacheVariables": { "CMAKE_C_COMPILER": "gcc" }
        }
      ]
    }`)

	// The top file includes common.json and defines a preset that inherits both
	// bases (base first — it must win on the shared generator field).
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 3,
      "include": ["common.json"],
      "configurePresets": [
        {
          "name": "dev",
          "displayName": "Dev build",
          "inherits": ["base", "base2"],
          "cacheVariables": { "CMAKE_BUILD_TYPE": "Debug" }
        }
      ],
      "buildPresets": [ { "name": "dev-build", "configurePreset": "dev" } ],
      "testPresets": [ { "name": "dev-test", "configurePreset": "dev" } ]
    }`)

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	res := p.Result()

	if res.Version != 3 {
		t.Errorf("version = %d, want 3", res.Version)
	}
	if len(res.Files) != 2 {
		t.Errorf("Files = %v, want both presets files loaded", res.Files)
	}

	dev := findPreset(t, res.ConfigurePresets, "dev")
	if dev.DisplayName != "Dev build" {
		t.Errorf("dev.DisplayName = %q", dev.DisplayName)
	}
	if len(dev.Inherits) != 2 || dev.Inherits[0] != "base" || dev.Inherits[1] != "base2" {
		t.Errorf("dev.Inherits = %v", dev.Inherits)
	}
	// generator: base wins over base2 (first in inherits list).
	if dev.ResolvedGenerator != "Ninja" {
		t.Errorf("dev.ResolvedGenerator = %q, want Ninja (base wins over base2)", dev.ResolvedGenerator)
	}
	// compiler + binaryDir are inherited from base.
	if dev.ResolvedCompiler != "clang++" {
		t.Errorf("dev.ResolvedCompiler = %q, want clang++", dev.ResolvedCompiler)
	}
	if dev.BinaryDir != "${sourceDir}/build/${presetName}" {
		t.Errorf("dev.BinaryDir = %q", dev.BinaryDir)
	}

	// Named build/test presets are surfaced with their configurePreset link.
	bp := findPreset(t, res.BuildPresets, "dev-build")
	if bp.ConfigurePreset != "dev" {
		t.Errorf("build preset configurePreset = %q, want dev", bp.ConfigurePreset)
	}
	tp := findPreset(t, res.TestPresets, "dev-test")
	if tp.ConfigurePreset != "dev" {
		t.Errorf("test preset configurePreset = %q, want dev", tp.ConfigurePreset)
	}

	// ResolvedBinaryDir returns the merged (inherited) binaryDir.
	bd, err := p.ResolvedBinaryDir("dev")
	if err != nil {
		t.Fatalf("ResolvedBinaryDir: %v", err)
	}
	if bd != "${sourceDir}/build/${presetName}" {
		t.Errorf("ResolvedBinaryDir = %q", bd)
	}
}

func TestLoadPresetsMissing(t *testing.T) {
	if _, err := LoadPresets(t.TempDir()); err == nil {
		t.Fatal("expected error for a directory with no presets file")
	}
}

// TestListPresetsExcludesHiddenBases proves inheritance-only presets still
// contribute their fields without being surfaced as runnable --preset choices.
func TestListPresetsExcludesHiddenBases(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 6,
      "configurePresets": [
        { "name": "base", "hidden": true, "generator": "Ninja",
          "binaryDir": "${sourceDir}/build/${presetName}" },
        { "name": "dev", "inherits": ["base"] }
      ],
      "buildPresets": [
        { "name": "base-build", "hidden": true, "configurePreset": "dev" },
        { "name": "dev-build", "inherits": ["base-build"] }
      ],
      "testPresets": [
        { "name": "base-test", "hidden": true, "configurePreset": "dev" },
        { "name": "dev-test", "inherits": ["base-test"] }
      ]
    }`)

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	res := p.Result()

	if len(res.ConfigurePresets) != 1 || res.ConfigurePresets[0].Name != "dev" {
		t.Fatalf("configure presets = %v, want only runnable child dev", res.ConfigurePresets)
	}
	if len(res.BuildPresets) != 1 || res.BuildPresets[0].Name != "dev-build" {
		t.Fatalf("build presets = %v, want only runnable child dev-build", res.BuildPresets)
	}
	if len(res.TestPresets) != 1 || res.TestPresets[0].Name != "dev-test" {
		t.Fatalf("test presets = %v, want only runnable child dev-test", res.TestPresets)
	}
	if got := res.ConfigurePresets[0].ResolvedGenerator; got != "Ninja" {
		t.Errorf("dev.ResolvedGenerator = %q, want inherited Ninja", got)
	}
	if got := res.BuildPresets[0].ConfigurePreset; got != "dev" {
		t.Errorf("dev-build.ConfigurePreset = %q, want inherited dev", got)
	}
	if got := res.TestPresets[0].ConfigurePreset; got != "dev" {
		t.Errorf("dev-test.ConfigurePreset = %q, want inherited dev", got)
	}
}

func TestResolveBuildDirWithinSourceGuard(t *testing.T) {
	src := t.TempDir()

	// Inside the source tree: allowed, returns the resolved absolute path.
	got, err := resolveBuildDirWithinSource("${sourceDir}/build", src, "dev")
	if err != nil {
		t.Fatalf("inside-source binaryDir rejected: %v", err)
	}
	if got != filepath.Join(realDir(t, src), "build") {
		t.Errorf("resolved = %q, want %q", got, filepath.Join(realDir(t, src), "build"))
	}

	// Escaping the source tree: refused.
	if _, err := resolveBuildDirWithinSource("${sourceDir}/../evil", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir escaping the source directory")
	}
	// The source directory itself: refused.
	if _, err := resolveBuildDirWithinSource("${sourceDir}", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir equal to the source directory")
	}
	// Unresolved macro: refused.
	if _, err := resolveBuildDirWithinSource("${unknownMacro}/build", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir with unresolved macros")
	}
}

// TestMergeConfigureSharedParentPrecedence pins the inherits-precedence fix: a
// grandparent shared across two inheritance branches must contribute at each
// branch's correct precedence. Here `dev` inherits [viaA, viaB]; both inherit
// `common` (binaryDir=common-build); viaB overrides it to b-build. Because viaA
// (which keeps common's value) is EARLIER in dev's inherits list, it wins, so
// dev.binaryDir must resolve to common-build. The earlier shared-visited-set
// design let the lower-precedence viaB value win.
func TestMergeConfigureSharedParentPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 3,
      "configurePresets": [
        { "name": "common", "hidden": true, "generator": "Ninja",
          "binaryDir": "${sourceDir}/common-build" },
        { "name": "viaA", "hidden": true, "inherits": ["common"] },
        { "name": "viaB", "hidden": true, "inherits": ["common"],
          "binaryDir": "${sourceDir}/b-build" },
        { "name": "dev", "inherits": ["viaA", "viaB"] }
      ]
    }`)

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	bd, err := p.ResolvedBinaryDir("dev")
	if err != nil {
		t.Fatalf("ResolvedBinaryDir: %v", err)
	}
	if bd != "${sourceDir}/common-build" {
		t.Errorf("dev binaryDir = %q, want ${sourceDir}/common-build (viaA outranks viaB)", bd)
	}
}

// TestMergeConfigureNestedDiamondNotExponential proves preset inherit resolution
// is memoized (O(V+E)), not O(2^depth). Every level's two nodes (a{i}, b{i})
// inherit BOTH of the next level's two nodes, so the graph is a legal ACYCLIC DAG
// with 2^levels distinct root->leaf paths. Without per-name memoization each
// shared subtree is re-resolved once per path and cmake_list_presets hangs for
// minutes-to-hours on a crafted (cloned-repo) presets file — an uninterruptible
// CPU DoS, since resolution is wrapped by no timeout and merge* has no ctx check.
// cycle detection never fires (acyclic), so only memoization bounds it. The
// single-diamond TestMergeConfigureSharedParentPrecedence is microseconds even
// unfixed and masks this blowup.
func TestMergeConfigureNestedDiamondNotExponential(t *testing.T) {
	const levels = 30 // 2^30 ≈ 1.07e9 merge calls without memoization → minutes+

	// root inherits [a0,b0]; a{i}/b{i} inherit [a{i+1},b{i+1}] for i in [0,levels);
	// the leaves a{levels}/b{levels} inherit nothing. The all-"a" branch is the
	// highest-precedence ancestor at every level (root's first parent is a0, a0's
	// first parent is a1, ...), so root must resolve to a{levels}'s values.
	var sb strings.Builder
	sb.WriteString(`{"version":3,"configurePresets":[`)
	sb.WriteString(`{"name":"root","inherits":["a0","b0"]}`)
	for i := 0; i < levels; i++ {
		fmt.Fprintf(&sb, `,{"name":"a%d","hidden":true,"inherits":["a%d","b%d"]}`, i, i+1, i+1)
		fmt.Fprintf(&sb, `,{"name":"b%d","hidden":true,"inherits":["a%d","b%d"]}`, i, i+1, i+1)
	}
	fmt.Fprintf(&sb, `,{"name":"a%d","hidden":true,"generator":"Ninja","binaryDir":"${sourceDir}/leaf-build"}`, levels)
	fmt.Fprintf(&sb, `,{"name":"b%d","hidden":true,"generator":"Unix Makefiles","binaryDir":"${sourceDir}/b-leaf"}`, levels)
	sb.WriteString(`]}`)

	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", sb.String())

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}

	// Result() resolves EVERY preset (the read-only cmake_list_presets path). Run
	// it off-goroutine: a memoized pass finishes in well under a second; an
	// exponential one cannot complete in 2s (it needs ~1e9 merge calls).
	done := make(chan PresetsResult, 1)
	go func() { done <- p.Result() }()
	select {
	case res := <-done:
		// Correctness under memoization: the cache must return path-independent
		// results WITHOUT corrupting precedence — the all-"a" branch wins, so root
		// resolves to a{levels}'s values, not b{levels}'s.
		root := findPreset(t, res.ConfigurePresets, "root")
		if root.ResolvedGenerator != "Ninja" {
			t.Errorf("root.ResolvedGenerator = %q, want Ninja (all-a branch wins)", root.ResolvedGenerator)
		}
		if root.BinaryDir != "${sourceDir}/leaf-build" {
			t.Errorf("root.BinaryDir = %q, want ${sourceDir}/leaf-build", root.BinaryDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cmake_list_presets did not finish in 2s on a nested-diamond preset graph — inherit resolution is still exponential (memoization missing)")
	}
}

// TestResolveBuildDirUnsetEnvMacroFailClosed proves an unset $env{} macro is a
// fail-closed error, never collapsed to an empty string the purge guard trusts.
func TestResolveBuildDirUnsetEnvMacroFailClosed(t *testing.T) {
	src := t.TempDir()

	// Unset env var → refused.
	if _, err := resolveBuildDirWithinSource("$env{MCP_CBUILD_DEFINITELY_UNSET}/build", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir referencing an unset $env{} macro")
	}
	if _, err := resolveBuildDirWithinSource("$penv{MCP_CBUILD_DEFINITELY_UNSET}/build", src, "dev"); err == nil {
		t.Error("expected refusal for a binaryDir referencing an unset $penv{} macro")
	}

	// Set env var pointing inside the tree → resolves.
	t.Setenv("MCP_CBUILD_TESTDIR", "build-from-env")
	got, err := resolveBuildDirWithinSource("$env{MCP_CBUILD_TESTDIR}", src, "dev")
	if err != nil {
		t.Fatalf("set env macro rejected: %v", err)
	}
	if got != filepath.Join(realDir(t, src), "build-from-env") {
		t.Errorf("resolved = %q, want %q", got, filepath.Join(realDir(t, src), "build-from-env"))
	}
}

// TestExpandEnvMacroSelfReferenceTerminates proves a self-referential env value
// cannot make expandEnvMacro loop forever (the scan cursor advances past each
// substitution).
func TestExpandEnvMacroSelfReferenceTerminates(t *testing.T) {
	t.Setenv("MCP_CBUILD_SELF", "$env{MCP_CBUILD_SELF}/x")

	done := make(chan string, 1)
	go func() {
		out, _ := expandEnvMacro("$env{MCP_CBUILD_SELF}", "$env{")
		done <- out
	}()
	select {
	case out := <-done:
		// The substituted value is treated as a literal (not re-expanded).
		if out != "$env{MCP_CBUILD_SELF}/x" {
			t.Errorf("expandEnvMacro = %q, want the literal single substitution", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expandEnvMacro did not terminate on a self-referential value")
	}
}

// TestNamedPresetInheritance proves build/test preset `inherits` is resolved so
// an inherited configurePreset is surfaced (previously omitted).
func TestNamedPresetInheritance(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 6,
      "configurePresets": [ { "name": "dev", "binaryDir": "${sourceDir}/build" } ],
      "buildPresets": [
        { "name": "base-build", "hidden": true, "configurePreset": "dev" },
        { "name": "child-build", "inherits": ["base-build"] }
      ],
      "testPresets": [
        { "name": "base-test", "hidden": true, "configurePreset": "dev" },
        { "name": "child-test", "inherits": ["base-test"] }
      ]
    }`)

	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	res := p.Result()

	child := findPreset(t, res.BuildPresets, "child-build")
	if child.ConfigurePreset != "dev" {
		t.Errorf("child-build configurePreset = %q, want dev (inherited from base-build)", child.ConfigurePreset)
	}
	ctest := findPreset(t, res.TestPresets, "child-test")
	if ctest.ConfigurePreset != "dev" {
		t.Errorf("child-test configurePreset = %q, want dev (inherited from base-test)", ctest.ConfigurePreset)
	}
}

// TestResolveBuildDirRejectsVendorMacro proves the purge guard fails closed on a
// vendor / unknown-namespace macro. Without the catch-all, filepath.Clean would
// collapse e.g. "$vendor{x}/../build" and RemoveAll could delete an in-tree
// source directory — even though CMake itself rejects such a preset.
func TestResolveBuildDirRejectsVendorMacro(t *testing.T) {
	src := t.TempDir()
	for _, bad := range []string{
		"$vendor{x}/../build",
		"$vendor{microsoft.com/CMake}/out",
		"$unknownns{y}/build",
		"${sourceDir}/$vendor{x}",
	} {
		if _, err := resolveBuildDirWithinSource(bad, src, "dev"); err == nil {
			t.Errorf("binaryDir %q: expected refusal for an unexpanded/unknown-namespace macro", bad)
		}
	}
}

// TestCleanResolvesBuildDirFromConfigurePreset pins the build-vs-configure fix:
// cmake_clean's non-purge path resolves the build DIRECTORY from the configure
// preset's binaryDir. It must never pass the configure preset NAME to
// `cmake --build --preset`, which fails "No such build preset" whenever the build
// preset is named differently (here configure "dev" vs build "dev-build").
func TestCleanResolvesBuildDirFromConfigurePreset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 3,
      "configurePresets": [
        { "name": "dev", "generator": "Ninja", "binaryDir": "${sourceDir}/out/dev" }
      ],
      "buildPresets": [ { "name": "dev-build", "configurePreset": "dev" } ]
    }`)
	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	binaryDir, err := p.ResolvedBinaryDir("dev")
	if err != nil {
		t.Fatalf("ResolvedBinaryDir: %v", err)
	}
	buildAbs, _, err := expandBinaryDirToAbs(binaryDir, dir, "dev")
	if err != nil {
		t.Fatalf("expandBinaryDirToAbs: %v", err)
	}
	want := filepath.Clean(filepath.Join(dir, "out", "dev"))
	if buildAbs != want {
		t.Errorf("resolved build dir = %q, want %q", buildAbs, want)
	}

	// There is NO build preset named "dev"; the old --build --preset dev path
	// would have failed. Confirm the configure preset is not itself a build preset.
	res := p.Result()
	for _, bp := range res.BuildPresets {
		if bp.Name == "dev" {
			t.Fatal("unexpected build preset named dev")
		}
	}
}

// TestEvalConditionForms exercises every documented condition form plus the
// fail-open cases (unknown type, unresolvable macro).
func TestEvalConditionForms(t *testing.T) {
	src := filepath.Join(t.TempDir(), "proj")
	host := cmakeHostSystemName()
	cases := []struct {
		raw  string
		want bool
	}{
		{`null`, true},
		{`true`, true},
		{`false`, false},
		{`{"type":"const","value":true}`, true},
		{`{"type":"const","value":false}`, false},
		{fmt.Sprintf(`{"type":"equals","lhs":"${hostSystemName}","rhs":%q}`, host), true},
		{`{"type":"equals","lhs":"${hostSystemName}","rhs":"NoSuchOS"}`, false},
		{fmt.Sprintf(`{"type":"notEquals","lhs":"${hostSystemName}","rhs":%q}`, host), false},
		{fmt.Sprintf(`{"type":"inList","string":"${hostSystemName}","list":[%q,"Zzz"]}`, host), true},
		{`{"type":"inList","string":"${hostSystemName}","list":["Zzz"]}`, false},
		{`{"type":"notInList","string":"${hostSystemName}","list":["Zzz"]}`, true},
		{fmt.Sprintf(`{"type":"matches","string":"${hostSystemName}","regex":"^%s$"}`, regexp.QuoteMeta(host)), true},
		{`{"type":"matches","string":"abc","regex":"^z"}`, false},
		{`{"type":"notMatches","string":"abc","regex":"^z"}`, true},
		{`{"type":"not","condition":{"type":"const","value":false}}`, true},
		{fmt.Sprintf(`{"type":"allOf","conditions":[{"type":"const","value":true},{"type":"equals","lhs":"${hostSystemName}","rhs":%q}]}`, host), true},
		{`{"type":"allOf","conditions":[{"type":"const","value":true},{"type":"const","value":false}]}`, false},
		{fmt.Sprintf(`{"type":"anyOf","conditions":[{"type":"const","value":false},{"type":"equals","lhs":"${hostSystemName}","rhs":%q}]}`, host), true},
		{`{"type":"anyOf","conditions":[{"type":"const","value":false},{"type":"const","value":false}]}`, false},
		{`{"type":"unknownFutureType"}`, true},                                   // fail-open on unknown type
		{`{"type":"equals","lhs":"$env{MCP_CBUILD_UNSET_XYZ}","rhs":"x"}`, true}, // unresolvable env → enabled
		{`{"type":"matches","string":"x","regex":"("}`, true},                    // uncompilable regex → enabled
	}
	for _, c := range cases {
		if got := evalCondition(json.RawMessage(c.raw), src, "p"); got != c.want {
			t.Errorf("evalCondition(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// TestListPresetsFiltersHostDisabled proves cmake_list_presets excludes a preset
// disabled on the current host by its condition, while keeping host-matching and
// unconditional presets — matching `cmake --list-presets`.
func TestListPresetsFiltersHostDisabled(t *testing.T) {
	dir := t.TempDir()
	host := cmakeHostSystemName()
	writeFile(t, dir, "CMakePresets.json", fmt.Sprintf(`{
      "version": 3,
      "configurePresets": [
        { "name": "match-host", "generator": "Ninja", "binaryDir": "${sourceDir}/b1",
          "condition": {"type":"equals","lhs":"${hostSystemName}","rhs":%q} },
        { "name": "other-host", "generator": "Ninja", "binaryDir": "${sourceDir}/b2",
          "condition": {"type":"equals","lhs":"${hostSystemName}","rhs":"NoSuchOS"} },
        { "name": "always", "generator": "Ninja", "binaryDir": "${sourceDir}/b3" }
      ],
      "buildPresets": [
        { "name": "b-match", "configurePreset": "match-host",
          "condition": {"type":"equals","lhs":"${hostSystemName}","rhs":%q} },
        { "name": "b-other", "configurePreset": "other-host",
          "condition": {"type":"equals","lhs":"${hostSystemName}","rhs":"NoSuchOS"} }
      ]
    }`, host, host))
	p, err := LoadPresets(dir)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	res := p.Result()

	cfg := map[string]bool{}
	for _, cp := range res.ConfigurePresets {
		cfg[cp.Name] = true
	}
	if !cfg["match-host"] {
		t.Errorf("host-matching configure preset was excluded; got %v", cfg)
	}
	if !cfg["always"] {
		t.Errorf("unconditional configure preset was excluded; got %v", cfg)
	}
	if cfg["other-host"] {
		t.Errorf("host-DISABLED configure preset was listed; got %v", cfg)
	}

	bld := map[string]bool{}
	for _, bp := range res.BuildPresets {
		bld[bp.Name] = true
	}
	if !bld["b-match"] {
		t.Errorf("host-matching build preset was excluded; got %v", bld)
	}
	if bld["b-other"] {
		t.Errorf("host-DISABLED build preset was listed; got %v", bld)
	}
}

// TestLoadPresetsPenvInclude proves an include entry carrying a $penv{} macro
// (version 7+) is expanded before the include path is resolved, so the included
// base file is actually loaded and its fields inherited.
func TestLoadPresetsPenvInclude(t *testing.T) {
	root := t.TempDir()
	incDir := filepath.Join(root, "presets")
	if err := os.Mkdir(incDir, 0o755); err != nil {
		t.Fatalf("mkdir incDir: %v", err)
	}
	writeFile(t, incDir, "base.json", `{
      "version": 7,
      "configurePresets": [
        { "name": "base", "hidden": true, "generator": "Ninja", "binaryDir": "${sourceDir}/build" }
      ]
    }`)
	t.Setenv("MCP_CBUILD_PRESET_DIR", incDir)
	writeFile(t, root, "CMakePresets.json", `{
      "version": 7,
      "include": ["$penv{MCP_CBUILD_PRESET_DIR}/base.json"],
      "configurePresets": [ { "name": "dev", "inherits": ["base"] } ]
    }`)

	p, err := LoadPresets(root)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	res := p.Result()
	dev := findPreset(t, res.ConfigurePresets, "dev")
	if dev.ResolvedGenerator != "Ninja" {
		t.Errorf("dev.ResolvedGenerator = %q, want Ninja (inherited from $penv-included base)", dev.ResolvedGenerator)
	}
}

// TestLoadPresetsIncludeUnsetPenvFailsClosed proves an include referencing an
// unset $penv{} is a fail-closed error, not a silently-collapsed empty path.
func TestLoadPresetsIncludeUnsetPenvFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CMakePresets.json", `{
      "version": 7,
      "include": ["$penv{MCP_CBUILD_DEFINITELY_UNSET_INC}/base.json"],
      "configurePresets": [ { "name": "dev" } ]
    }`)
	if _, err := LoadPresets(root); err == nil {
		t.Error("expected LoadPresets to fail closed on an unset $penv{} in an include path")
	}
}

func TestLoadPresetsRejectsDuplicateNamesAcrossIncludes(t *testing.T) {
	for _, tc := range []struct {
		kind string
		key  string
	}{
		{kind: "configure", key: "configurePresets"},
		{kind: "build", key: "buildPresets"},
		{kind: "test", key: "testPresets"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "common.json", fmt.Sprintf(`{
              "version": 4,
              %q: [{"name":"duplicate"}]
            }`, tc.key))
			writeFile(t, dir, "CMakePresets.json", fmt.Sprintf(`{
              "version": 4,
              "include": ["common.json"],
              %q: [{"name":"duplicate"}]
            }`, tc.key))

			_, err := LoadPresets(dir)
			if err == nil {
				t.Fatalf("LoadPresets accepted duplicate %s preset", tc.kind)
			}
			want := fmt.Sprintf("duplicate %s preset", tc.kind)
			if !strings.Contains(strings.ToLower(err.Error()), want) || !strings.Contains(err.Error(), `"duplicate"`) {
				t.Errorf("LoadPresets error = %q, want %q and preset name", err, want)
			}
		})
	}
}

func TestDollarEscapedEnvMacrosStayLiteral(t *testing.T) {
	src := t.TempDir()
	t.Setenv("MCP_CBUILD_ESCAPED", "expanded")

	for _, namespace := range []string{"env", "penv"} {
		t.Run(namespace, func(t *testing.T) {
			binaryDir := "${sourceDir}/${dollar}" + namespace + "{MCP_CBUILD_ESCAPED}"
			got, _, err := expandBinaryDirToAbs(binaryDir, src, "dev")
			if err != nil {
				t.Fatalf("expandBinaryDirToAbs(%q): %v", binaryDir, err)
			}
			want := filepath.Join(src, "$"+namespace+"{MCP_CBUILD_ESCAPED}")
			if got != want {
				t.Errorf("expanded path = %q, want literal escaped macro path %q", got, want)
			}
		})
	}
}
