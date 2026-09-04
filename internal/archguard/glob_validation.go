package archguard

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidatePathGlob rejects malformed path patterns before they can silently
// disable an architecture rule. Matching and validation share the same class
// parser, so accepted policy globs cannot later degrade into a non-match.
func ValidatePathGlob(pattern string) error {
	pattern = normalizeGlob(pattern)
	if pattern == "" {
		return fmt.Errorf("glob must not be empty")
	}
	if !utf8.ValidString(pattern) {
		return fmt.Errorf("glob must be valid UTF-8")
	}
	if strings.HasPrefix(pattern, "/") || looksLikeWindowsAbsoluteGlob(pattern) {
		return fmt.Errorf("glob must be repository-relative")
	}
	if pattern != "." {
		for _, component := range strings.Split(pattern, "/") {
			if component == "." || component == ".." {
				return fmt.Errorf("glob must not contain dot or parent-traversal components")
			}
		}
	}
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '[':
			_, next, ok := translateGlobClass(pattern, i)
			if !ok {
				return fmt.Errorf("malformed character class at byte %d", i)
			}
			i = next
		case ']':
			return fmt.Errorf("unmatched closing bracket at byte %d", i)
		default:
			r, size := utf8.DecodeRuneInString(pattern[i:])
			if size == 0 || r == utf8.RuneError && size == 1 {
				return fmt.Errorf("invalid UTF-8 at byte %d", i)
			}
			if unicode.IsControl(r) {
				return fmt.Errorf("glob contains control character at byte %d", i)
			}
			i += size
		}
	}
	return nil
}

func looksLikeWindowsAbsoluteGlob(pattern string) bool {
	return len(pattern) >= 3 && ((pattern[0] >= 'A' && pattern[0] <= 'Z') || (pattern[0] >= 'a' && pattern[0] <= 'z')) && pattern[1] == ':' && pattern[2] == '/'
}

func validateConfiguredGlobs(path, field string, globs []string) error {
	for i, pattern := range globs {
		if strings.TrimSpace(pattern) == "" {
			return fieldError(path, fmt.Sprintf("%s[%d]", field, i), "must not be empty")
		}
		if err := ValidatePathGlob(pattern); err != nil {
			return fieldError(path, fmt.Sprintf("%s[%d]", field, i), err.Error())
		}
	}
	return nil
}

func validatePolicyGlobs(path string, policy *Policy) error {
	if policy == nil {
		return nil
	}
	if err := validateConstructorRuleDuplicates(path, policy); err != nil {
		return err
	}
	if err := validateConfiguredGlobs(path, "exclude_globs", policy.ExcludeGlobs); err != nil {
		return err
	}
	if err := validateConfiguredGlobs(path, "history_allowed_globs", policy.HistoryAllowedGlobs); err != nil {
		return err
	}
	for i := range policy.ImportRules {
		if err := validateConfiguredGlobs(path, fmt.Sprintf("import_rules[%d].from", i), policy.ImportRules[i].From); err != nil {
			return err
		}
		if err := validateConfiguredGlobs(path, fmt.Sprintf("import_rules[%d].deny", i), policy.ImportRules[i].Deny); err != nil {
			return err
		}
	}
	groups := []struct {
		field string
		rules []SymbolRule
	}{
		{field: "api_constructors", rules: policy.APIConstructors},
		{field: "production_constructors", rules: policy.ProductionConstructors},
	}
	for _, group := range groups {
		for i := range group.rules {
			importPath := strings.TrimSpace(group.rules[i].ImportPath)
			if err := validateGoImportPath(importPath); err != nil {
				return fieldError(path, fmt.Sprintf("%s[%d].import_path", group.field, i), err.Error())
			}
			if err := validateConfiguredGlobs(path, fmt.Sprintf("%s[%d].allowed_globs", group.field, i), group.rules[i].AllowedGlobs); err != nil {
				return err
			}
		}
	}
	return nil
}
