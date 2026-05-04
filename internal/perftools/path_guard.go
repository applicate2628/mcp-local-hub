package perftools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateBinaryInsideRoot is the shared guard used by tools that take a
// project_root + a path to a target file (binary, source, etc.). It:
//
//  1. requires both arguments non-empty
//  2. resolves projectRoot via filepath.Abs + filepath.EvalSymlinks and
//     asserts it is an existing directory (not a filesystem root)
//  3. resolves the target path the same way (relative paths join under
//     projectRoot first) and asserts it is an existing file
//  4. asserts the resolved real path of the target stays inside the
//     resolved real path of projectRoot
//
// kind is the noun used in error messages so callers can keep their
// original wording ("binary", "file", ...).
//
// The returned string is the canonical resolved real path of the target.
func validateBinaryInsideRoot(projectRoot, targetPath, kind string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", fmt.Errorf("missing required parameter: project_root (directory boundary containing the %s)", kind)
	}
	if strings.TrimSpace(targetPath) == "" {
		return "", fmt.Errorf("missing required parameter: %s", kind)
	}

	rootAbs, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("invalid project_root %q: %w", projectRoot, err)
	}
	rootReal, err := filepath.EvalSymlinks(filepath.Clean(rootAbs))
	if err != nil {
		return "", fmt.Errorf("invalid project_root %q: %w", projectRoot, err)
	}
	rootInfo, err := os.Stat(rootReal)
	if err != nil {
		return "", fmt.Errorf("invalid project_root %q: %w", projectRoot, err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("invalid project_root %q: must be a directory", projectRoot)
	}
	if isFilesystemRoot(rootReal) {
		return "", fmt.Errorf("invalid project_root %q: filesystem root is not an allowed project boundary", projectRoot)
	}

	candidate := targetPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(rootReal, candidate)
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("invalid %s %q: %w", kind, targetPath, err)
	}
	targetReal, err := filepath.EvalSymlinks(filepath.Clean(candidateAbs))
	if err != nil {
		return "", fmt.Errorf("invalid %s %q: %w", kind, targetPath, err)
	}
	targetInfo, err := os.Stat(targetReal)
	if err != nil {
		return "", fmt.Errorf("invalid %s %q: %w", kind, targetPath, err)
	}
	if targetInfo.IsDir() {
		return "", fmt.Errorf("invalid %s %q: must be a file", kind, targetPath)
	}

	inside, err := pathInsideRoot(rootReal, targetReal)
	if err != nil {
		return "", fmt.Errorf("invalid %s %q: %w", kind, targetPath, err)
	}
	if !inside {
		return "", fmt.Errorf("invalid %s %q: must be inside project_root %q", kind, targetPath, projectRoot)
	}
	return targetReal, nil
}

// validatePerfToolExtraArgs rejects extra_args entries that would let a
// caller bypass the binary/file path guard by smuggling additional input
// files into the subprocess argv. Both llvm-objdump and IWYU accept
// multiple input files positionally and treat tokens starting with '@'
// as response-file directives ("@FILE" reads more arguments from FILE).
//
// Rule: every entry must look like a flag — start with '-' or '--'.
// Positional inputs and '@'-response-files are rejected because they
// cannot be path-validated without re-implementing each tool's argv
// grammar; a flag like "--demangle" or "--x86-asm-syntax=intel" cannot
// be re-interpreted as a file path.
func validatePerfToolExtraArgs(extra []string) error {
	for _, a := range extra {
		if a == "" {
			return fmt.Errorf("invalid extra_args: empty entry not allowed")
		}
		if strings.HasPrefix(a, "@") {
			return fmt.Errorf("invalid extra_args: %q must be a flag (start with '-' or '--'); positional input files and @-response-file directives are not allowed", a)
		}
		if !strings.HasPrefix(a, "-") {
			return fmt.Errorf("invalid extra_args: %q must be a flag (start with '-' or '--'); positional input files and @-response-file directives are not allowed", a)
		}
	}
	return nil
}
