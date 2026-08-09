// Package portname owns vcpkg port-name validation and the name-as-one-path-
// segment containment boundary shared by vcpkg MCP readers.
package portname

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var portNameRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Name is an opaque, validated vcpkg port name.
type Name struct {
	value string
}

// String returns the validated name for result echoes and path construction.
func (name Name) String() string { return name.value }

// InvalidNameError reports a caller value that is not one legal vcpkg name.
type InvalidNameError struct {
	Name string
}

func (err *InvalidNameError) Error() string {
	return fmt.Sprintf("%q is not a legal vcpkg port name (lowercase ASCII alphanumeric components separated by single hyphens)", err.Name)
}

// EscapesRootError reports a joined port directory that is outside its caller-
// supplied root. Callers own whether their root itself must be absolute.
type EscapesRootError struct {
	Root  string
	Name  string
	Path  string
	Cause error
}

func (err *EscapesRootError) Error() string {
	if err.Cause != nil {
		return fmt.Sprintf("port path escapes root %q: %v", err.Root, err.Cause)
	}
	return fmt.Sprintf("port path %q resolves to %q, outside %q", err.Name, err.Path, err.Root)
}

func (err *EscapesRootError) Unwrap() error { return err.Cause }

// Parse validates a caller-trimmed port name before it can reach filepath.Join.
func Parse(raw string) (Name, error) {
	if !portNameRE.MatchString(raw) {
		return Name{}, &InvalidNameError{Name: raw}
	}
	return Name{value: raw}, nil
}

// Join returns name's directory beneath root and proves the cleaned result is
// contained there. The zero Name is invalid; callers outside this package cannot
// construct a non-zero unvalidated Name.
func Join(root string, name Name) (string, error) {
	if name.value == "" {
		return "", &InvalidNameError{Name: name.value}
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(cleanRoot, name.value))
	rel, err := filepath.Rel(cleanRoot, joined)
	if err != nil {
		return "", &EscapesRootError{Root: cleanRoot, Name: name.value, Path: joined, Cause: err}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", &EscapesRootError{Root: cleanRoot, Name: name.value, Path: joined}
	}
	return joined, nil
}
