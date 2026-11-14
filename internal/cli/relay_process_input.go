package cli

import (
	"context"
	"errors"
	"os"
	"sync/atomic"

	"github.com/spf13/cobra"
)

// BindProcessRelayInput grants only the relay command authority over this
// process's inherited input. Call from the executable bootstrap before its
// single Execute, not from an embedding/library caller sharing that input.
// Binding performs no I/O, so other commands and --help do not depend on stdin
// being usable. A later relay.SetIn replaces this grant with borrowed input.
func BindProcessRelayInput(root *cobra.Command, input *os.File) error {
	if root == nil || input == nil {
		return errors.New("relay process input: root and inherited file are required")
	}
	for _, command := range root.Commands() {
		if command.Name() == "relay" {
			command.SetIn(&relayProcessInput{file: input})
			return nil
		}
	}
	return errors.New("relay process input: relay command is not registered")
}

// This private wrapper records a bootstrap grant. Method availability on a
// generic io.Reader/io.Closer is deliberately not used to infer ownership.
type relayProcessInput struct {
	file         *os.File
	interrupting atomic.Bool
}

func (r *relayProcessInput) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	if r.interrupting.Load() && errors.Is(err, os.ErrClosed) {
		err = context.Canceled
	}
	return n, err
}

func (r *relayProcessInput) interrupt() error {
	r.interrupting.Store(true)
	return r.file.Close()
}
