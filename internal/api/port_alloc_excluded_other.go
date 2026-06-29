//go:build !windows

package api

func osExcludedTCPPortRanges() ([]tcpPortRange, error) {
	return nil, nil
}
