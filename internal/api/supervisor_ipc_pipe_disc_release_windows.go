//go:build windows && !test_state_path_env

package api

// testPipeDiscriminator (release build) is a no-op: it always returns "" so
// SupervisorIPCAddress falls through to the production SID-based pipe name.
// The per-test discriminator that reads MCPHUB_STATE_DIR_OVERRIDE is compiled
// ONLY into `test_state_path_env`-tagged test binaries — see
// supervisor_ipc_pipe_disc_testenv_windows.go. This guarantees no release
// client (mcphub status / GUI / stop / respawn) ever branches on that env
// var, so an operator who sets MCPHUB_STATE_DIR_OVERRIDE in a production shell
// cannot redirect the pipe away from the running supervisor (codex bot
// PR #264 P2).
func testPipeDiscriminator() string { return "" }
