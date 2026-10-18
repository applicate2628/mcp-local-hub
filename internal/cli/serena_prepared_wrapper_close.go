package cli

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// This operation-local scope is armed before host start. Only its original
// wrapper enters Serve and issues the final server observation.
type preparedWrapperServerScope struct {
	adapter                           *preparedConsumerLifecycleAdapter
	server                            *http.Server
	serveDone                         chan error
	serveEntered, serveReceived       bool
	serveError                        error
	shutdownDone                      chan error
	shutdownEntered, shutdownReceived bool
	shutdownError                     error
	deadline                          time.Time
	finalized                         bool
	proof                             *preparedWrapperServerCloseObservation
}

type preparedWrapperServerCloseObservation struct {
	owner            *preparedWrapperServerScope
	deadline         time.Time
	completedInBound bool
	err              error
}

// Shutdown is started before stop so graceful HTTP requests cannot serially
// consume the original child's cleanup window. Both entered workers are then
// received under that exact context. A missing result remains visibly pending.
func receivePreparedWrapperShutdown(ctx context.Context, shutdown func(context.Context) error, closeServer func() error, stop func(context.Context) error, done chan error, serve <-chan error, serveReceived bool, serveErr error) (shutdownErr, stopErr, receivedServeErr error, shutdownReceived, receivedServe bool) {
	go func() {
		defer close(done)
		e := shutdown(ctx)
		if e != nil {
			e = errors.Join(e, closeServer())
		}
		done <- e
	}()
	stopErr = stop(ctx)
	// A result frame alone precedes the worker's final done close. Receive both
	// from the same slot, preserving an already received Serve frame.
	receive := func(ch <-chan error, gotFrame bool, value error) (error, bool) {
		for {
			select {
			case e, open := <-ch:
				if !open {
					if !gotFrame {
						return errors.New("SERENA_WRAPPER_WORKER_RESULT_MISSING"), false
					}
					return value, true
				}
				if gotFrame {
					return errors.Join(value, e, errors.New("SERENA_WRAPPER_WORKER_RESULT_DUPLICATE")), false
				}
				value, gotFrame = e, true
			case <-ctx.Done():
				return errors.Join(value, ctx.Err()), false
			}
		}
	}
	shutdownErr, shutdownReceived = receive(done, false, nil)
	receivedServeErr, receivedServe = receive(serve, serveReceived, serveErr)
	if receivedServeErr == http.ErrServerClosed {
		receivedServeErr = nil
	}
	return
}

func (s *preparedWrapperServerScope) receive(ctx context.Context) *preparedWrapperServerCloseObservation {
	if s.finalized {
		return s.proof
	}
	s.finalized = true
	d, finite := ctx.Deadline()
	err := errors.Join(s.serveError, s.shutdownError)
	if !finite || !d.Equal(s.deadline) || s.adapter == nil || s.adapter.wrapper != s ||
		(s.serveEntered && (!s.serveReceived || !s.shutdownEntered || !s.shutdownReceived)) ||
		(!s.serveEntered && (s.server != nil || s.shutdownEntered)) {
		err = errors.Join(err, errors.New("SERENA_WRAPPER_SERVER_RECEIVING_INCOMPLETE"))
	}
	s.proof = &preparedWrapperServerCloseObservation{owner: s, deadline: d, completedInBound: ctx.Err() == nil, err: err}
	return s.proof
}

func (o *preparedWrapperServerCloseObservation) validate(ctx context.Context, p *preparedConsumerLifecycleAdapter) error {
	if o == nil || o.owner == nil || p == nil || p.wrapper != o.owner || o.owner.adapter != p || !o.owner.finalized || o.owner.proof != o || !o.deadline.Equal(o.owner.deadline) || !o.completedInBound || o.err != nil || ctx == nil || ctx.Err() != nil {
		return errors.New("SERENA_WRAPPER_SERVER_CLOSE_UNQUALIFIED")
	}
	return nil
}
