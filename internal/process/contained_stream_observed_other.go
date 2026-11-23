//go:build !windows

package process

func observedContainedStreamDependencies() containedStreamDependencies {
	return defaultContainedStreamDependencies()
}
