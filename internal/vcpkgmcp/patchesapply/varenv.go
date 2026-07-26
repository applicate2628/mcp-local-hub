package patchesapply

import (
	"path/filepath"
	"regexp"
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
	// scalarTaints records, for scalars assigned by the portfile under a guard
	// this package could not decide, WHICH guard that was. A missing key means
	// the value is CERTAIN — triplet-file facts, caller overrides, and
	// unconditional set()s all have nothing to qualify.
	//
	// It exists because scalars alone cannot carry the distinction. list()
	// items were already tri-state (listItem.guard), so a patch spliced from a
	// conditional list(APPEND ...) correctly landed in the undecidable bucket,
	// while the very same patch reached through a conditional set() was
	// installed as plain text and reported as a CERTAIN apply/missing verdict.
	// One shape honest and the other guessing is worse than either alone: the
	// undecidable bucket exists precisely so a guard this parser cannot decide
	// is never answered.
	scalarTaints map[string]scalarTaint

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

// scalarTaint qualifies one scalar assignment whose applicability is not
// certain: guard is the Kleene value active where the assignment was written,
// guardText the verbatim condition chain, and unresolvedVars the variables
// that made the guard undecidable.
type scalarTaint struct {
	guard          Tri
	guardText      string
	unresolvedVars []string
}

// expansion is the result of substituting ${VAR}/$ENV{VAR} references in one
// token. It carries BOTH kinds of doubt, which are different facts:
//
//   - unresolved: a reference this package had no value for at all. The text
//     keeps the verbatim "${NAME}", which no real file can match, so a path
//     built from it fails closed at Stat().
//   - certainty/uncertainVars: every reference WAS substituted, but at least
//     one of the values came from an assignment under an undecided guard. The
//     text is usable, yet whatever is built from it is only as applicable as
//     that assignment was.
type expansion struct {
	text          string
	unresolved    []string
	certainty     Tri
	uncertainVars []string
}

// setScalar is the SINGLE OWNER of scalar assignment: it writes the value and
// its taint together, so the two can never drift apart. A CERTAIN assignment
// clears any prior taint — an unconditional set() genuinely does overwrite
// whatever a conditional one left.
func (env *varEnv) setScalar(name, value string, taint scalarTaint) {
	env.scalars[name] = value
	if taint.guard == TriTrue {
		delete(env.scalarTaints, name)
		return
	}
	if env.scalarTaints == nil {
		env.scalarTaints = map[string]scalarTaint{}
	}
	env.scalarTaints[name] = taint
}

// newVarEnv builds the evaluation environment.
//
// tripletFacts are the variables an ACTUAL triplet file established (see
// triplet.go); pass nil when no triplet file was available, which leaves
// every triplet variable unresolved and therefore undecidable. Nothing here
// derives a triplet fact from the triplet NAME — see triplet.go's package
// comment for why that had to go.
//
// Explicit VarOverrides win over triplet-file facts: the caller stating
// "evaluate as if VCPKG_LIBRARY_LINKAGE were static" is a deliberate
// what-if, and is the documented escape hatch when no triplet file can be
// supplied.
func newVarEnv(portDir, portName, vcpkgRoot string, overrides map[string]string, tripletFacts map[string]string) *varEnv {
	env := &varEnv{
		scalars:      map[string]string{},
		lists:        map[string][]listItem{},
		scalarTaints: map[string]scalarTaint{},
		vcpkgRoot:    vcpkgRoot,
		portName:     portName,
		portDir:      portDir,
	}
	for name, val := range tripletFacts {
		env.scalars[name] = val
	}
	for k, v := range overrides {
		env.scalars[k] = v
	}
	return env
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

// taintOf returns the qualification on name's value, or a certain taint when
// there is none. Safe on a nil map (parseTripletFacts builds a varEnv literal
// that never assigns scalars through setScalar).
func (env *varEnv) taintOf(name string) scalarTaint {
	if t, ok := env.scalarTaints[name]; ok {
		return t
	}
	return scalarTaint{guard: TriTrue}
}

// lookupCertain resolves name only when its value is CERTAIN, reporting nil
// for a scalar assigned under an undecided guard.
//
// This is what keeps an if() condition honest: a condition reading a variable
// whose own assignment may not have executed is itself undecidable, so the
// operand is reported unresolved and evalCondition returns Unknown — the
// documented "when the expression shape defeats you, return undecidable, not a
// guess" rule, applied to the VALUE as well as the shape.
func (env *varEnv) lookupCertain(name string) *string {
	if env.taintOf(name).guard != TriTrue {
		return nil
	}
	return env.lookup(name)
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
func (env *varEnv) expand(s string) expansion {
	var unresolved []string
	var uncertainVars []string
	certainty := TriTrue
	// $ENV{...} first: only VCPKG_ROOT is a modelled env read (the design's
	// own discovery-order doc names VCPKG_ROOT as THE documented env
	// convention for the vcpkg root; every other $ENV{} name is unresolved —
	// this package injects no other ambient environment).
	result := s
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
			// The value exists but may have been assigned under a guard this
			// package could not decide. Substituting it is right — the text IS
			// what CMake would produce IF that branch ran — but the doubt has
			// to travel with it, or the caller reports a certain verdict built
			// on an uncertain assignment.
			if taint := env.taintOf(name); taint.guard != TriTrue {
				certainty = kleeneAnd(certainty, taint.guard)
				uncertainVars = append(uncertainVars, name)
				uncertainVars = append(uncertainVars, taint.unresolvedVars...)
			}
			changed = true
			return *val
		})
		if !changed {
			break
		}
	}
	return expansion{
		text:          result,
		unresolved:    dedupStrings(unresolved),
		certainty:     certainty,
		uncertainVars: dedupStrings(uncertainVars),
	}
}

// expandToken applies env.expand to a token's text UNLESS the token is Raw
// (a bracket_argument) — per cmake-language(7), "No evaluation of the
// enclosed content, such as Escape Sequences or Variable References, is
// performed" inside one, so a Raw token's text is returned verbatim with no
// unresolved-variable reporting at all (there is nothing to resolve).
func (env *varEnv) expandToken(t token) expansion {
	if t.Raw {
		return expansion{text: t.Text, certainty: TriTrue}
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
