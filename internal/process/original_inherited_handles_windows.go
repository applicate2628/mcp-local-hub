//go:build windows

package process

import (
	"errors"
	"golang.org/x/sys/windows"
)

type originalInheritedCloseOutcome struct {
	NativeError             error
	ClosedKnown, OwnedKnown bool
}

type originalInheritedHandle struct {
	handle         windows.Handle
	closeAttempted bool
	actual         originalInheritedCloseOutcome
	reported       error
}

func (c *windowsContainedChild) receiveInheritedHandle(handle windows.Handle) {
	c.inheritedHandles = append(c.inheritedHandles, originalInheritedHandle{handle: handle})
}

func (c *windowsContainedChild) receiveInheritedClose(handle windows.Handle, actual originalInheritedCloseOutcome, reported error) {
	for index := range c.inheritedHandles {
		slot := &c.inheritedHandles[index]
		if slot.handle != handle {
			continue
		}
		if slot.closeAttempted {
			return
		}
		slot.closeAttempted, slot.actual, slot.reported = true, actual, reported
		c.inheritedCloseAttempts++
		c.inheritedCloseError = errors.Join(c.inheritedCloseError, actual.NativeError, reported)
		return
	}
	c.inheritedCloseError = errors.Join(c.inheritedCloseError, errors.New("original inherited close without acquired identity"))
}

func (c *windowsContainedChild) inheritedSettlementError() error {
	for _, slot := range c.inheritedHandles {
		if !slot.closeAttempted || !slot.actual.ClosedKnown {
			return errors.Join(c.inheritedCloseError, errors.New("original inherited handle closure incomplete"))
		}
	}
	return c.inheritedCloseError
}
