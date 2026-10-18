package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

func TestGenuineObservationAbsoluteDeadlineRefusesBeforeOwners(t *testing.T) {
	future := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	canceled, stop := context.WithCancel(context.Background()); stop()
	cases := []struct { name string; parent context.Context; flag observationDeadlineFlag }{
		{"missing", context.Background(), observationDeadlineFlag{}},
		{"duplicate", context.Background(), observationDeadlineFlag{future, 2}},
		{"empty", context.Background(), observationDeadlineFlag{"", 1}},
		{"invalid", context.Background(), observationDeadlineFlag{"not-an-instant", 1}},
		{"expired", context.Background(), observationDeadlineFlag{time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), 1}},
		{"nil-parent", nil, observationDeadlineFlag{future, 1}},
		{"canceled-parent", canceled, observationDeadlineFlag{future, 1}},
	}
	for _, tc := range cases { t.Run(tc.name, func(t *testing.T) {
		calls := 0
		ops := versionObservationOperations{
			native: func(context.Context) (*process.PublisherNativeObservation, error) { calls++; return nil, nil },
			image: func(context.Context, *process.PublisherNativeObservation) (*api.CurrentPublisherImageObservation, error) { calls++; return nil, nil },
			root: func(context.Context) (string, *api.StateDirResolutionObservation, error) { calls++; return "", nil, nil },
		}
		var out bytes.Buffer
		if observeRuntimePrerequisites(tc.parent, tc.flag, &out, ops) == nil || calls != 0 || !bytes.Contains(out.Bytes(), []byte(`"complete":false`)) { t.Fatal("deadline refusal entered owner or claimed success") }
	}) }
}

func TestGenuineObservationAbsoluteEndIsNotRenewed(t *testing.T) {
	end := time.Now().Add(time.Minute).UTC()
	parentEnd := end.Add(-time.Second)
	parent, stop := context.WithDeadline(context.Background(), parentEnd); defer stop()
	ctx, cancel, err := observationContext(parent, observationDeadlineFlag{end.Format(time.RFC3339Nano), 1})
	if err != nil { t.Fatal(err) }
	got, finite := ctx.Deadline()
	if !finite || !got.Equal(parentEnd) { t.Fatal("parent deadline was extended") }
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) { t.Fatal("original cancellation not retained") }
	ctx, cancel, err = observationContext(context.Background(), observationDeadlineFlag{end.Format(time.RFC3339Nano), 1})
	if err != nil { t.Fatal(err) }; defer cancel()
	got, finite = ctx.Deadline()
	if !finite || !got.Equal(end) { t.Fatal("absolute end renewed") }
	primary := errors.New("synthetic native owner failure")
	rootFailure := errors.New("synthetic independent root failure")
	var nativeContext, rootContext context.Context
	imageCalls := 0
	ops := versionObservationOperations{
		native: func(ctx context.Context) (*process.PublisherNativeObservation, error) { nativeContext = ctx; return nil, primary },
		image: func(context.Context, *process.PublisherNativeObservation) (*api.CurrentPublisherImageObservation, error) { imageCalls++; return nil, nil },
		root: func(ctx context.Context) (string, *api.StateDirResolutionObservation, error) { rootContext = ctx; return "", nil, rootFailure },
	}
	var out bytes.Buffer
	err = observeRuntimePrerequisites(context.Background(), observationDeadlineFlag{end.Format(time.RFC3339Nano), 1}, &out, ops)
	if !errors.Is(err, primary) || !errors.Is(err, rootFailure) || nativeContext == nil || nativeContext != rootContext || imageCalls != 0 || !errors.Is(nativeContext.Err(), context.Canceled) || !bytes.Contains(out.Bytes(), []byte(`"complete":false`)) { t.Fatal("independent failure receiving, shared context or cancellation lost") }
}

type unrenderableObservationCause struct { renders *int }
func (e unrenderableObservationCause) Error() string { (*e.renders)++; panic("native cause must not render") }
func TestGenuineObservationSafeErrorRetainsCause(t *testing.T) {
	renders := 0; cause := unrenderableObservationCause{&renders}
	err := &currentProductObservationError{cause: cause}
	if err.Error() != "PRODUCT_OBSERVATION_INCOMPLETE" || !errors.Is(err, cause) || renders != 0 { t.Fatal("native error rendered or lost") }
}
