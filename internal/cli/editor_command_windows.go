//go:build windows

package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

const WindowsConsoleChildRequiresGUIID = "E_WINDOWS_CONSOLE_CHILD_REQUIRES_GUI"

type windowsConsoleChildRequiresGUIError struct {
	editor string
	cause  error
}

func (e *windowsConsoleChildRequiresGUIError) Error() string {
	return fmt.Sprintf("%s: editor %q must be a Windows GUI executable: %v", WindowsConsoleChildRequiresGUIID, e.editor, e.cause)
}

func (e *windowsConsoleChildRequiresGUIError) Unwrap() error { return e.cause }
func (e *windowsConsoleChildRequiresGUIError) FailureID() string {
	return WindowsConsoleChildRequiresGUIID
}

func newEditorCommand(editor, path string) (*exec.Cmd, error) {
	resolved, err := exec.LookPath(editor)
	if err != nil {
		return nil, &windowsConsoleChildRequiresGUIError{editor: editor, cause: err}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, &windowsConsoleChildRequiresGUIError{editor: editor, cause: err}
	}
	if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = evaluated
	}
	if err := binaryadmission.AdmitWindowsGUI(resolved); err != nil {
		return nil, &windowsConsoleChildRequiresGUIError{editor: editor, cause: err}
	}
	cmd := exec.Command(resolved, path)
	process.NoConsole(cmd)
	return cmd, nil
}
