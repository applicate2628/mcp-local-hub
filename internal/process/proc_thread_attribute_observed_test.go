package process

import (
	"errors"
	"testing"
	"unsafe"
)

func TestObservedAttributePartialInitAndCloseOnce(t *testing.T) {
	insufficient := errors.New("insufficient")
	initFailure := errors.New("init failure")
	freeFailure := errors.New("free failure")
	modes := []string{"success", "init-failure", "free-failure", "size-zero"}
	if unsafe.Sizeof(uintptr(0)) > unsafe.Sizeof(uint32(0)) {
		modes = append(modes, "size-overflow")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			const allocation = uintptr(1) // Opaque native address; the double never dereferences it.
			deleteCalls, freeCalls := 0, 0
			native := procThreadAttributeNative{insufficientBuffer: insufficient,
				initialize: func(data uintptr, count uint32, size *uintptr) error {
					if data == 0 {
						*size = 64
						if mode == "size-overflow" {
							*size = uintptr(^uint32(0))
							*size++
						}
						if mode == "size-zero" {
							*size = 0
						}
						return insufficient
					}
					if mode == "init-failure" {
						return initFailure
					}
					return nil
				},
				allocate: func(uint32) (uintptr, error) { return allocation, nil },
				update:   func(uintptr, uintptr, unsafe.Pointer, uintptr) error { return nil },
				delete:   func(uintptr) error { deleteCalls++; return nil },
				free: func(uintptr) error {
					freeCalls++
					if mode == "free-failure" {
						return freeFailure
					}
					return nil
				}}
			owner, err := newProcThreadAttributeListWithNative(2, native)
			if mode == "success" || mode == "free-failure" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid acquisition admitted")
			}
			view, closeErr := owner.CloseObserved()
			again, againErr := owner.CloseObserved()
			if view != again || !errors.Is(closeErr, againErr) {
				t.Fatal("close not cached")
			}
			switch mode {
			case "success":
				if !view.Complete() || deleteCalls != 1 || freeCalls != 1 {
					t.Fatal("successful closure missing")
				}
			case "init-failure":
				if !view.Complete() || deleteCalls != 0 || freeCalls != 1 || !errors.Is(err, initFailure) || len(owner.pointers) != 0 {
					t.Fatal("partial allocation lost")
				}
			case "free-failure":
				if view.Complete() || freeCalls != 1 || !errors.Is(closeErr, freeFailure) {
					t.Fatal("uncertain free retried/qualified")
				}
			default:
				if freeCalls != 0 || deleteCalls != 0 || !view.Complete() {
					t.Fatal("failed size query caused allocation/deletion")
				}
			}
		})
	}
	if (ProcThreadAttributeListCloseObservation{}).Complete() {
		t.Fatal("default closure qualified")
	}
}

func TestObservedAttributePayloadLifetimeFollowsDeleteReturn(t *testing.T) {
	insufficient := errors.New("insufficient")
	deleteFailure := errors.New("delete procedure unavailable before invocation")
	freeFailure := errors.New("native free failure")
	for _, tc := range []struct {
		name        string
		deleteError error
		freeError   error
	}{
		{"delete-returned-free-failed", nil, freeFailure},
		{"delete-not-entered-free-failed", deleteFailure, freeFailure},
		{"delete-not-entered-free-returned", deleteFailure, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const allocation = uintptr(1) // Opaque native address; never dereferenced.
			payload := uint64(7)
			root := unsafe.Pointer(&payload)
			var owner *procThreadAttributeListObserved
			deleteCalls, freeCalls := 0, 0
			native := procThreadAttributeNative{
				insufficientBuffer: insufficient,
				initialize: func(data uintptr, _ uint32, size *uintptr) error {
					if data == 0 {
						*size = 64
						return insufficient
					}
					return nil
				},
				allocate: func(uint32) (uintptr, error) { return allocation, nil },
				update: func(data, attribute uintptr, value unsafe.Pointer, size uintptr) error {
					if data != allocation || attribute != 1 || value != root || size != unsafe.Sizeof(payload) {
						t.Fatal("native update lost the supplied payload")
					}
					return nil
				},
				delete: func(data uintptr) error {
					deleteCalls++
					if data != allocation || freeCalls != 0 || owner.data != allocation || len(owner.pointers) != 1 || owner.pointers[0] != root {
						t.Fatal("payload root or allocation lost before deletion returned")
					}
					return tc.deleteError
				},
				free: func(data uintptr) error {
					freeCalls++
					if data != allocation || deleteCalls != 1 {
						t.Fatal("native free changed allocation or preceded deletion")
					}
					return tc.freeError
				},
			}
			var err error
			owner, err = newProcThreadAttributeListWithNative(1, native)
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Update(1, root, unsafe.Sizeof(payload)); err != nil {
				t.Fatal(err)
			}
			view, closeErr := owner.CloseObserved()
			if !view.Returned || !view.AllocationKnown || !view.Allocated || !view.Initialized || !view.DeleteAttempted || !view.FreeAttempted || view.Complete() || owner.data != allocation || deleteCalls != 1 || freeCalls != 1 {
				t.Fatal("incomplete close lost original allocation or native observations")
			}
			if view.DeleteReturned != (tc.deleteError == nil) || view.Freed != (tc.freeError == nil) || !errors.Is(closeErr, tc.deleteError) && tc.deleteError != nil || !errors.Is(closeErr, tc.freeError) && tc.freeError != nil {
				t.Fatal("close lost deletion/free result or originating causes")
			}
			if tc.deleteError == nil {
				if owner.pointers != nil {
					t.Fatal("returned deletion retained payload roots after free failure")
				}
			} else if len(owner.pointers) != 1 || owner.pointers[0] != root {
				t.Fatal("unproved deletion released the original payload root")
			}
			again, againErr := owner.CloseObserved()
			if again != view || againErr != closeErr || deleteCalls != 1 || freeCalls != 1 || owner.data != allocation {
				t.Fatal("cached close retried native operations or changed the original outcome")
			}
		})
	}
}
