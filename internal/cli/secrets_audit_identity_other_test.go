//go:build !windows && !unix

package cli

import (
	"errors"
	"os"
)

func observeConcurrentSecretsObject(string, bool) (concurrentSecretsObjectIdentity, os.FileInfo, error) {
	return concurrentSecretsObjectIdentity{}, nil, errors.New("concurrent secrets fixture file identity is unavailable on this platform")
}
