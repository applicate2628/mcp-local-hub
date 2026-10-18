//go:build windows

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type preparedSerenaFileCloseError struct{ cause error }

func (e *preparedSerenaFileCloseError) Error() string { return "SERENA_PREPARED_FILE_CLOSE_UNSETTLED" }
func (e *preparedSerenaFileCloseError) Unwrap() error { return e.cause }
func preparedSerenaQualifiedFileMissing(err error) bool {
	var closeErr *preparedSerenaFileCloseError
	return err != nil && !errors.As(err, &closeErr) && windowsAnchoredReadErrIsNotExist(err)
}
func preparedSerenaCloseFileHandle(handle windows.Handle) error {
	if err := windows.CloseHandle(handle); err != nil {
		return &preparedSerenaFileCloseError{cause: err}
	}
	return nil
}

// Qualification reads the bytes and identity from one no-follow leaf handle.
// Each ancestor is opened relative to its retained predecessor. This does not
// apply the application-state DACL policy to an installed third-party image.
func qualifyPreparedSerenaFile(ctx context.Context, path string, capBytes int64) (result PreparedSerenaFile, raw []byte, err error) {
	return qualifyPreparedSerenaPath(ctx, path, capBytes, false)
}
func qualifyPreparedSerenaDirectory(ctx context.Context, path string) (PreparedSerenaFile, error) {
	identity, _, err := qualifyPreparedSerenaPath(ctx, path, 1, true)
	return identity, err
}
func qualifyPreparedSerenaPath(ctx context.Context, path string, capBytes int64, directory bool) (result PreparedSerenaFile, raw []byte, err error) {
	if ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || capBytes <= 0 {
		return result, nil, errors.New("SERENA_PREPARED_FILE_INPUT_INVALID")
	}
	volume := filepath.VolumeName(path)
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(path, volume), string(filepath.Separator)), string(filepath.Separator))
	if len(parts) == 0 {
		return result, nil, errors.New("SERENA_PREPARED_FILE_LEAF_MISSING")
	}
	parent, err := ntOpenStateReadRootMode(volume+string(filepath.Separator), windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return result, nil, err
	}
	parentOwned := true
	defer func() {
		if parentOwned {
			err = errors.Join(err, preparedSerenaCloseFileHandle(parent))
		}
	}()
	for _, name := range parts[:len(parts)-1] {
		if !serenaTreeName(name) {
			return result, nil, errors.New("SERENA_PREPARED_FILE_COMPONENT_INVALID")
		}
		if err = ctx.Err(); err != nil {
			return result, nil, err
		}
		dir := true
		next, openErr := ntOpenStateReadRelativeMode(parent, name, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, &dir, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil)
		if openErr != nil {
			return result, nil, openErr
		}
		closeErr := preparedSerenaCloseFileHandle(parent)
		parent = next
		if closeErr != nil {
			return result, nil, closeErr
		}
	}
	name := parts[len(parts)-1]
	if !serenaTreeName(name) {
		return result, nil, errors.New("SERENA_PREPARED_FILE_COMPONENT_INVALID")
	}
	fileKind := directory
	access := uint32(windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES)
	leaf, err := ntOpenStateReadRelativeMode(parent, name, access, &fileKind, windows.FILE_OPEN, windows.FILE_SHARE_READ, nil)
	if err != nil {
		return result, nil, err
	}
	defer func() { err = errors.Join(err, preparedSerenaCloseFileHandle(leaf)) }()
	before, err := serenaTreeHandleIdentity(leaf, nil)
	if err != nil {
		return result, nil, err
	}
	if before.Directory != directory || before.Bytes < 0 || (!directory && before.Bytes > capBytes) {
		return result, nil, errors.New("SERENA_PREPARED_FILE_SIZE_INVALID")
	}
	if directory {
		after, checkErr := serenaTreeHandleIdentity(leaf, nil)
		if checkErr != nil || !sameSerenaTreeObject(before, after) {
			return result, nil, errors.Join(checkErr, errors.New("SERENA_PREPARED_DIRECTORY_IDENTITY_DRIFT"))
		}
		binding := []byte(before.Device + ":" + before.FileID)
		result = PreparedSerenaFile{Path: path, SHA256: preparedSerenaHash(binding), Device: before.Device, FileID: before.FileID}
		return result, nil, ctx.Err()
	}
	raw = make([]byte, 0, int(before.Bytes))
	buffer := make([]byte, 32768)
	for {
		if err = ctx.Err(); err != nil {
			return result, nil, err
		}
		var n uint32
		readErr := windows.ReadFile(leaf, buffer, &n, nil)
		if readErr != nil && !errors.Is(readErr, windows.ERROR_HANDLE_EOF) {
			return result, nil, readErr
		}
		if int64(len(raw))+int64(n) > capBytes {
			return result, nil, errors.New("SERENA_PREPARED_FILE_SIZE_INVALID")
		}
		raw = append(raw, buffer[:n]...)
		if n == 0 || errors.Is(readErr, windows.ERROR_HANDLE_EOF) {
			break
		}
	}
	after, err := serenaTreeHandleIdentity(leaf, nil)
	if err != nil {
		return result, nil, err
	}
	if !sameSerenaTreeObject(before, after) || int64(len(raw)) != before.Bytes {
		return result, nil, errors.New("SERENA_PREPARED_FILE_IDENTITY_DRIFT")
	}
	h := sha256.Sum256(raw)
	result = PreparedSerenaFile{Path: path, SHA256: hex.EncodeToString(h[:]), Device: before.Device, FileID: before.FileID}
	return result, raw, nil
}

func revalidatePreparedSerenaFile(ctx context.Context, expected PreparedSerenaFile) error {
	actual, _, err := qualifyPreparedSerenaFile(ctx, expected.Path, 64<<20)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("SERENA_PREPARED_FILE_IDENTITY_DRIFT")
	}
	return nil
}
