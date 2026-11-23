//go:build unix

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func observeConcurrentSecretsObject(path string, dir bool) (concurrentSecretsObjectIdentity, os.FileInfo, error) {
	var identity concurrentSecretsObjectIdentity
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return identity, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var info unix.Stat_t
	nativeErr := unix.Fstat(fd, &info)
	stat, statErr := file.Stat()
	if err := errors.Join(nativeErr, statErr, file.Close()); err != nil {
		return identity, nil, err
	}
	if stat.IsDir() != dir || (!dir && (!stat.Mode().IsRegular() || info.Nlink != 1)) {
		return identity, nil, errors.New("concurrent secrets fixture requires an ordinary object")
	}
	identity = concurrentSecretsObjectIdentity{Volume: uint64(info.Dev), File: uint64(info.Ino)}
	if identity.File == 0 {
		return identity, nil, errors.New("concurrent secrets fixture file identity is unavailable")
	}
	return identity, stat, nil
}
