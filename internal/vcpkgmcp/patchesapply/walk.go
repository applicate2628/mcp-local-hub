package patchesapply

import "strings"

// declaredPatch is one patch reference collected while walking the
// portfile, in the exact order it was encountered — accumulation order for
// list(APPEND) items, call-site order for direct keyword-arg literals. That
// order IS vcpkg's own apply order once guard-false/undecidable entries are
// filtered out (see resolve.go).
type declaredPatch struct {
	// raw is the reference exactly as written in the portfile (before ${VAR}
	// expansion) — used as the display Filename in Result.
	raw string
	// expanded is raw after ${VAR}/$ENV{VAR} substitution using the varEnv
	// snapshot active at the point this reference was encountered. It may
	// still contain an unresolved "${NAME}" substring verbatim if expansion
	// failed — resolve.go deliberately does not special-case that: a path
	// resolved from it will simply never Stat() successfully.
	expanded string
	// guard is this entry's Kleene-evaluated applicability for the triplet
	// under evaluation.
	guard Tri
	// guardText is the verbatim, human-readable guard chain (every enclosing
	// if/elseif/else condition, joined); empty means unconditional.
	guardText string
	// unresolvedVars names the variables responsible for guard == Unknown.
	// Populated only in that case.
	unresolvedVars []string
	// pathUnresolved names variables that remain in expanded. They make path
	// existence unknowable even when the entry's control-flow guard is true.
	pathUnresolved []string
}

// ifFrame tracks one open if()/elseif()/else() block while walking.
type ifFrame struct {
	// parentActive is the cumulative active Tri of the ENCLOSING scope (the
	// frame stack below this one), captured when this if() was entered.
	parentActive Tri
	// priorConds/priorTexts/priorUnresolved record every if/elseif
	// condition's OWN (not AND'd with parent) Tri/text/unresolved-vars tried
	// in this frame so far — needed to compute a later elseif's or else's
	// "none of the previous branches fired" condition.
	priorConds      []Tri
	priorTexts      []string
	priorUnresolved [][]string
	// curActive/curText/curUnresolved describe the CURRENTLY open branch:
	// curActive = parentActive AND (this branch's own condition).
	curActive     Tri
	curText       string
	curUnresolved []string
}

// walkPortfile scans src (the full portfile.cmake text) and returns every
// PATCHES entry found, in accumulation/call order; sawPatchesKeyword
// distinguishes "genuinely no PATCHES declared anywhere" (the netgen shape)
// from "declared PATCHES but every branch happened to be guard-false", and
// truncated flags a whole-file structural break (unbalanced parens at EOF)
// that makes every extracted entry untrustworthy.
func walkPortfile(src string, env *varEnv) (entries []declaredPatch, sawPatchesKeyword bool, truncated bool) {
	stmts, ok := splitStatementsChecked(src)
	if !ok {
		return nil, false, true
	}

	var frames []ifFrame
	active := func() Tri {
		if len(frames) == 0 {
			return TriTrue
		}
		return frames[len(frames)-1].curActive
	}
	guardText := func() string {
		var parts []string
		for _, f := range frames {
			if f.curText != "" {
				parts = append(parts, f.curText)
			}
		}
		return strings.Join(parts, " AND ")
	}
	activeUnresolved := func() []string {
		var all []string
		for _, f := range frames {
			all = append(all, f.curUnresolved...)
		}
		return dedupStrings(all)
	}

	for _, st := range stmts {
		switch st.Name {
		case "if":
			cond, unresolved := evalCondition(st.Args, env)
			parent := active()
			frames = append(frames, ifFrame{
				parentActive:    parent,
				priorConds:      []Tri{cond},
				priorTexts:      []string{strings.TrimSpace(st.Args)},
				priorUnresolved: [][]string{unresolved},
				curActive:       kleeneAnd(parent, cond),
				curText:         strings.TrimSpace(st.Args),
				curUnresolved:   unresolved,
			})
		case "elseif":
			if len(frames) == 0 {
				continue // malformed (elseif without if); ignore, degrade gracefully
			}
			f := &frames[len(frames)-1]
			cond, unresolved := evalCondition(st.Args, env)
			notPrior, notPriorUnresolved := notAllPrior(f)
			local := kleeneAnd(notPrior, cond)
			localUnresolved := dedupStrings(append(append([]string{}, notPriorUnresolved...), unresolved...))
			f.priorConds = append(f.priorConds, cond)
			f.priorTexts = append(f.priorTexts, strings.TrimSpace(st.Args))
			f.priorUnresolved = append(f.priorUnresolved, unresolved)
			f.curActive = kleeneAnd(f.parentActive, local)
			f.curText = "NOT(" + strings.Join(f.priorTexts[:len(f.priorTexts)-1], " OR ") + ") AND " + strings.TrimSpace(st.Args)
			f.curUnresolved = localUnresolved
		case "else":
			if len(frames) == 0 {
				continue
			}
			f := &frames[len(frames)-1]
			notPrior, notPriorUnresolved := notAllPrior(f)
			f.curActive = kleeneAnd(f.parentActive, notPrior)
			f.curText = "NOT(" + strings.Join(f.priorTexts, " OR ") + ")"
			f.curUnresolved = notPriorUnresolved
		case "endif":
			if len(frames) > 0 {
				frames = frames[:len(frames)-1]
			}
		case "set":
			handleSet(st.Args, env, active(), guardText(), activeUnresolved())
		case "get_filename_component":
			handleGetFilenameComponent(st.Args, env, active())
		case "list":
			handleListAppend(st.Args, env, active(), guardText(), activeUnresolved())
		default:
			found, items := extractPatchesArg(st.Args, env, active(), guardText(), activeUnresolved())
			if found {
				sawPatchesKeyword = true
				entries = append(entries, items...)
			}
		}
	}
	return entries, sawPatchesKeyword, false
}

// notAllPrior computes NOT(cond1) AND NOT(cond2) AND ... for every branch
// condition tried so far in f, plus the union of the variables responsible
// wherever any of those was Unknown — the condition (and cause) an elseif's
// local guard or an else block's guard depends on.
func notAllPrior(f *ifFrame) (Tri, []string) {
	notPrior := TriTrue
	var unresolved []string
	for i, pc := range f.priorConds {
		notPrior = kleeneAnd(notPrior, kleeneNot(pc))
		unresolved = append(unresolved, f.priorUnresolved[i]...)
	}
	return notPrior, dedupStrings(unresolved)
}

// handleSet implements the one set() shape this package needs: set(VAR
// value...), optionally followed by CACHE/PARENT_SCOPE/FORCE trailing
// keywords which are recognized and ignored (their presence just stops the
// value token scan). If active is definitively False, the branch containing
// this set() would not execute for this triplet, so it is skipped entirely
// — the environment is left exactly as an earlier branch (or no branch)
// left it, matching real CMake control flow.
func handleSet(argsRaw string, env *varEnv, active Tri, guardText string, unresolvedVars []string) {
	if active == TriFalse {
		return
	}
	toks := tokenize(argsRaw)
	if len(toks) == 0 || toks[0].Text == "" {
		return
	}
	name := toks[0].Text
	var parts []string
	var items []listItem
	// The assignment is only as applicable as the branch it sits in, AND as
	// the values it reads: a set() under an undecided if() is uncertain, and so
	// is one whose value splices a variable that was itself assigned under one.
	valueCertainty := TriTrue
	valueUncertainVars := append([]string{}, unresolvedVars...)
	for _, t := range toks[1:] {
		if !t.Quoted && (t.Text == "CACHE" || t.Text == "PARENT_SCOPE" || t.Text == "FORCE") {
			break
		}
		ex := env.expandToken(t)
		valueCertainty = kleeneAnd(valueCertainty, ex.certainty)
		valueUncertainVars = append(valueUncertainVars, ex.uncertainVars...)
		parts = append(parts, ex.text)
		if ex.text != "" {
			items = append(items, listItem{
				text:           ex.text,
				guard:          kleeneAnd(active, ex.certainty),
				guardText:      guardText,
				unresolvedVars: dedupStrings(append(append([]string{}, unresolvedVars...), ex.uncertainVars...)),
				pathUnresolved: ex.unresolved,
			})
		}
	}
	assignedGuard := kleeneAnd(active, valueCertainty)
	// setScalar carries the value AND its guard together. Writing env.scalars
	// directly here is what used to lose the tri-state on the scalar shape
	// while the list items beside it kept theirs.
	env.setScalar(name, strings.Join(parts, ";"), scalarTaint{
		guard:          assignedGuard,
		guardText:      guardText,
		unresolvedVars: dedupStrings(valueUncertainVars),
	})
	// CMake set(VAR value...) establishes a list value, replacing any prior
	// value; later list(APPEND VAR ...) extends this declaration in source
	// order. Keeping the declaration here lets PATCHES ${VAR} expand both.
	env.lists[name] = items
}

// handleGetFilenameComponent implements get_filename_component(<var>
// <input> ABSOLUTE) — the one mode this package's traps require (resolving
// a path that may point outside the port directory, e.g. the licensepp
// shape). Any other mode (DIRECTORY, NAME, EXT, ...) is deliberately left
// unhandled: the variable stays unresolved rather than this package
// guessing an unsupported transform.
func handleGetFilenameComponent(argsRaw string, env *varEnv, active Tri) {
	if active == TriFalse {
		return
	}
	toks := tokenize(argsRaw)
	if len(toks) < 3 {
		return
	}
	name := toks[0].Text
	ex := env.expandToken(toks[1])
	mode := toks[2].Text
	if mode != "ABSOLUTE" {
		return
	}
	// Same tri-state rule as handleSet: the derived path is only as applicable
	// as the branch this call sits in and the variables it read.
	env.setScalar(name, env.getFilenameComponentAbsolute(ex.text), scalarTaint{
		guard:          kleeneAnd(active, ex.certainty),
		unresolvedVars: ex.uncertainVars,
	})
}

// handleListAppend implements list(APPEND <listvar> item...). Only the
// APPEND subcommand matters for PATCHES-style accumulation. Unlike
// handleSet/handleGetFilenameComponent, this does NOT skip when active is
// False: a reference under a false guard is still a REFERENCE (it must
// count toward "is this file mentioned anywhere in the portfile" for
// orphan detection — a patch guarded out for triplet A but real for
// triplet B must never look orphaned when evaluating triplet A). Every
// item is expanded now (so a later ${VAR} reassignment cannot
// retroactively change what was already appended, matching CMake's eager
// set()/list() semantics) and tagged with the guard active AT THIS
// APPEND — not the guard active when the list is later spliced into a
// PATCHES keyword-arg via ${listvar}.
func handleListAppend(argsRaw string, env *varEnv, active Tri, guardText string, unresolvedVars []string) {
	toks := tokenize(argsRaw)
	if len(toks) < 2 || toks[0].Quoted || toks[0].Text != "APPEND" {
		return
	}
	listName := toks[1].Text
	for _, t := range toks[2:] {
		ex := env.expandToken(t)
		env.lists[listName] = append(env.lists[listName], listItem{
			text:           ex.text,
			guard:          kleeneAnd(active, ex.certainty),
			guardText:      guardText,
			unresolvedVars: dedupStrings(append(append([]string{}, unresolvedVars...), ex.uncertainVars...)),
			pathUnresolved: ex.unresolved,
		})
	}
}

// extractPatchesArg scans one call statement's raw argument text for a bare
// PATCHES keyword token. Everything after it, up to the next token that
// looks like a different ALL-CAPS keyword-arg (or end of the call), is
// collected as a declared patch entry — except a token that is EXACTLY
// "${PATCHES}" (a bare list-variable splice), which instead expands to the
// full accumulated list(APPEND PATCHES ...) history (each item keeping ITS
// OWN guard from when it was appended, per handleListAppend above). This is
// what lets the python3-style conditional-accumulation shape and the flat
// literal-list shape share one code path.
func extractPatchesArg(argsRaw string, env *varEnv, active Tri, guardText string, unresolvedVars []string) (found bool, items []declaredPatch) {
	toks := tokenize(argsRaw)
	for i := 0; i < len(toks); i++ {
		if toks[i].Quoted || toks[i].Text != "PATCHES" {
			continue
		}
		found = true
		j := i + 1
		for j < len(toks) {
			t := toks[j]
			if !t.Quoted && looksLikeKeywordArg(t.Text) {
				break
			}
			if !t.Quoted {
				if m := reVarRefFull.FindStringSubmatch(t.Text); m != nil {
					if lst, ok := env.lists[m[1]]; ok {
						for _, li := range lst {
							items = append(items, declaredPatch{
								raw:            li.text,
								expanded:       li.text,
								guard:          li.guard,
								guardText:      li.guardText,
								unresolvedVars: li.unresolvedVars,
								pathUnresolved: li.pathUnresolved,
							})
						}
						j++
						continue
					}
				}
			}
			ex := env.expandToken(t)
			items = append(items, declaredPatch{
				raw:            t.Text,
				expanded:       ex.text,
				guard:          kleeneAnd(active, ex.certainty),
				guardText:      guardText,
				unresolvedVars: dedupStrings(append(append([]string{}, unresolvedVars...), ex.uncertainVars...)),
				pathUnresolved: ex.unresolved,
			})
			j++
		}
		i = j - 1
	}
	return found, items
}

// looksLikeKeywordArg identifies the vcpkg helper keyword names that end a
// PATCHES argument list. Unknown ALL-CAPS barewords remain patch candidates:
// they can be extensionless filenames and must never be silently omitted.
func looksLikeKeywordArg(s string) bool {
	// An unknown ALL-CAPS bareword can be an extensionless filename. Treat it
	// as a PATCHES value rather than silently dropping it; only documented
	// helper keyword names terminate this argument list.
	_, ok := knownKeywordArgs[s]
	return ok
}

var knownKeywordArgs = map[string]struct{}{
	"OUT_SOURCE_PATH":    {},
	"REPO":               {},
	"REF":                {},
	"SHA512":             {},
	"HEAD_REF":           {},
	"URL":                {},
	"URLS":               {},
	"FILENAME":           {},
	"DOWNLOADS":          {},
	"FILE_DISAMBIGUATOR": {},
}
