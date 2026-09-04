package archguard

import "strings"

// ParseViolationKind canonicalizes and validates a user-provided finding kind.
func ParseViolationKind(value string) (ViolationKind, bool) {
	kind := ViolationKind(strings.TrimSpace(value))
	for _, known := range KnownViolationKinds() {
		if kind == known {
			return kind, true
		}
	}
	return "", false
}

// KnownViolationKinds returns a newly allocated stable list for diagnostics and
// help output without introducing mutable package-level state.
func KnownViolationKinds() []ViolationKind {
	return []ViolationKind{
		KindAPIConstruction,
		KindEmbeddedDocument,
		KindFileBudget,
		KindGenericPackage,
		KindHistoryComment,
		KindImport,
		KindMutableGlobal,
		KindProductionConstructor,
		KindProductionTestHook,
		KindWorker,
	}
}
