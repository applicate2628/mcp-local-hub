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

// parserStructuralSignal reports a whole-portfile structural condition that
// prevents patch probing while still allowing the caller to retain evidence
// observed before parsing.
type parserStructuralSignal uint8

const (
	parserStructuralNone parserStructuralSignal = iota
	parserStructuralExpressionUnparsable
	parserStructuralDeferredBody
	parserStructuralExecutionUncertain
)

func declaredPatchFromSemanticItem(item semanticItem) declaredPatch {
	display := item.display
	if display == "" {
		display = item.text
	}
	return declaredPatch{
		raw:            display,
		expanded:       item.text,
		guard:          item.meta.guard,
		guardText:      item.meta.guardText,
		unresolvedVars: item.meta.unresolvedVars,
		pathUnresolved: item.meta.pathUnresolved,
	}
}

func semanticItemsFromToken(t token, env *varEnv, active Tri, guardText string, unresolvedVars []string) ([]semanticItem, valueResolution) {
	callMeta := provenanceMeta{
		guard:          active,
		guardText:      guardText,
		unresolvedVars: dedupStrings(unresolvedVars),
	}
	evaluated := env.evaluateValue(t, callMeta)
	if evaluated.resolution.failed() {
		return nil, evaluated.resolution
	}
	items := splitCMakeList(t, evaluated)
	if len(items) == 1 && items[0].text != "" {
		if evaluated.exactReference && items[0].meta.source != "" {
			items[0].display = items[0].meta.source
		} else {
			// Public result filenames retain the r2 source-display contract when a
			// scalar/generated value remains one command argument. Split serialized
			// values use their resulting semantic item text instead.
			items[0].display = t.Text
		}
	}
	return items, valueResolution{}
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
func walkPortfile(src string, env *varEnv) (entries []declaredPatch, sawPatchesKeyword bool, structural parserStructuralSignal) {
	stmts, ok := splitStatementsChecked(src)
	if !ok {
		return nil, false, parserStructuralExpressionUnparsable
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

	declarationDepth := 0
	var loopScopes []string
	declaredCommands := map[string]struct{}{}
	deferredCommandBody := false
	executionUncertain := false
	for _, st := range stmts {
		if declarationDepth > 0 {
			if containsPatchesKeyword(st.Args) {
				deferredCommandBody = true
			}
			switch st.Name {
			case "function", "macro":
				declarationDepth++
			case "endfunction", "endmacro":
				declarationDepth--
			}
			continue
		}
		if len(loopScopes) != 0 {
			if containsPatchesKeyword(st.Args) {
				sawPatchesKeyword = true
				deferredCommandBody = true
			}
			switch st.Name {
			case "return":
				executionUncertain = true
			case "foreach", "while":
				loopScopes = append(loopScopes, st.Name)
			case "endforeach":
				if loopScopes[len(loopScopes)-1] != "foreach" {
					return nil, false, parserStructuralExpressionUnparsable
				}
				loopScopes = loopScopes[:len(loopScopes)-1]
			case "endwhile":
				if loopScopes[len(loopScopes)-1] != "while" {
					return nil, false, parserStructuralExpressionUnparsable
				}
				loopScopes = loopScopes[:len(loopScopes)-1]
			}
			continue
		}
		switch st.Name {
		case "function", "macro":
			if toks := tokenize(st.Args); len(toks) > 0 && !toks[0].Quoted {
				declaredCommands[strings.ToLower(toks[0].Text)] = struct{}{}
			}
			declarationDepth = 1
		case "foreach", "while":
			loopScopes = append(loopScopes, st.Name)
		case "endforeach", "endwhile":
			return nil, false, parserStructuralExpressionUnparsable
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
				return nil, false, parserStructuralExpressionUnparsable
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
				return nil, false, parserStructuralExpressionUnparsable
			}
			f := &frames[len(frames)-1]
			notPrior, notPriorUnresolved := notAllPrior(f)
			f.curActive = kleeneAnd(f.parentActive, notPrior)
			f.curText = "NOT(" + strings.Join(f.priorTexts, " OR ") + ")"
			f.curUnresolved = notPriorUnresolved
		case "endif":
			if len(frames) == 0 {
				return nil, false, parserStructuralExpressionUnparsable
			}
			frames = frames[:len(frames)-1]
		case "return":
			switch active() {
			case TriTrue:
				return entries, sawPatchesKeyword, parserStructuralNone
			case TriUnknown:
				return nil, false, parserStructuralExecutionUncertain
			case TriFalse:
				continue
			}
		case "set":
			handleSet(st.Args, env, active(), guardText(), activeUnresolved())
		case "get_filename_component":
			handleGetFilenameComponent(st.Args, env, active(), guardText(), activeUnresolved())
		case "list":
			handleListAppend(st.Args, env, active(), guardText(), activeUnresolved())
		case "cmake_language":
			if classifyCMakeLanguage(st.Args, env, active(), guardText(), activeUnresolved()) == cmakeLanguageCallDeferred {
				deferredCommandBody = true
			}
			continue
		default:
			if _, declared := declaredCommands[st.Name]; declared {
				if declaredInvocationPatches(st.Args, env, active(), guardText(), activeUnresolved()) != patchesAbsent {
					deferredCommandBody = true
				}
				continue
			}
			found, items, resolution := extractPatchesArg(st.Args, env, active(), guardText(), activeUnresolved())
			if resolution.failed() {
				return nil, false, parserStructuralExpressionUnparsable
			}
			if found {
				sawPatchesKeyword = true
				entries = append(entries, items...)
			}
		}
	}
	if len(frames) != 0 || declarationDepth != 0 || len(loopScopes) != 0 {
		return nil, false, parserStructuralExpressionUnparsable
	}
	if deferredCommandBody {
		return entries, sawPatchesKeyword, parserStructuralDeferredBody
	}
	if executionUncertain {
		return nil, false, parserStructuralExecutionUncertain
	}
	return entries, sawPatchesKeyword, parserStructuralNone
}

type patchesPresence uint8

const (
	patchesAbsent patchesPresence = iota
	patchesPresent
	patchesUnprovable
)

func declaredInvocationPatches(argsRaw string, env *varEnv, active Tri, guardText string, unresolvedVars []string) patchesPresence {
	for _, t := range tokenize(argsRaw) {
		items, resolution := semanticItemsFromToken(t, env, active, guardText, unresolvedVars)
		if resolution.failed() {
			return patchesUnprovable
		}
		for _, item := range items {
			if item.text == "PATCHES" {
				return patchesPresent
			}
			if len(item.meta.pathUnresolved) != 0 {
				return patchesUnprovable
			}
		}
	}
	return patchesAbsent
}

type cmakeLanguageClass uint8

const (
	cmakeLanguageNotLiteralCall cmakeLanguageClass = iota
	cmakeLanguageCallConsumed
	cmakeLanguageCallDeferred
)

// classifyCMakeLanguage consumes cmake_language before generic PATCHES
// extraction can inspect its forwarded arguments. Only the lexical literal
// CALL form evaluates its target and operands.
func classifyCMakeLanguage(argsRaw string, env *varEnv, active Tri, guardText string, unresolvedVars []string) cmakeLanguageClass {
	tokens := tokenize(argsRaw)
	if len(tokens) == 0 || tokens[0].Text != "CALL" {
		return cmakeLanguageNotLiteralCall
	}
	if len(tokens) < 2 {
		return cmakeLanguageCallDeferred
	}
	target, resolution := semanticItemsFromToken(tokens[1], env, active, guardText, unresolvedVars)
	if resolution.failed() || len(target) != 1 || target[0].text == "" || len(target[0].meta.pathUnresolved) != 0 {
		return cmakeLanguageCallDeferred
	}
	for _, forwardedToken := range tokens[2:] {
		items, resolution := semanticItemsFromToken(forwardedToken, env, active, guardText, unresolvedVars)
		if resolution.failed() {
			return cmakeLanguageCallDeferred
		}
		for _, item := range items {
			if item.text == "PATCHES" || len(item.meta.pathUnresolved) != 0 {
				return cmakeLanguageCallDeferred
			}
		}
	}
	return cmakeLanguageCallConsumed
}

// containsPatchesKeyword detects a bare PATCHES token in a deferred command
// body. Function and macro declarations are intentionally never modeled as
// calls, so this is evidence of an unsupported declaration-body dependency,
// not an extraction request.
func containsPatchesKeyword(argsRaw string) bool {
	for _, t := range tokenize(argsRaw) {
		if !t.Quoted && t.Text == "PATCHES" {
			return true
		}
	}
	return false
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
	var items []semanticItem
	resolution := valueResolution{}
	for _, t := range toks[1:] {
		if !t.Quoted && (t.Text == "CACHE" || t.Text == "PARENT_SCOPE" || t.Text == "FORCE") {
			break
		}
		var evaluated []semanticItem
		evaluated, resolution = semanticItemsFromToken(t, env, active, guardText, unresolvedVars)
		items = append(items, evaluated...)
		if resolution.failed() {
			break
		}
	}
	// set(VAR value...) evaluates command arguments first, then stores their
	// semicolon serialization with metadata-only provenance spans.
	value := serializeItems(items)
	value.resolution = resolution
	env.setValue(name, value)
}

// handleGetFilenameComponent implements get_filename_component(<var>
// <input> ABSOLUTE) — the one mode this package's traps require (resolving
// a path that may point outside the port directory, e.g. the licensepp
// shape). Any other mode (DIRECTORY, NAME, EXT, ...) is deliberately left
// unhandled: the variable stays unresolved rather than this package
// guessing an unsupported transform.
func handleGetFilenameComponent(argsRaw string, env *varEnv, active Tri, guardText string, unresolvedVars []string) {
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
	meta := provenanceMeta{
		guard:          kleeneAnd(active, ex.certainty),
		guardText:      guardText,
		unresolvedVars: dedupStrings(append(append([]string{}, unresolvedVars...), ex.uncertainVars...)),
		pathUnresolved: ex.unresolved,
	}
	env.setValue(name, serializeItems([]semanticItem{{text: env.getFilenameComponentAbsolute(ex.text), meta: meta}}))
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
	if len(toks) < 2 || toks[0].Quoted {
		return
	}
	listName := toks[1].Text
	if toks[0].Text != "APPEND" {
		if active != TriFalse && listName != "" && listSubcommandMutatesInput(toks[0].Text) {
			env.setValue(listName, serializedValue{resolution: valueResolution{issue: valueResolutionMalformedReference}})
		}
		return
	}
	var appended []semanticItem
	resolution := valueResolution{}
	for _, t := range toks[2:] {
		var items []semanticItem
		items, resolution = semanticItemsFromToken(t, env, active, guardText, unresolvedVars)
		appended = append(appended, items...)
		if resolution.failed() {
			break
		}
	}
	value := serializeItems(appended)
	value.resolution = resolution
	env.setValue(listName, appendSerializedValue(env.values[listName], value))
}

func listSubcommandMutatesInput(subcommand string) bool {
	switch subcommand {
	case "PREPEND", "INSERT", "POP_BACK", "POP_FRONT", "REMOVE_ITEM", "REMOVE_AT", "REMOVE_DUPLICATES", "FILTER", "TRANSFORM", "REVERSE", "SORT":
		return true
	default:
		return false
	}
}

// extractPatchesArg scans one call statement's raw argument text for a bare
// PATCHES keyword token. Everything after it, up to the next token that
// looks like a different ALL-CAPS keyword-arg (or end of the call), is
// collected as declared patch entries. Every token, including an exact
// unquoted ${VAR}, follows the same serialized-value evaluation path.
func extractPatchesArg(argsRaw string, env *varEnv, active Tri, guardText string, unresolvedVars []string) (found bool, items []declaredPatch, resolution valueResolution) {
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
			semanticItems, itemResolution := semanticItemsFromToken(t, env, active, guardText, unresolvedVars)
			if itemResolution.failed() {
				return found, nil, itemResolution
			}
			for _, item := range semanticItems {
				items = append(items, declaredPatchFromSemanticItem(item))
			}
			j++
		}
		i = j - 1
	}
	return found, items, valueResolution{}
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
