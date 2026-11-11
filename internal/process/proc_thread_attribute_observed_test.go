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
				if !view.Complete() || deleteCalls != 0 || freeCalls != 1 || !errors.Is(err, initFailure) {
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
