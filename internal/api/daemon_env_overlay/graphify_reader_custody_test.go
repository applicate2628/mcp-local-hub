package daemon_env_overlay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func TestObservedOverlayRetainsClosedOriginalAndPrivateFailureData(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")
	const raw = "version: 1\ndaemons:\n  synthetic-task:\n    env:\n      INNOCUOUS: synthetic-private-sentinel\n"
	path := writeOverlayFileDirect(t, dir, raw)
	observed, err := LoadObserved(path)
	if err != nil || observed == nil || len(observed.attempts) != 1 {
		t.Fatal("protected-file observation unavailable")
	}
	read := observed.attempts[0]
	if read.reader == nil || !read.opened || !read.closeAttempted || !read.closeDefined || read.closeError != nil {
		t.Fatal("original successful closure data missing")
	}
	statInfo, statErr := read.reader.Stat()
	if statErr == nil || statInfo != nil {
		var pathErr *os.PathError
		pathErrPresent := errors.As(statErr, &pathErr)
		var innerErrno syscall.Errno
		innerErrnoDefined := false
		if pathErrPresent && pathErr != nil {
			innerErrnoDefined = errors.As(pathErr.Err, &innerErrno)
		}
		t.Fatalf("retained object does not reflect successful completed original Close: statErrPresent=%t statInfoPresent=%t isErrClosed=%t pathErrPresent=%t innerErrnoDefined=%t innerErrnoCode=%d", statErr != nil, statInfo != nil, errors.Is(statErr, os.ErrClosed), pathErrPresent, innerErrnoDefined, uint64(innerErrno))
	}
	if !bytes.Equal(read.raw, []byte(raw)) || observed.overlay == nil || len(observed.overlay.Daemons) != 1 {
		t.Fatal("private original payload lost")
	}
	assertOverlayPrivateFormatting(t, observed)
	// Synthetic degraded observation DATA checks custody/privacy only. It does
	// not cause the original owner to fail Close and cannot count its calls.
	degraded := *observed
	degraded.attempts = append([]overlayReadAttempt(nil), observed.attempts...)
	degraded.attempts[0].closeError = errors.New("synthetic-private-sentinel-close-outcome")
	_, retainedErr := func() (*ReadObservation, error) {
		return &degraded, &observedReadFailure{observation: &degraded, cause: degraded.attempts[0].closeError}
	}()
	var failure *observedReadFailure
	if !errors.As(retainedErr, &failure) || failure.observation != &degraded || failure.observation.attempts[0].reader != read.reader || failure.observation.attempts[0].closeError != degraded.attempts[0].closeError {
		t.Fatal("dropped partial return lost original reader/outcome")
	}
	if _, err := failure.observation.Fingerprint(); err == nil {
		t.Fatal("failed closure data qualified an observation")
	}
	assertOverlayPrivateFormatting(t, failure.observation)
	encoded, err := json.Marshal(failure)
	if err != nil || string(encoded) != "{}" || strings.Contains(retainedErr.Error(), "synthetic-private-sentinel") {
		t.Fatal("private failure exposed original internals")
	}
}

func assertOverlayPrivateFormatting(t *testing.T, observation *ReadObservation) {
	t.Helper()
	for _, value := range []any{observation, *observation, ReadObservation{}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if fmt.Sprintf(verb, value) != "overlay read observation (private)" {
				t.Fatal("whole overlay formatting exposed a reversible representation")
			}
		}
	}
	var absent *ReadObservation
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if fmt.Sprintf(verb, absent) != "<nil>" {
			t.Fatal("typed-nil formatting unsafe")
		}
	}
}

type countedOverlayOriginal struct {
	reader     *bytes.Reader
	info       fs.FileInfo
	readError  error
	statError  error
	closeError error
	readCount  int
	statCount  int
	closeCount int
}

func (f *countedOverlayOriginal) Read(raw []byte) (int, error) {
	f.readCount++
	if f.readError != nil {
		return 0, f.readError
	}
	return f.reader.Read(raw)
}
func (f *countedOverlayOriginal) Stat() (fs.FileInfo, error) {
	f.statCount++
	return f.info, f.statError
}
func (f *countedOverlayOriginal) Close() error { f.closeCount++; return f.closeError }

func TestConsumeOverlayOriginalCloseAndEarlyFailureCustody(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")
	path := writeOverlayFileDirect(t, dir, "version: 1\ndaemons: {}\n")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	readSentinel := errors.New("synthetic original Read failure")
	statSentinel := errors.New("synthetic original Stat failure")
	closeSentinel := errors.New("synthetic original Close failure")
	openSentinel := errors.New("synthetic original Open failure")
	for _, inputs := range []struct{ read, stat, close, open error }{{}, {close: closeSentinel}, {read: readSentinel, close: closeSentinel}, {stat: statSentinel, close: closeSentinel}, {open: openSentinel, close: closeSentinel}} {
		original := &countedOverlayOriginal{reader: bytes.NewReader([]byte("version: 1\ndaemons: {}\n")), info: info, readError: inputs.read, statError: inputs.stat, closeError: inputs.close}
		attempt := consumeOverlayReadAttempt(path, original, inputs.open)
		if attempt.reader != original || !attempt.opened || !attempt.closeAttempted || !attempt.closeDefined || original.closeCount != 1 || attempt.closeError != inputs.close {
			t.Fatal("original acquired object/one-Close outcome lost")
		}
		if inputs.open != nil && (original.readCount != 0 || original.statCount != 0 || !errors.Is(attempt.primary, inputs.open)) {
			t.Fatal("partial acquisition read data or lost original error")
		}
		if inputs.open == nil && original.statCount == 0 {
			t.Fatal("raw owner did not Stat the acquired original")
		}
		if inputs.open == nil && inputs.stat == nil && original.readCount == 0 {
			t.Fatal("raw owner did not Read the acquired original")
		}
		for _, cause := range []error{inputs.read, inputs.stat} {
			if cause != nil && !errors.Is(attempt.primary, cause) {
				t.Fatal("primary original cause lost")
			}
		}
		observation, retainedErr := finishOverlayReadObservation([]overlayReadAttempt{attempt})
		if inputs.read == nil && inputs.stat == nil && inputs.close == nil && inputs.open == nil {
			if retainedErr != nil {
				t.Fatal(retainedErr)
			}
			if _, err := observation.Fingerprint(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := observation.Fingerprint(); err == nil {
			t.Fatal("failed original outcome qualified")
		}
		observation = nil
		var failure *observedReadFailure
		if !errors.As(retainedErr, &failure) || failure.observation == nil || len(failure.observation.attempts) != 1 || failure.observation.attempts[0].reader != original || failure.observation.attempts[0].primary != attempt.primary || failure.observation.attempts[0].closeError != attempt.closeError {
			t.Fatal("dropped result lost actual aggregate/original outcomes")
		}
		for _, cause := range []error{inputs.read, inputs.stat, inputs.close, inputs.open} {
			if cause != nil && !errors.Is(retainedErr, cause) {
				t.Fatal("private failure lost original cause")
			}
		}
		if retainedErr.Error() != "GRAPHIFY_OVERLAY_READ_UNQUALIFIED" || original.closeCount != 1 {
			t.Fatal("typed failure exposed cause or repeated Close")
		}
		assertOverlayPrivateFormatting(t, failure.observation)
	}
	// Ordered actual producer results: a transient failure with its Close error
	// precedes a successful second read. No Close field or error is fabricated.
	firstOriginal := &countedOverlayOriginal{reader: bytes.NewReader(nil), info: info, statError: statSentinel, closeError: closeSentinel}
	firstAttempt := consumeOverlayReadAttempt(path, firstOriginal, nil)
	secondOriginal := &countedOverlayOriginal{reader: bytes.NewReader([]byte("version: 1\ndaemons: {}\n")), info: info}
	secondAttempt := consumeOverlayReadAttempt(path, secondOriginal, nil)
	if !errors.Is(firstAttempt.primary, errTransientOverlayRead) || secondAttempt.primary != nil || secondAttempt.closeError != nil {
		t.Fatal("actual ordered producer shape changed")
	}
	_, retainedErr := finishOverlayReadObservation([]overlayReadAttempt{firstAttempt, secondAttempt})
	var failure *observedReadFailure
	if !errors.As(retainedErr, &failure) || !errors.Is(retainedErr, closeSentinel) || failure.observation == nil || len(failure.observation.attempts) != 2 || failure.observation.attempts[0].reader != firstOriginal || failure.observation.attempts[1].reader != secondOriginal || firstOriginal.closeCount != 1 || secondOriginal.closeCount != 1 {
		t.Fatal("actual retry aggregate erased prior original Close outcome")
	}
	if failure.observation.attempts[0].primary != firstAttempt.primary || failure.observation.attempts[0].closeError != firstAttempt.closeError || failure.observation.attempts[1].primary != secondAttempt.primary || failure.observation.overlay != secondAttempt.overlay {
		t.Fatal("aggregate replaced ordered original producer outcomes")
	}
	if _, err := failure.observation.Fingerprint(); err == nil {
		t.Fatal("prior failed Close qualified retry success")
	}
	if observation, err := finishOverlayReadObservation(nil); err == nil || observation == nil || observation.absent || len(observation.attempts) != 0 {
		t.Fatal("empty attempts became successful absence")
	}
	for _, openErr := range []error{openSentinel, nil} {
		attempt := consumeOverlayReadAttempt(path, nil, openErr)
		if attempt.primary == nil || attempt.reader != nil || attempt.opened || attempt.closeAttempted || attempt.closeDefined || attempt.closeError != nil {
			t.Fatal("true nil became acquired/closed")
		}
	}
	// Exercise concrete *os.File nil normalization at the original wrapper.
	saved := openOverlayFile
	t.Cleanup(func() { openOverlayFile = saved })
	for _, openErr := range []error{openSentinel, nil} {
		openOverlayFile = func(string) (*os.File, error) { return nil, openErr }
		attempt := loadOnceObserved(path)
		if attempt.primary == nil || attempt.reader != nil || attempt.opened || attempt.closeAttempted || attempt.closeDefined || attempt.closeError != nil {
			t.Fatal("concrete nil boxed into an imaginary reader")
		}
		if openErr != nil && !errors.Is(attempt.primary, openErr) {
			t.Fatal("original concrete-nil acquisition cause lost")
		}
	}
}

func TestObservedOverlayAbsenceHasNoInventedOwnedReader(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")
	observed, err := LoadObserved(filepath.Join(dir, "absent-overlay.yaml"))
	if err != nil || observed == nil || !observed.absent || len(observed.attempts) != 2 {
		t.Fatal("owner-qualified persistent absence/retry changed")
	}
	for _, read := range observed.attempts {
		if read.reader != nil || read.opened || read.closeAttempted || read.closeDefined || read.closeError != nil || read.primary == nil {
			t.Fatal("not acquired became acquired/closed")
		}
	}
}
