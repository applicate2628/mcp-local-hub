package archguard

import "strings"

func isReservedTestBuildTag(tag string) bool {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return false
	}
	if isKnownGOOS(tag) || isKnownGOARCH(tag) {
		return true
	}
	switch tag {
	case "unix", "gc", "gccgo", "cgo", "race", "msan", "asan", "fuzz", "boringcrypto":
		return true
	}
	if strings.HasPrefix(tag, "go1.") || strings.HasPrefix(tag, "goexperiment.") {
		return true
	}
	if _, recognized := parseArchitectureFeatureTag(tag); recognized {
		return true
	}
	return false
}
