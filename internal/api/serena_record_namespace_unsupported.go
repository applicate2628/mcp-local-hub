//go:build !windows

package api

import (
	"context"
	"errors"
)

func acquireSerenaRecordNamespacePlatform(context.Context, PreparedSerenaRecordIO, string, string) (*SerenaRecordNamespaceObservation, error) {
	return nil, errors.New("SERENA_RECORD_NAMESPACE_PLATFORM_UNQUALIFIED")
}
func acquireSerenaRecordNamespaceReadPlatform(context.Context, PreparedSerenaRecordIO, string, string) (*SerenaRecordNamespaceObservation, error) {
	return nil, errors.New("SERENA_RECORD_NAMESPACE_PLATFORM_UNQUALIFIED")
}
func acquireSerenaOperationNamespacePlatform(context.Context, PreparedSerenaRecordIO, string, string, string, bool) (*SerenaRecordNamespaceObservation, error) {
	return nil, errors.New("SERENA_OPERATION_NAMESPACE_PLATFORM_UNQUALIFIED")
}

func acquireSupervisorStateNamespacePlatform(context.Context, *SupervisorStateNamespaceObservation) (serenaRecordNamespacePlatform, error) {
	return nil, errors.New("SUPERVISOR_NAMESPACE_PLATFORM_UNQUALIFIED")
}
