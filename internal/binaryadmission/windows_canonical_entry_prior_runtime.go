package binaryadmission

import "context"

type canonicalEntryRuntimeParentV1 interface {
	validateOriginalRuntime(context.Context, int, bool) error
	verifyOriginalRuntimeProcess(int, bool, uintptr) error
}

// PriorRuntime is a borrowed view of the same prior-set original handles. It
// has no Close method, duplicates no native handle and is invalidated by the
// prior owner's Close. Artifact is historical DATA; validation supplies current
// original-object authority under the prior owner's mutual-exclusion lifetime.
type WindowsCanonicalEntryPriorRuntimeV1 struct {
	owner    canonicalEntryRuntimeParentV1
	member   int
	complete bool
	artifact WindowsArtifact
}

func (v *WindowsCanonicalEntryPriorRuntimeV1) Artifact() WindowsArtifact {
	if v == nil {
		return WindowsArtifact{}
	}
	return v.artifact
}
