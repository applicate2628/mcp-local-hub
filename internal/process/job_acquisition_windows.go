//go:build windows

package process

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

type jobAcquisitionOps struct {
	create    func() (windows.Handle, error)
	configure func(windows.Handle, *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error
	close     func(windows.Handle) error
}

// NewJobObserved returns the actual partial owner together with acquisition
// failure. The caller must retain it: a failed close is not no-resource proof.
// Existing NewJob callers retain their established nil-on-error API.
func NewJobObserved(options JobOptions) (*Job, error) {
	return newJobObserved(options, jobAcquisitionOps{})
}

func newJobObserved(options JobOptions, ops jobAcquisitionOps) (*Job, error) {
	if ops.create == nil {
		ops.create = func() (windows.Handle, error) { return windows.CreateJobObject(nil, nil) }
	}
	if ops.configure == nil {
		ops.configure = func(h windows.Handle, info *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error {
			_, err := setJobInformation(h, windows.JobObjectExtendedLimitInformation, unsafe.Pointer(info), uint32(unsafe.Sizeof(*info)))
			return err
		}
	}
	h, err := ops.create()
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	if h == 0 || h == windows.InvalidHandle {
		return nil, errors.New("CreateJobObject returned invalid handle")
	}
	job := &Job{handle: h}
	if ops.close != nil {
		job.terminationOps = &jobTerminationOps{closeJob: ops.close}
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if options.KillOnClose {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	if options.BreakawayOK {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	}
	if err := ops.configure(h, &info); err != nil {
		return job, errors.Join(fmt.Errorf("SetInformationJobObject: %w", err), job.Close())
	}
	return job, nil
}
