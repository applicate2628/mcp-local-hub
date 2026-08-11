//go:build !windows

package cli

import "os/exec"

func newEditorCommand(editor, path string) (*exec.Cmd, error) {
	return exec.Command(editor, path), nil
}
