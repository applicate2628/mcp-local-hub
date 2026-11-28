//go:build !windows

package api

import (
	"context"
	"io"
	"os"
)

type productStateNamespaceNative struct{}

func openProductStateNamespace(context.Context, string) (*WindowsProductStateNamespace, error) {
	return nil, errHubRouteStateUnavailable
}
func (*productStateNamespaceNative) validate(context.Context) error {
	return errHubRouteStateUnavailable
}
func (*productStateNamespaceNative) close() error { return nil }
func (*productStateNamespaceNative) retain(error) {}
func (*productStateNamespaceNative) matchParent(context.Context, string) error {
	return errHubRouteStateUnavailable
}
func (*productStateNamespaceNative) openLock(context.Context, string, string, int) (*os.File, error) {
	return nil, errHubRouteStateUnavailable
}
func (*productStateNamespaceNative) read(context.Context, string) ([]byte, error) {
	return nil, errHubRouteStateUnavailable
}
func (*productStateNamespaceNative) withBoundFileReader(context.Context, string, func(io.Reader) error) error {
	return errHubRouteStateUnavailable
}
func (*productStateNamespaceNative) withHub(context.Context, func(*HubRouteState) error) error {
	return errHubRouteStateUnavailable
}
