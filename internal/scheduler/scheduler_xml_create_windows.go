//go:build windows

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CreateXMLIfAbsent submits the complete XML with native TASK_CREATE semantics.
// Registration triggers are suppressed, but an enabled task's other triggers
// may still fire. Credentials and task-object permissions are not backed up by
// XML alone. The operation never replaces, deletes, enables, or runs a task.
func (w *windowsScheduler) CreateXMLIfAbsent(name string, rawXML []byte) error {
	if !validRootTaskName(name) {
		return fmt.Errorf("%w: invalid root task name", ErrTaskCorrupt)
	}
	prepared, err := prepareTaskXMLForImport(rawXML)
	if err != nil {
		return fmt.Errorf("%w: prepare task XML: %v", ErrTaskCorrupt, err)
	}
	hasContent := false
	for i := 2; i < len(prepared); i += 2 {
		unit := rune(prepared[i]) | rune(prepared[i+1])<<8
		if !unicode.IsSpace(unit) {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return fmt.Errorf("%w: empty task XML", ErrTaskCorrupt)
	}
	response, err := schedulerCOM(context.Background(), schedulerCOMRequest{
		Operation:  "create_xml_if_absent",
		Name:       name,
		XMLUTF16LE: prepared,
	})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return fmt.Errorf("create task XML: outcome uncertain: %w (phase %s, HRESULT 0x%08X)", err, response.Phase, response.HRESULT)
		}
		return fmt.Errorf("create task XML: %w (phase %s, HRESULT 0x%08X)", err, response.Phase, response.HRESULT)
	}
	return nil
}

// Accept a literal root name with at most one leading backslash. No cleanup or
// wildcard/path normalization is performed before sending the name to COM.
func validRootTaskName(name string) bool {
	leaf := strings.TrimPrefix(name, `\`)
	if !utf8.ValidString(name) || leaf == "" || leaf != strings.TrimSpace(leaf) || leaf == "." || leaf == ".." || strings.ContainsAny(leaf, `\/:*?"<>|`) {
		return false
	}
	for _, r := range leaf {
		if r < 0x20 {
			return false
		}
	}
	return true
}

var _ XMLCreator = (*windowsScheduler)(nil)
