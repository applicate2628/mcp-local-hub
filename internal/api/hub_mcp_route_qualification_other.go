//go:build !windows

package api

import "context"

type hubRouteNative struct{}

func observeHubRouteStateDirectory(context.Context, string) (ConfigParentDirectoryIdentityV1, error) {
	return ConfigParentDirectoryIdentityV1{}, errHubRouteStateUnavailable
}
func withHubRouteState(context.Context, string, ConfigParentDirectoryIdentityV1, func(*HubRouteState) error) error {
	return errHubRouteStateUnavailable
}
func (*hubRouteNative) read(string, int) (hubRouteLeaf, error) {
	return hubRouteLeaf{}, errHubRouteStateUnavailable
}
func (*hubRouteNative) checkAbsent(string) error { return errHubRouteStateUnavailable }
func (*hubRouteNative) publish(context.Context, string, hubRouteLeaf, []byte, int) error {
	return errHubRouteStateUnavailable
}
func (*hubRouteNative) validate() error { return errHubRouteStateUnavailable }

func (*hubRouteNative) lockIdentity() (ConfigParentDirectoryIdentityV1, error) {
	return ConfigParentDirectoryIdentityV1{}, errHubRouteStateUnavailable
}
func (*hubRouteNative) removeOwnedReload(hubRouteLeaf) error { return errHubRouteStateUnavailable }
func (*hubRouteNative) publishObserved(context.Context, string, hubRouteLeaf, []byte, int, *hubRouteLeaf) error {
	return errHubRouteStateUnavailable
}
