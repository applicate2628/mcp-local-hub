//go:build !windows

package api

import (
	"context"
	"errors"
)

func acquireSerenaTreePlatform(context.Context, SerenaOwnedTreeRequest) (*SerenaOwnedTree, error) {
	return nil, errors.New("SERENA_TREE_PLATFORM_UNSUPPORTED")
}

func acquireSerenaPublishedGenerationPlatform(context.Context, SerenaOwnedTreeRequest) (serenaTreePlatform, error) {
	return nil, errors.New("SERENA_TREE_PLATFORM_UNSUPPORTED")
}
