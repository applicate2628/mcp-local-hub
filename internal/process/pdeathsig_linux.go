//go:build linux

package process

import (
	"os/exec"
	"syscall"
)

// SetParentDeathSignal configures cmd so the kernel delivers SIGKILL
// to the IMMEDIATE child when the spawning parent process dies. Must
// be called BEFORE cmd.Start() because Pdeathsig is applied between
// fork and exec. If mcphub is force-killed without running any
// cleanup, the kernel signals the direct child.
//
// SCOPE — best-effort direct-child mitigation, NOT full descendant
// tree protection. The signal is delivered to the immediate child
// only. For wrappers like uvx/npx the practical effect depends on
// whether the wrapper exec()s into the real server (then SIGKILL
// reaches the right process — typical case) or stays alive as a
// shim with the server forked underneath (then SIGKILL kills only
// the shim and leaves the grandchild as an orphan). This is a
// strictly weaker contract than the Windows Job Object KILL_ON_
// JOB_CLOSE in jobobject_windows.go, which does cascade to the
// entire tree via kernel-tracked job membership. Codex CLI review,
// 2026-05-01.
//
// Caveat: Linux prctl(PR_SET_PDEATHSIG) tracks the parent THREAD,
// not the parent PROCESS. If the Go scheduler retires the OS thread
// that called clone() before the parent process actually exits, the
// child gets a spurious early SIGKILL. Go's runtime typically keeps
// OS threads alive for the process lifetime via M-pool reuse, so
// the practical risk is low — but not guaranteed by any contract.
// If the runtime ever changes thread-retirement policy, tests like
// daemon.TestStdioHost_StartStop may flap.
//
// Robust Linux solution would be cgroups (cgroup.kill +
// memory.oom.group=1) or running under systemd-scope, both of
// which require privileged setup. Tracked under F2/F3 as part of
// `mcphub setup --server` for headless Linux deployments.
func SetParentDeathSignal(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
