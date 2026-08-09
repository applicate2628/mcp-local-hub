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
	// values is the one CMake-value owner. Its text is the exact serialized
	// value CMake stores; spans carry provenance only and never create list
	// element boundaries.
	values map[string]serializedValue

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

type provenanceMeta struct {
	source           string
	sourceProvenance []string
	guard            Tri
	guardText        string
	unresolvedVars   []string
	pathUnresolved   []string
}

// provenanceSpan is metadata over a byte range of one serialized CMake
// value. It deliberately contains no semantic-list item cache.
type provenanceSpan struct {
	start int
	end   int
	meta  provenanceMeta
}

type serializedValue struct {
	text       string
	spans      []provenanceSpan
	resolution valueResolution
}

// evaluatedValue is ephemeral token-evaluation state. protectedSemicolon is
// true only for a semicolon escaped in the current source token; serialized
// variable bytes never receive that source-lexical protection.
type evaluatedValue struct {
	text               []byte
	metas              []provenanceMeta
	protectedSemicolon []bool
	resolution         valueResolution
	exactReference     bool
	state              *resolutionState
}

type valueResolutionIssue uint8

const (
	valueResolutionOK valueResolutionIssue = iota
	valueResolutionMalformedReference
	valueResolutionNestedNameUnresolved
	valueResolutionCycle
	valueResolutionDepthExceeded
	valueResolutionByteBudgetExceeded
)

// valueResolution keeps a failed recursive evaluation inert until its value
// is actually consumed. serializedValue carries it through set()/list(APPEND)
// without making unrelated variables fail.
type valueResolution struct{ issue valueResolutionIssue }

func (r valueResolution) failed() bool { return r.issue != valueResolutionOK }

func mergeResolution(left, right valueResolution) valueResolution {
	if left.failed() {
		return left
	}
	return right
}

const maxVariableDereferenceDepth = 32

type semanticItem struct {
	text    string
	display string
	meta    provenanceMeta
	spans   []provenanceSpan
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

// setValue is the single serialized-value assignment owner. An overwrite
// replaces both bytes and provenance, so stale conditional metadata cannot
// survive a later unconditional set().
func (env *varEnv) setValue(name string, value serializedValue) {
	if env.values == nil {
		env.values = map[string]serializedValue{}
	}
	env.values[name] = value
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
		values:    map[string]serializedValue{},
		vcpkgRoot: vcpkgRoot,
		portName:  portName,
		portDir:   portDir,
	}
	for name, val := range tripletFacts {
		env.setValue(name, serializedValue{text: val})
	}
	for k, v := range overrides {
		env.setValue(k, serializedValue{text: v})
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
	if value, ok := env.values[name]; ok {
		v := value.text
		return &v
	}
	return nil
}

// taintOf summarizes a serialized value for condition evaluation. Patch
// extraction preserves the finer per-span information instead.
func (env *varEnv) taintOf(name string) provenanceMeta {
	value, ok := env.values[name]
	if !ok || len(value.spans) == 0 {
		return provenanceMeta{guard: TriTrue}
	}
	meta := provenanceMeta{guard: TriTrue}
	for _, span := range value.spans {
		meta = combineProvenance(meta, span.meta)
	}
	return meta
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

// expandListToken evaluates the list-serialized lexical view through the
// same substitution and taint implementation as ordinary token expansion.
// Keeping this delegation here prevents list mode from drifting from the
// evaluator's unresolved-variable and conditional-assignment semantics.
func (env *varEnv) expandListToken(t token) expansion {
	if t.Raw {
		return expansion{text: t.Text, certainty: TriTrue}
	}
	return env.expand(t.listText)
}

func certainProvenance() provenanceMeta { return provenanceMeta{guard: TriTrue} }

type provenanceRelation uint8

const (
	provenancePeer provenanceRelation = iota
	provenanceSelectedValue
)

func provenanceSources(meta provenanceMeta) []string {
	sources := append([]string{}, meta.sourceProvenance...)
	if meta.source != "" {
		sources = append(sources, meta.source)
	}
	return dedupStrings(sources)
}

// mergeProvenance is the only metadata-combination owner. Selector metadata
// is dependency provenance, while the selected serialized byte remains the
// scalar display-source owner.
func mergeProvenance(left, right provenanceMeta, relation provenanceRelation) provenanceMeta {
	merged := provenanceMeta{
		guard:            kleeneAnd(left.guard, right.guard),
		guardText:        joinGuardText(left.guardText, right.guardText),
		unresolvedVars:   dedupStrings(append(append([]string{}, left.unresolvedVars...), right.unresolvedVars...)),
		pathUnresolved:   dedupStrings(append(append([]string{}, left.pathUnresolved...), right.pathUnresolved...)),
		sourceProvenance: dedupStrings(append(provenanceSources(left), provenanceSources(right)...)),
	}
	if relation == provenanceSelectedValue {
		merged.source = right.source
	} else {
		merged.source = mergeSource(left.source, right.source)
	}
	return merged
}

// combineProvenance remains the lexer-facing peer relation adapter. The
// evaluator itself selects the relation explicitly at each owner boundary.
func combineProvenance(left, right provenanceMeta) provenanceMeta {
	return mergeProvenance(left, right, provenancePeer)
}

func mergeSource(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" || left == right {
		return left
	}
	return ""
}

func joinGuardText(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" || right == left {
		return left
	}
	return left + " AND " + right
}

func appendEvaluatedByte(value *evaluatedValue, b byte, meta provenanceMeta, protected bool) bool {
	if value.resolution.failed() {
		return false
	}
	if value.state != nil {
		if value.state.remainingBytes == 0 {
			return failEvaluation(value, valueResolutionByteBudgetExceeded)
		}
		value.state.remainingBytes--
	} else if int64(len(value.text)) >= MaxPortfileBytes {
		return failEvaluation(value, valueResolutionByteBudgetExceeded)
	}
	value.text = append(value.text, b)
	value.metas = append(value.metas, meta)
	value.protectedSemicolon = append(value.protectedSemicolon, protected)
	return true
}

func failEvaluation(value *evaluatedValue, issue valueResolutionIssue) bool {
	if !value.resolution.failed() {
		value.resolution = valueResolution{issue: issue}
	}
	value.text = nil
	value.metas = nil
	value.protectedSemicolon = nil
	value.exactReference = false
	return false
}

func appendLiteral(value *evaluatedValue, text string, meta provenanceMeta) bool {
	for i := 0; i < len(text); i++ {
		if !appendEvaluatedByte(value, text[i], meta, false) {
			return false
		}
	}
	return true
}

func appendBareSource(value *evaluatedValue, source string, meta provenanceMeta) bool {
	for i := 0; i < len(source); {
		if source[i] != '\\' {
			if !appendEvaluatedByte(value, source[i], meta, false) {
				return false
			}
			i++
			continue
		}
		if i+1 >= len(source) {
			return appendEvaluatedByte(value, '\\', meta, false)
		}
		next := source[i+1]
		switch {
		case next == '\n':
			i += 2
			continue
		case next == '\r' && i+2 < len(source) && source[i+2] == '\n':
			i += 3
			continue
		case next == 't':
			next = '\t'
		case next == 'r':
			next = '\r'
		case next == 'n':
			next = '\n'
		case (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') || (next >= '0' && next <= '9'):
			if !appendEvaluatedByte(value, '\\', meta, false) || !appendEvaluatedByte(value, next, meta, false) {
				return false
			}
			i += 2
			continue
		}
		if !appendEvaluatedByte(value, next, meta, next == ';') {
			return false
		}
		i += 2
	}
	return true
}

func serializedMetaAt(serialized serializedValue, i int) provenanceMeta {
	for _, span := range serialized.spans {
		if i >= span.start && i < span.end {
			return span.meta
		}
	}
	return certainProvenance()
}

func appendSerializedRange(value *evaluatedValue, serialized serializedValue, start, end int, callMeta provenanceMeta) bool {
	for i := start; i < end; i++ {
		meta := certainProvenance()
		meta = serializedMetaAt(serialized, i)
		if !appendEvaluatedByte(value, serialized.text[i], mergeProvenance(callMeta, meta, provenanceSelectedValue), false) {
			return false
		}
	}
	return true
}

func (env *varEnv) serializedFor(name string) (serializedValue, bool) {
	switch name {
	case "PORT":
		return serializedValue{text: env.portName}, true
	case "CURRENT_PORT_DIR":
		return serializedValue{text: env.portDir}, true
	}
	value, ok := env.values[name]
	return value, ok
}

type resolutionState struct {
	depth          int
	active         map[string]struct{}
	remainingBytes int64
}

func (state *resolutionState) enterReference(value *evaluatedValue) bool {
	if state.depth == maxVariableDereferenceDepth {
		return failEvaluation(value, valueResolutionDepthExceeded)
	}
	state.depth++
	return true
}

func (state *resolutionState) leaveReference() { state.depth-- }

type variableReference struct {
	start int
	end   int
	env   bool
	inner string
}

type referenceScanState uint8

const (
	referenceScanNone referenceScanState = iota
	referenceScanFound
	referenceScanMalformed
)

type referenceScanResult struct {
	state       referenceScanState
	reference   variableReference
	malformedAt int
}

func scanNextVariableReference(text string) referenceScanResult {
	for start := 0; start < len(text); start++ {
		if text[start] != '$' || start+2 >= len(text) {
			continue
		}
		if strings.HasPrefix(text[start:], "$ENV{") {
			depth := 1
			for i := start + 5; i < len(text); i++ {
				if openerWidth := variableReferenceOpenerWidth(text[i:]); openerWidth != 0 {
					depth++
					i += openerWidth - 1
					continue
				}
				if text[i] == '}' {
					depth--
					if depth == 0 {
						return referenceScanResult{state: referenceScanFound, reference: variableReference{start: start, end: i + 1, env: true, inner: text[start+5 : i]}}
					}
				}
			}
			return referenceScanResult{state: referenceScanMalformed, malformedAt: start}
		}
		if text[start+1] != '{' {
			continue
		}
		depth := 1
		for i := start + 2; i < len(text); i++ {
			if openerWidth := variableReferenceOpenerWidth(text[i:]); openerWidth != 0 {
				depth++
				i += openerWidth - 1
				continue
			}
			if text[i] == '}' {
				depth--
				if depth == 0 {
					return referenceScanResult{state: referenceScanFound, reference: variableReference{start: start, end: i + 1, inner: text[start+2 : i]}}
				}
			}
		}
		return referenceScanResult{state: referenceScanMalformed, malformedAt: start}
	}
	return referenceScanResult{state: referenceScanNone}
}

// variableReferenceOpenerWidth returns the byte width of every supported
// reference opener. scanNextVariableReference owns balanced reference grammar,
// so normal and environment forms must share this axis.
func variableReferenceOpenerWidth(text string) int {
	if strings.HasPrefix(text, "$ENV{") {
		return len("$ENV{")
	}
	if strings.HasPrefix(text, "${") {
		return len("${")
	}
	return 0
}

func (env *varEnv) appendValueReference(value *evaluatedValue, reference variableReference, callMeta provenanceMeta, state *resolutionState, nestedName bool) bool {
	if !state.enterReference(value) {
		return false
	}
	defer state.leaveReference()
	if reference.env {
		name := reference.inner
		nameMeta := certainProvenance()
		if strings.Contains(name, "${") || strings.Contains(name, "$ENV{") {
			resolvedName := evaluatedValue{state: state}
			if !env.appendSource(&resolvedName, name, false, certainProvenance(), state, true) || resolvedName.resolution.failed() || len(resolvedName.metas) == 0 {
				if resolvedName.resolution.failed() && resolvedName.resolution.issue != valueResolutionNestedNameUnresolved {
					return failEvaluation(value, resolvedName.resolution.issue)
				}
				return failEvaluation(value, valueResolutionNestedNameUnresolved)
			}
			name = string(resolvedName.text)
			for _, meta := range resolvedName.metas {
				nameMeta = mergeProvenance(nameMeta, meta, provenancePeer)
			}
		}
		if name == "VCPKG_ROOT" && env.vcpkgRoot != "" {
			return appendLiteral(value, env.vcpkgRoot, mergeProvenance(callMeta, nameMeta, provenancePeer))
		}
		if nestedName {
			return failEvaluation(value, valueResolutionNestedNameUnresolved)
		}
		unresolved := callMeta
		unresolved.pathUnresolved = dedupStrings(append(unresolved.pathUnresolved, "$ENV{"+name+"}"))
		return appendLiteral(value, "$ENV{"+name+"}", mergeProvenance(unresolved, nameMeta, provenancePeer))
	}

	name := reference.inner
	selectorMeta := certainProvenance()
	if strings.Contains(name, "${") || strings.Contains(name, "$ENV{") {
		resolvedName := evaluatedValue{state: state}
		if !env.appendSource(&resolvedName, name, false, certainProvenance(), state, true) || resolvedName.resolution.failed() || len(resolvedName.metas) == 0 {
			if resolvedName.resolution.failed() && resolvedName.resolution.issue != valueResolutionNestedNameUnresolved {
				return failEvaluation(value, resolvedName.resolution.issue)
			}
			return failEvaluation(value, valueResolutionNestedNameUnresolved)
		}
		name = string(resolvedName.text)
		for _, meta := range resolvedName.metas {
			selectorMeta = mergeProvenance(selectorMeta, meta, provenancePeer)
		}
	}
	selectorMeta = mergeProvenance(callMeta, selectorMeta, provenancePeer)
	if name == "" {
		return failEvaluation(value, valueResolutionNestedNameUnresolved)
	}
	serialized, ok := env.serializedFor(name)
	if !ok {
		if nestedName || reference.inner != name {
			return failEvaluation(value, valueResolutionNestedNameUnresolved)
		}
		unresolved := callMeta
		unresolved.pathUnresolved = dedupStrings(append(unresolved.pathUnresolved, name))
		return appendLiteral(value, "${"+name+"}", unresolved)
	}
	if serialized.resolution.failed() {
		return failEvaluation(value, serialized.resolution.issue)
	}
	if _, active := state.active[name]; active {
		return failEvaluation(value, valueResolutionCycle)
	}
	state.active[name] = struct{}{}
	ok = env.appendSerializedResolved(value, serialized, selectorMeta, state)
	delete(state.active, name)
	return ok
}

func (env *varEnv) appendSerializedResolved(value *evaluatedValue, serialized serializedValue, callMeta provenanceMeta, state *resolutionState) bool {
	for offset := 0; offset < len(serialized.text); {
		scan := scanNextVariableReference(serialized.text[offset:])
		if scan.state == referenceScanMalformed {
			return failEvaluation(value, valueResolutionMalformedReference)
		}
		if scan.state == referenceScanNone {
			return appendSerializedRange(value, serialized, offset, len(serialized.text), callMeta)
		}
		reference := scan.reference
		reference.start += offset
		reference.end += offset
		if !appendSerializedRange(value, serialized, offset, reference.start, callMeta) {
			return false
		}
		if !env.appendValueReference(value, reference, mergeProvenance(callMeta, serializedMetaAt(serialized, reference.start), provenancePeer), state, false) {
			return false
		}
		offset = reference.end
	}
	return true
}

func (env *varEnv) appendSource(value *evaluatedValue, source string, bare bool, callMeta provenanceMeta, state *resolutionState, nestedName bool) bool {
	for offset := 0; offset < len(source); {
		scan := scanNextVariableReference(source[offset:])
		if scan.state == referenceScanMalformed {
			return failEvaluation(value, valueResolutionMalformedReference)
		}
		if scan.state == referenceScanNone {
			if bare {
				return appendBareSource(value, source[offset:], callMeta)
			}
			return appendLiteral(value, source[offset:], callMeta)
		}
		reference := scan.reference
		reference.start += offset
		reference.end += offset
		if bare {
			if !appendBareSource(value, source[offset:reference.start], callMeta) {
				return false
			}
		} else if !appendLiteral(value, source[offset:reference.start], callMeta) {
			return false
		}
		if !env.appendValueReference(value, reference, callMeta, state, nestedName) {
			return false
		}
		offset = reference.end
	}
	return true
}

// evaluateValue performs the narrow lexical and recursively bounded variable
// phases before the sole list splitter. It normalizes escapes in source
// literals only; bytes inserted from a serialized variable remain serialized
// bytes.
func (env *varEnv) evaluateValue(t token, callMeta provenanceMeta) evaluatedValue {
	state := &resolutionState{active: map[string]struct{}{}, remainingBytes: MaxPortfileBytes}
	value := evaluatedValue{state: state}
	if t.Raw {
		appendLiteral(&value, t.Text, callMeta)
		return value
	}
	source := t.Text
	bare := false
	if !t.Quoted {
		source, bare = t.listText, true
	}
	scan := scanNextVariableReference(t.Text)
	value.exactReference = !t.Quoted && scan.state == referenceScanFound && scan.reference.start == 0 && scan.reference.end == len(t.Text)
	env.appendSource(&value, source, bare, callMeta, state, false)
	if value.resolution.failed() {
		value.exactReference = false
	}
	return value
}

func spansFromMetas(metas []provenanceMeta) []provenanceSpan {
	if len(metas) == 0 {
		return nil
	}
	spans := make([]provenanceSpan, 0, 1)
	start := 0
	for i := 1; i <= len(metas); i++ {
		if i == len(metas) || !sameProvenance(metas[start], metas[i]) {
			spans = append(spans, provenanceSpan{start: start, end: i, meta: metas[start]})
			start = i
		}
	}
	return spans
}

func sameProvenance(left, right provenanceMeta) bool {
	return left.source == right.source && strings.Join(left.sourceProvenance, "\x00") == strings.Join(right.sourceProvenance, "\x00") && left.guard == right.guard && left.guardText == right.guardText &&
		strings.Join(left.unresolvedVars, "\x00") == strings.Join(right.unresolvedVars, "\x00") &&
		strings.Join(left.pathUnresolved, "\x00") == strings.Join(right.pathUnresolved, "\x00")
}

func serializeItems(items []semanticItem) serializedValue {
	var value serializedValue
	for i, item := range items {
		if i != 0 {
			separatorMeta := certainProvenance()
			if len(value.spans) > 0 {
				separatorMeta = value.spans[len(value.spans)-1].meta
			}
			value.spans = append(value.spans, provenanceSpan{start: len(value.text), end: len(value.text) + 1, meta: separatorMeta})
			value.text += ";"
		}
		offset := len(value.text)
		value.text += item.text
		spans := item.spans
		if len(spans) == 0 && item.text != "" {
			spans = []provenanceSpan{{start: 0, end: len(item.text), meta: item.meta}}
		}
		for _, span := range spans {
			meta := span.meta
			if meta.source == "" {
				meta.source = item.display
			}
			value.spans = append(value.spans, provenanceSpan{start: offset + span.start, end: offset + span.end, meta: meta})
		}
	}
	return value
}

func appendSerializedValue(existing, appended serializedValue) serializedValue {
	resolution := mergeResolution(existing.resolution, appended.resolution)
	if existing.text == "" {
		appended.resolution = resolution
		return appended
	}
	if appended.text == "" {
		existing.resolution = resolution
		return existing
	}
	result := serializedValue{text: existing.text, spans: append([]provenanceSpan{}, existing.spans...), resolution: resolution}
	separatorMeta := certainProvenance()
	if len(result.spans) > 0 {
		separatorMeta = result.spans[len(result.spans)-1].meta
	}
	separatorOffset := len(result.text)
	result.text += ";" + appended.text
	result.spans = append(result.spans, provenanceSpan{start: separatorOffset, end: separatorOffset + 1, meta: separatorMeta})
	for _, span := range appended.spans {
		result.spans = append(result.spans, provenanceSpan{
			start: separatorOffset + 1 + span.start,
			end:   separatorOffset + 1 + span.end,
			meta:  span.meta,
		})
	}
	return result
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
