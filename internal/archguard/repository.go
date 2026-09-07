package archguard

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type fileContext struct {
	Path             string
	PackageDir       string
	IsTest           bool
	Generated        bool
	Imports          map[string]string
	DotImports       map[string]struct{}
	UnaliasedImports map[string]int
	ImportPaths      []string
	File             *ast.File
	FSet             *token.FileSet
	LineCount        int
	Source           []byte
	StringValues     map[ast.Expr][]string
}

func collectFiles(ctx context.Context, root string, policy Policy) ([]fileContext, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	seen := map[string]struct{}{}
	var out []fileContext
	for _, sourceRoot := range policy.SourceRoots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start, err := configuredSourceRootPath(rootAbs, sourceRoot)
		if err != nil {
			return nil, err
		}
		err = filepath.WalkDir(start, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if path == start {
					return fmt.Errorf("configured source root %q became a symlink during scanning", sourceRoot)
				}
				rel, relErr := filepath.Rel(rootAbs, path)
				if relErr != nil {
					rel = path
				}
				return fmt.Errorf("configured source root %q contains symlink %q; symlinked source trees are not allowed", sourceRoot, filepath.ToSlash(rel))
			}
			if path == start && !entry.IsDir() {
				return fmt.Errorf("configured source root %q ceased to be a directory during scanning", sourceRoot)
			}
			rel, err := filepath.Rel(rootAbs, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			base := entry.Name()
			if path != start && entry.IsDir() && (base == "testdata" || base == "vendor") {
				return filepath.SkipDir
			}
			if path != start && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if path != start && matchesAnyGlob(policy.ExcludeGlobs, rel, true) {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || matchesAnyGlob(policy.ExcludeGlobs, rel, false) {
				return nil
			}
			if _, ok := seen[rel]; ok {
				return nil
			}
			seen[rel] = struct{}{}
			fc, err := parseFile(path, rel, policy.TestOnlyBuildTags)
			if err != nil {
				return err
			}
			out = append(out, fc)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", sourceRoot, err)
		}
	}
	if err := resolveLocalImportNames(out, policy.Module); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func parseFile(path, rel string, testOnlyBuildTags []string) (fileContext, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return fileContext{}, fmt.Errorf("read %s: %w", rel, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.ParseComments)
	if err != nil {
		return fileContext{}, fmt.Errorf("parse %s: %w", rel, err)
	}
	imports := make(map[string]string, len(file.Imports))
	dotImports := make(map[string]struct{})
	unaliasedImports := make(map[string]int)
	importPaths := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		importPaths = append(importPaths, importPath)
		alias := ""
		if spec.Name != nil {
			alias = spec.Name.Name
		} else {
			alias = pathpkg.Base(importPath)
			unaliasedImports[importPath] = fset.Position(spec.Pos()).Line
		}
		switch alias {
		case "_":
		case ".":
			dotImports[importPath] = struct{}{}
		default:
			imports[alias] = importPath
		}
	}
	lineCount := bytes.Count(source, []byte{'\n'})
	if len(source) > 0 && source[len(source)-1] != '\n' {
		lineCount++
	}
	pkgDir := filepath.ToSlash(filepath.Dir(rel))
	if pkgDir == "." {
		pkgDir = ""
	}
	return fileContext{
		Path:             filepath.ToSlash(rel),
		PackageDir:       pkgDir,
		IsTest:           strings.HasSuffix(rel, "_test.go") || isTestOnlyEffectiveBuildFile(filepath.ToSlash(rel), file, fset, source, testOnlyBuildTags),
		Generated:        isGeneratedFile(file),
		Imports:          imports,
		DotImports:       dotImports,
		UnaliasedImports: unaliasedImports,
		ImportPaths:      importPaths,
		File:             file,
		FSet:             fset,
		LineCount:        lineCount,
		Source:           source,
	}, nil
}

func resolveLocalImportNames(files []fileContext, module string) error {
	module = strings.TrimSuffix(strings.TrimSpace(module), "/")
	packageVariants := make(map[string][]localPackageVariant)
	for _, file := range files {
		// A custom test-tagged file remains part of its package under that tag
		// and may provide the declared import name. Only physical _test.go files
		// are not importable package members.
		if strings.HasSuffix(file.Path, "_test.go") {
			continue
		}
		importPath := module
		if file.PackageDir != "" {
			importPath += "/" + file.PackageDir
		}
		candidate := localPackageVariant{Name: file.File.Name.Name, Constraint: buildConstraintForFile(file)}
		if packageVariantsConflict(packageVariants[importPath], candidate) {
			return fmt.Errorf("local package %s declares conflicting names under compatible build constraints", importPath)
		}
		packageVariants[importPath] = append(packageVariants[importPath], candidate)
	}
	for i := range files {
		importerConstraint := buildConstraintForFile(files[i])
		for importPath := range files[i].UnaliasedImports {
			name, ok := compatiblePackageName(packageVariants[importPath], importerConstraint)
			if !ok {
				continue
			}
			defaultAlias := pathpkg.Base(importPath)
			if files[i].Imports[defaultAlias] == importPath {
				delete(files[i].Imports, defaultAlias)
			}
			if prior, exists := files[i].Imports[name]; exists && prior != importPath {
				return fmt.Errorf("%s: imported package name %q is ambiguous between %s and %s", files[i].Path, name, prior, importPath)
			}
			files[i].Imports[name] = importPath
			delete(files[i].UnaliasedImports, importPath)
		}
	}
	return nil
}

func isGeneratedFile(file *ast.File) bool {
	if file == nil {
		return false
	}
	const prefix = "// Code generated "
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			break
		}
		for _, comment := range group.List {
			text := strings.TrimSuffix(comment.Text, "\r")
			if !strings.HasPrefix(text, prefix) {
				continue
			}
			rest := strings.TrimPrefix(text, prefix)
			if strings.HasSuffix(rest, " DO NOT EDIT.") {
				return true
			}
		}
	}
	return false
}

func matchesAnyGlob(patterns []string, value string, directory bool) bool {
	for _, pattern := range patterns {
		if matchGlob(pattern, value) || (directory && matchGlob(pattern, strings.TrimSuffix(value, "/")+"/")) {
			return true
		}
	}
	return false
}

func MatchPathGlob(pattern, value string) bool {
	return matchGlob(pattern, value)
}

func matchGlob(pattern, value string) bool {
	pattern = normalizeGlob(pattern)
	value = filepath.ToSlash(strings.TrimPrefix(value, "./"))
	if pattern == "" {
		return value == ""
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		if matchGlob(prefix, value) {
			return true
		}
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		case '[':
			class, next, ok := translateGlobClass(pattern, i)
			if !ok {
				return false
			}
			b.WriteString(class)
			i = next
		default:
			r, size := utf8.DecodeRuneInString(pattern[i:])
			if r == utf8.RuneError && size == 1 {
				return false
			}
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size
		}
	}
	b.WriteString("$")
	matched, err := regexp.MatchString(b.String(), value)
	return err == nil && matched
}
