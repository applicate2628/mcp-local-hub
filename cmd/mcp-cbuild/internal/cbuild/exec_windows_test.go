//go:build windows

package cbuild

import (
	"os"
	"os/exec"
	"testing"
)

func TestProcGroupStartDisablesJobAfterOpenProcessFailure(t *testing.T) {
	pg := newProcGroup()
	pg.configure(&exec.Cmd{})
	if pg.job == 0 {
		t.Skip("Windows Job Objects unavailable")
	}
	defer pg.close()

	pg.start(&exec.Cmd{Process: &os.Process{Pid: -1}})

	pg.mu.Lock()
	job := pg.job
	pg.mu.Unlock()
	if job != 0 {
		t.Error("failed job assignment left a live empty job; kill would skip the direct-process fallback")
	}
}
