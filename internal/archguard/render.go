package archguard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func RenderJSON(report Report) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}

func RenderMarkdown(report Report) string {
	var b strings.Builder
	b.WriteString("# Architecture Report\n\n")
	fmt.Fprintf(&b, "- Schema version: `%d`\n", report.SchemaVersion)
	fmt.Fprintf(&b, "- Module: %s\n", markdownCodeSpan(report.Module))
	fmt.Fprintf(&b, "- Root: %s\n", markdownCodeSpan(report.Root))
	fmt.Fprintf(&b, "- Violations: **%d**\n\n", len(report.Violations))
	b.WriteString("## Summary\n\n| Kind | Count |\n|---|---:|\n")
	kinds := make([]string, 0, len(report.Summary))
	for kind := range report.Summary {
		kinds = append(kinds, string(kind))
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(&b, "| %s | %d |\n", markdownCell(kind), report.Summary[ViolationKind(kind)])
	}
	b.WriteString("\n## Violations\n\n")
	if len(report.Violations) == 0 {
		b.WriteString("No violations.\n")
		return b.String()
	}
	b.WriteString("| Kind | Location | Symbol | Metric | Message | Fingerprint |\n|---|---|---|---:|---|---|\n")
	for _, violation := range report.Violations {
		location := violation.Location.Path
		if violation.Location.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, violation.Location.Line)
		}
		metric := ""
		if violation.Metric > 0 {
			metric = fmt.Sprintf("%d", violation.Metric)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | `%s` |\n",
			markdownCell(string(violation.Kind)), markdownCell(location), markdownCell(violation.Location.Symbol), metric, markdownCell(violation.Message), violation.Fingerprint)
	}
	return b.String()
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	// Escape literal backslashes before escaping table separators. A path such
	// as a\|b then becomes a\\\|b: Markdown consumes the first pair as one
	// literal backslash and the remaining backslash still protects the pipe.
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	return value
}

func markdownCodeSpan(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")

	maxRun := 0
	currentRun := 0
	for _, r := range value {
		if r == '`' {
			currentRun++
			if currentRun > maxRun {
				maxRun = currentRun
			}
			continue
		}
		currentRun = 0
	}
	delimiter := strings.Repeat("`", maxRun+1)
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") || strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") {
		return delimiter + " " + value + " " + delimiter
	}
	return delimiter + value + delimiter
}
