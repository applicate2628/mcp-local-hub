//go:build !windows

package cli

import "testing"

func installEditorTestCommand(t *testing.T, script string, _ func(string) error) string {
	t.Helper()
	return script
}
