//go:build windows && !test_state_path_env

package tray

func startTestTrayControl(*trayChild) func() { return func() {} }
