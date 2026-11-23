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

func qualifyPreparedSerenaOwnedFile(context.Context, *SerenaOwnedTree, string, int64) (PreparedSerenaFile, []byte, error) {
	return PreparedSerenaFile{}, nil, errors.New("SERENA_PREPARED_FILE_PLATFORM_UNQUALIFIED")
}
func revalidatePreparedSerenaOwnedFile(context.Context, *SerenaOwnedTree, PreparedSerenaFile) error {
	return errors.New("SERENA_PREPARED_FILE_PLATFORM_UNQUALIFIED")
}
func validatePreparedSerenaGeneration(context.Context, *SerenaOwnedTree, PreparedSerenaRequest, PreparedSerenaFile) error {
	return errors.New("SERENA_PREPARED_FILE_PLATFORM_UNQUALIFIED")
}
func qualifyPreparedSerenaOwnedFiles(context.Context, *SerenaOwnedTree, []string, int64) (map[string]PreparedSerenaFile, error) {
	return nil, errors.New("SERENA_PREPARED_FILE_PLATFORM_UNQUALIFIED")
}
