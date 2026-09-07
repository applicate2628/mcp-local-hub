package archguard

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func ApplyOwners(report Report, owners Owners) (Baseline, error) {
	baseline := Baseline{SchemaVersion: 1, GeneratedFrom: report.Module, Entries: make([]BaselineEntry, 0, len(report.Violations))}
	for _, violation := range report.Violations {
		matched := false
		for i, rule := range owners.Rules {
			if !ownerRuleMatches(rule, violation) {
				continue
			}
			if err := validateOwnerRule(rule); err != nil {
				return Baseline{}, fmt.Errorf("owners rule %d: %w", i, err)
			}
			entry := BaselineEntry{
				Violation:   violation,
				Owner:       strings.TrimSpace(rule.Owner),
				WorkPackage: strings.TrimSpace(rule.WorkPackage),
				RemoveBy:    strings.TrimSpace(rule.RemoveBy),
				Reason:      strings.TrimSpace(rule.Reason),
			}
			if violation.Metric > 0 {
				entry.MaxMetric = violation.Metric
			}
			baseline.Entries = append(baseline.Entries, entry)
			matched = true
			break
		}
		if !matched {
			return Baseline{}, fmt.Errorf("no owner rule for %s %s at %s", violation.Kind, violation.Fingerprint, violation.Location.Path)
		}
	}
	sort.Slice(baseline.Entries, func(i, j int) bool { return baseline.Entries[i].Fingerprint < baseline.Entries[j].Fingerprint })
	return baseline, nil
}

func ownerRuleMatches(rule OwnerRule, violation Violation) bool {
	if !matchesAnyGlob(rule.Globs, violation.Location.Path, false) {
		return false
	}
	if len(rule.Kinds) == 0 {
		return true
	}
	for _, raw := range rule.Kinds {
		kind, ok := ParseViolationKind(string(raw))
		if ok && kind == violation.Kind {
			return true
		}
	}
	return false
}

func validateOwnerRule(rule OwnerRule) error {
	if strings.TrimSpace(rule.Owner) == "" || strings.TrimSpace(rule.WorkPackage) == "" || strings.TrimSpace(rule.Reason) == "" || strings.TrimSpace(rule.RemoveBy) == "" {
		return fmt.Errorf("owner, work_package, remove_by, and reason are required")
	}
	for i, raw := range rule.Kinds {
		if _, ok := ParseViolationKind(string(raw)); !ok {
			return fmt.Errorf("kinds[%d] contains unknown violation kind %q", i, raw)
		}
	}
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(rule.RemoveBy)); err != nil {
		return fmt.Errorf("remove_by must be YYYY-MM-DD: %w", err)
	}
	return nil
}
