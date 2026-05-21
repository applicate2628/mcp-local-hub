package api

import "strings"

// lspEntryPrefix is the canonical scheduler-task / client-config entry-name
// prefix written by `mcphub register` for LSP-bridge language servers.
// See internal/api/register.go ResolveEntryName for the producer side.
const lspEntryPrefix = "mcp-language-server-"

// ParseEntryName reverses the entry-name shape written by ResolveEntryName
// (mcp-language-server-<lang>(-<suffix>)?) back into its (language, suffix)
// components. It is used by the Servers matrix LSP recognition algorithm
// (Task 3.3) to classify scheduler tasks discovered at runtime.
//
// The langs slice is the authoritative set of known languages from the
// loaded manifest. ParseEntryName performs LONGEST-PREFIX matching so that
// hyphenated language names (e.g. "vscode-html", "vscode-css") are not
// mis-parsed as their shorter prefixes. Naive splitting on "-" would
// classify "mcp-language-server-vscode-html" as (vscode, html) — wrong.
//
// Returns:
//   - (lang, "")               for a plain base entry (no disambiguating suffix).
//   - (lang, "<short|full>")   for a disambiguated entry. The suffix is whatever
//     remains after the matched language; the caller decides whether the suffix
//     length is meaningful (workspaceKey[:4] vs full workspaceKey).
//   - ("", "")                 when entryName lacks the LSP prefix or its tail does
//     not match any candidate in langs.
func ParseEntryName(entryName string, langs []string) (lang, suffix string) {
	rest, ok := strings.CutPrefix(entryName, lspEntryPrefix)
	if !ok || rest == "" {
		return "", ""
	}

	// Longest-prefix match: scan every candidate, keep the longest that
	// either equals rest (no suffix) or is followed by '-' (suffix path).
	bestLang := ""
	bestSuffix := ""
	bestLen := -1
	for _, cand := range langs {
		if cand == "" {
			continue
		}
		switch {
		case rest == cand:
			if len(cand) > bestLen {
				bestLang = cand
				bestSuffix = ""
				bestLen = len(cand)
			}
		case strings.HasPrefix(rest, cand+"-"):
			if len(cand) > bestLen {
				bestLang = cand
				bestSuffix = rest[len(cand)+1:]
				bestLen = len(cand)
			}
		}
	}

	if bestLen < 0 {
		return "", ""
	}
	return bestLang, bestSuffix
}
