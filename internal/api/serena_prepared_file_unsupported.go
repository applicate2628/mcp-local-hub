//go:build !windows

package api

import (
	"context"
	"errors"
)

func qualifyPreparedSerenaFile(context.Context, string, int64) (PreparedSerenaFile, []byte, error) {
	return PreparedSerenaFile{}, nil, errors.New("SERENA_PREPARED_FILE_PLATFORM_UNQUALIFIED")
}
func revalidatePreparedSerenaFile(context.Context, PreparedSerenaFile) error {
	return errors.New("SERENA_PREPARED_FILE_PLATFORM_UNQUALIFIED")
}
func qualifyPreparedSerenaDirectory(context.Context, string) (PreparedSerenaFile, error) {
	return PreparedSerenaFile{}, errors.New("SERENA_PREPARED_DIRECTORY_PLATFORM_UNQUALIFIED")
}

func preparedSerenaQualifiedFileMissing(error) bool { return false }
