//go:build !windows

package daemon

import "fmt"

func productionLaunchCapabilityOps() launchCapabilityOps {
	return launchCapabilityOps{
		random32: func(*[32]byte) error { return fmt.Errorf("launch capability is Windows-only") },
		zero32:   func(*[32]byte) {},
		openPipe: func() (launchCapabilityPipe, error) { return nil, fmt.Errorf("launch capability is Windows-only") },
	}
}
