package archguard

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

var reservedWindowsImportNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

func validateGoImportPath(importPath string) error {
	if !utf8.ValidString(importPath) {
		return fmt.Errorf("must be valid UTF-8")
	}
	if importPath == "" {
		return fmt.Errorf("must not be empty")
	}
	if importPath[0] == '-' {
		return fmt.Errorf("must not begin with a dash")
	}
	if strings.HasPrefix(importPath, "/") || strings.HasSuffix(importPath, "/") || strings.Contains(importPath, "//") {
		return fmt.Errorf("must be a slash-separated relative Go import path without empty elements")
	}
	for _, element := range strings.Split(importPath, "/") {
		if err := validateGoImportPathElement(element); err != nil {
			return fmt.Errorf("invalid path element %q: %w", element, err)
		}
	}
	return nil
}

func validateGoImportPathElement(element string) error {
	if element == "" || strings.Count(element, ".") == len(element) {
		return fmt.Errorf("must not be empty or consist only of dots")
	}
	if strings.HasSuffix(element, ".") {
		return fmt.Errorf("must not end with a dot")
	}
	for _, r := range element {
		if isGoImportPathRune(r) {
			continue
		}
		return fmt.Errorf("contains invalid character %q", r)
	}
	short := element
	if dot := strings.IndexByte(short, '.'); dot >= 0 {
		short = short[:dot]
	}
	if _, reserved := reservedWindowsImportNames[strings.ToUpper(short)]; reserved {
		return fmt.Errorf("component %q is reserved on Windows", short)
	}
	if tilde := strings.LastIndexByte(short, '~'); tilde >= 0 && tilde < len(short)-1 {
		suffix := short[tilde+1:]
		allDigits := true
		for _, r := range suffix {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return fmt.Errorf("must not resemble a Windows short name")
		}
	}
	return nil
}

func isGoImportPathRune(r rune) bool {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '-', '.', '_', '~', '+':
		return true
	default:
		return false
	}
}
