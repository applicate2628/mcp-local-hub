//go:build !windows

package daemon

import (
	"context"
	"os/exec"
)

func (h *StdioHost) startOriginal(_ context.Context, _ *exec.Cmd) (bool, error) { return false, nil }
