package binaryadmission

import (
	"errors"
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
	acquisition                            *InstalledCanonicalEntryAcquisitionObservation
}

// An observed acquisition retains the original opens and the dispatch owner
// that settles them. A failed open's nonzero undefined output is evidence
// of uncertainty only. It never gives this owner permission to close it.
type InstalledCanonicalEntryAcquisitionObservation struct {
	owner    *WindowsCanonicalEntryDispatchSetV2
	opens    canonicalEntryObservedOpens
	returned bool
	err      error
}

func (o *InstalledCanonicalEntryAcquisitionObservation) ResourcesFinalized() bool {
	if o == nil || !o.returned {
		return false
	}
	if o.owner != nil {
		o.owner.mu.Lock()
		defer o.owner.mu.Unlock()
	}
	return o.opens.resourcesFinalized()
}

type canonicalEntryObservedOpen struct {
	native                        uintptr
	present                       bool
	openErr                       error
	closeNative                   func() error
	file                          *os.File
	closeAttempted, closeReturned bool
	closeErr                      error
}

type canonicalEntryObservedOpens struct{ slots []*canonicalEntryObservedOpen }

func (o *canonicalEntryObservedOpens) register(native uintptr, present bool, openErr error, closeNative func() error) {
	o.slots = append(o.slots, &canonicalEntryObservedOpen{native: native, present: present, openErr: openErr, closeNative: closeNative})
}

func (o *canonicalEntryObservedOpens) resourcesFinalized() bool {
	for _, slot := range o.slots {
		if slot.present && (slot.openErr != nil || !slot.closeAttempted || !slot.closeReturned || slot.closeErr != nil) {
			return false
		}
	}
	return true
}

func (o *canonicalEntryObservedOpens) close() (err error) {
	for i := len(o.slots) - 1; i >= 0; i-- {
		slot := o.slots[i]
		if !slot.present || slot.openErr != nil {
			continue
		}
		if !slot.closeAttempted {
			slot.closeAttempted = true
			if slot.file != nil {
				slot.closeErr = slot.file.Close()
			} else {
				slot.closeErr = slot.closeNative()
			}
			slot.closeReturned = true
		}
		err = errors.Join(err, slot.closeErr)
	}
	return err
}
