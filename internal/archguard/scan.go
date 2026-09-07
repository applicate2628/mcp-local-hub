package archguard

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type ScanOptions struct {
	Root   string
	Policy Policy
}

type scanRule func(fileContext, Policy) []Violation

func Scan(ctx context.Context, opts ScanOptions) (Report, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return Report{}, fmt.Errorf("scan root must not be empty")
	}
	opts.Policy = clonePolicy(opts.Policy)
	if err := opts.Policy.validate("scan policy"); err != nil {
		return Report{}, err
	}
	reportRoot, err := filepath.Abs(opts.Root)
	if err != nil {
		return Report{}, err
	}
	reportRoot = filepath.ToSlash(filepath.Clean(reportRoot))
	if err := validateSourceRoots(opts.Root, opts.Policy.SourceRoots); err != nil {
		return Report{}, err
	}
	if err := validatePolicyModule(opts.Root, opts.Policy.Module); err != nil {
		return Report{}, err
	}
	files, err := collectFiles(ctx, opts.Root, opts.Policy)
	if err != nil {
		return Report{}, err
	}
	if err := evaluatePackageStringConstants(files, opts.Policy.Module); err != nil {
		return Report{}, err
	}
	if err := evaluateDeclaredStringConstants(files, opts.Policy.Module); err != nil {
		return Report{}, err
	}
	rules := []scanRule{
		ruleImports,
		ruleMutableGlobals,
		ruleAPIConstructors,
		ruleProductionConstructors,
		ruleAPICurrentPackageConstructorCalls,
		ruleProductionCurrentPackageConstructorCalls,
		ruleAPIUnresolvedConstructorCalls,
		ruleProductionUnresolvedConstructorCalls,
		ruleAPIConstructorReferences,
		ruleProductionConstructorReferences,
		ruleProductionTestHooks,
		ruleProductionTypeFieldHooks,
		ruleHistoryComments,
		ruleHistoryAdditionalOccurrences,
		ruleHistoryEmptyMatches,
		ruleResolvedEmbeddedDocuments,
		ruleFileBudgets,
		ruleWorkers,
		ruleGenericPackages,
	}
	byFingerprint := make(map[string]Violation)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		for _, rule := range rules {
			for _, violation := range rule(file, opts.Policy) {
				if isCountedOccurrenceKind(violation.Kind) && violation.Metric <= 0 {
					violation.Metric = 1
				}
				violation.Location.Path = filepath.ToSlash(violation.Location.Path)
				violation.Evidence = normalizeEvidence(violation.Evidence)
				violation.Fingerprint = Fingerprint(violation)
				existing, ok := byFingerprint[violation.Fingerprint]
				if !ok {
					byFingerprint[violation.Fingerprint] = violation
					continue
				}
				if isCountedOccurrenceKind(violation.Kind) {
					existing.Metric += violation.Metric
					if earlierViolationLine(violation, existing) {
						existing.Location.Line = violation.Location.Line
						existing.Message = violation.Message
					}
					byFingerprint[violation.Fingerprint] = existing
					continue
				}
				if earlierViolationLine(violation, existing) {
					byFingerprint[violation.Fingerprint] = violation
				}
			}
		}
	}
	violations := make([]Violation, 0, len(byFingerprint))
	for _, violation := range byFingerprint {
		violations = append(violations, violation)
	}
	sort.Slice(violations, func(i, j int) bool {
		a, b := violations[i], violations[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Location.Path != b.Location.Path {
			return a.Location.Path < b.Location.Path
		}
		if a.Location.Symbol != b.Location.Symbol {
			return a.Location.Symbol < b.Location.Symbol
		}
		return a.Evidence < b.Evidence
	})
	summary := make(map[ViolationKind]int)
	for _, violation := range violations {
		summary[violation.Kind]++
	}
	return Report{SchemaVersion: 1, Module: opts.Policy.Module, Root: reportRoot, Violations: violations, Summary: summary}, nil
}

func isCountedOccurrenceKind(kind ViolationKind) bool {
	return kind == KindAPIConstruction || kind == KindProductionConstructor || kind == KindHistoryComment
}

func earlierViolationLine(candidate, existing Violation) bool {
	return candidate.Location.Line > 0 && (existing.Location.Line == 0 || candidate.Location.Line < existing.Location.Line)
}
