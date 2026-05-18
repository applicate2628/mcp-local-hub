//go:build darwin

package process

import (
	"os/exec"
	"testing"
)

func TestStartWithJobDarwin_SetsProcessGroup(t *testing.T) {
	cmd := exec.Command("/path/that/does/not/exist")
	_, _ = StartWithJob(nil, cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("StartWithJob must initialize SysProcAttr on Darwin")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("StartWithJob must set SysProcAttr.Setpgid on Darwin")
	}
}
