//go:build !windows

package api

import (
	"context"
	"errors"
)

func restoreSupervisorIntentAbsencePlatform(context.Context, *SupervisorIntentAbsenceRestorationObservation) error {
	return errors.New("observed intent absence restoration unsupported")
}
