package api

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// WindowsProductStateNamespace holds the original ordinary Windows state
// directory chain. Acquiring it has no Product, GUI or state-write effect.
// The composing command closes it after all of its owner operations settle.
type WindowsProductStateNamespace struct{ native *productStateNamespaceNative }

func OpenWindowsProductStateNamespace(ctx context.Context, stateDir string) (*WindowsProductStateNamespace, error) {
	return openProductStateNamespace(ctx, stateDir)
}

func (s *WindowsProductStateNamespace) Validate(ctx context.Context) error {
	if s == nil || s.native == nil {
		return errHubRouteStateUnavailable
	}
	return s.native.validate(ctx)
}

func (s *WindowsProductStateNamespace) Close() error {
	if s == nil || s.native == nil {
		return nil
	}
	return s.native.close()
}

// RetainAfterUnconfirmedRelease keeps the original guards resident if a bound
// leaf owner cannot confirm release. None of them can admit more work.
func (s *WindowsProductStateNamespace) RetainAfterUnconfirmedRelease(cause error) {
	if s != nil && s.native != nil && cause != nil {
		s.native.retain(cause)
	}
}

// BoundLockOption hands flock an already verified native file. Only existing
// lock owners call this with their fixed lock basename and creation policy.
func (s *WindowsProductStateNamespace) BoundLockOption(ctx context.Context, path, leaf string) (flock.Option, error) {
	if s == nil || s.native == nil {
		return nil, errHubRouteStateUnavailable
	}
	if err := s.validatePath(ctx, path, leaf); err != nil {
		return nil, err
	}
	return flock.SetFileOpener(func(name string, flags int) (*os.File, error) {
		if name != path {
			return nil, errHubRouteStateChanged
		}
		return s.native.openLock(ctx, path, leaf, flags)
	}), nil
}

// ReadBoundFile uses the same held parent for an existing owner's state leaf.
// Only an actual absent leaf is returned as os.ErrNotExist.
func (s *WindowsProductStateNamespace) ReadBoundFile(ctx context.Context, path, leaf string) ([]byte, error) {
	if s == nil || s.native == nil {
		return nil, errHubRouteStateUnavailable
	}
	if err := s.validatePath(ctx, path, leaf); err != nil {
		return nil, err
	}
	return s.native.read(ctx, leaf)
}

// WithBoundFileReader lends the private regular file at an exact basename
// under this held namespace. The synchronous callback must consume the whole
// file. It receives neither a path nor a handle, and its reader is revoked
// when it returns. A successful return includes original-file and resource
// readback, so results produced by the callback remain provisional until then.
func (s *WindowsProductStateNamespace) WithBoundFileReader(ctx context.Context, leaf string, read func(io.Reader) error) error {
	if s == nil || s.native == nil || ctx == nil || read == nil {
		return errHubRouteStateUnavailable
	}
	return s.native.withBoundFileReader(ctx, leaf, read)
}

func (s *WindowsProductStateNamespace) validatePath(ctx context.Context, path, leaf string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != leaf || leaf == "." || leaf == ".." {
		return errors.New("bound Product state leaf requires the owner's exact path")
	}
	return s.native.matchParent(ctx, filepath.Dir(path))
}

// WithHubRouteState borrows this namespace for the original H lock owner.
func (s *WindowsProductStateNamespace) WithHubRouteState(ctx context.Context, run func(*HubRouteState) error) error {
	if s == nil || s.native == nil || run == nil {
		return errHubRouteStateUnavailable
	}
	return s.native.withHub(ctx, run)
}
