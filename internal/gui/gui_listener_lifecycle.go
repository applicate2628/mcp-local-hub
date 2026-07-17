package gui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// GUIListenerMode identifies the request surface currently exposed by the GUI
// listener. The gate is process-local; the listener owner is its only writer.
type GUIListenerMode uint8

const (
	GUIListenerModeStandby GUIListenerMode = iota
	GUIListenerModeFull
	GUIListenerModeGrace
)

// GUIListenerHandlerMode pairs a lifecycle mode with the handler exposed in
// that mode. It is accepted by AdoptAndServe for future handoff callers.
type GUIListenerHandlerMode struct {
	Mode    GUIListenerMode
	Handler http.Handler
}

type guiListenerServeGeneration struct {
	listener net.Listener
	done     chan struct{}
	expected atomic.Bool
	serving  bool
}

// GUIListenerOwner is the sole owner of the GUI net.Listener, http.Server,
// handler-mode gate, and close/rebind lifecycle. Closing its listener never
// closes the Server's hub component, event broadcaster, or process context.
type GUIListenerOwner struct {
	mu            sync.Mutex
	server        *http.Server
	gate          *guiListenerHandlerGate
	current       *guiListenerServeGeneration
	errors        chan error
	activated     chan struct{}
	activatedOnce sync.Once
}

// NewGUIListenerOwner constructs an unbound owner. A zero timeout preserves
// net/http's zero-value behavior; Server uses its existing 10-second timeout.
func NewGUIListenerOwner(readHeaderTimeout time.Duration) *GUIListenerOwner {
	gate := newGUIListenerHandlerGate()
	owner := &GUIListenerOwner{
		gate:      gate,
		errors:    make(chan error, 1),
		activated: make(chan struct{}),
	}
	owner.server = &http.Server{
		Handler:           gate,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return owner
}

// Activated closes once this owner first exposes the full GUI handler. Standby
// callers use it to keep pollers, tray, and browser work behind activation.
func (o *GUIListenerOwner) Activated() <-chan struct{} {
	if o == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return o.activated
}

// Errors reports unexpected Serve failures. Expected listener-only close and
// full Shutdown paths do not publish an error.
func (o *GUIListenerOwner) Errors() <-chan error { return o.errors }

func (o *GUIListenerOwner) bind(ctx context.Context, port int) (net.Listener, error) {
	if o == nil {
		return nil, fmt.Errorf("bind GUI listener: nil owner")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	if o.current != nil {
		o.mu.Unlock()
		return nil, fmt.Errorf("bind GUI listener: owner already has a bound listener")
	}
	o.mu.Unlock()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("bind 127.0.0.1:%d: %w", port, err)
	}
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		return nil, err
	}

	generation := &guiListenerServeGeneration{listener: listener}
	o.mu.Lock()
	if o.current != nil {
		o.mu.Unlock()
		_ = listener.Close()
		return nil, fmt.Errorf("bind GUI listener: another listener was adopted concurrently")
	}
	o.current = generation
	o.mu.Unlock()
	return listener, nil
}

// BindStandby binds and serves only the supplied readiness handler. It does not
// open the Activated gate.
func (o *GUIListenerOwner) BindStandby(ctx context.Context, port int, readiness http.Handler) (net.Listener, error) {
	bound, err := o.bind(ctx, port)
	if err != nil {
		return nil, err
	}
	if err := o.AdoptAndServe(bound, GUIListenerHandlerMode{Mode: GUIListenerModeStandby, Handler: readiness}); err != nil {
		_ = bound.Close()
		o.clearCurrent(bound)
		return nil, err
	}
	return bound, nil
}

// BindForRecovery returns an already-bound exclusive loopback listener. The
// caller contract requires continued ownership of the GUI flock until the
// listener is either adopted or closed.
func (o *GUIListenerOwner) BindForRecovery(ctx context.Context, port int) (net.Listener, error) {
	return o.bind(ctx, port)
}

// ServeFull adopts bound when necessary, exposes the full handler, and opens
// the Activated gate. Reusing the current listener performs only a mode swap.
func (o *GUIListenerOwner) ServeFull(bound net.Listener, fullHandler http.Handler) error {
	if err := o.AdoptAndServe(bound, GUIListenerHandlerMode{Mode: GUIListenerModeFull, Handler: fullHandler}); err != nil {
		return err
	}
	o.activatedOnce.Do(func() { close(o.activated) })
	return nil
}

// EnterGrace rejects new requests through graceHandler immediately, then waits
// only for mutating requests admitted by the previous full handler to drain.
// Existing safe-method streams such as SSE are deliberately not drained.
func (o *GUIListenerOwner) EnterGrace(ctx context.Context, graceHandler http.Handler) error {
	if o == nil {
		return fmt.Errorf("enter GUI listener grace: nil owner")
	}
	drained := o.gate.setMode(GUIListenerHandlerMode{Mode: GUIListenerModeGrace, Handler: graceHandler})
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AdoptAndServe transfers a bound listener already owned by this lifecycle into
// net/http Serve. If it is already serving, this is only a handler-mode swap.
func (o *GUIListenerOwner) AdoptAndServe(bound net.Listener, mode GUIListenerHandlerMode) error {
	if o == nil {
		return fmt.Errorf("adopt GUI listener: nil owner")
	}
	if bound == nil {
		return fmt.Errorf("adopt GUI listener: nil listener")
	}
	if mode.Handler == nil {
		return fmt.Errorf("adopt GUI listener: nil handler")
	}
	if mode.Mode != GUIListenerModeStandby && mode.Mode != GUIListenerModeFull && mode.Mode != GUIListenerModeGrace {
		return fmt.Errorf("adopt GUI listener: unknown handler mode %d", mode.Mode)
	}

	o.mu.Lock()
	generation := o.current
	if generation == nil || generation.listener != bound {
		o.mu.Unlock()
		return fmt.Errorf("adopt GUI listener: listener is not owned by this lifecycle")
	}
	o.gate.setMode(mode)
	if generation.serving {
		o.mu.Unlock()
		return nil
	}
	generation.serving = true
	generation.done = make(chan struct{})
	server := o.server
	o.mu.Unlock()

	go func() {
		err := server.Serve(bound)
		close(generation.done)
		if err == nil || errors.Is(err, http.ErrServerClosed) || generation.expected.Load() {
			return
		}
		select {
		case o.errors <- err:
		default:
		}
	}()
	return nil
}

// CloseListener stops accepting new GUI connections and waits for that Serve
// loop to return. Active connections remain owned by http.Server, allowing an
// existing SSE stream to survive while the owner binds a replacement listener.
func (o *GUIListenerOwner) CloseListener(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	generation := o.current
	if generation == nil {
		o.mu.Unlock()
		return nil
	}
	generation.expected.Store(true)
	listener := generation.listener
	done := generation.done
	o.mu.Unlock()

	closeErr := listener.Close()
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return closeErr
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	o.clearCurrent(listener)
	return nil
}

func (o *GUIListenerOwner) clearCurrent(listener net.Listener) {
	o.mu.Lock()
	if o.current != nil && o.current.listener == listener {
		o.current = nil
	}
	o.mu.Unlock()
}

// Shutdown drains all GUI HTTP connections. Unlike CloseListener this is the
// terminal process-lifetime operation; the http.Server is not reused afterward.
func (o *GUIListenerOwner) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	generation := o.current
	if generation != nil {
		generation.expected.Store(true)
	}
	o.mu.Unlock()

	err := o.server.Shutdown(ctx)
	if err != nil {
		_ = o.server.Close()
	}
	if generation != nil {
		if generation.done != nil {
			select {
			case <-generation.done:
			case <-ctx.Done():
				if err != nil {
					return err
				}
				return ctx.Err()
			}
		} else if generation.listener != nil {
			// A bound-but-never-served generation (done == nil, e.g. a future
			// BindForRecovery without a subsequent AdoptAndServe): net/http Serve
			// never registered this listener, so o.server.Shutdown above did NOT
			// close it. Close it explicitly before dropping the owner's reference,
			// else the port stays bound and a later same-port recovery bind fails.
			// An already-closed listener is fine.
			if cerr := generation.listener.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
				err = cerr
			}
		}
		// Always clear the current generation for any non-nil generation, even the
		// bound-but-never-served one above: otherwise o.current would survive
		// Shutdown as a latent trap for the D-J handoff coordinator.
		o.clearCurrent(generation.listener)
	}
	return err
}

type guiListenerHandlerGate struct {
	mu               sync.Mutex
	mode             GUIListenerHandlerMode
	admittedMutators int
	drained          chan struct{}
}

func newGUIListenerHandlerGate() *guiListenerHandlerGate {
	drained := make(chan struct{})
	close(drained)
	return &guiListenerHandlerGate{drained: drained}
}

func (g *guiListenerHandlerGate) setMode(mode GUIListenerHandlerMode) <-chan struct{} {
	g.mu.Lock()
	g.mode = mode
	drained := g.drained
	g.mu.Unlock()
	return drained
}

func (g *guiListenerHandlerGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	mode := g.mode
	mutating := mode.Mode == GUIListenerModeFull && guiRequestMutates(r.Method)
	if mutating {
		if g.admittedMutators == 0 {
			g.drained = make(chan struct{})
		}
		g.admittedMutators++
	}
	g.mu.Unlock()

	if mutating {
		defer func() {
			g.mu.Lock()
			g.admittedMutators--
			if g.admittedMutators == 0 {
				close(g.drained)
			}
			g.mu.Unlock()
		}()
	}
	if mode.Handler == nil {
		http.Error(w, "GUI listener is not activated", http.StatusServiceUnavailable)
		return
	}
	mode.Handler.ServeHTTP(w, r)
}

func guiRequestMutates(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
