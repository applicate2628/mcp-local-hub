package archguard

import (
	"fmt"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const buildTagPattern = `^[A-Za-z0-9_][A-Za-z0-9_.]*$`

func LoadPolicy(path string) (Policy, error) {
	var p Policy
	if err := decodeKnownFields(path, &p); err != nil {
		return Policy{}, err
	}
	if err := p.validate(path); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func LoadBaseline(path string) (Baseline, error) {
	var b Baseline
	if err := decodeKnownFields(path, &b); err != nil {
		return Baseline{}, err
	}
	if b.SchemaVersion != 1 {
		return Baseline{}, fieldError(path, "schema_version", "must equal 1")
	}
	b.GeneratedFrom = strings.TrimSpace(b.GeneratedFrom)
	if b.GeneratedFrom == "" {
		return Baseline{}, fieldError(path, "generated_from", "must not be empty")
	}
	seen := make(map[string]struct{}, len(b.Entries))
	for i := range b.Entries {
		fp := strings.TrimSpace(b.Entries[i].Fingerprint)
		if fp == "" {
			return Baseline{}, fieldError(path, fmt.Sprintf("entries[%d].fingerprint", i), "must not be empty")
		}
		if _, ok := seen[fp]; ok {
			return Baseline{}, fieldError(path, fmt.Sprintf("entries[%d].fingerprint", i), "duplicate fingerprint")
		}
		if expected := Fingerprint(b.Entries[i].Violation); fp != expected {
			return Baseline{}, fieldError(path, fmt.Sprintf("entries[%d].fingerprint", i), fmt.Sprintf("does not match violation identity; want %s", expected))
		}
		b.Entries[i].Fingerprint = fp
		seen[fp] = struct{}{}
	}
	return b, nil
}

func LoadWorkers(path string) (Workers, error) {
	var w Workers
	if err := decodeKnownFields(path, &w); err != nil {
		return Workers{}, err
	}
	if w.SchemaVersion != 1 {
		return Workers{}, fieldError(path, "schema_version", "must equal 1")
	}
	seen := make(map[string]struct{}, len(w.Entries))
	for i := range w.Entries {
		fp := strings.TrimSpace(w.Entries[i].Fingerprint)
		if fp == "" {
			return Workers{}, fieldError(path, fmt.Sprintf("entries[%d].fingerprint", i), "must not be empty")
		}
		if _, ok := seen[fp]; ok {
			return Workers{}, fieldError(path, fmt.Sprintf("entries[%d].fingerprint", i), "duplicate fingerprint")
		}
		w.Entries[i].Fingerprint = fp
		w.Entries[i].RemoveBy = strings.TrimSpace(w.Entries[i].RemoveBy)
		seen[fp] = struct{}{}
	}
	return w, nil
}

func LoadOwners(path string) (Owners, error) {
	var o Owners
	if err := decodeKnownFields(path, &o); err != nil {
		return Owners{}, err
	}
	if o.SchemaVersion != 1 {
		return Owners{}, fieldError(path, "schema_version", "must equal 1")
	}
	for i := range o.Rules {
		r := &o.Rules[i]
		for j := range r.Globs {
			r.Globs[j] = normalizeGlob(r.Globs[j])
		}
		if len(r.Globs) == 0 {
			return Owners{}, fieldError(path, fmt.Sprintf("rules[%d].globs", i), "must not be empty")
		}
		if err := validateConfiguredGlobs(path, fmt.Sprintf("rules[%d].globs", i), r.Globs); err != nil {
			return Owners{}, err
		}
		if err := validateOwnerRule(*r); err != nil {
			return Owners{}, fieldError(path, fmt.Sprintf("rules[%d]", i), err.Error())
		}
	}
	return o, nil
}

func decodeKnownFields(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: open: %w", path, err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%s: decode: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s: trailing YAML document is not allowed", path)
		}
		return fmt.Errorf("%s: trailing content: %w", path, err)
	}
	return nil
}

func (p *Policy) validate(path string) error {
	if p.SchemaVersion != 1 {
		return fieldError(path, "schema_version", "must equal 1")
	}
	p.Module = strings.TrimSpace(p.Module)
	if p.Module == "" {
		return fieldError(path, "module", "must not be empty")
	}
	if len(p.SourceRoots) == 0 {
		return fieldError(path, "source_roots", "must not be empty")
	}
	if p.FileBudgets.ProductionAdvisoryLines <= 0 || p.FileBudgets.ProductionHardLines <= 0 || p.FileBudgets.TestReviewLines <= 0 {
		return fieldError(path, "file_budgets", "all budgets must be positive")
	}
	if p.FileBudgets.ProductionAdvisoryLines > p.FileBudgets.ProductionHardLines {
		return fieldError(path, "file_budgets", "production_advisory_lines must not exceed production_hard_lines")
	}
	if p.EmbeddedDocumentMinBytes <= 0 {
		return fieldError(path, "embedded_document_min_bytes", "must be positive")
	}
	for i := range p.SourceRoots {
		root := normalizeGlob(p.SourceRoots[i])
		if !safeSourceRoot(root) {
			return fieldError(path, fmt.Sprintf("source_roots[%d]", i), "must be a repository-relative directory without glob metacharacters or parent traversal")
		}
		p.SourceRoots[i] = root
	}
	for i := range p.ExcludeGlobs {
		p.ExcludeGlobs[i] = normalizeGlob(p.ExcludeGlobs[i])
	}
	for i := range p.HistoryAllowedGlobs {
		p.HistoryAllowedGlobs[i] = normalizeGlob(p.HistoryAllowedGlobs[i])
	}
	seenHistoryPatterns := make(map[string]struct{}, len(p.HistoryCommentPatterns))
	for i := range p.HistoryCommentPatterns {
		pattern := strings.TrimSpace(p.HistoryCommentPatterns[i])
		if pattern == "" {
			return fieldError(path, fmt.Sprintf("history_comment_patterns[%d]", i), "pattern must not be empty")
		}
		if _, duplicate := seenHistoryPatterns[pattern]; duplicate {
			return fieldError(path, fmt.Sprintf("history_comment_patterns[%d]", i), "duplicate history pattern")
		}
		seenHistoryPatterns[pattern] = struct{}{}
		p.HistoryCommentPatterns[i] = pattern
	}
	seenTestTags := make(map[string]struct{}, len(p.TestOnlyBuildTags))
	for i := range p.TestOnlyBuildTags {
		tag := strings.TrimSpace(p.TestOnlyBuildTags[i])
		valid, err := regexp.MatchString(buildTagPattern, tag)
		if err != nil || !valid {
			return fieldError(path, fmt.Sprintf("test_only_build_tags[%d]", i), "must be a valid Go build tag")
		}
		if isReservedTestBuildTag(tag) {
			return fieldError(path, fmt.Sprintf("test_only_build_tags[%d]", i), "must be a custom tag, not a reserved GOOS, GOARCH, compiler, release, or toolchain tag")
		}
		if _, duplicate := seenTestTags[tag]; duplicate {
			return fieldError(path, fmt.Sprintf("test_only_build_tags[%d]", i), "duplicate build tag")
		}
		seenTestTags[tag] = struct{}{}
		p.TestOnlyBuildTags[i] = tag
	}
	sort.Strings(p.TestOnlyBuildTags)
	for i := range p.ImportRules {
		if len(p.ImportRules[i].From) == 0 || len(p.ImportRules[i].Deny) == 0 {
			return fieldError(path, fmt.Sprintf("import_rules[%d]", i), "from and deny must not be empty")
		}
		for j := range p.ImportRules[i].From {
			p.ImportRules[i].From[j] = normalizeGlob(p.ImportRules[i].From[j])
			if p.ImportRules[i].From[j] == "" {
				return fieldError(path, fmt.Sprintf("import_rules[%d].from[%d]", i, j), "must not be empty")
			}
		}
		for j := range p.ImportRules[i].Deny {
			p.ImportRules[i].Deny[j] = normalizeGlob(p.ImportRules[i].Deny[j])
			if p.ImportRules[i].Deny[j] == "" {
				return fieldError(path, fmt.Sprintf("import_rules[%d].deny[%d]", i, j), "must not be empty")
			}
		}
	}
	constructorGroups := []struct {
		field string
		rules []SymbolRule
	}{
		{field: "api_constructors", rules: p.APIConstructors},
		{field: "production_constructors", rules: p.ProductionConstructors},
	}
	for _, group := range constructorGroups {
		for i := range group.rules {
			group.rules[i].ImportPath = strings.TrimSpace(group.rules[i].ImportPath)
			group.rules[i].Symbol = strings.TrimSpace(group.rules[i].Symbol)
			if group.rules[i].ImportPath == "" || group.rules[i].Symbol == "" {
				return fieldError(path, fmt.Sprintf("%s[%d]", group.field, i), "import_path and symbol must not be empty")
			}
			if group.rules[i].Symbol == "_" || !token.IsIdentifier(group.rules[i].Symbol) {
				return fieldError(path, fmt.Sprintf("%s[%d].symbol", group.field, i), "must be a valid non-blank Go identifier")
			}
			for j := range group.rules[i].AllowedGlobs {
				group.rules[i].AllowedGlobs[j] = normalizeGlob(group.rules[i].AllowedGlobs[j])
			}
		}
	}
	seenGenericPackages := make(map[string]struct{}, len(p.GenericPackageNames))
	for i := range p.GenericPackageNames {
		name := strings.TrimSpace(p.GenericPackageNames[i])
		if name == "" || name == "_" || !token.IsIdentifier(name) {
			return fieldError(path, fmt.Sprintf("generic_package_names[%d]", i), "must be a valid non-blank Go package identifier")
		}
		if _, duplicate := seenGenericPackages[name]; duplicate {
			return fieldError(path, fmt.Sprintf("generic_package_names[%d]", i), "duplicate package name")
		}
		seenGenericPackages[name] = struct{}{}
		p.GenericPackageNames[i] = name
	}
	sort.Strings(p.GenericPackageNames)
	if err := validatePolicyGlobs(path, p); err != nil {
		return err
	}
	if err := p.compilePatterns(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (p *Policy) compilePatterns() error {
	var err error
	if p.compiledAllowedGlobals, err = compileRegexes("allowed_global_name_patterns", p.AllowedGlobalNamePatterns); err != nil {
		return err
	}
	if p.compiledTestHooks, err = compileRegexes("test_hook_name_patterns", p.TestHookNamePatterns); err != nil {
		return err
	}
	if p.compiledHistory, err = compileRegexes("history_comment_patterns", p.HistoryCommentPatterns); err != nil {
		return err
	}
	return nil
}

func compileRegexes(field string, patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for i, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("field %s[%d]: pattern must not be empty", field, i)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("field %s[%d]: %w", field, i, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func fieldError(path, field, message string) error {
	return fmt.Errorf("%s: field %s: %s", path, field, message)
}

func normalizeGlob(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\\", "/"))
	s = strings.TrimPrefix(s, "./")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return strings.TrimSuffix(s, "/")
}

func safeSourceRoot(root string) bool {
	if root == "" || filepath.IsAbs(root) || strings.ContainsAny(root, "*?[") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(root), "/") {
		if part == ".." {
			return false
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(root)))
	return clean == "." || (clean != ".." && !strings.HasPrefix(clean, "../"))
}
