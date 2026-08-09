package patchesapply

import (
	"regexp"
	"strconv"
	"strings"
)

// This file evaluates one portfile.cmake if()/elseif() condition STRING
// against a varEnv, producing a Tri (never a guessed bool) plus the list of
// variable names that could not be resolved. It is a small recursive-descent
// parser over the CMake if() boolean grammar this package's traps actually
// exercise: NOT/AND/OR, parenthesised grouping, STREQUAL, and the VERSION_*
// comparison family. Anything structurally outside that grammar (an
// unrecognised operator, unbalanced parens, trailing tokens) makes the
// WHOLE condition Unknown rather than guessing a partial answer — this is
// the package's one deliberate parser boundary named in the design contract
// ("when the expression shape defeats you, return undecidable, not a
// guess").
var reVarRefFull = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

var cmakeConstants = map[string]bool{
	"TRUE": true, "FALSE": true, "ON": true, "OFF": true,
	"YES": true, "NO": true, "Y": true, "N": true, "IGNORE": true,
}

var comparisonOps = map[string]bool{
	"STREQUAL":              true,
	"EQUAL":                 true,
	"VERSION_EQUAL":         true,
	"VERSION_GREATER":       true,
	"VERSION_GREATER_EQUAL": true,
	"VERSION_LESS":          true,
	"VERSION_LESS_EQUAL":    true,
}

type condParser struct {
	toks       []token
	pos        int
	env        *varEnv
	unresolved []string
	errored    bool
}

func (p *condParser) peek() token {
	if p.pos >= len(p.toks) {
		return token{}
	}
	return p.toks[p.pos]
}

func (p *condParser) next() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *condParser) atEnd() bool { return p.pos >= len(p.toks) }

func (p *condParser) isKeyword(t token, kw string) bool {
	return !t.Quoted && t.Text == kw
}

func (p *condParser) parseLogical() Tri {
	v := p.parseUnary()
	for !p.atEnd() && (p.isKeyword(p.peek(), "AND") || p.isKeyword(p.peek(), "OR")) {
		op := p.next().Text
		rhs := p.parseUnary()
		if op == "AND" {
			v = kleeneAnd(v, rhs)
		} else {
			v = kleeneOr(v, rhs)
		}
	}
	return v
}

func (p *condParser) parseUnary() Tri {
	if !p.atEnd() && p.isKeyword(p.peek(), "NOT") {
		p.next()
		return kleeneNot(p.parseUnary())
	}
	return p.parseAtom()
}

func (p *condParser) parseAtom() Tri {
	if p.atEnd() {
		p.errored = true
		return TriUnknown
	}
	if p.isKeyword(p.peek(), "(") {
		p.next()
		v := p.parseLogical()
		if p.atEnd() || !p.isKeyword(p.peek(), ")") {
			p.errored = true
			return TriUnknown
		}
		p.next()
		return v
	}
	lhsTok := p.next()
	lhsVal, lhsUnresolved := resolveOperand(lhsTok, p.env)
	p.unresolved = append(p.unresolved, lhsUnresolved...)

	if !p.atEnd() && !p.peek().Quoted && comparisonOps[p.peek().Text] {
		opTok := p.next()
		if p.atEnd() {
			p.errored = true
			return TriUnknown
		}
		rhsTok := p.next()
		rhsVal, rhsUnresolved := resolveOperand(rhsTok, p.env)
		p.unresolved = append(p.unresolved, rhsUnresolved...)
		return evalComparison(opTok.Text, lhsVal, rhsVal)
	}
	if lhsTok.Quoted {
		return truthyQuoted(lhsVal)
	}
	return truthy(lhsVal)
}

// truthyQuoted applies CMake's CMP0054 NEW operand rule: a quoted operand is
// not a variable dereference and is true only when its expanded value is a
// documented true constant or a non-zero number. An unresolved expansion
// remains Unknown rather than being guessed false.
func truthyQuoted(val *string) Tri {
	if val == nil {
		return TriUnknown
	}
	v := strings.ToUpper(strings.TrimSpace(*val))
	if cmakeFalseConstants[v] || strings.HasSuffix(v, "-NOTFOUND") {
		return TriFalse
	}
	if number, err := strconv.ParseFloat(v, 64); err == nil {
		if number == 0 {
			return TriFalse
		}
		return TriTrue
	}
	if cmakeConstants[v] {
		return TriTrue
	}
	return TriFalse
}

// resolveOperand resolves one if()-condition operand token to a concrete
// value, or nil ("unresolved") plus the variable name(s) responsible.
// Resolution order mirrors real CMake if() semantics: a quoted string is
// always a literal (after ${VAR} expansion); an unquoted token that is
// EXACTLY "${IDENT}" dereferences IDENT; an unquoted CMake boolean constant
// (TRUE/FALSE/ON/OFF/...) is never looked up as a variable even if a
// same-named variable happened to exist; any other plain identifier is
// looked up as a bare variable dereference (if() does this implicitly);
// anything else (numbers, bare paths) is used as a literal, still expanded
// for embedded ${VAR} references.
func resolveOperand(tok token, env *varEnv) (*string, []string) {
	if tok.Quoted {
		return operandFromExpansion(env.expandToken(tok))
	}
	if m := reVarRefFull.FindStringSubmatch(tok.Text); m != nil {
		// lookupCertain, not lookup: a variable assigned under a guard this
		// parser could not decide has no certain value, so a condition reading
		// it is itself undecidable and must report Unknown rather than
		// evaluating against a value that may never have been assigned.
		v := env.lookupCertain(m[1])
		if v == nil {
			return nil, []string{m[1]}
		}
		return resolveExpandedUnquoted(*v, env)
	}
	upper := strings.ToUpper(tok.Text)
	if cmakeConstants[upper] || strings.HasSuffix(upper, "-NOTFOUND") {
		s := tok.Text
		return &s, nil
	}
	if rePlainIdent.MatchString(tok.Text) {
		v := env.lookupCertain(tok.Text)
		if v == nil {
			return nil, []string{tok.Text}
		}
		return v, nil
	}
	return operandFromExpansion(env.expandToken(tok))
}

func resolveExpandedUnquoted(value string, env *varEnv) (*string, []string) {
	upper := strings.ToUpper(value)
	if cmakeConstants[upper] || strings.HasSuffix(upper, "-NOTFOUND") {
		resolved := value
		return &resolved, nil
	}
	if rePlainIdent.MatchString(value) {
		resolved := env.lookupCertain(value)
		if resolved == nil {
			return nil, []string{value}
		}
		return resolved, nil
	}
	return operandFromExpansion(env.expandToken(token{Text: value}))
}

// operandFromExpansion turns an expansion into an if()-operand. An expansion
// that substituted an uncertain scalar yields NO value: the same rule as
// lookupCertain, applied to a token that merely EMBEDS such a variable rather
// than being one.
// An expansion carrying an UNRESOLVED reference also yields no value, even when
// its certainty is TriTrue. expandToken leaves the verbatim `${NAME}` in .text
// and records the name in .unresolved WITHOUT lowering certainty, so gating on
// certainty alone let a definite operand through whose value was the literal
// string "${WINSDK_VERSION}". The comparison then produced a DEFINITE verdict —
// `conditional_not_applied`, whose contract is "definitively FALSE for this
// triplet" — about a variable this package never had a value for.
//
// That reached the operator on the default python3 call: WINSDK_VERSION comes
// from the MSVC toolchain scripts, not from any triplet file. The mirror case
// was worse — `if("${SOME_UNKNOWN_FLAG}")` reported `applied` where real CMake
// evaluates FALSE.
//
// triplet.go's header claims this class was closed by removing name-derivation.
// That closed the UNQUOTED spelling; the quoted spelling above stayed live — an
// instance fix on an open class. Pinned by TestOperandFromExpansion_*.
func operandFromExpansion(ex expansion) (*string, []string) {
	if ex.certainty != TriTrue || len(ex.unresolved) > 0 {
		return nil, dedupStrings(append(append([]string{}, ex.unresolved...), ex.uncertainVars...))
	}
	text := ex.text
	return &text, ex.unresolved
}

var rePlainIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func evalComparison(op string, lhs, rhs *string) Tri {
	if lhs == nil || rhs == nil {
		return TriUnknown
	}
	boolTri := func(b bool) Tri {
		if b {
			return TriTrue
		}
		return TriFalse
	}
	switch op {
	case "STREQUAL":
		return boolTri(*lhs == *rhs)
	case "EQUAL":
		li, lerr := strconv.Atoi(strings.TrimSpace(*lhs))
		ri, rerr := strconv.Atoi(strings.TrimSpace(*rhs))
		if lerr != nil || rerr != nil {
			return TriUnknown
		}
		return boolTri(li == ri)
	case "VERSION_EQUAL":
		return boolTri(compareVersions(*lhs, *rhs) == 0)
	case "VERSION_GREATER":
		return boolTri(compareVersions(*lhs, *rhs) > 0)
	case "VERSION_GREATER_EQUAL":
		return boolTri(compareVersions(*lhs, *rhs) >= 0)
	case "VERSION_LESS":
		return boolTri(compareVersions(*lhs, *rhs) < 0)
	case "VERSION_LESS_EQUAL":
		return boolTri(compareVersions(*lhs, *rhs) <= 0)
	default:
		return TriUnknown
	}
}

// evalCondition parses+evaluates raw (the text between the parens of an
// if()/elseif() call) against env. A structurally unparsable expression
// (unbalanced parens, trailing tokens, an atom where an operator was
// expected) returns Unknown rather than any partial guess.
func evalCondition(raw string, env *varEnv) (Tri, []string) {
	toks := tokenize(raw)
	p := &condParser{toks: toks, env: env}
	v := p.parseLogical()
	if p.errored || !p.atEnd() {
		return TriUnknown, dedupStrings(p.unresolved)
	}
	return v, dedupStrings(p.unresolved)
}
