//go:build !windows

package api

import (
	"context"
	"errors"
)

type privateSerenaCacheSeedMaterialization struct {
	success bool
}

func (m *privateSerenaCacheSeedMaterialization) closeOriginal() error {
	return nil
}

func materializePrivateSerenaCacheSeed(context.Context, *preparedSerenaAcquisition) error {
	return errors.New("SERENA_PRIVATE_CACHE_SEED_PLATFORM_UNSUPPORTED")
}
