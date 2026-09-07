package archguard

import "fmt"

// ruleHistoryAdditionalOccurrences complements ruleHistoryComments, which
// emits the first non-empty match for each pattern in a comment group. Every
// additional match is emitted with the same stable identity so Scan can
// aggregate it into a count-ratcheted metric.
func ruleHistoryAdditionalOccurrences(ctx fileContext, policy Policy) []Violation {
	if ctx.Generated || matchesAnyGlob(policy.HistoryAllowedGlobs, ctx.Path, false) {
		return nil
	}
	var out []Violation
	for _, group := range ctx.File.Comments {
		text := group.Text()
		for _, pattern := range policy.compiledHistory {
			matches := pattern.FindAllString(text, -1)
			if len(matches) < 2 {
				continue
			}
			for _, match := range matches[1:] {
				if match == "" {
					continue
				}
				out = append(out, Violation{
					Kind:     KindHistoryComment,
					Location: Location{Path: ctx.Path, Symbol: enclosingSymbol(ctx, group.Pos()), Line: lineOf(ctx, group.Pos())},
					Evidence: match,
					Message:  fmt.Sprintf("production comment contains repeated review history marker %q; move history to an ADR or archive", match),
				})
			}
		}
	}
	return out
}
