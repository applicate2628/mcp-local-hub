# Fresh native outer receiving on Windows

This isolates the controller boundary on exact Source e66f5c6f and the candidate sealed in 8b1d2729. The candidate Go files matched the published SHA-256 table. Only the documented scratch-import placeholder was substituted. The fixture is separate from runtime and keeps the adapter disabled.

Go 1.27.4, Windows/amd64: the focused test with the race detector ran twice. Both parents and all eight subcases passed. Real local children and the production stream observer supplied original receipts. No original objects from the prior failed campaign or Serena issuer were opened.

- A delayed reader stays pending after native EOF. Borrowed streams stay open until the controller receives the original return.
- A test-owned child waits at a temporary-file barrier and exits normally after release. The reader releases separately; original exit 0, native EOF, and physical completion are checked.
- A fixture consumer returns an error after actual EOF. The error remains distinct from physical completion.
- An injected record of a prior close refusal does not retry or close the sink. It is not an OS-level close error.

To reproduce, stage the candidate controller and owner packages under .scratch/riff-fresh-receiving-v1 on the exact Source using that scratch import name, then copy the fixture as a Go test into controller. Run the focused test, not the controller executable:

    go test -race -json -count 2 -timeout 120s -run '^TestRiffFreshNativeOuterReceiving$' ./.scratch/riff-fresh-receiving-v1/controller

This is native proof for the outer command/reader-to-controller boundary only. The bound GUI replay and successor through the external adapter, Serena migration, installation, and the complete suites remain unqualified. The fixture grants neither qualification nor access to prior captures.
