package archguard

func clonePolicy(policy Policy) Policy {
	cloned := policy
	cloned.SourceRoots = append([]string(nil), policy.SourceRoots...)
	cloned.ExcludeGlobs = append([]string(nil), policy.ExcludeGlobs...)
	cloned.AllowedGlobalNamePatterns = append([]string(nil), policy.AllowedGlobalNamePatterns...)
	cloned.TestHookNamePatterns = append([]string(nil), policy.TestHookNamePatterns...)
	cloned.HistoryCommentPatterns = append([]string(nil), policy.HistoryCommentPatterns...)
	cloned.HistoryAllowedGlobs = append([]string(nil), policy.HistoryAllowedGlobs...)
	cloned.TestOnlyBuildTags = append([]string(nil), policy.TestOnlyBuildTags...)
	cloned.GenericPackageNames = append([]string(nil), policy.GenericPackageNames...)

	cloned.ImportRules = make([]ImportRule, len(policy.ImportRules))
	for i, rule := range policy.ImportRules {
		cloned.ImportRules[i] = rule
		cloned.ImportRules[i].From = append([]string(nil), rule.From...)
		cloned.ImportRules[i].Deny = append([]string(nil), rule.Deny...)
	}
	cloned.APIConstructors = cloneSymbolRules(policy.APIConstructors)
	cloned.ProductionConstructors = cloneSymbolRules(policy.ProductionConstructors)

	// Validation recompiles these from the cloned pattern strings.
	cloned.compiledAllowedGlobals = nil
	cloned.compiledTestHooks = nil
	cloned.compiledHistory = nil
	return cloned
}

func cloneSymbolRules(rules []SymbolRule) []SymbolRule {
	cloned := make([]SymbolRule, len(rules))
	for i, rule := range rules {
		cloned[i] = rule
		cloned[i].AllowedGlobs = append([]string(nil), rule.AllowedGlobs...)
	}
	return cloned
}
