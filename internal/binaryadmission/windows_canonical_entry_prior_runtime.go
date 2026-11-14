package binaryadmission

import "context"

type canonicalEntryRuntimeParentV1 interface {
	validateOriginalRuntime(context.Context, int, bool) error
	verifyOriginalRuntimeProcess(int, bool, uintptr) error
}

// Runtime is a borrowed view of original file handles. It has no Close method,
// duplicates no native handle and is invalidated when its owning set closes.
// Artifact is historical DATA; validation supplies current original-object
// authority under the original owners' mutual-exclusion lifetime.
type WindowsCanonicalEntryRuntimeV1 struct {
	owner    canonicalEntryRuntimeParentV1
	member   int
	complete bool
	artifact WindowsArtifact
}

// Preserve the prior-runtime API for existing recovery callers. Successor
// receiving uses the same concrete view, issued by its staged original owner.
type WindowsCanonicalEntryPriorRuntimeV1 = WindowsCanonicalEntryRuntimeV1

func (v *WindowsCanonicalEntryRuntimeV1) Artifact() WindowsArtifact {
	if v == nil {
		return WindowsArtifact{}
	}
	return v.artifact
}
