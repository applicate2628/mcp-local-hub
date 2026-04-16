package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/atotto/clipboard"
	"golang.org/x/term"
)

// copyToClipboard places value on the OS clipboard.
// Non-Windows platforms also supported via github.com/atotto/clipboard.
func copyToClipboard(value string) error {
	return clipboard.WriteAll(value)
}

// promptHidden writes prompt to w and reads a line from stdin with input hidden.
func promptHidden(w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	b, err := term.ReadPassword(0) // fd 0 = stdin
	fmt.Fprintln(w)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
