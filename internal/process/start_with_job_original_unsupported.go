//go:build !windows

package process

import (
	"os"
	"os/exec"
)

func StartWithJobFilesOriginal(*Job, *exec.Cmd, *os.File, *os.File, *os.File) (*AtCreateJobProcess, error) {
	return nil, ErrContainmentUnavailable
}
