package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/archguard"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type commandOptions struct {
	root          string
	policy        string
	owners        string
	baseline      string
	workers       string
	reportJSON    string
	reportMD      string
	generatedFrom string
	kinds         stringList
	paths         stringList
	unclassified  bool
}

func run(args []string, stdout, stderr io.Writer) (code int) {
	checkedStdout := &checkedWriter{destination: stdout}
	stdout = checkedStdout
	defer func() {
		if checkedStdout.err != nil && code == 0 {
			fmt.Fprintf(stderr, "write stdout: %v\n", checkedStdout.err)
			code = 2
		}
	}()

	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: archcheck <scan|baseline|verify|workers> [flags]")
		return 2
	}
	command := args[0]
	opts, err := parseOptions(command, args[1:], stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := validateCommandPaths(command, opts); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if (command == "verify" || command == "baseline") && (len(opts.kinds) > 0 || len(opts.paths) > 0) {
		fmt.Fprintln(stderr, "--kind and --path are diagnostic filters available only to scan and workers")
		return 2
	}
	policy, err := archguard.LoadPolicy(opts.policy)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	report, err := archguard.Scan(context.Background(), archguard.ScanOptions{Root: opts.root, Policy: policy})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	switch command {
	case "scan":
		presented := filterReport(report, opts.kinds, opts.paths)
		if err := writeReports(presented, opts.reportJSON, opts.reportMD); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		body, err := archguard.RenderJSON(presented)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		_, _ = stdout.Write(append(body, '\n'))
		return 0
	case "baseline":
		if err := writeReports(report, opts.reportJSON, opts.reportMD); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		owners, err := archguard.LoadOwners(opts.owners)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		baseline, err := archguard.ApplyOwners(report, owners)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		baseline.GeneratedFrom = opts.generatedFrom
		body, err := yaml.Marshal(baseline)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if opts.baseline != "" {
			if err := writeFile(opts.baseline, body); err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
		} else {
			_, _ = stdout.Write(append(body, '\n'))
		}
		return 0
	case "verify":
		if opts.owners != "" {
			if _, err := archguard.LoadOwners(opts.owners); err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
		}
		baseline, err := archguard.LoadBaseline(opts.baseline)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		workers, err := archguard.LoadWorkers(opts.workers)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		verification := archguard.Verify(report, baseline, workers, archguard.VerifyOptions{Now: time.Now().UTC()})
		if err := writeReports(report, opts.reportJSON, opts.reportMD); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		writeVerificationSummary(stderr, verification)
		if !verification.OK() {
			return 1
		}
		fmt.Fprintln(stdout, "architecture verification passed")
		return 0
	case "workers":
		workersReport := filterReport(report, []string{string(archguard.KindWorker)}, opts.paths)
		if opts.unclassified {
			workers, err := archguard.LoadWorkers(opts.workers)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			baseline, err := archguard.LoadBaseline(opts.baseline)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			known := make(map[string]struct{}, len(workers.Entries)+len(baseline.Entries))
			for _, entry := range workers.Entries {
				known[entry.Fingerprint] = struct{}{}
			}
			for _, entry := range baseline.Entries {
				if entry.Kind == archguard.KindWorker {
					known[entry.Fingerprint] = struct{}{}
				}
			}
			filtered := workersReport.Violations[:0]
			for _, violation := range workersReport.Violations {
				if _, ok := known[violation.Fingerprint]; !ok {
					filtered = append(filtered, violation)
				}
			}
			workersReport.Violations = filtered
			workersReport.Summary = map[archguard.ViolationKind]int{archguard.KindWorker: len(filtered)}
		}
		workersReport = filterReport(workersReport, opts.kinds, opts.paths)
		if err := writeReports(workersReport, opts.reportJSON, opts.reportMD); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		body, err := archguard.RenderJSON(workersReport)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		_, _ = stdout.Write(append(body, '\n'))
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		return 2
	}
}

func parseOptions(command string, args []string, stderr io.Writer) (commandOptions, error) {
	if command != "scan" && command != "baseline" && command != "verify" && command != "workers" {
		return commandOptions{}, fmt.Errorf("unknown command %q", command)
	}
	opts := commandOptions{root: "."}
	fs := flag.NewFlagSet("archcheck "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.root, "root", opts.root, "repository root")
	fs.StringVar(&opts.policy, "policy", "", "architecture policy YAML (required)")
	fs.StringVar(&opts.owners, "owners", "", "owner mapping YAML")
	fs.StringVar(&opts.baseline, "baseline", "", "baseline YAML")
	fs.StringVar(&opts.workers, "workers", "", "worker registry YAML")
	fs.StringVar(&opts.reportJSON, "report-json", "", "write JSON report")
	fs.StringVar(&opts.reportMD, "report-md", "", "write Markdown report")
	fs.StringVar(&opts.generatedFrom, "generated-from", "", "source commit for generated baseline")
	fs.Var(&opts.kinds, "kind", "repeatable violation kind filter for scan/workers")
	fs.Var(&opts.paths, "path", "repeatable repository-relative path filter for scan/workers")
	fs.BoolVar(&opts.unclassified, "unclassified", false, "workers: show only unclassified workers")
	if err := fs.Parse(args); err != nil {
		return commandOptions{}, err
	}
	if len(fs.Args()) != 0 {
		return commandOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := normalizeKindFilters(&opts.kinds); err != nil {
		return commandOptions{}, err
	}
	if err := normalizePathFilters(&opts.paths); err != nil {
		return commandOptions{}, err
	}
	if strings.TrimSpace(opts.root) == "" {
		return commandOptions{}, fmt.Errorf("--root is required")
	}
	if strings.TrimSpace(opts.policy) == "" {
		return commandOptions{}, fmt.Errorf("--policy is required")
	}
	if command == "baseline" && strings.TrimSpace(opts.owners) == "" {
		return commandOptions{}, fmt.Errorf("--owners is required")
	}
	if command == "baseline" && strings.TrimSpace(opts.generatedFrom) == "" {
		return commandOptions{}, fmt.Errorf("--generated-from is required")
	}
	if command == "verify" && (strings.TrimSpace(opts.baseline) == "" || strings.TrimSpace(opts.workers) == "") {
		return commandOptions{}, fmt.Errorf("--baseline and --workers are required")
	}
	if opts.unclassified && command != "workers" {
		return commandOptions{}, fmt.Errorf("--unclassified is available only to workers")
	}
	if command == "workers" && opts.unclassified && (strings.TrimSpace(opts.baseline) == "" || strings.TrimSpace(opts.workers) == "") {
		return commandOptions{}, fmt.Errorf("workers --unclassified requires --baseline and --workers")
	}
	return opts, nil
}

func normalizeKindFilters(kinds *stringList) error {
	if kinds == nil || len(*kinds) == 0 {
		return nil
	}
	known := archguard.KnownViolationKinds()
	knownNames := make([]string, len(known))
	for i, kind := range known {
		knownNames[i] = string(kind)
	}
	for i, raw := range *kinds {
		kind, ok := archguard.ParseViolationKind(raw)
		if !ok {
			return fmt.Errorf("unknown --kind %q; valid values: %s", raw, strings.Join(knownNames, ", "))
		}
		(*kinds)[i] = string(kind)
	}
	return nil
}

func normalizePathFilters(paths *stringList) error {
	if paths == nil || len(*paths) == 0 {
		return nil
	}
	for i, raw := range *paths {
		pattern := filepath.ToSlash(strings.TrimSpace(raw))
		if pattern == "" {
			return fmt.Errorf("--path must not be empty")
		}
		if err := archguard.ValidatePathGlob(pattern); err != nil {
			return fmt.Errorf("invalid --path %q: %w", raw, err)
		}
		(*paths)[i] = pattern
	}
	return nil
}

func filterReport(report archguard.Report, kinds, paths []string) archguard.Report {
	kindSet := make(map[archguard.ViolationKind]struct{}, len(kinds))
	for _, kind := range kinds {
		kindSet[archguard.ViolationKind(kind)] = struct{}{}
	}
	filtered := make([]archguard.Violation, 0, len(report.Violations))
	for _, violation := range report.Violations {
		if len(kindSet) > 0 {
			if _, ok := kindSet[violation.Kind]; !ok {
				continue
			}
		}
		if len(paths) > 0 && !pathMatchesAny(paths, violation.Location.Path) {
			continue
		}
		filtered = append(filtered, violation)
	}
	report.Violations = filtered
	report.Summary = make(map[archguard.ViolationKind]int)
	for _, violation := range filtered {
		report.Summary[violation.Kind]++
	}
	return report
}

func pathMatchesAny(patterns []string, value string) bool {
	value = filepath.ToSlash(strings.TrimPrefix(value, "./"))
	for _, rawPattern := range patterns {
		pattern := filepath.ToSlash(strings.TrimSpace(rawPattern))
		pattern = strings.TrimPrefix(pattern, "./")
		if pattern == "." {
			return true
		}
		if archguard.MatchPathGlob(pattern, value) {
			return true
		}
		if !strings.ContainsAny(pattern, "*?[") {
			literal := strings.TrimSuffix(pattern, "/")
			if value == literal || strings.HasPrefix(value, literal+"/") {
				return true
			}
		}
	}
	return false
}

func writeReports(report archguard.Report, jsonPath, markdownPath string) error {
	if err := validateDistinctReportPaths(jsonPath, markdownPath); err != nil {
		return err
	}
	if jsonPath != "" {
		body, err := archguard.RenderJSON(report)
		if err != nil {
			return err
		}
		if err := writeFile(jsonPath, append(body, '\n')); err != nil {
			return err
		}
	}
	if markdownPath != "" {
		if err := writeFile(markdownPath, []byte(archguard.RenderMarkdown(report))); err != nil {
			return err
		}
	}
	return nil
}

func validateDistinctReportPaths(jsonPath, markdownPath string) error {
	return validateDistinctNamedPaths(
		namedPath{name: "--report-json", path: jsonPath},
		namedPath{name: "--report-md", path: markdownPath},
	)
}

func canonicalOutputPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(abs)
	cursor := clean
	var suffix []string
	for {
		if _, err := os.Lstat(cursor); err == nil {
			resolved, err := filepath.EvalSymlinks(cursor)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return canonicalOutputPathKey(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return canonicalOutputPathKey(clean), nil
		}
		suffix = append(suffix, filepath.Base(cursor))
		cursor = parent
	}
}

func canonicalOutputPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func writeVerificationSummary(w io.Writer, verification archguard.Verification) {
	rows := []struct {
		name  string
		count int
	}{
		{"new", len(verification.New)},
		{"grown", len(verification.Grown)},
		{"expired", len(verification.Expired)},
		{"stale", len(verification.Stale)},
		{"unowned", len(verification.Unowned)},
		{"workers", len(verification.Workers)},
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("%s=%d", row.name, row.count))
	}
	fmt.Fprintf(w, "architecture verification: %s\n", strings.Join(parts, " "))
}
