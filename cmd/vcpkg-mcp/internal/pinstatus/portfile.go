package pinstatus

import (
	"regexp"
	"strings"
)

// parsedPortfile is the internal result of textually scanning one
// portfile.cmake for its source-acquisition call. This package NEVER
// executes CMake (per the design doc ban on running untrusted build
// scripts to answer a read-only question) — every field here comes from a
// best-effort text scan, never evaluation.
type parsedPortfile struct {
	Remote Remote
	Pin    Pin
	// HeadRef is vcpkg_from_github/vcpkg_from_git/vcpkg_from_gitlab's own
	// optional HEAD_REF parameter — real vcpkg syntax whose documented
	// purpose IS exactly this "is this port up to date" check: it names
	// which branch to track for currency, overriding the remote's default
	// branch. Empty when not supplied (or supplied as an unresolvable
	// ${VARIABLE} — HEAD_REF is enrichment-only, so a failed resolution
	// degrades to the default-branch fallback rather than failing the
	// whole call).
	HeadRef string
}

// fetchFuncNames are the vcpkg source-acquisition helpers this package
// recognizes, in no particular priority — findFetchCall picks whichever
// occurs FIRST (leftmost) in the file. vcpkg_from_git is a strict textual
// prefix of vcpkg_from_github/vcpkg_from_gitlab, so every lookup requires
// the opening '(' (optionally preceded by whitespace) to immediately follow
// the name — "vcpkg_from_github(" never matches the vcpkg_from_git pattern
// because 'h' follows, not '(' or whitespace.
var fetchFuncNames = []string{
	"vcpkg_from_github",
	"vcpkg_from_git",
	"vcpkg_from_gitlab",
	"vcpkg_download_distfile",
}

// knownArgKeys are the CMake keyword arguments this package extracts from a
// fetch call's argument block. Multi-value keys (PATCHES, URLS, SHA512) are
// recognized only so the tokenizer can find the NEXT known key correctly;
// this package never uses their values.
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
	variableRefRE = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
	commitHexRE   = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	// tagLikeRE is a heuristic ONLY — informational classification of a
	// literal (non-variable, non-commit) REF as "looks like a version tag".
	// It never drives the ok/unknown verdict, which always keys off
	// isCommitHex(effectiveRef) or a live existence check, never this
	// label. vcpkg portfiles cannot syntactically distinguish a tag name
	// from a branch name, so this is best-effort metadata, not a claim.
	tagLikeRE = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+){1,3}`)
)

// isCommitHex reports whether ref is a literal 40-hex-character commit SHA
// — the only shape this package compares against a remote tip with
// confidence.
func isCommitHex(ref string) bool { return commitHexRE.MatchString(ref) }

// looksLikeTag is the heuristic described on tagLikeRE.
func looksLikeTag(ref string) bool { return tagLikeRE.MatchString(ref) }

// stripComments removes '#'-to-end-of-line CMake comments outside quoted
// strings, one line at a time, so neither the fetch-call finder nor the
// set() variable scanner is confused by a commented-out example.
func stripComments(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		inQuotes := false
		for j := 0; j < len(line); j++ {
			switch line[j] {
			case '"':
				inQuotes = !inQuotes
			case '#':
				if !inQuotes {
					lines[i] = line[:j]
					j = len(line) // break out of the inner loop
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// findFetchCall locates the first (leftmost) recognized fetch call in
// content and returns its function name plus the raw text between its
// balanced parentheses. ok is false only when a recognized function NAME
// was found but its parentheses never balance before EOF — a genuinely
// malformed file this package's text-only parser cannot make sense of.
func findFetchCall(content string) (name, argsBlock string, ok bool) {
	bestIdx := -1
	bestName := ""
	for _, n := range fetchFuncNames {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\s*\(`)
		loc := re.FindStringIndex(content)
		if loc == nil {
			continue
		}
		if bestIdx == -1 || loc[0] < bestIdx {
			bestIdx = loc[0]
			bestName = n
		}
	}
	if bestIdx == -1 {
		// No recognized call at all — a legitimate metapackage/provider
		// shape, reported by the caller as RemoteNone, not a parse failure.
		return "", "", true
	}

	parenOffset := strings.Index(content[bestIdx:], "(")
	if parenOffset == -1 {
		return bestName, "", false
	}
	start := bestIdx + parenOffset + 1
	depth := 1
	for i := start; i < len(content); i++ {
		switch content[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return bestName, content[start:i], true
			}
		}
	}
	// Reached EOF without the parentheses ever balancing.
	return bestName, "", false
}

// tokenizeArgs splits a fetch call's argument block on whitespace, treating
// a double-quoted span (no escape handling needed for vcpkg's own portfile
// conventions) as one token with the quotes stripped.
func tokenizeArgs(block string) []string {
	var tokens []string
	var cur strings.Builder
	inQuotes := false
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(block); i++ {
		c := block[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case !inQuotes && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return tokens
}

// extractKeyedArgs walks tokens picking out KEY VALUE pairs for every key
// in knownArgKeys. Only the first value token following a key is kept
// (sufficient for every field this package reads — REPO/REF/URL/
// GITLAB_URL/HEAD_REF/FETCH_REF are all single-value vcpkg parameters).
func extractKeyedArgs(tokens []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(tokens); i++ {
		key := tokens[i]
		if !knownArgKeys[key] {
			continue
		}
		if i+1 < len(tokens) && !knownArgKeys[tokens[i+1]] {
			out[key] = tokens[i+1]
			i++
		} else {
			out[key] = ""
		}
	}
	return out
}

// resolveSetVariable searches content for a `set(<varName> <value>)` call
// (optionally quoted value) and returns its literal value. This is a text
// scan, never CMake evaluation — it deliberately does not handle nested
// variable expansion, generator expressions, or conditional set() calls,
// matching the design doc's ban on executing untrusted build scripts.
func resolveSetVariable(content, varName string) (string, bool) {
	re := regexp.MustCompile(`(?m)\bset\(\s*` + regexp.QuoteMeta(varName) + `\s+"?([^")\s]+)"?\s*\)`)
	m := re.FindStringSubmatch(content)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// resolveMaybeVariable resolves raw (a keyed-arg value) when it is a
// ${VARIABLE} reference, via resolveSetVariable against content. wasVar
// reports whether raw was syntactically a variable reference at all; ok
// reports whether a usable literal value was ultimately obtained (false for
// both an empty/absent raw value and a variable that failed to resolve).
func resolveMaybeVariable(content, raw string) (value string, wasVar, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, false
	}
	if m := variableRefRE.FindStringSubmatch(raw); m != nil {
		val, found := resolveSetVariable(content, m[1])
		return val, true, found
	}
	return raw, false, true
}

// buildPin classifies refRaw (the fetch call's REF argument value) into a
// Pin, resolving a ${VARIABLE} reference via a local set() when needed.
func buildPin(content, refRaw string) Pin {
	refRaw = strings.TrimSpace(refRaw)
	if refRaw == "" {
		return Pin{Shape: RefShapeNone}
	}
	if m := variableRefRE.FindStringSubmatch(refRaw); m != nil {
		pin := Pin{Ref: refRaw, Shape: RefShapeVariableResolved}
		if val, ok := resolveSetVariable(content, m[1]); ok {
			pin.ResolvedRef = val
		}
		return pin
	}
	if isCommitHex(refRaw) {
		return Pin{Ref: refRaw, Shape: RefShapeCommit40Hex}
	}
	if looksLikeTag(refRaw) {
		return Pin{Ref: refRaw, Shape: RefShapeTag}
	}
	return Pin{Ref: refRaw, Shape: RefShapeBranch}
}

// parsePortfile textually scans data (a portfile.cmake's content) for one
// recognized fetch call. ok is false only for a genuinely malformed file
// (a recognized function name whose parentheses never balance) — every
// other outcome, including "no fetch call found at all" (RemoteNone), is a
// valid parse.
func parsePortfile(content string) (parsedPortfile, bool) {
	clean := stripComments(content)
	funcName, block, ok := findFetchCall(clean)
	if !ok {
		return parsedPortfile{}, false
	}
	if funcName == "" {
		return parsedPortfile{Remote: Remote{Kind: RemoteNone}}, true
	}

	tokens := tokenizeArgs(block)
	kv := extractKeyedArgs(tokens)
	headRef, _, _ := resolveMaybeVariable(clean, kv["HEAD_REF"])

	switch funcName {
	case "vcpkg_download_distfile":
		return parsedPortfile{Remote: Remote{Kind: RemoteDistfile}}, true

	case "vcpkg_from_github":
		repo, _, _ := resolveMaybeVariable(clean, kv["REPO"])
		remote := Remote{Kind: RemoteGitHub, Repo: repo}
		if repo != "" {
			remote.URL = "https://github.com/" + repo + ".git"
		}
		return parsedPortfile{
			Remote:  remote,
			Pin:     buildPin(clean, kv["REF"]),
			HeadRef: headRef,
		}, true

	case "vcpkg_from_git":
		url, _, _ := resolveMaybeVariable(clean, kv["URL"])
		return parsedPortfile{
			Remote:  Remote{Kind: RemoteGit, URL: url},
			Pin:     buildPin(clean, kv["REF"]),
			HeadRef: headRef,
		}, true

	case "vcpkg_from_gitlab":
		// GITLAB_URL defaults to vcpkg's own documented "https://gitlab.com"
		// ONLY when the field is genuinely absent. When it IS present but a
		// ${VARIABLE} reference that fails to resolve, that is a real
		// resolution failure, not "absent" — defaulting past it would
		// silently point the query at the wrong host, so gitlabURL is left
		// empty instead, which propagates to an empty Remote.URL below and
		// is reported as portfile_unparsable by the caller.
		var gitlabURL string
		if strings.TrimSpace(kv["GITLAB_URL"]) == "" {
			gitlabURL = "https://gitlab.com"
		} else if val, _, ok := resolveMaybeVariable(clean, kv["GITLAB_URL"]); ok {
			gitlabURL = val
		}
		repo, _, _ := resolveMaybeVariable(clean, kv["REPO"])
		remote := Remote{Kind: RemoteGitLab, Repo: repo}
		if repo != "" && gitlabURL != "" {
			remote.URL = strings.TrimRight(gitlabURL, "/") + "/" + repo + ".git"
		}
		return parsedPortfile{
			Remote:  remote,
			Pin:     buildPin(clean, kv["REF"]),
			HeadRef: headRef,
		}, true
	}

	// Unreachable: funcName always matches one of fetchFuncNames, which is
	// exactly the switch's case set.
	return parsedPortfile{}, false
}
