//go:build !windows

package cli

// Inherited Unix stdin is not necessarily registered with Go's poller.
// Preserve existing behavior until an OS-specific interruption mechanism is
// natively qualified; compilation or an os.Pipe test is not that evidence.
func processRelayInputInterrupt(_ *relayProcessInput) (func() error, error) {
	return nil, nil
}
