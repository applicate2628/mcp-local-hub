//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

// StartWithJobFilesOriginal extends the existing at-create builder and
// transfers its original handles before any post-create wrapper can fail.
// The caller owns all three files; no nil/partial standard-file set is valid.
func StartWithJobFilesOriginal(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (*AtCreateJobProcess, error) {
	return startWithJobFilesOriginalWithOptions(job, cmd, stdin, stdout, stderr, nil)
}

// StartWithJobFilesOriginalAdmitted retains the original suspended process and
// atomically assigned Job before admission. On failure the returned partial
// owner still owns every acquired native resource; this function does not reap.
// This direct starter is explicitly contextless; foreground composition passes
// its invocation context through the private admitted path below.
func StartWithJobFilesOriginalAdmitted(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File, admission ForegroundCreateAdmission) (*AtCreateJobProcess, error) {
	return startWithJobFilesOriginalAdmittedContext(nil, job, cmd, stdin, stdout, stderr, admission)
}

func startWithJobFilesOriginalAdmittedContext(ctx context.Context, job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File, admission ForegroundCreateAdmission) (*AtCreateJobProcess, error) {
	if admission == nil {
		return nil, startWithJobError(StartWithJobInvalid, errors.New("foreground creation admission required"))
	}
	return startWithJobFilesOriginalCore(job, cmd, stdin, stdout, stderr, nil, admission, ctx)
}

func startWithJobFilesOriginalWithOptions(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File, testOptions *startWithJobObserverOptions) (*AtCreateJobProcess, error) {
	return startWithJobFilesOriginalCore(job, cmd, stdin, stdout, stderr, testOptions, nil, nil)
}

func startWithJobFilesOriginalCore(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File, testOptions *startWithJobObserverOptions, admission ForegroundCreateAdmission, admissionContext context.Context) (*AtCreateJobProcess, error) {
	if job == nil || cmd == nil || stdin == nil || stdout == nil || stderr == nil {
		return nil, startWithJobError(StartWithJobInvalid, errors.New("original standard files required"))
	}
	child := &windowsContainedChild{cmd: cmd, job: job, observed: true, wrapperOwned: true}
	owner := &AtCreateJobProcess{child: child, observer: child, waitDone: make(chan struct{})}
	source, err := installOriginalMemberSource(job, func() (originalMemberRootBinding, error) {
		if child.creationError != nil {
			return originalMemberRootBinding{}, child.creationError
		}
		return originalMemberRootBinding{job: job, original: child.original, startedAt: child.creationTime}, nil
	})
	if err != nil {
		return owner, err
	}
	child.memberSource = source
	options := startWithJobObserverOptions{attributeListClosed: func(view ProcThreadAttributeListCloseObservation) { child.attributeClose = view }, attributeListOwner: func(owner *procThreadAttributeListObserved) { child.attributeOwner = owner }, childHandleAcquired: child.receiveInheritedHandle, originalChildHandleClosed: child.receiveInheritedClose, retainCreated: func(pi windows.ProcessInformation) {
		child.process, child.thread = pi.Process, pi.Thread
		child.original = containedJobOriginal{job: job, handle: pi.Process, pid: pi.ProcessId, atCreate: true}
		var created, exited, kernel, user windows.Filetime
		child.creationError = windows.GetProcessTimes(pi.Process, &created, &exited, &kernel, &user)
		if child.creationError == nil {
			if created.HighDateTime == 0 && created.LowDateTime == 0 {
				child.creationError = errors.New("original birth unavailable")
			} else {
				child.creationTime = time.Unix(0, created.Nanoseconds()).UTC().Format(time.RFC3339Nano)
				child.creationFILETIME = uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
			}
		}
	}}
	if admission != nil {
		options.resumeContext = admissionContext
		options.beforeResume = func() error {
			if child.creationError != nil {
				return child.creationError
			}
			return admission.AfterCreateBeforeResume(owner)
		}
	}
	if testOptions != nil {
		options.duplicateHandle = testOptions.duplicateHandle
		options.closeChildHandle = testOptions.closeChildHandle
		options.findProcess = testOptions.findProcess
		options.newAttributeList = testOptions.newAttributeList
		options.resumeThread = testOptions.resumeThread
	}
	_, err = startWithJobFilesObserved(job, cmd, stdin, stdout, stderr, &options)
	if child.thread != 0 {
		child.threadCloseAttempted = true
		child.threadCloseError = windows.CloseHandle(child.thread)
		if child.threadCloseError == nil {
			child.thread = 0
		}
	}
	return owner, errors.Join(err, child.creationError, child.threadCloseError, child.inheritedSettlementError())
}
