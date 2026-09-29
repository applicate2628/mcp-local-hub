//go:build !windows

package sonnet

func defaultRegistryRoots() (map[string]string, error) { return map[string]string{}, nil }
