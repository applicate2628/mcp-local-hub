//go:build !windows

package api

import (
	"context"
	"errors"
)

func acquirePreparedSerenaBuildConstraint(context.Context, *SerenaOwnedTree) (*preparedSerenaBuildConstraint, error) {
	return nil, errors.New("SERENA_BUILD_CONSTRAINT_PLATFORM_UNSUPPORTED")
}
func beginPreparedSerenaBuildConstraintUse(context.Context, *preparedSerenaBuildConstraint) error {
	return errors.New("SERENA_BUILD_CONSTRAINT_PLATFORM_UNSUPPORTED")
}
func finishPreparedSerenaBuildConstraintUse(*preparedSerenaBuildConstraint, preparedSerenaCommandResult) error {
	return errors.New("SERENA_BUILD_CONSTRAINT_PLATFORM_UNSUPPORTED")
}
func closePreparedSerenaBuildConstraint(constraint *preparedSerenaBuildConstraint) error {
	if constraint == nil {
		return nil
	}
	return errors.New("SERENA_BUILD_CONSTRAINT_PLATFORM_UNSUPPORTED")
}
