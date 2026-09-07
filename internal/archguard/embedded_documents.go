package archguard

import (
	"fmt"
	"go/ast"
	"strings"
)

func ruleResolvedEmbeddedDocuments(ctx fileContext, policy Policy) []Violation {
	if ctx.Generated {
		return nil
	}
	packageSpecs := packageLevelValueSpecs(ctx.File)
	localOccurrences := make(map[string]int)
	var out []Violation
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name == nil || name.Name == "_" {
				continue
			}
			values := ctx.StringValues[name]
			if len(values) == 0 && i < len(spec.Values) {
				expr := spec.Values[i]
				values = ctx.StringValues[expr]
				if len(values) == 0 {
					if value, ok := stringConstant(expr); ok {
						values = []string{value}
					}
				}
			}
			maxBytes := 0
			for _, value := range values {
				if len(value) < policy.EmbeddedDocumentMinBytes || embeddedMarkdownMarkers(value) < 2 {
					continue
				}
				if len(value) > maxBytes {
					maxBytes = len(value)
				}
			}
			if maxBytes == 0 {
				continue
			}

			symbol := name.Name
			if _, packageLevel := packageSpecs[spec]; !packageLevel {
				scope := enclosingSymbol(ctx, spec.Pos())
				if scope == "" {
					scope = ctx.File.Name.Name
				}
				base := scope + "." + name.Name
				localOccurrences[base]++
				symbol = base
				if localOccurrences[base] > 1 {
					symbol = fmt.Sprintf("%s#%d", base, localOccurrences[base])
				}
			}
			out = append(out, Violation{
				Kind:     KindEmbeddedDocument,
				Location: Location{Path: ctx.Path, Symbol: symbol, Line: lineOf(ctx, name.Pos())},
				Evidence: "markdown string constant",
				Metric:   maxBytes,
				Message:  fmt.Sprintf("embedded Markdown document is %d bytes in at least one valid build variant; move it to an embedded .md file", maxBytes),
			})
		}
		return true
	})
	return out
}

func packageLevelValueSpecs(file *ast.File) map[*ast.ValueSpec]struct{} {
	out := make(map[*ast.ValueSpec]struct{})
	if file == nil {
		return out
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, rawSpec := range gen.Specs {
			if spec, ok := rawSpec.(*ast.ValueSpec); ok {
				out[spec] = struct{}{}
			}
		}
	}
	return out
}

func embeddedMarkdownMarkers(s string) int {
	markers := 0
	if hasMarkdownHeading(s) {
		markers++
	}
	if hasFencedCodeBlock(s) {
		markers++
	}
	if strings.Contains(s, "\n|") && strings.Contains(s, "|\n") {
		markers++
	}
	return markers
}

func hasMarkdownHeading(s string) bool {
	return hasATXHeading(s) || hasSetextHeading(s)
}

func hasATXHeading(s string) bool {
	for _, rawLine := range strings.Split(s, "\n") {
		line := trimMarkdownIndent(strings.TrimSuffix(rawLine, "\r"))
		hashes := 0
		for hashes < len(line) && line[hashes] == '#' {
			hashes++
		}
		if hashes < 1 || hashes > 6 {
			continue
		}
		if hashes == len(line) || line[hashes] == ' ' || line[hashes] == '\t' {
			return true
		}
	}
	return false
}

func hasSetextHeading(s string) bool {
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(strings.TrimSuffix(lines[i-1], "\r")) == "" {
			continue
		}
		line := strings.TrimRight(trimMarkdownIndent(strings.TrimSuffix(lines[i], "\r")), " \t")
		if line == "" {
			continue
		}
		marker := line[0]
		if marker != '=' && marker != '-' {
			continue
		}
		valid := true
		for j := 1; j < len(line); j++ {
			if line[j] != marker {
				valid = false
				break
			}
		}
		if valid {
			return true
		}
	}
	return false
}

func hasFencedCodeBlock(s string) bool {
	for _, rawLine := range strings.Split(s, "\n") {
		line := trimMarkdownIndent(strings.TrimSuffix(rawLine, "\r"))
		if len(line) < 3 || line[0] != '`' && line[0] != '~' {
			continue
		}
		marker := line[0]
		run := 0
		for run < len(line) && line[run] == marker {
			run++
		}
		if run >= 3 {
			return true
		}
	}
	return false
}

func trimMarkdownIndent(line string) string {
	spaces := 0
	for spaces < len(line) && spaces < 3 && line[spaces] == ' ' {
		spaces++
	}
	return line[spaces:]
}
