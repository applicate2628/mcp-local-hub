package process

import "errors"

// ErrProcessAlreadyExited marks a terminate request that lost a race
// because the target PID was already gone by the time the signal ran.
var ErrProcessAlreadyExited = errors.New("process already exited")
