//go:build !windows

package process

func currentProcessKillOnCloseJob() (bool, error) { return false, nil }
