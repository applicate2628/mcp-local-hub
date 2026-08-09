//go:build !windows && !linux

package process

import "time"

// Other POSIX targets retain their existing group-only settlement contract:
// this correction does not infer a portable zombie-state source.
func platformContainedGroupClassifier() posixGroupClassifier { return nil }

func reapPlatformContainedGroup(time.Time, int) error { return nil }
