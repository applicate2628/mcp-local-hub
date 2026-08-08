//go:build !windows && !linux

package process

// Other POSIX targets retain their existing group-only settlement contract:
// this correction does not infer a portable zombie-state source.
func platformContainedGroupClassifier() posixGroupClassifier { return nil }
