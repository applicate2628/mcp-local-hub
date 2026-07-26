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

func (p *condParser) parseOr() Tri {
	v := p.parseAnd()
	for !p.atEnd() && p.isKeyword(p.peek(), "OR") {
		p.next()
		v = kleeneOr(v, p.parseAnd())
	}
	return v
}

func (p *condParser) parseAnd() Tri {
	v := p.parseUnary()
	for !p.atEnd() && p.isKeyword(p.peek(), "AND") {
		p.next()
		v = kleeneAnd(v, p.parseUnary())
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
		v := p.parseOr()
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
	return truthy(lhsVal)
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
		expanded, unresolved := env.expandToken(tok)
		return &expanded, unresolved
	}
	if m := reVarRefFull.FindStringSubmatch(tok.Text); m != nil {
		v := env.lookup(m[1])
		if v == nil {
			return nil, []string{m[1]}
		}
		return v, nil
	}
	upper := strings.ToUpper(tok.Text)
	if cmakeConstants[upper] || strings.HasSuffix(upper, "-NOTFOUND") {
		s := tok.Text
		return &s, nil
	}
	if rePlainIdent.MatchString(tok.Text) {
		v := env.lookup(tok.Text)
		if v == nil {
			return nil, []string{tok.Text}
		}
		return v, nil
	}
	expanded, unresolved := env.expandToken(tok)
	return &expanded, unresolved
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
	v := p.parseOr()
	if p.errored || !p.atEnd() {
		return TriUnknown, dedupStrings(p.unresolved)
	}
	return v, dedupStrings(p.unresolved)
}
