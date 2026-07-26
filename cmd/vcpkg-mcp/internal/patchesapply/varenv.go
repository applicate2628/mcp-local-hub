package patchesapply

import (
	"path/filepath"
	"regexp"
	"strings"
)

// varEnv is the partial variable model a portfile.cmake is evaluated against.
// It is deliberately partial: only what the triplet name unambiguously
// implies, what the caller explicitly overrode, and what the portfile itself
// established via set()/get_filename_component() are known. Every other
// ${VAR} reference resolves to nil ("unresolved"), which is the mechanism
// that produces the undecidable bucket instead of a guess.
type varEnv struct {
	// scalars holds resolved string values. A missing key or a nil pointer
	// value both mean "unresolved"; scalars never stores an explicit nil, so
	// presence of the key is enough to mean "known".
	scalars map[string]string
	// lists holds accumulated list(APPEND ...) items, in append order, each
	// tagged with the guard Tri value + verbatim guard text active at the
	// point of the append.
	lists map[string][]listItem

	// vcpkgRoot backs $ENV{VCPKG_ROOT} expansion (Args.VcpkgRoot).
	vcpkgRoot string
	// portName backs the ${PORT} builtin.
	portName string
	// portDir backs the ${CURRENT_PORT_DIR} builtin; also the base directory
	// relative-path expressions in get_filename_component(... ABSOLUTE) are
	// resolved against (approximating CMAKE_CURRENT_SOURCE_DIR for a
	// portfile.cmake, which vcpkg sets to the port directory).
	portDir string
}

type listItem struct {
	text           string
	guard          Tri
	guardText      string
	unresolvedVars []string
	pathUnresolved []string
}

func newVarEnv(portDir, portName, vcpkgRoot string, overrides map[string]string, triplet string) *varEnv {
	env := &varEnv{
		scalars:   map[string]string{},
		lists:     map[string][]listItem{},
		vcpkgRoot: vcpkgRoot,
		portName:  portName,
		portDir:   portDir,
	}
	for name, tri := range deriveTripletFacts(triplet) {
		env.scalars[name] = tri
	}
	// Explicit overrides win over triplet-derived facts.
	for k, v := range overrides {
		env.scalars[k] = v
	}
	return env
}

// deriveTripletFacts derives the closed set of triplet facts this package
// claims to know from the triplet name alone, per the accepted contract:
// VCPKG_TARGET_IS_WINDOWS for *-windows*, VCPKG_TARGET_IS_MINGW for *-mingw*,
// VCPKG_LIBRARY_LINKAGE from a -static component (else "dynamic"),
// VCPKG_TARGET_IS_LINUX / VCPKG_TARGET_IS_OSX likewise. Each is derived
// independently from its own component match — never cross-inferred from
// real-world OS-family knowledge (e.g. this does NOT set
// VCPKG_TARGET_IS_WINDOWS=true for a *-mingw* triplet just because MinGW
// targets Windows; the contract names each derivation rule independently and
// Kleene AND/OR still produces the right guard result via the explicit
// VCPKG_TARGET_IS_MINGW term wherever that matters). Anything not in this
// list (VCPKG_CROSSCOMPILING, WINSDK_VERSION, ...) is intentionally absent
// here and stays unresolved unless the caller supplies an override.
func deriveTripletFacts(triplet string) map[string]string {
	comps := strings.Split(strings.ToLower(triplet), "-")
	has := func(name string) bool {
		for _, c := range comps {
			if c == name {
				return true
			}
		}
		return false
	}
	onOff := func(b bool) string {
		if b {
			return "ON"
		}
		return "OFF"
	}
	facts := map[string]string{
		"VCPKG_TARGET_IS_WINDOWS": onOff(has("windows")),
		"VCPKG_TARGET_IS_MINGW":   onOff(has("mingw")),
		"VCPKG_TARGET_IS_LINUX":   onOff(has("linux")),
		"VCPKG_TARGET_IS_OSX":     onOff(has("osx")),
	}
	if has("static") {
		facts["VCPKG_LIBRARY_LINKAGE"] = "static"
	} else {
		facts["VCPKG_LIBRARY_LINKAGE"] = "dynamic"
	}
	return facts
}

// lookup resolves a bare variable name to its known value, or nil if this
// package has no basis for a value (the caller must then treat any
// expression depending on it as Unknown, never guess).
func (env *varEnv) lookup(name string) *string {
	switch name {
	case "PORT":
		return &env.portName
	case "CURRENT_PORT_DIR":
		return &env.portDir
	}
	if v, ok := env.scalars[name]; ok {
		return &v
	}
	return nil
}

var (
	reVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	reEnvRef = regexp.MustCompile(`\$ENV\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// expand substitutes every ${VAR} and $ENV{VAR} reference in s using env.
// It returns the best-effort substituted string (unresolved references are
// left verbatim, e.g. "${UNKNOWN}/foo.patch" — a literal string no real file
// on disk can match, so downstream Stat() calls fail closed without any
// special-cased "could not resolve" branch) plus the list of variable names
// that could not be resolved, so a caller can report exactly which reference
// defeated resolution instead of a bare "unknown".
func (env *varEnv) expand(s string) (result string, unresolved []string) {
	// $ENV{...} first: only VCPKG_ROOT is a modelled env read (the design's
	// own discovery-order doc names VCPKG_ROOT as THE documented env
	// convention for the vcpkg root; every other $ENV{} name is unresolved —
	// this package injects no other ambient environment).
	result = s
	// Iterate both $ENV{} and ${} expansion. A resolved ${VAR} can reveal an
	// unresolved $ENV{} reference from an earlier set(), which must be carried
	// to the caller instead of becoming a false missing path.
	for pass := 0; pass < 8; pass++ {
		changed := false
		result = reEnvRef.ReplaceAllStringFunc(result, func(m string) string {
			name := reEnvRef.FindStringSubmatch(m)[1]
			if name == "VCPKG_ROOT" {
				if env.vcpkgRoot == "" {
					unresolved = append(unresolved, "$ENV{VCPKG_ROOT}")
					return m
				}
				changed = true
				return env.vcpkgRoot
			}
			unresolved = append(unresolved, "$ENV{"+name+"}")
			return m
		})
		result = reVarRef.ReplaceAllStringFunc(result, func(m string) string {
			name := reVarRef.FindStringSubmatch(m)[1]
			val := env.lookup(name)
			if val == nil {
				unresolved = append(unresolved, name)
				return m
			}
			changed = true
			return *val
		})
		if !changed {
			break
		}
	}
	return result, dedupStrings(unresolved)
}

// expandToken applies env.expand to a token's text UNLESS the token is Raw
// (a bracket_argument) — per cmake-language(7), "No evaluation of the
// enclosed content, such as Escape Sequences or Variable References, is
// performed" inside one, so a Raw token's text is returned verbatim with no
// unresolved-variable reporting at all (there is nothing to resolve).
func (env *varEnv) expandToken(t token) (string, []string) {
	if t.Raw {
		return t.Text, nil
	}
	return env.expand(t.Text)
}

func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// getFilenameComponentAbsolute implements the one get_filename_component()
// mode this package needs: ABSOLUTE. input has already been through
// env.expand(); this normalizes it (collapsing ".."/"." segments) and, if it
// is not already absolute, resolves it against portDir — approximating
// CMAKE_CURRENT_SOURCE_DIR for a portfile.cmake. This is the mechanism that
// lets a portfile compute a path OUTSIDE its own port directory (the
// licensepp trap: a get_filename_component(...) chain landing in the
// builtin ports tree) without this package assuming every patch reference is
// port-dir-relative.
func (env *varEnv) getFilenameComponentAbsolute(input string) string {
	if input == "" {
		return ""
	}
	if filepath.IsAbs(input) {
		return filepath.Clean(input)
	}
	return filepath.Clean(filepath.Join(env.portDir, input))
}
