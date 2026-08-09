package patchesapply

import "strings"

// This file implements a deliberately narrow CMake lexer: it is NOT a CMake
// parser and does not execute anything. It recognizes just enough shape —
// statements, quoted/bare/bracket tokens, comments, parenthesis nesting — to
// locate set()/get_filename_component()/if()/elseif()/else()/endif()/
// foreach()/endforeach()/while()/endwhile()/list()/and any other call's
// argument list, and to hand the argument text
// to the higher-level walk in walk.go. Anything outside this shape is left
// as an opaque call the walk simply does not special-case.
//
// Grammar productions relied on below are quoted from the official
// cmake-language(7) manual
// (https://cmake.org/cmake/help/latest/manual/cmake-language.7.html):
//
//	bracket_comment  ::= '#' bracket_argument
//	bracket_argument ::= bracket_open bracket_content bracket_close
//	bracket_open     ::= '[' '='* '['
//	bracket_content  ::= <any text not containing a bracket_close with
//	                      the same number of '=' as the bracket_open>
//	bracket_close    ::= ']' '='* ']'
//	quoted_argument     ::= '"' quoted_element* '"'
//	quoted_element      ::= <any character except '\' or '"'> |
//	                         escape_sequence | quoted_continuation
//	quoted_continuation ::= '\' newline
//
// "No evaluation of the enclosed content, such as Escape Sequences or
// Variable References, is performed" inside a bracket_argument — this is
// why bracket-derived tokens are marked Raw (never ${VAR}-expanded) below.
// "The final `\` on any line ending in an odd number of backslashes is
// treated as a line continuation and ignored along with the immediately
// following newline character" — this is why quoted_continuation resolves
// to NOTHING (not a preserved newline) in unescapeQuoted.

// statement is one top-level `name(args...)` call found in a portfile,
// with args as the raw, comment-stripped text between the outermost
// parentheses (not yet tokenized — different callers tokenize differently:
// condition text vs. call-argument text).
//
// Name is always LOWER-CASE. cmake-language(7) states "Command names are
// case-insensitive", so a portfile writing IF(...) / SET(...) / LIST(...) is
// ordinary valid CMake. Normalizing at the single point statements are
// produced makes every consumer's comparison correct by construction; folding
// at each call site instead is what let walkPortfile's switch stay
// case-SENSITIVE (so an upper-case IF()/SET() was silently misattributed to
// the default branch, giving every patch inside it the WRONG guard) while
// parseTripletFacts beside it was already folded. The original spelling is not
// retained because nothing in this package displays a command name.
type statement struct {
	Name string
	Args string
	Line int // 1-based line of the command name
}

// splitStatementsChecked scans src for a sequence of `identifier(...)`
// statements, skipping line/bracket comments and respecting quoted strings,
// bracket arguments, and parenthesis nesting. Text it cannot make sense of
// between statements (e.g. unparenthesized garbage) is simply skipped, and
// the walk degrades whatever it does not recognize into "no PATCHES
// declared" or an unresolved guard rather than a hard parser failure — the
// design's own "when the shape defeats you, return undecidable/unknown, not
// a guess" principle applied at the lexer's own boundary. The one thing
// this lexer DOES treat as a hard failure (ok=false) is a call whose
// parentheses never balance before EOF, or a bracket comment/argument that
// never closes: every statement extracted after that point rests on a
// mis-scanned boundary and cannot be trusted.
func splitStatementsChecked(src string) (out []statement, ok bool) {
	i := 0
	line := 1
	n := len(src)
	// countLines is a PURE helper: it only tallies newlines in [from, to),
	// it never mutates i itself. Every call site below sets i = to exactly
	// once, right after calling this — an earlier version instead had a
	// helper mutate the shared i as a side effect AND had call sites
	// separately reassign/increment i again afterward, silently skipping or
	// duplicating characters. Single ownership of "what i becomes next"
	// here is the fix.
	countLines := func(from, to int) {
		for k := from; k < to; k++ {
			if src[k] == '\n' {
				line++
			}
		}
	}
	for i < n {
		c := src[i]
		switch {
		case c == '#':
			if eq, contentStart, isBracket := matchBracketOpen(src, i+1); isBracket {
				// bracket_comment ::= '#' bracket_argument — spans lines;
				// the ENTIRE thing (including any decoy statement text
				// inside it) must never be treated as code.
				_, afterClose, closed := findBracketClose(src, contentStart, eq)
				if !closed {
					countLines(i, n)
					return out, false
				}
				countLines(i, afterClose)
				i = afterClose
				continue
			}
			// Plain line comment: skip to end of line.
			j := strings.IndexByte(src[i:], '\n')
			var to int
			if j < 0 {
				to = n
			} else {
				to = i + j + 1
			}
			countLines(i, to)
			i = to
		case c == '"':
			// Skip a bare quoted string sitting outside any call (rare, but
			// don't misinterpret its contents as statement syntax).
			end := skipQuoted(src, i)
			countLines(i, end)
			i = end
		case c == '[':
			if eq, contentStart, isBracket := matchBracketOpen(src, i); isBracket {
				_, afterClose, closed := findBracketClose(src, contentStart, eq)
				if !closed {
					countLines(i, n)
					return out, false
				}
				countLines(i, afterClose)
				i = afterClose
				continue
			}
			countLines(i, i+1)
			i++
		case isIdentStart(c):
			start := i
			idEnd := i
			for idEnd < n && isIdentByte(src[idEnd]) {
				idEnd++
			}
			name := src[start:idEnd]
			// CMake's command_invocation grammar permits only horizontal
			// space between the identifier and '('. A line ending terminates
			// recognition; accepting it would execute syntax CMake rejects.
			j := idEnd
			for j < n && (src[j] == ' ' || src[j] == '\t') {
				j++
			}
			if j >= n || src[j] != '(' {
				countLines(i, j)
				i = j
				continue
			}
			countLines(i, j)
			nameLine := line
			argStart := j + 1
			depth := 1
			k := argStart
			for k < n && depth > 0 {
				switch {
				case src[k] == '\\' && k+1 < n:
					// An unquoted escape keeps its following delimiter inside the
					// bare argument; escaped # and parentheses are not syntax here.
					if src[k+1] == '\r' && k+2 < n && src[k+2] == '\n' {
						k += 3
					} else {
						k += 2
					}
					continue
				case src[k] == '"':
					k = skipQuoted(src, k)
					continue
				case src[k] == '#':
					if eq, contentStart, isBracket := matchBracketOpen(src, k+1); isBracket {
						_, afterClose, closed := findBracketClose(src, contentStart, eq)
						if !closed {
							k = n
							break
						}
						k = afterClose
						continue
					}
					jj := strings.IndexByte(src[k:], '\n')
					if jj < 0 {
						k = n
					} else {
						k = k + jj + 1
					}
					continue
				case src[k] == '[':
					if eq, contentStart, isBracket := matchBracketOpen(src, k); isBracket {
						if _, afterClose, closed := findBracketClose(src, contentStart, eq); closed {
							k = afterClose
							continue
						}
						// An unclosed bracket argument invalidates every later
						// statement boundary; do not reinterpret it as a bareword.
						countLines(i, n)
						return out, false
					}
					k++
				case src[k] == '(':
					depth++
					k++
				case src[k] == ')':
					depth--
					k++
				default:
					k++
				}
			}
			if depth > 0 {
				// Never closed before EOF — the whole file's statement
				// boundaries from here on are unreliable.
				countLines(i, n)
				return out, false
			}
			argEnd := k - 1
			if argEnd < argStart {
				argEnd = argStart
			}
			args := src[argStart:argEnd]
			// Lower-cased HERE, once — see the statement type's doc comment.
			out = append(out, statement{Name: strings.ToLower(name), Args: args, Line: nameLine})
			countLines(j, k)
			i = k
		default:
			countLines(i, i+1)
			i++
		}
	}
	return out, true
}

// skipQuoted returns the index just past a double-quoted string starting at
// src[start] (src[start] == '"'), honoring backslash escapes — including a
// quoted_continuation ('\' + newline), which this simply treats as an
// escaped pair like any other and keeps scanning across, never ending the
// string at that newline.
func skipQuoted(src string, start int) int {
	i := start + 1
	n := len(src)
	for i < n {
		if src[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if src[i] == '"' {
			return i + 1
		}
		i++
	}
	return n
}

// matchBracketOpen checks whether src[at] begins a bracket_open
// ('[' '='* '['). Returns the number of '=' characters and the index just
// past the opening pair when it does.
func matchBracketOpen(src string, at int) (equals int, contentStart int, ok bool) {
	n := len(src)
	if at >= n || src[at] != '[' {
		return 0, 0, false
	}
	p := at + 1
	for p < n && src[p] == '=' {
		p++
	}
	if p >= n || src[p] != '[' {
		return 0, 0, false
	}
	return p - (at + 1), p + 1, true
}

// findBracketClose searches from contentStart for a bracket_close
// (']' '='* ']') with EXACTLY the given equals count — per the grammar, a
// bracket_close with a DIFFERENT equals count does not terminate the
// bracket (e.g. a literal "]]" inside a "[=[ ... ]=]" argument is just
// content). Returns the content end (start of the close sequence) and the
// index just past it; ok is false if no matching close exists before EOF.
func findBracketClose(src string, contentStart, equals int) (contentEnd, afterClose int, ok bool) {
	n := len(src)
	for p := contentStart; p < n; p++ {
		if src[p] != ']' {
			continue
		}
		q := p + 1
		matched := true
		for e := 0; e < equals; e++ {
			if q >= n || src[q] != '=' {
				matched = false
				break
			}
			q++
		}
		if matched && q < n && src[q] == ']' {
			return p, q + 1, true
		}
	}
	return 0, 0, false
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isSpaceOrNL(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// token is one lexical unit from tokenize. Quoted matters downstream: a
// quoted token is always a literal string (subject only to ${VAR}
// expansion), while an unquoted bareword may itself BE a variable name that
// if() dereferences directly — collapsing that distinction would make a
// literal string like "static" indistinguishable from the variable name
// VCPKG_LIBRARY_LINKAGE. Raw additionally marks a bracket_argument: per
// cmake-language(7), "No evaluation of the enclosed content, such as Escape
// Sequences or Variable References, is performed" — a Raw token's Text must
// never be run through env.expand (see varEnv.expandToken).
type token struct {
	// Text is the literal argument text after lexical escapes such as \; have
	// been materialized. listText retains the list serialization marker so the
	// one list splitter below can distinguish an escaped literal semicolon from
	// a separator after variable substitution.
	Text     string
	listText string
	Quoted   bool
	Raw      bool
}

// tokenize splits a raw argument or condition text into CMake-ish tokens:
// whitespace-separated barewords, double-quoted strings as single tokens
// (quotes stripped, backslash-escapes resolved per cmake-language(7)),
// bracket arguments ([[...]], [=[...]=], ...) as single Raw tokens (content
// verbatim, delimiter-matched by equals count — a "]]" inside a "[=[...]=]"
// argument is content, not a close), plus standalone parentheses tokens
// ("(" / ")") so the condition parser can recognize grouping. Line and
// bracket comments are stripped.
func tokenize(s string) []token {
	var tokens []token
	i := 0
	n := len(s)
	for i < n {
		c := s[i]
		switch {
		case isSpaceOrNL(c):
			i++
		case c == '#':
			if eq, contentStart, isBracket := matchBracketOpen(s, i+1); isBracket {
				if _, afterClose, closed := findBracketClose(s, contentStart, eq); closed {
					i = afterClose
					continue
				}
			}
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i = i + j + 1
			}
		case c == '"':
			end := skipQuoted(s, i)
			raw := s[i+1 : end-1]
			text := unescapeQuoted(raw)
			tokens = append(tokens, token{Text: text, listText: text, Quoted: true})
			i = end
		case c == '[':
			if eq, contentStart, isBracket := matchBracketOpen(s, i); isBracket {
				if contentEnd, afterClose, closed := findBracketClose(s, contentStart, eq); closed {
					content := s[contentStart:contentEnd]
					// CMake bracket arguments ignore one newline immediately after
					// the opening delimiter; keeping it turns a real filename into
					// a different missing path.
					if strings.HasPrefix(content, "\r\n") {
						content = content[2:]
					} else if strings.HasPrefix(content, "\n") {
						content = content[1:]
					}
					tokens = append(tokens, token{Text: content, listText: content, Quoted: true, Raw: true})
					i = afterClose
					continue
				}
			}
			// Not a valid/closed bracket_open: treat '[' as an ordinary
			// bareword character (same scan rule as the default case).
			start := i
			i = scanBareEnd(s, i)
			raw := s[start:i]
			tokens = append(tokens, token{Text: unescapeBare(raw), listText: raw})
		case c == '(' || c == ')':
			tokens = append(tokens, token{Text: string(c), listText: string(c)})
			i++
		default:
			start := i
			i = scanBareEnd(s, i)
			raw := s[start:i]
			tokens = append(tokens, token{Text: unescapeBare(raw), listText: raw})
		}
	}
	return tokens
}

func scanBareEnd(s string, start int) int {
	for i := start; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\r' && i+2 < len(s) && s[i+2] == '\n' {
				i += 3
			} else {
				i += 2
			}
			continue
		}
		if isSpaceOrNL(s[i]) || s[i] == '"' || s[i] == '(' || s[i] == ')' || s[i] == '#' {
			return i
		}
		i++
	}
	return len(s)
}

// splitCMakeList is the sole CMake argument-boundary owner. Source-escaped
// semicolons arrive marked by evaluateValue; serialized bytes inserted from a
// variable are governed by CMake's serialized list rules instead.
func splitCMakeList(t token, value evaluatedValue) []semanticItem {
	items, _ := splitCMakeListBounded(t, value, int(^uint(0)>>1))
	return items
}

// splitCMakeListBounded retains at most maxItems semantic arguments and
// reports whether another non-empty argument existed. PATCHES extraction uses
// this while scanning the serialized bytes so expansion cannot first create
// an unbounded temporary slice and only then discover the result is too large.
func splitCMakeListBounded(t token, value evaluatedValue, maxItems int) ([]semanticItem, bool) {
	if t.Quoted || t.Raw {
		if maxItems < 1 {
			return nil, true
		}
		return []semanticItem{semanticItem{
			text:              string(value.text),
			display:           string(value.text),
			meta:              combineItemProvenance(value.metas),
			spans:             spansFromMetas(value.metas),
			literalReferences: t.Raw,
		}}, false
	}

	items := make([]semanticItem, 0, 1)
	current := make([]byte, 0, len(value.text))
	metas := make([]provenanceMeta, 0, len(value.text))
	exceeded := false
	flush := func() {
		if len(current) == 0 {
			return // empty unquoted list elements are not command arguments.
		}
		if len(items) >= maxItems {
			exceeded = true
			return
		}
		items = append(items, semanticItem{
			text:    string(current),
			display: string(current),
			meta:    combineItemProvenance(metas),
			spans:   spansFromMetas(metas),
		})
		current = current[:0]
		metas = metas[:0]
	}
	openBrackets, closeBrackets := 0, 0
	for i, b := range value.text {
		switch b {
		case '[':
			openBrackets++
		case ']':
			closeBrackets++
		case ';':
			if value.protectedSemicolon[i] || openBrackets != closeBrackets {
				current = append(current, b)
				metas = append(metas, value.metas[i])
				continue
			}
			backslashes := 0
			for j := len(current) - 1; j >= 0 && current[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes != 0 {
				// CMake 4.4 treats every non-empty immediately preceding
				// serialized run as protective and consumes exactly one slash.
				current = current[:len(current)-1]
				metas = metas[:len(metas)-1]
				current = append(current, b)
				metas = append(metas, value.metas[i])
				continue
			}
			flush()
			if exceeded {
				return items, true
			}
			openBrackets, closeBrackets = 0, 0
			continue
		}
		current = append(current, b)
		metas = append(metas, value.metas[i])
	}
	flush()
	return items, exceeded
}

func combineItemProvenance(metas []provenanceMeta) provenanceMeta {
	combined := certainProvenance()
	for _, meta := range metas {
		combined = combineProvenance(combined, meta)
	}
	return combined
}

// unescapeBare materializes the bare-argument escape that matters to CMake
// list semantics while listText retains the source marker for splitCMakeList.
func unescapeBare(s string) string {
	return unescapeQuoted(s)
}

// unescapeQuoted resolves quoted_argument escape productions per
// cmake-language(7):
//
//   - quoted_continuation ('\' + newline, or '\' + CRLF): ignored along with
//     the newline — contributes NOTHING to the value (not a preserved
//     newline).
//   - escape_encoded ('\t', '\r', '\n' — backslash followed by the LITERAL
//     letter): produces the actual TAB/CR/LF control byte.
//   - escape_identity ('\' + any non-alphanumeric, non-';' character) and
//     escape_semicolon ('\;'): produce that literal character.
//   - anything else ('\' followed by another alphanumeric): not a defined
//     escape_sequence production at all — kept verbatim (backslash AND the
//     character) rather than silently eating the backslash for an
//     unrecognized escape.
func unescapeQuoted(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	n := len(s)
	for i := 0; i < n; i++ {
		if s[i] != '\\' || i+1 >= n {
			b.WriteByte(s[i])
			continue
		}
		next := s[i+1]
		switch {
		case next == '\n':
			i++ // quoted_continuation: drop both the backslash and the newline.
		case next == '\r' && i+2 < n && s[i+2] == '\n':
			i += 2 // CRLF variant of the same continuation.
		case next == 't':
			b.WriteByte('\t')
			i++
		case next == 'r':
			b.WriteByte('\r')
			i++
		case next == 'n':
			b.WriteByte('\n')
			i++
		case (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') || (next >= '0' && next <= '9'):
			b.WriteByte(s[i])
			b.WriteByte(next)
			i++
		default:
			b.WriteByte(next)
			i++
		}
	}
	return b.String()
}
