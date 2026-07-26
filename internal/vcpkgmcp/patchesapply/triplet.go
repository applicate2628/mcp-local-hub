package patchesapply

import (
	"path/filepath"
	"regexp"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// This file establishes the triplet variables a portfile's if() guards are
// evaluated against, by READING THE TRIPLET FILE — replacing the previous
// implementation, which derived them from the triplet NAME.
//
// Why the name-derived version had to go: a triplet is a CMake file, and its
// name is a convention, not a contract. A custom overlay triplet
//
//	corp-windows.cmake:
//	  set(VCPKG_LIBRARY_LINKAGE static)
//
// contains no "static" component in its name, so the name-derived model
// asserted VCPKG_LIBRARY_LINKAGE=dynamic — a fact invented out of a string
// match. Every `if(VCPKG_LIBRARY_LINKAGE STREQUAL "static")` guard then
// evaluated FALSE, and static-only patches were confidently reported as "not
// applied". The same applied to the operator's own real triplets (`cl`,
// `wingpl`), whose names contain none of the recognised components at all,
// so every target-platform variable was asserted OFF.
//
// The replacement rule is: a triplet variable is known if and only if an
// actual triplet file establishes it unconditionally. Anything else stays
// unresolved and flows into the undecidable bucket — the mechanism this
// package already has for "cannot be decided", used instead of a guess.

// tripletNameRE bounds a triplet name to ONE safe path segment before it is
// joined into a lookup path. Same class of guard as the port-name rule in the
// lastfailure package: `filepath.Join(dir, triplet+".cmake")` would otherwise
// normalise a `..`-bearing name into a read outside the roots the caller
// granted. A name that fails this check simply yields no triplet file (every
// variable stays unresolved) rather than a hard error — the honest degraded
// answer, not a new rejection path.
var tripletNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// tripletFileCandidates returns the ordered lookup path for a triplet file,
// mirroring vcpkg's own resolution: every --overlay-triplets root in
// precedence order first, then the builtin <root>/triplets and its
// community/ subdirectory.
//
// Roots come only from the caller's own parameters (overlay_triplets,
// vcpkg_root) — never from ambient machine layout.
func tripletFileCandidates(triplet string, overlayTriplets []string, vcpkgRoot string) []string {
	if !tripletNameRE.MatchString(triplet) {
		return nil
	}
	file := triplet + ".cmake"
	var out []string
	for _, dir := range overlayTriplets {
		dir = strings.TrimSpace(dir)
		// A relative overlay root would bind to the hub daemon's working
		// directory, so it can only produce a confident answer about an
		// unrelated tree. Skipped rather than guessed.
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		out = append(out, filepath.Join(dir, file))
	}
	if root := strings.TrimSpace(vcpkgRoot); root != "" && filepath.IsAbs(root) {
		out = append(out,
			filepath.Join(root, "triplets", file),
			filepath.Join(root, "triplets", "community", file),
		)
	}
	return out
}

// resolveTripletFile finds the triplet file that governs this evaluation.
//
// Returns PresenceExists with the winning path; PresenceAbsent when no
// candidate exists (or none could be formed); PresenceUnreadable — with the
// path that refused to be read — when a candidate could not be probed. The
// unreadable case is fail-closed on purpose: an unreadable higher-precedence
// candidate might be the file that actually governs, so silently continuing
// to a lower-precedence one could report facts from a file vcpkg would not
// have used.
func resolveTripletFile(deps Deps, triplet string, overlayTriplets []string, vcpkgRoot string) (string, evidence.Presence, error) {
	for _, cand := range tripletFileCandidates(triplet, overlayTriplets, vcpkgRoot) {
		p, err := evidence.ProbeFile(evidence.StatFunc(deps.Stat), cand)
		switch p {
		case evidence.PresenceExists:
			return cand, evidence.PresenceExists, nil
		case evidence.PresenceUnreadable:
			return cand, evidence.PresenceUnreadable, err
		}
	}
	return "", evidence.PresenceAbsent, nil
}

// parseTripletFacts extracts the variables a triplet file establishes
// UNCONDITIONALLY, reusing this package's existing CMake statement splitter
// and tokenizer rather than introducing a second parser.
//
// Two deliberate restrictions keep every returned fact true:
//
//   - Only top-level set() calls count. Triplet files routinely carry
//     port-specific overrides such as `if(PORT MATCHES "qt") set(...)
//     endif()`; whether such a branch executes depends on state this static
//     evaluation does not have, so a variable it sets is left unresolved
//     rather than asserted for every port.
//   - A value that still contains an unresolved ${VAR} reference after
//     expansion is DROPPED. A half-expanded string is not a fact, and
//     letting it through would re-introduce exactly the invented-value
//     failure this file exists to remove.
func parseTripletFacts(src, portDir, portName, vcpkgRoot string) map[string]string {
	stmts, ok := splitStatementsChecked(src)
	if !ok {
		// A structurally broken triplet file establishes nothing.
		return nil
	}

	facts := map[string]string{}
	env := &varEnv{
		scalars:   facts,
		lists:     map[string][]listItem{},
		vcpkgRoot: vcpkgRoot,
		portName:  portName,
		portDir:   portDir,
	}

	depth := 0
	for _, st := range stmts {
		// CMake command names are case-insensitive (cmake-language(7)).
		switch strings.ToLower(st.Name) {
		case "if", "foreach", "while", "function", "macro":
			depth++
			continue
		case "endif", "endforeach", "endwhile", "endfunction", "endmacro":
			if depth > 0 {
				depth--
			}
			continue
		case "elseif", "else":
			continue
		case "set":
			if depth != 0 {
				continue
			}
		default:
			continue
		}

		toks := tokenize(st.Args)
		if len(toks) == 0 || toks[0].Text == "" {
			continue
		}
		name := toks[0].Text
		var parts []string
		unresolvedSeen := false
		for _, t := range toks[1:] {
			if !t.Quoted && (t.Text == "CACHE" || t.Text == "PARENT_SCOPE" || t.Text == "FORCE") {
				break
			}
			expanded, unresolved := env.expandToken(t)
			if len(unresolved) > 0 {
				unresolvedSeen = true
				break
			}
			parts = append(parts, expanded)
		}
		if unresolvedSeen {
			// Do not let a partially-expanded value masquerade as a fact.
			delete(facts, name)
			continue
		}
		facts[name] = strings.Join(parts, ";")
	}
	return facts
}
