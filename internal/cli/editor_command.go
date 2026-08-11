package cli

import (
	"io"
	"os/exec"
)

var editorCommandRun = func(cmd *exec.Cmd) error { return cmd.Run() }

func runEditorForFile(editor, path string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd, err := newEditorCommand(editor, path)
	if err != nil {
		return err
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return editorCommandRun(cmd)
}
