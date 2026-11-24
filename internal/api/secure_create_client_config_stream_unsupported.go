//go:build !windows

package api

import (
	"context"
	"errors"
	"io"
)

func SecureCreateOwnerOnlyFileStream(context.Context, string, func(io.Writer) error) (bool, error) {
	return false, errors.ErrUnsupported
}
