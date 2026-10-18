package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestProjectServiceOperationFailureRetainsPrivateCustody(t *testing.T) {
	// Unissued zero-profile DATA tests only the private error custodian. This
	// neither issues a native observation nor exercises the common factory.
	source := &clients.CodexConfiguredProjectProfile{}
	operation := &projectServicePlan{source: source, manifest: []byte("synthetic-private-operation-sentinel")}
	if operation.retainFailure(nil) != nil {
		t.Fatal("successful operation acquired a failure")
	}
	laterCause := errors.New("synthetic-private-later-cause")
	stage := &AdoptStageError{Stage: "manifest-verify", CommitState: "committed_unverified", Cause: laterCause}
	observation := &ProjectServiceLaunchPreparationObservation{originalFailure: laterCause}
	observedFailure := &projectServiceLaunchObservationFailure{observation: observation, cause: stage}
	releaseCause := errors.New("synthetic-private-release-cause")
	release := &AdoptStageError{Stage: "lease-release", CommitState: "pending", Cause: releaseCause}
	for _, row := range []struct {
		name        string
		cause       error
		observation *ProjectServiceLaunchPreparationObservation
	}{
		{name: "before-observation", cause: stage},
		{name: "existing-observation", cause: observedFailure, observation: observation},
		{name: "joined-release", cause: errors.Join(observedFailure, release, context.Canceled), observation: observation},
	} {
		t.Run(row.name, func(t *testing.T) {
			operation.observation = row.observation
			err := operation.retainFailure(row.cause)
			var failure *projectServiceLaunchObservationFailure
			var actualStage *AdoptStageError
			if !errors.As(err, &failure) || failure.operation != operation || failure.operation.source != source || failure.observation != row.observation || !errors.Is(err, laterCause) || !errors.As(err, &actualStage) || actualStage != stage {
				t.Fatal("dropped result lost private custody, original observation or stage")
			}
			if row.name == "existing-observation" && failure != observedFailure {
				t.Fatal("existing failure was replaced")
			}
			if row.name == "joined-release" && (!errors.Is(err, releaseCause) || !errors.Is(err, context.Canceled) || !errors.Is(err, observedFailure)) {
				t.Fatal("final joined cleanup or cancellation cause was lost")
			}
			for _, value := range []any{err, failure, *failure} {
				encoded, jsonErr := json.Marshal(value)
				if jsonErr != nil || string(encoded) != "{}" {
					t.Fatal("private operation serialized")
				}
				for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
					if fmt.Sprintf(verb, value) != "PROJECT_SERVICE_LAUNCH_OBSERVATION_UNQUALIFIED" {
						t.Fatal("whole failure exposed private operation or joined cause")
					}
				}
			}
		})
	}
}

func TestProjectServiceManifestNameConflictRetainsTypedCause(t *testing.T) {
	root := filepath.Join(t.TempDir(), "synthetic-private-name-conflict")
	const name = "common-producer-name-conflict-fixture"
	path := filepath.Join(root, name, "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte("synthetic-private-manifest-preimage")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	cause := NewAPI().ManifestCreateIn(root, name, "must not replace the existing file")
	if !errors.Is(cause, ErrManifestNameExists) || !strings.Contains(cause.Error(), fmt.Sprintf("manifest %q already exists at %s; use edit instead", name, path)) {
		t.Fatal("manifest owner lost its typed conflict or contextual diagnostic")
	}
	stage := newAdoptStageError("manifest-create", "pending", cause)
	operation := &projectServicePlan{}
	err := operation.retainFailure(errors.Join(stage, context.Canceled))
	var failure *projectServiceLaunchObservationFailure
	var actualStage *AdoptStageError
	if !errors.Is(err, ErrManifestNameExists) || !errors.Is(err, context.Canceled) || !errors.As(err, &actualStage) || actualStage != stage || !errors.As(err, &failure) || failure.operation != operation {
		t.Fatal("private custody lost the real manifest conflict, stage or joined cause")
	}
	encoded, jsonErr := json.Marshal(err)
	if jsonErr != nil || string(encoded) != "{}" {
		t.Fatal("private manifest failure serialized")
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if fmt.Sprintf(verb, err) != "PROJECT_SERVICE_LAUNCH_OBSERVATION_UNQUALIFIED" {
			t.Fatal("private manifest failure exposed its contextual path")
		}
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("manifest-exists refusal changed the original file")
	}
}

func TestProjectServiceProviderReadRequiresOriginalReceiptAndMasksBytes(t *testing.T) {
	read := ProjectServiceProviderFileRead{Observation: &StateFileReadObservation{}, Raw: []byte("synthetic-private-provider-source")}
	if err := read.validate(context.Background()); err == nil {
		t.Fatal("DATA reconstructed an original provider read")
	}
	encoded, err := json.Marshal(read)
	if err != nil || string(encoded) != "{}" || strings.Contains(fmt.Sprintf("%+v %#v", read, read), "synthetic-private-provider-source") {
		t.Fatal("provider source or original receipt leaked through public formatting")
	}
}

func TestGraphifyObservedManifestUsesSameOverrideAndRetainsPrivateBytes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
	name := "graphify-observed-fixture"
	path := filepath.Join(root, name, "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("name: graphify-observed-fixture\nenv:\n  INNOCUOUS: synthetic-private-sentinel\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := NewAPI().ManifestGet(name)
	if err != nil || !bytes.Equal([]byte(legacy), raw) {
		t.Fatal("legacy selector changed")
	}
	observed, err := NewAPI().ManifestGetObserved(context.Background(), name)
	if err != nil || !bytes.Equal(observed.Bytes(), raw) || len(observed.attempts) != 1 || !observed.attempts[0].opened || observed.attempts[0].reader == nil || !observed.attempts[0].closeAttempted || !observed.attempts[0].closeDefined || observed.attempts[0].closeError != nil {
		t.Fatal("original selected read/close outcome unavailable")
	}
	statInfo, statErr := observed.attempts[0].reader.Stat()
	if statErr == nil || statInfo != nil {
		t.Fatal("retained original reader did not observe completed successful Close")
	}
	clone := observed.Bytes()
	clone[0] = 'X'
	if !bytes.Equal(observed.Bytes(), raw) {
		t.Fatal("returned bytes mutated observation")
	}
	encoded, err := json.Marshal(observed)
	if err != nil || string(encoded) != "{}" || strings.Contains(fmt.Sprintf("%+v %#v", observed, observed), "synthetic-private-sentinel") {
		t.Fatal("selected manifest leaked through public representation")
	}
	assertManifestPrivateFormatting(t, observed)
	fromDir, err := loadManifestYAMLEmbedFirstFromDir(name, t.TempDir())
	if err != nil || !bytes.Equal(fromDir, raw) {
		t.Fatal("FromDir stopped using same override owner")
	}
	var selectedPaths []string
	fromReader, err := loadManifestYAMLEmbedFirstWithReader(name, func(selected string) ([]byte, error) {
		selectedPaths = append(selectedPaths, selected)
		return os.ReadFile(selected)
	})
	if err != nil || !bytes.Equal(fromReader, raw) || len(selectedPaths) != 1 || selectedPaths[0] != observed.selectedPath {
		t.Fatal("retained reader and original observed reader selected different sources")
	}
	readerFailure := errors.New("synthetic retained reader failure")
	fromReader, err = loadManifestYAMLEmbedFirstWithReader(name, func(string) ([]byte, error) {
		return nil, readerFailure
	})
	if !errors.Is(err, readerFailure) || len(fromReader) != 0 {
		t.Fatal("retained reader failure was replaced by another acquisition")
	}
	// Synthetic degraded observation DATA exercises only the private typed-error
	// boundary. The original reader was really and successfully closed above;
	// this does not inject an actual original-owner Close failure or count calls.
	originalReader := observed.attempts[0].reader
	degraded := *observed
	degraded.attempts = append([]manifestSourceReadAttempt(nil), observed.attempts...)
	degraded.attempts[0].closeError = errors.New("synthetic-private-sentinel-close-outcome")
	_, retainedErr := func() (*ManifestSourceObservation, error) {
		return &degraded, degraded.ValidateProjectServiceLaunchRead(context.Background())
	}()
	var retainedFailure *manifestSourceObservationFailure
	if !errors.As(retainedErr, &retainedFailure) || retainedFailure.observation != &degraded || retainedFailure.observation.attempts[0].reader != originalReader || retainedFailure.observation.attempts[0].closeError != degraded.attempts[0].closeError {
		t.Fatal("dropped partial return lost original reader/outcome")
	}
	assertManifestPrivateFormatting(t, retainedFailure.observation)
	encoded, err = json.Marshal(retainedFailure)
	if err != nil || string(encoded) != "{}" || strings.Contains(retainedErr.Error(), "synthetic-private-sentinel") || strings.Contains(fmt.Sprint(retainedFailure.observation), "synthetic-private-sentinel") {
		t.Fatal("private error observation exposed retained internals")
	}
	portObservation := &adoptPortInventoryObservation{reads: append([]manifestSourceReadAttempt(nil), degraded.attempts...)}
	_, portErr := func() (*adoptPortInventoryObservation, error) {
		return portObservation, &adoptPortInventoryFailure{observation: portObservation, cause: degraded.attempts[0].closeError}
	}()
	var portFailure *adoptPortInventoryFailure
	if !errors.As(portErr, &portFailure) || portFailure.observation != portObservation || portFailure.observation.reads[0].reader != originalReader {
		t.Fatal("port private error lost original reader after dropped result")
	}
	encoded, err = json.Marshal(portFailure)
	if err != nil || string(encoded) != "{}" || strings.Contains(portErr.Error(), "synthetic-private-sentinel") {
		t.Fatal("port error published reader/outcome internals")
	}
}

func assertManifestPrivateFormatting(t *testing.T, observation *ManifestSourceObservation) {
	t.Helper()
	for _, value := range []any{observation, *observation, ManifestSourceObservation{}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if fmt.Sprintf(verb, value) != "manifest source observation (private)" {
				t.Fatal("manifest pointer/value exposed a reversible representation")
			}
		}
	}
	var absent *ManifestSourceObservation
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if fmt.Sprintf(verb, absent) != "<nil>" {
			t.Fatal("typed-nil manifest formatting unsafe")
		}
	}
}

func TestGraphifyManifestAbsentReaderHasNoInventedClosure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
	observation, err := NewAPI().ManifestGetObserved(context.Background(), "graphify-absent-fixture")
	var failure *manifestSourceObservationFailure
	if err == nil || !errors.As(err, &failure) || failure.observation != observation || len(observation.attempts) != 1 {
		t.Fatal("original acquisition failure lost")
	}
	read := observation.attempts[0]
	if read.reader != nil || read.opened || read.closeAttempted || read.closeDefined || read.closeError != nil || read.readError == nil {
		t.Fatal("unacquired reader became closed/acquired")
	}
	encoded, err := json.Marshal(failure)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("private failure published reader internals")
	}
}

func TestGraphifyManifestObservationRefusesPriorUnknownFailureAndCancelledRead(t *testing.T) {
	for _, attempts := range [][]manifestSourceReadAttempt{
		{{kind: "embedded", readError: errors.New("synthetic unknown lookup failure")}, {kind: "disk", opened: true, closeDefined: true}},
	} {
		observation := &ManifestSourceObservation{name: "graphify-unit", selectedKind: "disk", selectedPath: "synthetic", bytes: []byte("synthetic-private-sentinel"), attempts: attempts}
		err := observation.ValidateProjectServiceLaunchRead(context.Background())
		var failure *manifestSourceObservationFailure
		if err == nil || !errors.As(err, &failure) || failure.observation != observation || strings.Contains(err.Error(), "synthetic-private-sentinel") {
			t.Fatal("original failure was lost or published")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observation, err := NewAPI().ManifestGetObserved(ctx, "graphify-unit")
	var failure *manifestSourceObservationFailure
	if err == nil || !errors.As(err, &failure) || failure.observation != observation || len(observation.attempts) != 0 {
		t.Fatal("cancelled acquisition touched a source or lost its typed outcome")
	}
}

type countedManifestOriginal struct {
	*bytes.Reader
	readError  error
	closeError error
	readCount  int
	closeCount int
}

func (f *countedManifestOriginal) Read(raw []byte) (int, error) {
	f.readCount++
	if f.readError != nil {
		return 0, f.readError
	}
	return f.Reader.Read(raw)
}
func (*countedManifestOriginal) Stat() (fs.FileInfo, error) {
	return nil, errors.New("manifest original Stat is unused")
}
func (f *countedManifestOriginal) Close() error { f.closeCount++; return f.closeError }

func TestConsumeManifestOriginalCloseAndFailureCustody(t *testing.T) {
	readSentinel := errors.New("synthetic original Read failure")
	closeSentinel := errors.New("synthetic original Close failure")
	openSentinel := errors.New("synthetic original Open failure")
	for _, inputs := range []struct{ read, close, open error }{{}, {close: closeSentinel}, {read: readSentinel, close: closeSentinel}, {open: openSentinel, close: closeSentinel}} {
		original := &countedManifestOriginal{Reader: bytes.NewReader([]byte("synthetic-private-sentinel")), readError: inputs.read, closeError: inputs.close}
		attempt := consumeManifestSourceAttempt("disk", "synthetic-original", original, inputs.open)
		if attempt.reader != original || !attempt.opened || !attempt.closeAttempted || !attempt.closeDefined || original.closeCount != 1 || attempt.closeError != inputs.close {
			t.Fatal("original object/one-Close result lost")
		}
		if inputs.open != nil && (attempt.readError != inputs.open || original.readCount != 0) {
			t.Fatal("failed acquisition was read or lost")
		}
		if inputs.open == nil && original.readCount == 0 {
			t.Fatal("acquired raw owner did not read the original")
		}
		if inputs.read != nil && attempt.readError != inputs.read {
			t.Fatal("primary Read cause lost")
		}
		observation := &ManifestSourceObservation{name: "graphify-counted", selectedKind: "disk", selectedPath: attempt.path, bytes: attempt.bytes, attempts: []manifestSourceReadAttempt{attempt}}
		_, err := func() (*ManifestSourceObservation, error) {
			return observation, observation.ValidateProjectServiceLaunchRead(context.Background())
		}()
		if inputs.read == nil && inputs.close == nil && inputs.open == nil {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var failure *manifestSourceObservationFailure
		if !errors.As(err, &failure) || failure.observation != observation || failure.observation.attempts[0].reader != original {
			t.Fatal("dropped result lost actual consumed original")
		}
		for _, cause := range []error{inputs.read, inputs.close, inputs.open} {
			if cause != nil && !errors.Is(err, cause) {
				t.Fatal("original cause disappeared")
			}
		}
		encoded, jsonErr := json.Marshal(failure)
		if jsonErr != nil || string(encoded) != "{}" || strings.Contains(err.Error(), "synthetic-private-sentinel") {
			t.Fatal("private original exposed")
		}
		if original.closeCount != 1 {
			t.Fatal("consumer retried original Close")
		}
	}
	for _, openErr := range []error{openSentinel, nil} {
		attempt := consumeManifestSourceAttempt("disk", "synthetic-original", nil, openErr)
		if attempt.reader != nil || attempt.opened || attempt.closeAttempted || attempt.closeDefined || attempt.closeError != nil || attempt.readError == nil {
			t.Fatal("true-nil acquisition fabricated closure")
		}
	}
	// An undefined result is deliberate degradation, not original execution.
	original := &countedManifestOriginal{Reader: bytes.NewReader(nil)}
	attempt := consumeManifestSourceAttempt("disk", "synthetic-original", original, nil)
	attempt.closeDefined = false
	observation := &ManifestSourceObservation{name: "graphify-counted", selectedKind: "disk", selectedPath: attempt.path, attempts: []manifestSourceReadAttempt{attempt}}
	if observation.ValidateProjectServiceLaunchRead(context.Background()) == nil || original.closeCount != 1 {
		t.Fatal("undefined result qualified or caused a second Close")
	}
}

type countedProbeOriginal struct {
	closeCount int
	closeError error
}

func (*countedProbeOriginal) Accept() (net.Conn, error) {
	return nil, errors.New("unused fixture Accept")
}
func (*countedProbeOriginal) Addr() net.Addr { return &net.TCPAddr{} }
func (l *countedProbeOriginal) Close() error { l.closeCount++; return l.closeError }

func TestFinishPortProbeOriginalCloseAndFailureCustody(t *testing.T) {
	closeSentinel := errors.New("synthetic original listener Close failure")
	acquireSentinel := errors.New("synthetic original listener acquisition failure")
	for _, inputs := range []struct{ close, acquire error }{{}, {close: closeSentinel}, {close: closeSentinel, acquire: acquireSentinel}} {
		original := &countedProbeOriginal{closeError: inputs.close}
		observation, err := finishAdoptPortProbeObserved(9300, original, inputs.acquire)
		if observation.listener != original || !observation.closeAttempted || !observation.closeDefined || original.closeCount != 1 || observation.closeError != inputs.close {
			t.Fatal("listener original/one-Close result lost")
		}
		if inputs.close == nil && inputs.acquire == nil {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		observation = adoptPortProbeObservation{}
		var failure *adoptPortProbeFailure
		if !errors.As(err, &failure) || failure.observation.listener != original {
			t.Fatal("listener failure dropped original")
		}
		for _, cause := range []error{inputs.close, inputs.acquire} {
			if cause != nil && !errors.Is(err, cause) {
				t.Fatal("listener original cause lost")
			}
		}
		if err.Error() != "GRAPHIFY_PORT_PROBE_UNQUALIFIED" || original.closeCount != 1 {
			t.Fatal("listener failure leaked or retried Close")
		}
		encoded, jsonErr := json.Marshal(failure)
		if jsonErr != nil || string(encoded) != "{}" {
			t.Fatal("listener private failure published the original")
		}
	}
	for _, acquireErr := range []error{acquireSentinel, nil} {
		observation, err := finishAdoptPortProbeObserved(9300, nil, acquireErr)
		if err == nil || observation.listener != nil || observation.closeAttempted || observation.closeDefined || observation.closeError != nil {
			t.Fatal("nil listener fabricated capacity")
		}
	}
}
