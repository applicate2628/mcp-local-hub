//go:build !windows

package cli

import "io"

func canonicalizeProductToTarget(w io.Writer, src, target string) error {
	return canonicalizeSingleBinaryToTarget(w, src, target)
}
