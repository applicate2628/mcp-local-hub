//go:build windows

package cli

import (
	"fmt"
	"os"
)

// Only Windows pipe input has been natively qualified for this mechanism.
// A regular file or console keeps the existing non-interrupting behavior.
func processRelayInputInterrupt(input *relayProcessInput) (func() error, error) {
	info, err := input.file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect inherited relay input: %w", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return nil, nil
	}
	return input.interrupt, nil
}
