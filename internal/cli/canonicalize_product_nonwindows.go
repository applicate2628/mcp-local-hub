//go:build !windows

package cli

import (
	"context"
	"io"
)

func canonicalizeProductToTarget(w io.Writer, src, target string) error {
	return canonicalizeSingleBinaryToTarget(w, src, target)
}

func canonicalizeProductToTargetWithWindowsCanonicalEntryInput(_ context.Context, w io.Writer, src, target string, _ windowsCanonicalEntryCommandInput) error {
	return canonicalizeSingleBinaryToTarget(w, src, target)
}
