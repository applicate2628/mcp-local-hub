package archguard

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type VerifyOptions struct {
	Now time.Time
}

func (v Verification) OK() bool {
	return len(v.New) == 0 && len(v.Grown) == 0 && len(v.Expired) == 0 && len(v.Stale) == 0 && len(v.Unowned) == 0 && len(v.Workers) == 0
}

func Verify(report Report, baseline Baseline, workers Workers, opts VerifyOptions) Verification {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	day := opts.Now.UTC().Truncate(24 * time.Hour)
	current := make(map[string]Violation, len(report.Violations))
	for _, violation := range report.Violations {
		current[violation.Fingerprint] = violation
	}
	known := make(map[string]BaselineEntry, len(baseline.Entries))
	var result Verification
	for _, entry := range baseline.Entries {
		known[entry.Fingerprint] = entry
		if baselineEntryUnowned(entry) {
			result.Unowned = append(result.Unowned, entry)
		}
		removeBy, err := time.Parse("2006-01-02", strings.TrimSpace(entry.RemoveBy))
		if err != nil {
			if !containsBaselineFingerprint(result.Unowned, entry.Fingerprint) {
				result.Unowned = append(result.Unowned, entry)
			}
		} else if removeBy.Before(day) {
			result.Expired = append(result.Expired, entry)
		}
		violation, ok := current[entry.Fingerprint]
		if !ok {
			result.Stale = append(result.Stale, entry)
			continue
		}
		if violation.Metric > 0 {
			if entry.MaxMetric <= 0 {
				if !containsBaselineFingerprint(result.Unowned, entry.Fingerprint) {
					result.Unowned = append(result.Unowned, entry)
				}
			} else if violation.Metric > entry.MaxMetric {
				result.Grown = append(result.Grown, MetricChange{Violation: violation, Baseline: entry})
			}
		}
	}
	registeredWorkers := make(map[string]struct{}, len(workers.Entries))
	for _, record := range workers.Entries {
		registeredWorkers[record.Fingerprint] = struct{}{}
	}
	for _, violation := range report.Violations {
		if _, ok := known[violation.Fingerprint]; ok {
			continue
		}
		if violation.Kind == KindWorker {
			if _, ok := registeredWorkers[violation.Fingerprint]; ok {
				continue
			}
		}
		result.New = append(result.New, violation)
	}
	result.Workers = verifyWorkers(report, workers, known, strings.TrimSpace(baseline.GeneratedFrom) != "", day)
	sortVerification(&result)
	return result
}

func baselineEntryUnowned(entry BaselineEntry) bool {
	return strings.TrimSpace(entry.Owner) == "" || strings.TrimSpace(entry.WorkPackage) == "" || strings.TrimSpace(entry.Reason) == "" || strings.TrimSpace(entry.RemoveBy) == ""
}

func containsBaselineFingerprint(entries []BaselineEntry, fingerprint string) bool {
	for _, entry := range entries {
		if entry.Fingerprint == fingerprint {
			return true
		}
	}
	return false
}

func verifyWorkers(report Report, workers Workers, baseline map[string]BaselineEntry, allowLegacyBaseline bool, now time.Time) []WorkerProblem {
	registered := make(map[string]WorkerRecord, len(workers.Entries))
	var problems []WorkerProblem
	for _, record := range workers.Entries {
		registered[record.Fingerprint] = record
		if workerRecordInvalid(record) {
			problems = append(problems, WorkerProblem{Fingerprint: record.Fingerprint, Problem: "worker record has an empty, unknown, or none ownership field"})
		}
		deadline := strings.TrimSpace(record.RemoveBy)
		if deadline != "" {
			removeBy, err := time.Parse("2006-01-02", deadline)
			if err != nil {
				problems = append(problems, WorkerProblem{Fingerprint: record.Fingerprint, Problem: "worker remove_by is not YYYY-MM-DD"})
			} else if removeBy.Before(now) {
				problems = append(problems, WorkerProblem{Fingerprint: record.Fingerprint, Problem: "worker ownership deadline expired"})
			}
		}
	}
	current := map[string]struct{}{}
	for _, violation := range report.Violations {
		if violation.Kind != KindWorker {
			continue
		}
		current[violation.Fingerprint] = struct{}{}
		baselineEntry, inBaseline := baseline[violation.Fingerprint]
		inBaseline = allowLegacyBaseline && inBaseline && baselineEntry.Kind == KindWorker
		_, inRegistry := registered[violation.Fingerprint]
		switch {
		case inBaseline && inRegistry:
			problems = append(problems, WorkerProblem{Fingerprint: violation.Fingerprint, Problem: "worker is present in both baseline and worker registry"})
		case !inBaseline && !inRegistry:
			problems = append(problems, WorkerProblem{Fingerprint: violation.Fingerprint, Problem: fmt.Sprintf("unclassified worker at %s:%s", violation.Location.Path, violation.Location.Symbol)})
		}
	}
	for fingerprint := range registered {
		if _, ok := current[fingerprint]; !ok {
			problems = append(problems, WorkerProblem{Fingerprint: fingerprint, Problem: "stale worker registry entry"})
		}
	}
	return problems
}

func workerRecordInvalid(record WorkerRecord) bool {
	values := []string{record.Fingerprint, record.Component, record.Owner, record.Start, record.Cancel, record.Join, record.BoundedBy, record.ContractTest, record.WorkPackage}
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" || normalized == "unknown" || normalized == "none" {
			return true
		}
	}
	if strings.EqualFold(strings.TrimSpace(record.Cancel), "process-exit") && strings.TrimSpace(record.Reason) == "" {
		return true
	}
	return false
}

func sortVerification(result *Verification) {
	sort.Slice(result.New, func(i, j int) bool { return result.New[i].Fingerprint < result.New[j].Fingerprint })
	sort.Slice(result.Grown, func(i, j int) bool {
		return result.Grown[i].Violation.Fingerprint < result.Grown[j].Violation.Fingerprint
	})
	sort.Slice(result.Expired, func(i, j int) bool { return result.Expired[i].Fingerprint < result.Expired[j].Fingerprint })
	sort.Slice(result.Stale, func(i, j int) bool { return result.Stale[i].Fingerprint < result.Stale[j].Fingerprint })
	sort.Slice(result.Unowned, func(i, j int) bool { return result.Unowned[i].Fingerprint < result.Unowned[j].Fingerprint })
	sort.Slice(result.Workers, func(i, j int) bool {
		if result.Workers[i].Fingerprint != result.Workers[j].Fingerprint {
			return result.Workers[i].Fingerprint < result.Workers[j].Fingerprint
		}
		return result.Workers[i].Problem < result.Workers[j].Problem
	})
}
