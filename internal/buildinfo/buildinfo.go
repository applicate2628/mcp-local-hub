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

import (
	"errors"
	"strings"
	"time"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
	// Supplied only by the marked builder after source-table verification.
	publisherSourceEpoch = ""
)

var ErrPublisherSourceUnmarked = errors.New("PUBLISHER_SOURCE_EPOCH_UNMARKED")
var ErrPublisherSourceMismatch = errors.New("PUBLISHER_SOURCE_EPOCH_MISMATCH")

type publisherSourceIssuer struct{ marker byte }
var compiledSourceIssuer = &publisherSourceIssuer{marker: 1}

// PublisherSourceObservation is issued by the compiled metadata owner only.
// The cooperative source association is not a signature or memory attestation.
type PublisherSourceObservation struct {
	issuer *publisherSourceIssuer
	epoch, version, commit, date string
}

func validPublisherHex(value string, min, max int) bool {
	if len(value) < min || len(value) > max { return false }
	for _, c := range value { if (c < '0' || c > '9') && (c < 'a' || c > 'f') { return false } }
	return true
}

func CurrentPublisherSource() (*PublisherSourceObservation, error) {
	if !validPublisherHex(publisherSourceEpoch, 64, 64) || version == "" || version == "dev" || version == "unknown" || strings.TrimSpace(version) != version || !validPublisherHex(commit, 7, 64) {
		return nil, ErrPublisherSourceUnmarked
	}
	parsed, err := time.Parse("2006-01-02T15:04:05Z", date)
	if err != nil || parsed.Format("2006-01-02T15:04:05Z") != date { return nil, ErrPublisherSourceUnmarked }
	return &PublisherSourceObservation{compiledSourceIssuer, publisherSourceEpoch, version, commit, date}, nil
}

func (o *PublisherSourceObservation) Epoch() string {
	if o == nil { return "" }
	return o.epoch
}

func (o *PublisherSourceObservation) Validate(expectedEpoch string) error {
	if o == nil || o.issuer != compiledSourceIssuer { return ErrPublisherSourceUnmarked }
	current, err := CurrentPublisherSource()
	if err != nil { return err }
	if !validPublisherHex(expectedEpoch, 64, 64) || o.epoch != expectedEpoch || o.epoch != current.epoch || o.version != current.version || o.commit != current.commit || o.date != current.date { return ErrPublisherSourceMismatch }
	return nil
}

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
