//go:build windows

package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

var errPreparedFinalizationStop = errors.New("controlled stop before package acquisition")

// Stop Prepare before installation, but keep its actual work context and the
// concrete finalizer. The fixture issues a real state-file writer and namespace;
// it supplies no publisher, native-child, MCP, or Ready publication evidence.
type preparedFinalizationEffects struct {
	*preparedSerenaRuntimeEffects
	onLock    func(context.Context, string) error
	work      context.Context
	writer    *StateFilePublicationObservation
	lockErr   error
	closeErr  error
	closeView PreparedSerenaPreparationCloseSnapshot
	finished  bool
}

func (e *preparedFinalizationEffects) LockPackage(ctx context.Context, key string) (func() error, error) {
	e.work = ctx
	if e.onLock != nil {
		e.lockErr = e.onLock(ctx, key)
	}
	return nil, errors.Join(errPreparedFinalizationStop, e.lockErr)
}

func (e *preparedFinalizationEffects) FinishPreparation(ctx context.Context, r PreparedSerenaRequest, _ *StateFilePublicationObservation) (*PreparedSerenaPreparationCloseObservation, error) {
	e.finished = true
	proof, err := e.preparedSerenaRuntimeEffects.FinishPreparation(ctx, r, e.writer)
	if err == nil {
		err = proof.Validate(ctx, e.preparedSerenaRuntimeEffects, r)
	}
	e.closeErr = err
	e.closeView = proof.Snapshot()
	// The seam intentionally stopped Prepare: never expose its partial fixture
	// as a successful preparation or an accepted runtime publication.
	return proof, errors.Join(errPreparedFinalizationStop, err)
}

func finalizationRequest() PreparedSerenaRequest {
	r := preparedRequest()
	r.WorkLimit = time.Minute
	r.CleanupLimit = 30 * time.Second
	r.WorkDeadline = time.Now().Add(r.WorkLimit)
	r.AbsoluteCleanupDeadline = r.WorkDeadline.Add(r.CleanupLimit)
	return r
}

func newFinalizationEffects(r PreparedSerenaRequest) *preparedFinalizationEffects {
	return &preparedFinalizationEffects{preparedSerenaRuntimeEffects: &preparedSerenaRuntimeEffects{
		PreparedSerenaRecordIO: PreparedSerenaRecordIO{operationID: r.OperationID},
		acquisition:            preparedSerenaAcquisition{inputs: SerenaPreparationInputs{Request: r}},
		publication:            NewPreparedSerenaPublicationOwner(),
	}}
}

func (e *preparedFinalizationEffects) writeOutcome(ctx context.Context, key string) error {
	r := e.acquisition.inputs.Request
	recordIO, err := NewPreparedSerenaRecordIO(r.Namespace, filepath.Join(r.Namespace, "intent.json"), r.OperationID, strings.Repeat("b", 64))
	if err != nil {
		return err
	}
	e.PreparedSerenaRecordIO = recordIO
	e.readyNamespaceAttempted = true
	e.namespace, err = recordIO.AcquireRecordNamespaceObserved(ctx, key)
	if err != nil {
		return err
	}
	bound, err := recordIO.BindRecordNamespace(ctx, e.namespace)
	if err != nil {
		return err
	}
	e.PreparedSerenaRecordIO = bound
	e.operationNamespaceAttempted = true
	e.operationNamespace, err = e.PreparedSerenaRecordIO.AcquireOperationNamespaceObserved(ctx, key, r.OperationID, true)
	if err != nil {
		return err
	}
	path := filepath.Join(r.Namespace, key, "operations", r.OperationID+".json")
	fresh, err := NewStateFileFreshCreationEvidence(ctx, e.operationNamespace, path, "prepared-operation")
	if err != nil {
		return err
	}
	// These bytes are deliberately not a preparation record. Only the original
	// observed writer's lifetime and native resource close are under test.
	raw := []byte("{\"fixture\":\"finalization-only\"}\n")
	e.writer, err = WriteStateFileBytesAtomicObserved(clients.ConfigWriteRequest{Context: ctx, Path: path, Contents: raw, FreshCreationEvidence: fresh})
	if e.writer != nil {
		err = errors.Join(err, e.writer.bindProgressNamespace(e.operationNamespace))
		e.operationNamespace.retainPublication(e.writer)
	}
	if err != nil {
		return err
	}
	if err = e.writer.ValidateProgress(ctx, e.operationNamespace, preparedSerenaHash(raw)); err != nil {
		return err
	}
	if e.writer.ResourcesClosed() || e.writer.Complete() {
		return errors.New("original operation namespace closed before finalization")
	}
	return nil
}

func TestPreparedFinalizationWriterContext(t *testing.T) {
	for _, tc := range []struct {
		name             string
		earlierParent    bool
		cancelAfterWrite bool
	}{
		{name: "request-deadline"},
		{name: "earlier-parent", earlierParent: true},
		{name: "parent-canceled", cancelAfterWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := finalizationRequest()
			r.Namespace = hardenedTempDir(t)
			parentEnd := r.WorkDeadline.Add(time.Minute)
			expectedEnd := r.WorkDeadline
			if tc.earlierParent {
				parentEnd = r.WorkDeadline.Add(-15 * time.Second)
				expectedEnd = parentEnd
			}
			parent, cancel := context.WithDeadline(context.Background(), parentEnd)
			defer cancel()
			e := newFinalizationEffects(r)
			e.onLock = func(ctx context.Context, key string) error {
				if end, ok := ctx.Deadline(); !ok || !end.Equal(expectedEnd) {
					return errors.New("original work deadline changed")
				}
				if err := e.writeOutcome(ctx, key); err != nil {
					return err
				}
				if tc.cancelAfterWrite {
					cancel()
				}
				return nil
			}
			owner, err := NewPreparedSerenaRuntimeOwner(e)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := owner.Prepare(parent, r)
			if !errors.Is(err, errPreparedFinalizationStop) || binding.Schema != "" || owner.completedRecord != nil || owner.completedBinding != nil {
				t.Fatalf("controlled stop returned publication: binding=%+v err=%v", binding, err)
			}
			if e.lockErr != nil || e.writer == nil || !e.finished {
				t.Fatalf("native writer fixture: writer=%v finished=%v err=%v", e.writer != nil, e.finished, e.lockErr)
			}
			if e.writer.request.Context != e.work || !errors.Is(e.work.Err(), context.Canceled) {
				t.Fatalf("original work context was replaced or not canceled after return: %v", e.work.Err())
			}
			if !e.writer.ResourcesClosed() || e.closeView.OperationNamespace != "closed" || e.closeView.ReadyNamespace != "closed" || !e.closeView.InOriginalBound {
				t.Fatalf("original native resources were not closed: %+v", e.closeView)
			}
			if tc.cancelAfterWrite {
				if e.closeErr == nil || !strings.Contains(e.closeErr.Error(), "SERENA_PREPARATION_OUTCOME_FINAL_CLOSE_PENDING") || e.writer.Complete() || e.writer.ReceiptSHA256() != "" {
					t.Fatalf("parent cancellation was promoted to successful completion: close=%v", e.closeErr)
				}
			} else if e.closeErr != nil || !e.writer.Complete() || !preparedSerenaDigest(e.writer.ReceiptSHA256()) || parent.Err() != nil {
				t.Fatalf("original writer not received before local cancellation: close=%v parent=%v", e.closeErr, parent.Err())
			}
			if len(e.publication.operations) != 0 {
				t.Fatal("fixture fabricated runtime publication evidence")
			}
		})
	}
}

func TestPreparedFinalizationEarlyExit(t *testing.T) {
	for _, name := range []string{"nil-context", "invalid-request", "expired-parent", "canceled-parent", "expired-work"} {
		t.Run(name, func(t *testing.T) {
			r := finalizationRequest()
			var parent context.Context = context.Background()
			want := ""
			switch name {
			case "nil-context":
				parent = nil
				want = "SERENA_PREPARED_OWNER_CONTEXT_MISSING"
			case "invalid-request":
				r.OperationID = ""
				want = "SERENA_PREPARED_REQUEST_INVALID"
			case "expired-parent":
				expired, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
				defer cancel()
				parent = expired
				want = context.DeadlineExceeded.Error()
			case "canceled-parent":
				canceled, cancel := context.WithCancel(parent)
				cancel()
				parent = canceled
				want = context.Canceled.Error()
			case "expired-work":
				r.WorkDeadline = time.Now().Add(-time.Second)
				r.AbsoluteCleanupDeadline = r.WorkDeadline.Add(r.CleanupLimit)
				want = context.DeadlineExceeded.Error()
			}
			e := newFinalizationEffects(r)
			owner, err := NewPreparedSerenaRuntimeOwner(e)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := owner.Prepare(parent, r)
			if err == nil || !strings.Contains(err.Error(), want) || binding.Schema != "" || owner.completedRecord != nil {
				t.Fatalf("early refusal lost: want=%q binding=%+v err=%v", want, binding, err)
			}
			if e.work != nil || e.writer != nil || !e.finished || e.closeErr != nil || !e.closeView.InOriginalBound {
				t.Fatalf("early exit attempted work or skipped bounded finalization: work=%v close=%v view=%+v", e.work != nil, e.closeErr, e.closeView)
			}
		})
	}
}
