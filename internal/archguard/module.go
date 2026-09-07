package archguard

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func validatePolicyModule(root, policyModule string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	actual, err := readGoModulePath(filepath.Join(rootAbs, "go.mod"))
	if err != nil {
		return err
	}
	policyModule = strings.TrimSpace(policyModule)
	if actual != policyModule {
		return fmt.Errorf("policy module %q does not match go.mod module %q", policyModule, actual)
	}
	return nil
}

func readGoModulePath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read module declaration from %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	modulePath := ""
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "module" {
			continue
		}
		if modulePath != "" {
			return "", fmt.Errorf("%s:%d: duplicate module directive", path, lineNumber)
		}
		if len(fields) < 2 {
			return "", fmt.Errorf("%s:%d: module path is missing", path, lineNumber)
		}
		raw := fields[1]
		value := raw
		if strings.HasPrefix(raw, "\"") || strings.HasPrefix(raw, "`") {
			value, err = strconv.Unquote(raw)
			if err != nil {
				return "", fmt.Errorf("%s:%d: invalid quoted module path: %w", path, lineNumber, err)
			}
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s:%d: module path is empty", path, lineNumber)
		}
		if len(fields) > 2 && !strings.HasPrefix(fields[2], "//") {
			return "", fmt.Errorf("%s:%d: unexpected tokens after module path", path, lineNumber)
		}
		modulePath = value
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", path, err)
	}
	if modulePath == "" {
		return "", fmt.Errorf("%s: module directive not found", path)
	}
	return modulePath, nil
}
