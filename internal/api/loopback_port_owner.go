package api

// LoopbackPortOwnerPID resolves the PID that owns the LISTENING socket on
// 127.0.0.1:<port>. Windows uses the existing netstat-backed owner lookup;
// Linux maps /proc/net/tcp socket inodes back to /proc/<pid>/fd owners; other
// platforms fail closed until an OS-level owner proof is implemented.
func LoopbackPortOwnerPID(port int) (int, bool, error) {
	return loopbackPortOwnerPID(port)
}
