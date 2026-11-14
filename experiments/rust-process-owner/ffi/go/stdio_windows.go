//go:build windows && amd64 && cgo

package caller

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Stdio grants one synchronous creation the use of three caller-owned files.
// All fields are required. The caller may close them after NewWithStdio returns.
// NewWithStdio never changes their inheritance flags or closes their handles.
// Files must not be concurrently used for I/O/seek while the child uses the same
// kernel object: DuplicateHandle shares file position, it does not copy data.
type Stdio struct{ Stdin, Stdout, Stderr *os.File }

// StdioCopyError records acquisition or one-shot close of a temporary copy.
// A close failure is NOT proof of resource release. It is never retried using
// a potentially reused numeric handle. A non-nil Owner still belongs to caller
// even when this error is returned after successful native creation.
type StdioCopyError struct {
	Operation string
	Stream    int
	Err       error
}

func (e *StdioCopyError) Error() string {
	return fmt.Sprintf("stdio %s slot %d: %v", e.Operation, e.Stream, e.Err)
}
func (e *StdioCopyError) Unwrap() error { return e.Err }

type stdioCopies [3]syscall.Handle

// duplicateStdio owns at most three copies. RawConn.Control retains each source
// through DuplicateHandle; no Fd-to-handle race or original flag mutation.
// Aliased sources deliberately get distinct handles to the SAME kernel object.
func duplicateStdio(s Stdio) (copies stdioCopies, err error) {
	sources := [3]*os.File{s.Stdin, s.Stdout, s.Stderr}
	for _, file := range sources {
		if file == nil {
			return copies, &CallError{argument, 0}
		}
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, copies.close())
		}
	}()
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return copies, err
	}
	for i, file := range sources {
		raw, e := file.SyscallConn()
		if e != nil {
			return copies, &StdioCopyError{"borrow", i, e}
		}
		var duplicateErr error
		e = raw.Control(func(handle uintptr) {
			duplicateErr = syscall.DuplicateHandle(process, syscall.Handle(handle), process, &copies[i], 0, true, syscall.DUPLICATE_SAME_ACCESS)
		})
		if e != nil || duplicateErr != nil {
			return copies, &StdioCopyError{"duplicate", i, errors.Join(e, duplicateErr)}
		}
	}
	return copies, nil
}
func (c *stdioCopies) close() error {
	var failures []error
	for i, handle := range c {
		c[i] = 0 // consume once; even an error does not authorize stale-handle retry
		if handle != 0 {
			if e := syscall.CloseHandle(handle); e != nil {
				failures = append(failures, &StdioCopyError{"close", i, e})
			}
		}
	}
	return errors.Join(failures...)
}

// NewWithStdio accepts non-inheritable Go files through short-lived inheritable
// copies, plus a full explicit environment. A non-nil Owner must be settled and
// released even if a late reporting/copy-close error accompanies it. This is not
// an I/O pump, asynchronous cancellation API or system-wide inheritance lock.
func (l *Library) NewWithStdio(program, cwd string, args []string, environment []EnvironmentEntry, stdio Stdio) (*Owner, Outcome, error) {
	return l.newRequest(program, cwd, args, 1, environment, true, &stdio)
}
