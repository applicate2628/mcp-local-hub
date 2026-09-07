package archguard

import (
	"fmt"
	"strings"
)

func validateConstructorRuleDuplicates(path string, policy *Policy) error {
	if policy == nil {
		return nil
	}
	groups := []struct {
		field string
		rules []SymbolRule
	}{
		{field: "api_constructors", rules: policy.APIConstructors},
		{field: "production_constructors", rules: policy.ProductionConstructors},
	}
	for _, group := range groups {
		seen := make(map[string]int, len(group.rules))
		for i, rule := range group.rules {
			identity := strings.TrimSpace(rule.ImportPath) + "\x00" + strings.TrimSpace(rule.Symbol)
			if prior, duplicate := seen[identity]; duplicate {
				return fieldError(path, fmt.Sprintf("%s[%d]", group.field, i), fmt.Sprintf("duplicate constructor identity; already declared at %s[%d]", group.field, prior))
			}
			seen[identity] = i
		}
	}
	return nil
}
