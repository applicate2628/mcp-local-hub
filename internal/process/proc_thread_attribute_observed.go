package process

import (
	"errors"
	"sync"
	"unsafe"
)

type ProcThreadAttributeListCloseObservation struct {
	Returned, AllocationKnown, Allocated, Initialized     bool
	DeleteAttempted, DeleteReturned, FreeAttempted, Freed bool
	Error                                                 error
}

func (o ProcThreadAttributeListCloseObservation) Complete() bool {
	return o.Returned && o.AllocationKnown && o.Error == nil && (!o.Allocated || (o.FreeAttempted && o.Freed && (!o.Initialized || (o.DeleteAttempted && o.DeleteReturned))))
}

type procThreadAttributeNative struct {
	initialize         func(unsafe.Pointer, uint32, *uintptr) error
	allocate           func(uint32) (uintptr, error)
	update             func(unsafe.Pointer, uintptr, unsafe.Pointer, uintptr) error
	delete             func(unsafe.Pointer) error
	free               func(uintptr) error
	insufficientBuffer error
}
type procThreadAttributeListObserved struct {
	mu                                    sync.Mutex
	native                                procThreadAttributeNative
	data                                  uintptr
	initialized, allocationKnown, closing bool
	pointers                              []unsafe.Pointer
	closed                                *ProcThreadAttributeListCloseObservation
}

func newProcThreadAttributeListWithNative(count uint32, native procThreadAttributeNative) (al *procThreadAttributeListObserved, retErr error) {
	if count == 0 || native.initialize == nil || native.allocate == nil || native.update == nil || native.delete == nil || native.free == nil || native.insufficientBuffer == nil {
		return nil, errors.New("PROC_ATTRIBUTE_NATIVE_INPUT_INVALID")
	}
	al = &procThreadAttributeListObserved{native: native, allocationKnown: true}
	var size uintptr
	if err := native.initialize(nil, count, &size); !errors.Is(err, native.insufficientBuffer) {
		return al, errors.Join(err, errors.New("PROC_ATTRIBUTE_SIZE_QUERY_UNDEFINED"))
	}
	if size == 0 || uint64(size) > uint64(^uint32(0)) {
		return al, errors.New("PROC_ATTRIBUTE_SIZE_INVALID")
	}
	allocation, err := native.allocate(uint32(size))
	al.data = allocation
	if err != nil {
		return al, err
	}
	if allocation == 0 {
		return al, errors.New("PROC_ATTRIBUTE_ALLOCATION_UNDEFINED")
	}
	if err = native.initialize(unsafe.Pointer(allocation), count, &size); err != nil {
		return al, err
	}
	al.initialized = true
	return al, nil
}
func (al *procThreadAttributeListObserved) Update(attribute uintptr, value unsafe.Pointer, size uintptr) error {
	if al == nil || !al.initialized || al.data == 0 || al.closing || al.closed != nil {
		return errors.New("PROC_ATTRIBUTE_LIST_UNAVAILABLE")
	}
	al.pointers = append(al.pointers, value)
	return al.native.update(unsafe.Pointer(al.data), attribute, value, size)
}
func (al *procThreadAttributeListObserved) List() unsafe.Pointer {
	if al == nil || !al.initialized || al.data == 0 || al.closing || al.closed != nil {
		return nil
	}
	return unsafe.Pointer(al.data)
}
func (al *procThreadAttributeListObserved) CloseObserved() (ProcThreadAttributeListCloseObservation, error) {
	if al == nil {
		return ProcThreadAttributeListCloseObservation{}, errors.New("PROC_ATTRIBUTE_LIST_UNAVAILABLE")
	}
	al.mu.Lock()
	if al.closed != nil {
		view := *al.closed
		al.mu.Unlock()
		return view, view.Error
	}
	if al.closing {
		al.mu.Unlock()
		return ProcThreadAttributeListCloseObservation{}, errors.New("PROC_ATTRIBUTE_CLOSE_PENDING")
	}
	al.closing = true
	view := ProcThreadAttributeListCloseObservation{AllocationKnown: al.allocationKnown, Allocated: al.data != 0, Initialized: al.initialized}
	al.mu.Unlock()
	if view.Allocated {
		if view.Initialized {
			view.DeleteAttempted = true
			err := al.native.delete(unsafe.Pointer(al.data))
			view.DeleteReturned = err == nil
			view.Error = errors.Join(view.Error, err)
		}
		view.FreeAttempted = true
		err := al.native.free(al.data)
		view.Freed = err == nil
		view.Error = errors.Join(view.Error, err)
	}
	view.Returned = true
	al.mu.Lock()
	al.closed = &view
	al.pointers = nil
	al.mu.Unlock()
	return view, view.Error
}
