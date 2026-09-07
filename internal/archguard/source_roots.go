package archguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func validateSourceRoots(root string, sourceRoots []string) error {
	rootAbs, err := canonicalRepositoryRoot(root)
	if err != nil {
		return err
	}
	for _, sourceRoot := range sourceRoots {
		if _, err := configuredSourceRootPath(rootAbs, sourceRoot); err != nil {
			return err
		}
	}
	return nil
}

func canonicalRepositoryRoot(root string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	rootAbs = filepath.Clean(rootAbs)
	volume := filepath.VolumeName(rootAbs)
	current := volume + string(os.PathSeparator)
	if volume == "" {
		current = string(os.PathSeparator)
	}
	remainder := strings.TrimPrefix(rootAbs, current)
	for _, component := range strings.Split(remainder, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect repository root component %q: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("repository root traverses symlink component %q; symlinked repository roots are not allowed", component)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("repository root component %q is not a directory", component)
		}
	}
	return rootAbs, nil
}

func configuredSourceRootPath(rootAbs, sourceRoot string) (string, error) {
	current := filepath.Clean(rootAbs)
	clean := filepath.Clean(filepath.FromSlash(sourceRoot))
	if clean == "." {
		info, err := os.Stat(current)
		if err != nil {
			return "", fmt.Errorf("configured source root %q: %w", sourceRoot, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("configured source root %q is not a directory", sourceRoot)
		}
		return current, nil
	}

	for _, component := range strings.Split(clean, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("configured source root %q: inspect component %q: %w", sourceRoot, component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("configured source root %q traverses symlink component %q; symlinked source roots are not allowed", sourceRoot, component)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("configured source root %q component %q is not a directory", sourceRoot, component)
		}
	}
	return current, nil
}
