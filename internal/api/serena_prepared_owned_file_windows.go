//go:build windows

package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// Only the original live generation supplies this reader. Reopening its path
// would conflict with its retained DELETE handle; sharing delete globally
// would weaken the independent reader's ancestor protection.
func preparedSerenaOwnedGeneration(ctx context.Context, tree *SerenaOwnedTree) (*windowsSerenaOwnedTree, string, error) {
	owner, err := preparedSerenaOwnedGenerationOwner(ctx, tree)
	if err != nil {
		return nil, "", err
	}
	root, err := tree.WorkingDirectory(ctx)
	return owner, root, err
}

func preparedSerenaOwnedGenerationOwner(ctx context.Context, tree *SerenaOwnedTree) (*windowsSerenaOwnedTree, error) {
	if ctx == nil {
		return nil, errors.New("SERENA_PREPARED_OWNED_FILE_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tree == nil || tree.request.Kind != "generation" {
		return nil, errors.New("SERENA_PREPARED_GENERATION_UNQUALIFIED")
	}
	owner, ok := tree.platform.(*windowsSerenaOwnedTree)
	if !ok || owner == nil || owner.readExisting {
		return nil, errors.New("SERENA_PREPARED_ORIGINAL_GENERATION_REQUIRED")
	}
	if owner.request.Namespace != tree.request.Namespace || owner.request.PackageIdentitySHA256 != tree.request.PackageIdentitySHA256 || owner.request.OperationID != tree.request.OperationID || owner.request.GenerationID != tree.request.GenerationID || owner.request.Kind != tree.request.Kind || owner.request.Limits != tree.request.Limits || !owner.request.AbsoluteCleanupDeadline.Equal(tree.request.AbsoluteCleanupDeadline) {
		return nil, errors.New("SERENA_PREPARED_GENERATION_BINDING_DRIFT")
	}
	if owner.closeResult.Attempted || owner.removeAttempted || owner.root == nil || owner.parent == nil || owner.root.close.Attempted || owner.parent.close.Attempted {
		return nil, errors.New("SERENA_TREE_ROOT_UNAVAILABLE")
	}
	if owner.inventoryCloseError != nil {
		return nil, owner.inventoryCloseError
	}
	return owner, nil
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
	if tree == nil || tree.request.Kind != "generation" || tree.request.Namespace != request.Namespace || tree.request.PackageIdentitySHA256 != key || tree.request.OperationID != request.OperationID || tree.request.GenerationID != request.GenerationID || tree.request.Limits != request.OwnedTreeLimits || !tree.request.AbsoluteCleanupDeadline.Equal(request.AbsoluteCleanupDeadline) {
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

// One bounded read transaction retains the original generation and policy.
// Results stay private until the same full namespace/source validation succeeds
// again. Rehashing the publisher image per member would consume the work bound.
func qualifyPreparedSerenaOwnedFiles(ctx context.Context, tree *SerenaOwnedTree, paths []string, capBytes int64) (map[string]PreparedSerenaFile, error) {
	owner, root, err := preparedSerenaOwnedGeneration(ctx, tree)
	if err != nil {
		return nil, err
	}
	policy, creation, parent, retained := owner.parentPolicy, owner.creation, owner.parent, owner.root
	request := owner.request
	files, err := readPreparedSerenaOwnedFiles(ctx, tree, owner, root, paths, capBytes)
	if err != nil {
		return nil, err
	}
	current, path, err := preparedSerenaOwnedGeneration(ctx, tree)
	if err != nil {
		return nil, err
	}
	if current != owner || path != root || owner.parentPolicy != policy || owner.creation != creation || owner.parent != parent || owner.root != retained || owner.request.Namespace != request.Namespace || owner.request.PackageIdentitySHA256 != request.PackageIdentitySHA256 || owner.request.OperationID != request.OperationID || owner.request.GenerationID != request.GenerationID || owner.request.Limits != request.Limits || !owner.request.AbsoluteCleanupDeadline.Equal(request.AbsoluteCleanupDeadline) {
		return nil, errors.New("SERENA_PREPARED_GENERATION_BINDING_DRIFT")
	}
	return files, nil
}

// Native traversal only, not namespace admission. The caller fully validates
// the original owner before and after this body; no capability escapes it.
func readPreparedSerenaOwnedFiles(ctx context.Context, tree *SerenaOwnedTree, owner *windowsSerenaOwnedTree, root string, paths []string, capBytes int64) (map[string]PreparedSerenaFile, error) {
	current, err := preparedSerenaOwnedGenerationOwner(ctx, tree)
	if err != nil {
		return nil, err
	}
	if current != owner || owner.parentPolicy == nil || owner.parentPolicy.policy == nil || !owner.request.Limits.valid() || len(paths) == 0 || capBytes <= 0 {
		return nil, errors.New("SERENA_PREPARED_OWNED_FILE_BATCH_INPUT_INVALID")
	}
	if _, finite := ctx.Deadline(); !finite {
		return nil, errors.New("SERENA_PREPARED_GENERATION_WORK_DEADLINE_INVALID")
	}
	policy, creation, parent, retained := owner.parentPolicy, owner.creation, owner.parent, owner.root
	parentIdentity, rootIdentity := owner.parentIdentity, owner.rootIdentity
	request := owner.request
	if root != filepath.Join(request.Namespace, request.PackageIdentitySHA256, "generations", owner.rootName) {
		return nil, errors.New("SERENA_PREPARED_GENERATION_BINDING_DRIFT")
	}
	files := make(map[string]PreparedSerenaFile)
	var pathBytes, logicalBytes int64
	for _, path := range paths {
		current, err = preparedSerenaOwnedGenerationOwner(ctx, tree)
		if err != nil {
			return nil, err
		}
		if current != owner || owner.parentPolicy != policy || owner.creation != creation || owner.parent != parent || owner.root != retained || owner.request.Namespace != request.Namespace || owner.request.PackageIdentitySHA256 != request.PackageIdentitySHA256 || owner.request.OperationID != request.OperationID || owner.request.GenerationID != request.GenerationID || owner.request.Limits != request.Limits || !owner.request.AbsoluteCleanupDeadline.Equal(request.AbsoluteCleanupDeadline) {
			return nil, errors.New("SERENA_PREPARED_GENERATION_BINDING_DRIFT")
		}
		if policy.policy.view.EnvIdentitySHA256 != StateReadPolicyEnvironmentIdentity() {
			return nil, errors.New("SERENA_PREPARED_GENERATION_POLICY_DRIFT")
		}
		p, identityErr := serenaTreeHandleIdentity(parent.handle, nil)
		if identityErr != nil {
			return nil, identityErr
		}
		if !p.Directory || p.Device != parentIdentity.Device || p.FileID != parentIdentity.FileID {
			return nil, errors.New("SERENA_TREE_PARENT_DRIFT")
		}
		r, identityErr := serenaTreeHandleIdentity(retained.handle, nil)
		if identityErr != nil {
			return nil, identityErr
		}
		if !r.Directory || r.Device != rootIdentity.Device || r.FileID != rootIdentity.FileID {
			return nil, errors.New("SERENA_TREE_ROOT_DRIFT")
		}
		if _, alreadyRead := files[path]; alreadyRead {
			continue
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New("SERENA_PREPARED_FILE_OUTSIDE_GENERATION")
		}
		var encodedBytes int64
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			if !serenaTreeName(part) {
				return nil, errors.New("SERENA_PREPARED_FILE_COMPONENT_INVALID")
			}
			encodedBytes += int64(len(utf16.Encode([]rune(part)))) * 2
		}
		// Match inventory's unique-entry and relative UTF-16 component budget.
		if len(files) >= request.Limits.Entries || encodedBytes > request.Limits.EncodedPathBytes-pathBytes {
			return nil, errors.New("SERENA_PREPARED_FILE_BATCH_LIMIT")
		}
		actual, raw, readErr := qualifyPreparedSerenaFileBeneath(ctx, root, retained.handle, path, capBytes, request.Limits)
		var closeErr *preparedSerenaFileCloseError
		if errors.As(readErr, &closeErr) {
			owner.inventoryCloseError = errors.Join(owner.inventoryCloseError, readErr)
		}
		if readErr != nil {
			return nil, readErr
		}
		if int64(len(raw)) > request.Limits.LogicalBytes-logicalBytes {
			return nil, errors.New("SERENA_PREPARED_FILE_BATCH_LIMIT")
		}
		pathBytes += encodedBytes
		logicalBytes += int64(len(raw))
		files[path] = actual
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return files, nil
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
