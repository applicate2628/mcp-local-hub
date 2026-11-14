//go:build windows && amd64 && cgo

package caller

// EnvironmentEntry is an ordinary Windows name/value. NewWithEnvironment
// accepts a complete already Windows-ordinal-ignore-case-sorted, unique set,
// not an overlay. Empty (including nil) means empty, never inherited. Rust
// validates name/order semantics with the existing native LaunchContext.
// Input strings must be valid UTF-8, have no NUL and fit the explicit budgets;
// they are copied into C memory and then into Rust before the call returns.
// Caller must not mutate entries concurrently with the call. No global parent
// environment mutation, hidden SDK/PATH/SystemRoot merge or secret logging.
type EnvironmentEntry struct{ Name, Value string }
