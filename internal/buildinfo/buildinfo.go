// Package buildinfo holds version metadata baked into the mcphub
// binary at build time via ldflags. Exposed as a tiny standalone
// package so the cli (`mcphub version` command) and the gui
// (`/api/version` handler) can both consume it without one importing
// the other. cmd/mcphub/main.go calls Set() before cobra dispatch.
//
// Defaults ("dev" / "unknown") apply to `go run` invocations and to
// `go build` without the build scripts (build.sh / build.ps1 inject
// real values).
package buildinfo

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Set is called once from main.main() before any consumer reads the
// values. Empty inputs are ignored so partial overrides keep their
// existing default. Concurrent calls are not expected; the entire
// program runs sequentially through main before any goroutines
// could read these values.
func Set(v, c, d string) {
	if v != "" {
		version = v
	}
	if c != "" {
		commit = c
	}
	if d != "" {
		date = d
	}
}

// Get returns the current build metadata. Always returns the
// current values; callers requiring a snapshot must copy at the
// call site.
func Get() (v, c, d string) {
	return version, commit, date
}
