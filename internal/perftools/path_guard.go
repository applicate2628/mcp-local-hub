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

// validatePerfToolExtraArgsBasic rejects extra_args entries that would
// let a caller bypass the binary/file path guard by smuggling additional
// input files into the subprocess argv. Both llvm-objdump and IWYU
// accept multiple input files positionally and treat tokens starting
// with '@' as response-file directives ("@FILE" reads more arguments
// from FILE).
//
// Rule: every entry must look like a flag — start with '-' or '--'.
// Positional inputs and '@'-response-files are rejected because they
// cannot be path-validated without re-implementing each tool's argv
// grammar.
//
// This is the shared base check. Each tool then layers its own
// forbidden-flag-prefix list on top to reject path-VALUED flags
// (e.g. `--build-id=<dir>`, `--mapping_file=<file>`) that would
// otherwise pass the "starts with -" gate but read attacker-chosen
// files outside project_root. See validateLLVMObjdumpExtraArgs and
// validateIWYUExtraArgs.
func validatePerfToolExtraArgsBasic(extra []string) error {
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

// forbiddenLLVMObjdumpFlagPrefixes lists llvm-objdump flags that take a
// path value (file or directory). Allowing them in extra_args would let
// a caller read arbitrary filesystem paths despite the project_root
// guard on `binary` — llvm-objdump opens these paths with the running
// process's privileges.
//
// Both spellings (`--flag=PATH` and `--flag PATH`) must be rejected:
// the equals-form is caught by HasPrefix(arg, "--flag="), the
// space-separated form is caught by exact equality (arg == "--flag")
// because the value would land on the next argv slot which we can't
// path-validate without re-implementing llvm-objdump's grammar.
//
// Reference: `llvm-objdump --help` documents these as path-valued.
var forbiddenLLVMObjdumpFlagPrefixes = []string{
	"--build-id",
	"--debug-file-directory",
	"--dsym",
	"--prefix",
	"--prefix-strip",
}

// validateLLVMObjdumpExtraArgs runs the shared basic check then rejects
// path-valued llvm-objdump flags listed in
// forbiddenLLVMObjdumpFlagPrefixes. Benign flags like `-d`, `--demangle`,
// `--x86-asm-syntax=intel` continue to pass.
func validateLLVMObjdumpExtraArgs(extra []string) error {
	if err := validatePerfToolExtraArgsBasic(extra); err != nil {
		return err
	}
	for _, a := range extra {
		if matchesForbiddenFlagPrefix(a, forbiddenLLVMObjdumpFlagPrefixes) {
			return fmt.Errorf("invalid extra_args: %q is a path-valued llvm-objdump flag; not allowed in extra_args because it bypasses the project_root guard", a)
		}
	}
	return nil
}

// forbiddenIWYUFlagPrefixes lists IWYU flags whose value is a filesystem
// path. IWYU exposes these via the `-Xiwyu` pass-through, so the
// HasPrefix check covers both `-Xiwyu --mapping_file=PATH` (where
// `-Xiwyu` is one arg and `--mapping_file=PATH` is the next) and the
// degenerate cases.
//
// `-Xiwyu` itself is also rejected as a bare token because the next
// argv slot is unbounded — we cannot path-validate it without
// re-implementing IWYU's grammar.
//
// Plain clang flags `-std=...`, `-I...`, `-D...`, `-W...` are NOT
// rejected: they affect compile semantics, not file access via IWYU's
// own flag set.
var forbiddenIWYUFlagPrefixes = []string{
	"-Xiwyu",
	"--mapping_file",
	"--export_mappings",
	"--check_also",
	"--keep",
}

// validateIWYUExtraArgs runs the shared basic check then rejects
// path-valued IWYU flags. Returns the original args unchanged on
// success — the signature returns the slice so future callers can
// add rewriting (e.g. canonicalizing relative paths inside extra_args)
// without changing the call sites.
func validateIWYUExtraArgs(extra []string) ([]string, error) {
	if err := validatePerfToolExtraArgsBasic(extra); err != nil {
		return nil, err
	}
	for _, a := range extra {
		if matchesForbiddenFlagPrefix(a, forbiddenIWYUFlagPrefixes) {
			return nil, fmt.Errorf("invalid extra_args: %q is a path-valued IWYU flag; not allowed in extra_args because it bypasses the project_root guard", a)
		}
	}
	return extra, nil
}

// matchesForbiddenFlagPrefix returns true when arg equals any forbidden
// prefix (the bare-flag-with-separate-value form, e.g. `--mapping_file
// /etc/passwd`) or starts with `<prefix>=` (the equals-form). No other
// prefix-matching is performed — `--build-id` must not falsely match
// `--build-id-something-unrelated`.
func matchesForbiddenFlagPrefix(arg string, prefixes []string) bool {
	for _, p := range prefixes {
		if arg == p || strings.HasPrefix(arg, p+"=") {
			return true
		}
	}
	return false
}
