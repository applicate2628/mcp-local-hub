package pinstatus

import (
	"encoding/json"
	"regexp"
	"strings"
)

// This is a small, faithful mirror of patchesapply/lexer.go's CMake lexical
// rules. Keep the two implementations aligned: bracket comments/arguments
// have delimiter-matched closing brackets, bracket values are Raw, and a
// quoted trailing-backslash newline is a continuation, not a token boundary.

// statement is one `name(args...)` call found in a portfile. Name is always
// LOWER-CASE: CMake command names are case-insensitive (cmake-language(7)),
// and normalizing once in splitStatementsChecked is what keeps every consumer
// from having to remember to fold. The original spelling is not retained
// because nothing in this package displays a command name.
type statement struct {
	Name string
	Args string
}

type token struct {
	Text   string
	Quoted bool
	Raw    bool
}

type argValue struct {
	Text string
	Raw  bool
}

type versionManifest struct {
	Version       string `json:"version"`
	VersionString string `json:"version-string"`
	VersionDate   string `json:"version-date"`
	VersionSemver string `json:"version-semver"`
}

// parsedPortfile is the internal result of textually scanning a portfile. It
// never executes CMake; an unresolved guard is returned explicitly instead of
// selecting a plausible-looking source call.
type parsedPortfile struct {
	Remote                    Remote
	Pin                       Pin
	HeadRef                   string
	UnresolvedHeadRefVariable string
	Candidates                []FetchCandidate
	UnresolvedGuardVariable   string
	MultipleFetchCalls        bool
	CandidateLimitExceeded    bool
}

func retainedFetchCandidateBytes(candidate FetchCandidate) int {
	const fixedCandidateBytes = 256
	return fixedCandidateBytes +
		len(candidate.Remote.Kind) + len(candidate.Remote.Repo) + len(candidate.Remote.URL) +
		len(candidate.Pin.Ref) + len(candidate.Pin.ResolvedRef) + len(candidate.Pin.Shape) +
		len(candidate.HeadRef) + len(candidate.UnresolvedHeadRefVariable) +
		len(candidate.Guard) + len(candidate.GuardVariable)
}

var fetchFuncNames = map[string]bool{
	"vcpkg_from_github":       true,
	"vcpkg_from_git":          true,
	"vcpkg_from_gitlab":       true,
	"vcpkg_download_distfile": true,
}

var knownArgKeys = map[string]bool{
	"REPO":                    true,
	"REF":                     true,
	"SHA512":                  true,
	"HEAD_REF":                true,
	"PATCHES":                 true,
	"URL":                     true,
	"URLS":                    true,
	"FETCH_REF":               true,
	"GITLAB_URL":              true,
	"AUTHORIZATION_TOKEN":     true,
	"FILENAME":                true,
	"OUT_SOURCE_PATH":         true,
	"FILE_DISABLE_SUBMODULES": true,
}

var (
	// embeddedVarRE matches a ${NAME} reference ANYWHERE in a token.
	//
	// It deliberately replaces an earlier ANCHORED `^\$\{...\}$` pattern that
	// matched only a token consisting of NOTHING BUT one variable. That
	// anchoring was the root cause of a field-reported P1: "${VTK_GIT_REF}"
	// matched and was correctly reported unresolvable, while the far more
	// common "v${VERSION}" did not match, fell through to the literal-token
	// path, and was then compared VERBATIM against the remote's ref names —
	// producing a confident "this ref does not exist upstream" for refs that
	// do exist. A wrong negative is the worst output this contract can
	// produce, so containment, not equality, is the correct test.
	embeddedVarRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	commitHexRE   = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	// abbrevCommitHexRE matches a pure-hex token too short to be a full SHA
	// but long enough to be an ABBREVIATED commit. The lower bound is 7, git's
	// own default short-SHA length (`--short` / core.abbrev): below that a
	// pure-hex token in a portfile is far more likely to be a name that merely
	// happens to be hex ("beef", "2024") than a commit abbreviation, and this
	// package must not manufacture a commit out of a tag.
	abbrevCommitHexRE = regexp.MustCompile(`^[0-9a-fA-F]{7,39}$`)
	tagLikeRE         = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+){1,3}`)
)

func isCommitHex(ref string) bool { return commitHexRE.MatchString(ref) }

// isAbbreviatedCommitHex reports whether ref has the SHAPE of an abbreviated
// commit. It is a syntactic claim only, exactly like looksLikeTag — the caller
// decides what it means, and pinstatus.go deliberately consults it only AFTER
// the remote has been asked whether it advertises a ref by this literal name,
// so a real branch/tag that happens to be hex is classified by evidence rather
// than by shape.
func isAbbreviatedCommitHex(ref string) bool { return abbrevCommitHexRE.MatchString(ref) }

func looksLikeTag(ref string) bool { return tagLikeRE.MatchString(ref) }

// stripComments is retained for focused parser callers. Unlike the former
// line-based implementation it recognizes bracket comments and keeps quoted
// and bracket arguments intact. Newlines are preserved for diagnostic shape.
func stripComments(content string) string {
	var out strings.Builder
	for i := 0; i < len(content); {
		switch content[i] {
		case '#':
			if eq, start, ok := matchBracketOpen(content, i+1); ok {
				_, after, closed := findBracketClose(content, start, eq)
				if !closed {
					return out.String()
				}
				out.WriteString(strings.Map(func(r rune) rune {
					if r == '\n' || r == '\r' {
						return r
					}
					return ' '
				}, content[i:after]))
				i = after
				continue
			}
			j := strings.IndexByte(content[i:], '\n')
			if j < 0 {
				return out.String()
			}
			out.WriteString(content[i : i+j+1])
			i += j + 1
		case '"':
			end := skipQuoted(content, i)
			out.WriteString(content[i:end])
			i = end
		case '[':
			if eq, start, ok := matchBracketOpen(content, i); ok {
				_, after, closed := findBracketClose(content, start, eq)
				if closed {
					out.WriteString(content[i:after])
					i = after
					continue
				}
			}
			out.WriteByte(content[i])
			i++
		default:
			out.WriteByte(content[i])
			i++
		}
	}
	return out.String()
}

func splitStatementsChecked(src string) (out []statement, ok bool) {
	i, n := 0, len(src)
	for i < n {
		switch {
		case src[i] == '#':
			if eq, start, bracket := matchBracketOpen(src, i+1); bracket {
				_, after, closed := findBracketClose(src, start, eq)
				if !closed {
					return out, false
				}
				i = after
				continue
			}
			if j := strings.IndexByte(src[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = n
			}
		case src[i] == '"':
			i = skipQuoted(src, i)
		case src[i] == '[':
			if eq, start, bracket := matchBracketOpen(src, i); bracket {
				_, after, closed := findBracketClose(src, start, eq)
				if !closed {
					return out, false
				}
				i = after
				continue
			}
			i++
		case isIdentStart(src[i]):
			nameStart := i
			for i < n && isIdentByte(src[i]) {
				i++
			}
			name := src[nameStart:i]
			j := i
			for j < n && isSpaceOrNL(src[j]) {
				j++
			}
			if j >= n || src[j] != '(' {
				i = j
				continue
			}
			argStart, depth, k := j+1, 1, j+1
			for k < n && depth > 0 {
				switch {
				case src[k] == '"':
					k = skipQuoted(src, k)
				case src[k] == '#':
					if eq, start, bracket := matchBracketOpen(src, k+1); bracket {
						_, after, closed := findBracketClose(src, start, eq)
						if !closed {
							return out, false
						}
						k = after
					} else if lineEnd := strings.IndexByte(src[k:], '\n'); lineEnd >= 0 {
						k += lineEnd + 1
					} else {
						k = n
					}
				case src[k] == '[':
					if eq, start, bracket := matchBracketOpen(src, k); bracket {
						_, after, closed := findBracketClose(src, start, eq)
						if !closed {
							return out, false
						}
						k = after
					} else {
						k++
					}
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
			if depth != 0 {
				return out, false
			}
			// Name is LOWER-CASED here, at the single point statements are
			// produced, because cmake-language(7) states "Command names are
			// case-insensitive" — a portfile writing IF(...) / SET(...) /
			// VCPKG_FROM_GITHUB(...) is ordinary valid CMake. Normalizing at
			// the lexer makes every downstream comparison correct by
			// construction; comparing case-insensitively at each call site
			// instead is what let vcpkg_from_* dispatch stay case-SENSITIVE
			// (so an upper-case fetch call was silently invisible) while the
			// if/endif handling beside it was already folded.
			out = append(out, statement{Name: strings.ToLower(name), Args: src[argStart : k-1]})
			i = k
		default:
			i++
		}
	}
	return out, true
}

func skipQuoted(src string, start int) int {
	for i := start + 1; i < len(src); i++ {
		if src[i] == '\\' && i+1 < len(src) {
			i++
			continue
		}
		if src[i] == '"' {
			return i + 1
		}
	}
	return len(src)
}

func matchBracketOpen(src string, at int) (equals, contentStart int, ok bool) {
	if at >= len(src) || src[at] != '[' {
		return 0, 0, false
	}
	p := at + 1
	for p < len(src) && src[p] == '=' {
		p++
	}
	if p >= len(src) || src[p] != '[' {
		return 0, 0, false
	}
	return p - at - 1, p + 1, true
}

func findBracketClose(src string, contentStart, equals int) (contentEnd, afterClose int, ok bool) {
	for p := contentStart; p < len(src); p++ {
		if src[p] != ']' {
			continue
		}
		q := p + 1
		for e := 0; e < equals && q < len(src) && src[q] == '='; e++ {
			q++
		}
		if q == p+1+equals && q < len(src) && src[q] == ']' {
			return p, q + 1, true
		}
	}
	return 0, 0, false
}

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdentByte(c byte) bool  { return isIdentStart(c) || c >= '0' && c <= '9' }
func isSpaceOrNL(c byte) bool  { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func tokenize(s string) []token {
	var out []token
	for i := 0; i < len(s); {
		switch {
		case isSpaceOrNL(s[i]):
			i++
		case s[i] == '#':
			if eq, start, bracket := matchBracketOpen(s, i+1); bracket {
				if _, after, closed := findBracketClose(s, start, eq); closed {
					i = after
					continue
				}
			}
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(s)
			}
		case s[i] == '"':
			end := skipQuoted(s, i)
			out = append(out, token{Text: unescapeQuoted(s[i+1 : end-1]), Quoted: true})
			i = end
		case s[i] == '[':
			if eq, start, bracket := matchBracketOpen(s, i); bracket {
				if end, after, closed := findBracketClose(s, start, eq); closed {
					out = append(out, token{Text: s[start:end], Quoted: true, Raw: true})
					i = after
					continue
				}
			}
			fallthrough
		default:
			start := i
			for i < len(s) && !isSpaceOrNL(s[i]) && s[i] != '"' && s[i] != '#' && s[i] != '(' && s[i] != ')' {
				i++
			}
			if start != i {
				out = append(out, token{Text: s[start:i]})
			} else {
				i++
			}
		}
	}
	return out
}

func unescapeQuoted(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		next := s[i+1]
		switch {
		case next == '\n':
			i++
		case next == '\r' && i+2 < len(s) && s[i+2] == '\n':
			i += 2
		case next == 't':
			out.WriteByte('\t')
			i++
		case next == 'r':
			out.WriteByte('\r')
			i++
		case next == 'n':
			out.WriteByte('\n')
			i++
		case next >= 'A' && next <= 'Z' || next >= 'a' && next <= 'z' || next >= '0' && next <= '9':
			out.WriteByte(s[i])
			out.WriteByte(next)
			i++
		default:
			out.WriteByte(next)
			i++
		}
	}
	return out.String()
}

// tokenizeArgs remains the simple string view used by focused existing tests.
func tokenizeArgs(block string) []string {
	tokens := tokenize(block)
	out := make([]string, len(tokens))
	for i := range tokens {
		out[i] = tokens[i].Text
	}
	return out
}

func extractKeyedArgValues(tokens []token) map[string]argValue {
	out := map[string]argValue{}
	for i := 0; i < len(tokens); i++ {
		if !knownArgKeys[tokens[i].Text] {
			continue
		}
		if i+1 < len(tokens) && !knownArgKeys[tokens[i+1].Text] {
			out[tokens[i].Text] = argValue{Text: tokens[i+1].Text, Raw: tokens[i+1].Raw}
			i++
		} else {
			out[tokens[i].Text] = argValue{}
		}
	}
	return out
}

// extractKeyedArgs is retained as the string-only view for existing callers.
func extractKeyedArgs(tokens []string) map[string]string {
	wrapped := make([]token, len(tokens))
	for i := range tokens {
		wrapped[i].Text = tokens[i]
	}
	values := extractKeyedArgValues(wrapped)
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value.Text
	}
	return out
}

// variableEnvironment is the parser's call-site model of local CMake state.
// A binding is absent, known, or unknown; a dynamic assignment establishes an
// unknown floor for every older binding until that name is assigned definitely.
// It never leaves parsePortfileWithManifest or rescans portfile text.
type variableEnvironment struct {
	bindings            map[string]variableBinding
	generation          int
	dynamicUnknownFloor int
}

type variableBinding struct {
	value      string
	known      bool
	generation int
}

type localVariableState uint8

const (
	localVariableAbsent localVariableState = iota
	localVariableKnown
	localVariableUnknown
)

func newVariableEnvironment() variableEnvironment {
	return variableEnvironment{bindings: make(map[string]variableBinding)}
}

func (environment *variableEnvironment) nextGeneration() int {
	environment.generation++
	return environment.generation
}

func (environment *variableEnvironment) setKnown(name, value string) {
	environment.bindings[name] = variableBinding{value: value, known: true, generation: environment.nextGeneration()}
}

func (environment *variableEnvironment) setUnknown(name string) {
	environment.bindings[name] = variableBinding{generation: environment.nextGeneration()}
}

func (environment *variableEnvironment) setAllUnknown() {
	environment.dynamicUnknownFloor = environment.nextGeneration()
}

func (environment variableEnvironment) resolve(name string) (string, localVariableState) {
	binding, found := environment.bindings[name]
	if found && binding.generation > environment.dynamicUnknownFloor {
		if binding.known {
			return binding.value, localVariableKnown
		}
		return "", localVariableUnknown
	}
	if environment.dynamicUnknownFloor != 0 {
		return "", localVariableUnknown
	}
	return "", localVariableAbsent
}

// recordSetAssignment updates the call-site environment only for a definite,
// supported top-level set(NAME value). Unknown guards, unsupported scopes, and
// unsupported assignment shapes become explicit unknown state instead of a
// guessed value. A dynamically-computed assignment target taints all older
// bindings because it may have overwritten any of them.
func recordSetAssignment(environment *variableEnvironment, state guardState, unsupportedScope bool, args string) {
	if !state.active {
		return
	}
	tokens := tokenize(args)
	if len(tokens) == 0 || tokens[0].Raw || tokens[0].Text == "" || strings.Contains(tokens[0].Text, "$") {
		environment.setAllUnknown()
		return
	}
	name := tokens[0].Text
	if state.unknown != "" || unsupportedScope || len(tokens) != 2 || tokens[1].Raw {
		environment.setUnknown(name)
		return
	}
	environment.setKnown(name, tokens[1].Text)
}

// recordVariableInvalidation covers CMake commands that can change an
// existing local binding without using set(). The lexical parser does not
// execute list operations or model cache/parent scopes, so the only sound
// postcondition is unknown: an older value must never remain authoritative.
func recordVariableInvalidation(environment *variableEnvironment, state guardState, args string, destination int) {
	if !state.active {
		return
	}
	tokens := tokenize(args)
	if destination < 0 {
		destination += len(tokens)
	}
	if destination < 0 || destination >= len(tokens) || tokens[destination].Raw ||
		tokens[destination].Text == "" || strings.Contains(tokens[destination].Text, "$") {
		environment.setAllUnknown()
		return
	}
	environment.setUnknown(tokens[destination].Text)
}

func recordListMutation(environment *variableEnvironment, state guardState, args string) {
	tokens := tokenize(args)
	if len(tokens) < 2 || tokens[0].Raw {
		if state.active {
			environment.setAllUnknown()
		}
		return
	}
	switch strings.ToUpper(tokens[0].Text) {
	case "LENGTH", "GET", "JOIN", "SUBLIST", "FIND":
		recordVariableInvalidation(environment, state, args, -1)
	default:
		recordVariableInvalidation(environment, state, args, 1)
	}
}

type unsupportedScope struct {
	opener string
	name   string
}

func closeUnsupportedScope(scopes []unsupportedScope, opener string) ([]unsupportedScope, bool) {
	if len(scopes) == 0 || scopes[len(scopes)-1].opener != opener {
		return scopes, false
	}
	return scopes[:len(scopes)-1], true
}

// expandVariables is the SINGLE OWNER of ${NAME} substitution for every
// variable-bearing portfile field (REF, HEAD_REF, REPO, URL, GITLAB_URL).
// They previously each went through the anchored whole-token regex, so the
// same wrong-negative defect existed on every one of them; routing them all
// through here is what makes the fix cover the class rather than one field.
//
// Resolution order per variable, most specific first:
//  1. a call-site local binding in the SAME portfile,
//  2. ${VERSION} from the sibling vcpkg.json,
//  3. ${PORT} from the port directory's own name.
//
// It fails CLOSED and TOTALLY: if any single variable cannot be resolved,
// ok is false and unresolved names it. A partially-expanded string is never
// returned, because a partially-expanded string compared against a remote is
// exactly how a wrong negative is manufactured.
func expandVariables(environment variableEnvironment, manifest []byte, portName, text string) (expanded string, source RefValueSource, unresolved string, ok bool) {
	if !strings.Contains(text, "${") {
		return text, "", "", true
	}
	sources := map[RefValueSource]bool{}
	expanded = embeddedVarRE.ReplaceAllStringFunc(text, func(match string) string {
		if unresolved != "" {
			return match
		}
		name := embeddedVarRE.FindStringSubmatch(match)[1]
		value, src, resolved := resolveRefVariable(environment, manifest, portName, name)
		if !resolved {
			unresolved = name
			return match
		}
		sources[src] = true
		return value
	})
	if unresolved != "" {
		return "", "", unresolved, false
	}
	// A substituted value may itself contain a variable reference (e.g.
	// set(A "${B}")). One pass is deliberate — refusing is honest, whereas
	// looping risks a cycle and returning the half-expanded text would be
	// the very defect this function exists to prevent.
	if strings.Contains(expanded, "${") {
		if m := embeddedVarRE.FindStringSubmatch(expanded); m != nil {
			return "", "", m[1], false
		}
		return "", "", "nested_expansion", false
	}
	return expanded, singleSource(sources), "", true
}

// singleSource reports the one source every substitution came from, or
// RefValueSourceMixed when a token drew on several. It is never left empty
// for a successful expansion — an empty ResolvedFrom means "not resolved".
func singleSource(sources map[RefValueSource]bool) RefValueSource {
	switch len(sources) {
	case 0:
		return ""
	case 1:
		for s := range sources {
			return s
		}
	}
	return RefValueSourceMixed
}

func resolveRefVariable(environment variableEnvironment, manifest []byte, portName, name string) (string, RefValueSource, bool) {
	if value, state := environment.resolve(name); state != localVariableAbsent {
		if state == localVariableKnown {
			return value, RefValueSourceLocalSet, true
		}
		return "", "", false
	}
	switch name {
	case "VERSION":
		if value := manifestVersion(manifest); value != "" {
			return value, RefValueSourceManifest, true
		}
	case "PORT":
		if portName != "" {
			return portName, RefValueSourcePortName, true
		}
	}
	return "", "", false
}

func resolveMaybeVariable(environment variableEnvironment, manifest []byte, portName string, raw argValue) (value string, wasVar, ok bool) {
	text := strings.TrimSpace(raw.Text)
	if text == "" {
		return "", false, false
	}
	if raw.Raw || !strings.Contains(text, "${") {
		return text, false, true
	}
	expanded, _, _, expandedOK := expandVariables(environment, manifest, portName, text)
	return expanded, true, expandedOK
}

func manifestVersion(data []byte) string {
	var manifest versionManifest
	if len(data) == 0 || json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	for _, value := range []string{manifest.Version, manifest.VersionString, manifest.VersionDate, manifest.VersionSemver} {
		if value != "" {
			return value
		}
	}
	return ""
}

func buildPin(environment variableEnvironment, manifest []byte, portName string, ref argValue) Pin {
	refRaw := strings.TrimSpace(ref.Text)
	if refRaw == "" {
		return Pin{Shape: RefShapeNone}
	}
	// A bracket argument ([[...]]) is uninterpreted CMake text, so ${...}
	// inside it is literal and must NOT be expanded.
	if !ref.Raw && strings.Contains(refRaw, "${") {
		pin := Pin{Ref: refRaw, Shape: RefShapeVariableResolved}
		expanded, source, unresolved, ok := expandVariables(environment, manifest, portName, refRaw)
		if !ok {
			pin.UnresolvedVariable = unresolved
			return pin
		}
		pin.ResolvedRef, pin.ResolvedFrom = expanded, source
		return pin
	}
	if isCommitHex(refRaw) {
		return Pin{Ref: refRaw, Shape: RefShapeCommit40Hex, Literal: ref.Raw}
	}
	if isAbbreviatedCommitHex(refRaw) {
		// Named as the commit it is, NOT demoted to tag/branch. The comparison
		// path still refuses it (see RefShapeCommitAbbrev): ls-remote
		// advertises only full SHAs, so an abbreviation cannot be matched
		// against a tip without resolving it server-side, which this package
		// never does.
		return Pin{Ref: refRaw, Shape: RefShapeCommitAbbrev, Literal: ref.Raw}
	}
	if looksLikeTag(refRaw) {
		return Pin{Ref: refRaw, Shape: RefShapeTag, Literal: ref.Raw}
	}
	return Pin{Ref: refRaw, Shape: RefShapeBranch, Literal: ref.Raw}
}

func parseFetchCandidate(name string, environment variableEnvironment, manifest []byte, portName, args string) FetchCandidate {
	kv := extractKeyedArgValues(tokenize(args))
	headRef, headRefWasVariable, headRefResolved := resolveMaybeVariable(environment, manifest, portName, kv["HEAD_REF"])
	candidate := FetchCandidate{HeadRef: headRef, BindsSourcePath: kv["OUT_SOURCE_PATH"].Text == "SOURCE_PATH"}
	if headRefWasVariable && !headRefResolved {
		candidate.UnresolvedHeadRefVariable = unresolvedVariableName(kv["HEAD_REF"])
	}
	switch name {
	case "vcpkg_download_distfile":
		candidate.Remote = Remote{Kind: RemoteDistfile}
	case "vcpkg_from_github":
		repo, _, _ := resolveMaybeVariable(environment, manifest, portName, kv["REPO"])
		candidate.Remote = Remote{Kind: RemoteGitHub, Repo: repo}
		if repo != "" {
			candidate.Remote.URL = "https://github.com/" + repo + ".git"
		}
		candidate.Pin = buildPin(environment, manifest, portName, kv["REF"])
	case "vcpkg_from_git":
		url, _, _ := resolveMaybeVariable(environment, manifest, portName, kv["URL"])
		candidate.Remote = Remote{Kind: RemoteGit, URL: url}
		candidate.Pin = buildPin(environment, manifest, portName, kv["REF"])
	case "vcpkg_from_gitlab":
		gitlabURL := "https://gitlab.com"
		if strings.TrimSpace(kv["GITLAB_URL"].Text) != "" {
			if value, _, ok := resolveMaybeVariable(environment, manifest, portName, kv["GITLAB_URL"]); ok {
				gitlabURL = value
			} else {
				gitlabURL = ""
			}
		}
		repo, _, _ := resolveMaybeVariable(environment, manifest, portName, kv["REPO"])
		candidate.Remote = Remote{Kind: RemoteGitLab, Repo: repo}
		if repo != "" && gitlabURL != "" {
			candidate.Remote.URL = strings.TrimRight(gitlabURL, "/") + "/" + repo + ".git"
		}
		candidate.Pin = buildPin(environment, manifest, portName, kv["REF"])
	}
	return candidate
}

func unresolvedVariableName(raw argValue) string {
	if raw.Raw {
		return ""
	}
	m := embeddedVarRE.FindStringSubmatch(strings.TrimSpace(raw.Text))
	if m == nil {
		return ""
	}
	return m[1]
}

type guardState struct {
	active  bool
	unknown string
	text    string
}
type conditionFrame struct {
	parent       guardState
	conditions   []string
	priorTrue    bool
	priorUnknown string
	elseSeen     bool
}

func defaultCondition(tokens []token) (truth bool, unresolved string) {
	if len(tokens) == 0 {
		return false, "condition"
	}
	not := false
	if strings.EqualFold(tokens[0].Text, "NOT") {
		not, tokens = true, tokens[1:]
	}
	if len(tokens) != 1 || tokens[0].Raw {
		return false, "condition"
	}
	value := tokens[0].Text
	var known bool
	switch strings.ToUpper(value) {
	case "VCPKG_USE_HEAD_VERSION":
		known, truth = true, false
	case "ON", "TRUE", "YES", "Y", "1":
		known, truth = true, true
	case "OFF", "FALSE", "NO", "N", "0", "", "IGNORE", "NOTFOUND":
		known, truth = true, false
	}
	if !known {
		return false, value
	}
	if not {
		truth = !truth
	}
	return truth, ""
}

func conditionText(tokens []token) string {
	parts := make([]string, len(tokens))
	for i := range tokens {
		parts[i] = tokens[i].Text
	}
	return strings.Join(parts, " ")
}

func negatedConditions(conditions []string) string {
	parts := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		parts = append(parts, "NOT ("+condition+")")
	}
	return strings.Join(parts, " AND ")
}

func joinGuards(parent, branch string) string {
	if parent == "" {
		return branch
	}
	if branch == "" {
		return parent
	}
	return parent + " AND " + branch
}

func guardedState(parent guardState, active bool, unresolved, branch string) guardState {
	text := joinGuards(parent.text, branch)
	if !parent.active || !active {
		return guardState{text: text}
	}
	if parent.unknown != "" {
		unresolved = parent.unknown
	}
	return guardState{active: true, unknown: unresolved, text: text}
}

// parsePortfile preserves the original internal helper surface. Production
// passes sibling manifest bytes through parsePortfileWithManifest.
func parsePortfile(content string) (parsedPortfile, bool) {
	return parsePortfileWithManifest(content, nil, "")
}

func parsePortfileWithManifest(content string, manifest []byte, portName string) (parsedPortfile, bool) {
	statements, ok := splitStatementsChecked(content)
	if !ok {
		return parsedPortfile{}, false
	}
	state := guardState{active: true}
	variables := newVariableEnvironment()
	var stack []conditionFrame
	var unsupportedScopes []unsupportedScope
	declaredCommands := map[string]struct{}{}
	var candidates []FetchCandidate
	var viableCandidates []FetchCandidate
	retainedCandidateBytes := 0
	var unresolved string
statementLoop:
	for _, st := range statements {
		// st.Name is already lower-cased by splitStatementsChecked, so this
		// switch, the fetchFuncNames lookup, and parseFetchCandidate's own
		// dispatch are all case-correct without re-folding at each site.
		switch st.Name {
		case "if":
			tokens := tokenize(st.Args)
			condition := conditionText(tokens)
			truth, unknown := defaultCondition(tokens)
			stack = append(stack, conditionFrame{parent: state, conditions: []string{condition}, priorTrue: unknown == "" && truth, priorUnknown: unknown})
			state = guardedState(state, unknown != "" || truth, unknown, condition)
		case "elseif":
			if len(stack) == 0 {
				return parsedPortfile{}, false
			}
			frame := &stack[len(stack)-1]
			if frame.elseSeen {
				return parsedPortfile{}, false
			}
			tokens := tokenize(st.Args)
			condition := conditionText(tokens)
			truth, unknown := defaultCondition(tokens)
			branch := negatedConditions(frame.conditions)
			if branch != "" {
				branch += " AND "
			}
			branch += "(" + condition + ")"
			state = guardedState(frame.parent, !frame.priorTrue && (unknown != "" || truth), firstNonEmpty(frame.priorUnknown, unknown), branch)
			frame.conditions = append(frame.conditions, condition)
			if unknown == "" && truth {
				frame.priorTrue = true
			}
			if frame.priorUnknown == "" && unknown != "" {
				frame.priorUnknown = unknown
			}
		case "else":
			if len(stack) == 0 {
				return parsedPortfile{}, false
			}
			frame := &stack[len(stack)-1]
			if frame.elseSeen {
				return parsedPortfile{}, false
			}
			frame.elseSeen = true
			state = guardedState(frame.parent, !frame.priorTrue, frame.priorUnknown, negatedConditions(frame.conditions))
		case "endif":
			if len(stack) == 0 {
				return parsedPortfile{}, false
			}
			state = stack[len(stack)-1].parent
			stack = stack[:len(stack)-1]
		case "foreach", "while":
			unsupportedScopes = append(unsupportedScopes, unsupportedScope{opener: st.Name})
		case "function", "macro":
			name := ""
			if tokens := tokenize(st.Args); len(tokens) != 0 && !tokens[0].Raw {
				name = strings.ToLower(tokens[0].Text)
				if name != "" {
					declaredCommands[name] = struct{}{}
				}
			}
			unsupportedScopes = append(unsupportedScopes, unsupportedScope{opener: st.Name, name: name})
		case "endforeach":
			var closed bool
			unsupportedScopes, closed = closeUnsupportedScope(unsupportedScopes, "foreach")
			if !closed {
				return parsedPortfile{}, false
			}
		case "endwhile":
			var closed bool
			unsupportedScopes, closed = closeUnsupportedScope(unsupportedScopes, "while")
			if !closed {
				return parsedPortfile{}, false
			}
		case "endfunction":
			var closed bool
			unsupportedScopes, closed = closeUnsupportedScope(unsupportedScopes, "function")
			if !closed {
				return parsedPortfile{}, false
			}
		case "endmacro":
			var closed bool
			unsupportedScopes, closed = closeUnsupportedScope(unsupportedScopes, "macro")
			if !closed {
				return parsedPortfile{}, false
			}
		case "set":
			recordSetAssignment(&variables, state, len(unsupportedScopes) != 0, st.Args)
		case "unset":
			recordVariableInvalidation(&variables, state, st.Args, 0)
		case "list":
			recordListMutation(&variables, state, st.Args)
		case "return":
			if len(unsupportedScopes) != 0 || !state.active {
				continue
			}
			if state.unknown != "" {
				return parsedPortfile{}, false
			}
			stack = nil
			break statementLoop
		default:
			if !fetchFuncNames[st.Name] {
				_, declared := declaredCommands[st.Name]
				indirectSource := st.Name == "include" || st.Name == "add_subdirectory" || st.Name == "cmake_language"
				if len(unsupportedScopes) == 0 && state.active && (declared || indirectSource) {
					// Static selection cannot execute declarations or load more CMake.
					return parsedPortfile{}, false
				}
				continue
			}
			if len(unsupportedScopes) != 0 {
				continue
			}
			candidate := parseFetchCandidate(st.Name, variables, manifest, portName, st.Args)
			candidate.GuardVariable = state.unknown
			candidate.Guard = state.text
			candidate.ActiveForDefault = state.active
			candidateBytes := retainedFetchCandidateBytes(candidate)
			if len(candidates) >= MaxFetchCandidatesPerPort ||
				candidateBytes > MaxRetainedFetchCandidateBytesPerPort-retainedCandidateBytes {
				return parsedPortfile{Candidates: candidates, CandidateLimitExceeded: true}, true
			}
			candidates = append(candidates, candidate)
			retainedCandidateBytes += candidateBytes
			if !state.active {
				continue
			}
			viableCandidates = append(viableCandidates, candidate)
			if state.unknown != "" {
				if unresolved == "" {
					unresolved = state.unknown
				}
			}
		}
	}
	if len(stack) != 0 || len(unsupportedScopes) != 0 {
		return parsedPortfile{}, false
	}
	if len(candidates) == 0 {
		return parsedPortfile{Remote: Remote{Kind: RemoteNone}}, true
	}
	if len(viableCandidates) == 0 {
		return parsedPortfile{Remote: Remote{Kind: RemoteNone}, Candidates: candidates}, true
	}
	if unresolved != "" {
		return parsedPortfile{Candidates: candidates, UnresolvedGuardVariable: unresolved}, true
	}
	if len(viableCandidates) > 1 {
		bound := -1
		for i := range viableCandidates {
			if viableCandidates[i].BindsSourcePath {
				if bound >= 0 {
					return parsedPortfile{Candidates: candidates, MultipleFetchCalls: true}, true
				}
				bound = i
			}
		}
		if bound < 0 {
			return parsedPortfile{Candidates: candidates, MultipleFetchCalls: true}, true
		}
		selected := viableCandidates[bound]
		return parsedPortfile{Remote: selected.Remote, Pin: selected.Pin, HeadRef: selected.HeadRef, UnresolvedHeadRefVariable: selected.UnresolvedHeadRefVariable, Candidates: candidates}, true
	}
	selected := viableCandidates[0]
	return parsedPortfile{Remote: selected.Remote, Pin: selected.Pin, HeadRef: selected.HeadRef, UnresolvedHeadRefVariable: selected.UnresolvedHeadRefVariable, Candidates: candidates}, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
