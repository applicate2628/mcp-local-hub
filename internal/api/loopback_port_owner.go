package api

// LoopbackPortOwnerPID resolves the PID that owns the LISTENING socket on
// 127.0.0.1:<port>. Windows uses the existing netstat-backed owner lookup;
// non-Windows currently returns the platform fail-closed error from the
// implementation stub.
func LoopbackPortOwnerPID(port int) (int, bool, error) {
	return loopbackPortOwnerPID(port)
}
