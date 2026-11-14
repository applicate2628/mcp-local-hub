//go:build windows

package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Only the original live generation supplies this reader. Reopening its path
// would conflict with its retained DELETE handle; sharing delete globally
// would weaken the independent reader's ancestor protection.
func preparedSerenaOwnedGeneration(ctx context.Context, tree *SerenaOwnedTree) (*windowsSerenaOwnedTree, string, error) {
	if ctx == nil {
		return nil, "", errors.New("SERENA_PREPARED_OWNED_FILE_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if tree == nil || tree.request.Kind != "generation" {
		return nil, "", errors.New("SERENA_PREPARED_GENERATION_UNQUALIFIED")
	}
	owner, ok := tree.platform.(*windowsSerenaOwnedTree)
	if !ok || owner == nil || owner.readExisting {
		return nil, "", errors.New("SERENA_PREPARED_ORIGINAL_GENERATION_REQUIRED")
	}
	if owner.request.Namespace != tree.request.Namespace || owner.request.PackageIdentitySHA256 != tree.request.PackageIdentitySHA256 || owner.request.OperationID != tree.request.OperationID || owner.request.GenerationID != tree.request.GenerationID || owner.request.Kind != tree.request.Kind || owner.request.Limits != tree.request.Limits {
		return nil, "", errors.New("SERENA_PREPARED_GENERATION_BINDING_DRIFT")
	}
	if owner.closeResult.Attempted || owner.removeAttempted || owner.root == nil || owner.parent == nil || owner.root.close.Attempted || owner.parent.close.Attempted {
		return nil, "", errors.New("SERENA_TREE_ROOT_UNAVAILABLE")
	}
	if owner.inventoryCloseError != nil {
		return nil, "", owner.inventoryCloseError
	}
	root, err := tree.WorkingDirectory(ctx)
	return owner, root, err
}

func validatePreparedSerenaGeneration(ctx context.Context, tree *SerenaOwnedTree, request PreparedSerenaRequest, root PreparedSerenaFile) error {
	if ctx == nil {
		return errors.New("SERENA_PREPARED_OWNED_FILE_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, defined := ctx.Deadline()
	if !defined || deadline.After(request.WorkDeadline) {
		return errors.New("SERENA_PREPARED_GENERATION_WORK_DEADLINE_INVALID")
	}
	key, err := request.Identity()
	if err != nil {
		return err
	}
	if tree == nil || tree.request.Kind != "generation" || tree.request.Namespace != request.Namespace || tree.request.PackageIdentitySHA256 != key || tree.request.OperationID != request.OperationID || tree.request.GenerationID != request.GenerationID || tree.request.Limits != request.OwnedTreeLimits {
		return errors.New("SERENA_PREPARED_GENERATION_BINDING_DRIFT")
	}
	owner, path, err := preparedSerenaOwnedGeneration(ctx, tree)
	if err != nil {
		return err
	}
	if !preparedSerenaFile(root) || root.Path != path || root.Device != owner.rootIdentity.Device || root.FileID != owner.rootIdentity.FileID {
		return errors.New("SERENA_PREPARED_GENERATION_ROOT_DRIFT")
	}
	return nil
}

func qualifyPreparedSerenaOwnedFile(ctx context.Context, tree *SerenaOwnedTree, path string, capBytes int64) (PreparedSerenaFile, []byte, error) {
	owner, root, err := preparedSerenaOwnedGeneration(ctx, tree)
	if err != nil {
		return PreparedSerenaFile{}, nil, err
	}
	file, raw, err := qualifyPreparedSerenaFileBeneath(ctx, root, owner.root.handle, path, capBytes, owner.request.Limits)
	var closeErr *preparedSerenaFileCloseError
	if errors.As(err, &closeErr) {
		// A failed close remains an obligation of the original tree owner.
		owner.inventoryCloseError = errors.Join(owner.inventoryCloseError, err)
	}
	return file, raw, err
}

func revalidatePreparedSerenaOwnedFile(ctx context.Context, tree *SerenaOwnedTree, expected PreparedSerenaFile) error {
	actual, _, err := qualifyPreparedSerenaOwnedFile(ctx, tree, expected.Path, 64<<20)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("SERENA_PREPARED_FILE_IDENTITY_DRIFT")
	}
	return nil
}

// Native traversal primitive, not namespace admission. The caller keeps the
// original root alive; only descendants opened here are closed here.
func qualifyPreparedSerenaFileBeneath(ctx context.Context, root string, retained windows.Handle, path string, capBytes int64, limits SerenaTreeLimits) (result PreparedSerenaFile, raw []byte, err error) {
	if ctx == nil || retained == 0 || retained == windows.InvalidHandle || !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(path) || filepath.Clean(path) != path || capBytes <= 0 || !limits.valid() {
		return result, nil, errors.New("SERENA_PREPARED_OWNED_FILE_INPUT_INVALID")
	}
	if err = ctx.Err(); err != nil {
		return result, nil, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return result, nil, errors.Join(err, errors.New("SERENA_PREPARED_FILE_OUTSIDE_GENERATION"))
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for _, name := range parts {
		if !serenaTreeName(name) {
			return result, nil, errors.New("SERENA_PREPARED_FILE_COMPONENT_INVALID")
		}
	}
	// Match the inventory's depth/open-handle reserve while pinning every
	// descendant name until the leaf observation and its close have completed.
	if len(parts)-1 > limits.Depth || len(parts)+3 > limits.OpenHandles {
		return result, nil, errors.New("SERENA_PREPARED_FILE_DEPTH_HANDLE_LIMIT")
	}
	parent := retained
	ancestors := make([]windows.Handle, 0, len(parts)-1)
	defer func() {
		for i := len(ancestors) - 1; i >= 0; i-- {
			err = errors.Join(err, preparedSerenaCloseFileHandle(ancestors[i]))
		}
	}()
	for _, name := range parts[:len(parts)-1] {
		if err = ctx.Err(); err != nil {
			return result, nil, err
		}
		directory := true
		next, openErr := ntOpenStateReadRelativeMode(parent, name, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, &directory, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil)
		if openErr != nil {
			return result, nil, openErr
		}
		ancestors = append(ancestors, next)
		parent = next
	}
	return qualifyPreparedSerenaLeaf(ctx, path, capBytes, false, parent, parts[len(parts)-1])
}
