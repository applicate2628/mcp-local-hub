//go:build !windows

package daemon

import (
	"fmt"
	"os"
)

func productionLaunchCapabilityOps() launchCapabilityOps {
	return launchCapabilityOps{
		random32: func(*[32]byte) error { return fmt.Errorf("launch capability is Windows-only") },
		zero32:   func(*[32]byte) {},
		openPipe: func() (launchCapabilityPipe, error) { return nil, fmt.Errorf("launch capability is Windows-only") },
	}
}

func openCstDirectIdentityFile(string) (*os.File, error) {
	return nil, fmt.Errorf("cst-direct-v1 is Windows-only")
}
