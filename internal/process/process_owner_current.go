package process

import "errors"

// ErrProcessOwnerUnsupported reports that the current platform has no
// trustworthy current-user ownership proof for an arbitrary process.
var ErrProcessOwnerUnsupported = errors.New("process owner verification is unsupported on this platform")
