package binaryadmission

import (
	"os"
	"sync"
)

// WindowsCanonicalEntryDispatchSnapshotV2 is a detached, read-only dispatch
// projection of retained current originals. It is not the complete-cohort V2
// transaction snapshot and cannot be serialized as a V1 journal or receipt.
type WindowsCanonicalEntryDispatchSnapshotV2 struct {
	BuildID, RegistryProjectionSHA256, SourceEpoch  string
	CanonicalEntry, Product, Identity, RuntimeIndex WindowsCanonicalEntryFileV1
	ProductArtifact                                 WindowsArtifact
}

// WindowsCanonicalEntryDispatchSetV2 owns only read acquisition. A dispatch projection
// confers no staging, mutation, recovery, or complete runtime-cohort authority.
// Every partial result must be closed, including after acquisition failure.
type WindowsCanonicalEntryDispatchSetV2 struct {
	mu                                     sync.Mutex
	files                                  []*os.File
	originals                              []*canonicalEntryNativeFile
	entry, product, identity, runtimeIndex *canonicalEntryNativeFile
	snapshot                               WindowsCanonicalEntryDispatchSnapshotV2
	ready, closed                          bool
	closeError                             error
}
