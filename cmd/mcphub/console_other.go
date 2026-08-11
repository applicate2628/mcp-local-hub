//go:build !windows

package main

import "mcp-local-hub/internal/cli"

// applyWindowsConsolePolicy is a signature-compatible no-op outside Windows.
func applyWindowsConsolePolicy(cli.WindowsConsolePolicy) (bool, error) { return false, nil }
