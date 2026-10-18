package cbuild

import "context"

// Project-wide rather than preset-wide: distinct presets may share binaryDir.
// Entries count both holders and waiters and disappear after the last user.
type projectOperation struct {
	token chan struct{}
	users int
}

func (b *builder) lockProjectOperation(ctx context.Context, dir string) (func(), error) {
	key, err := canonicalExistingDir(dir)
	if err != nil {
		return nil, err
	}
	key = projectPathKey(key)
	b.operationsMu.Lock()
	if b.operations == nil {
		b.operations = make(map[string]*projectOperation)
	}
	op := b.operations[key]
	if op == nil {
		op = &projectOperation{token: make(chan struct{}, 1)}
		b.operations[key] = op
	}
	op.users++
	b.operationsMu.Unlock()
	drop := func() {
		b.operationsMu.Lock()
		op.users--
		if op.users == 0 {
			delete(b.operations, key)
		}
		b.operationsMu.Unlock()
	}
	select {
	case op.token <- struct{}{}:
		// A ready token and cancellation may race in select.
		if err := ctx.Err(); err != nil {
			<-op.token
			drop()
			return nil, err
		}
		return func() { <-op.token; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// Caller holds the project operation lock. Configure/workflow may modify a
// shared binary directory, so invalidate all bindings for this source.
func (b *builder) forgetConfiguredProject(dir string) error {
	source, err := canonicalExistingDir(dir)
	if err != nil {
		return err
	}
	b.configuredMu.Lock()
	defer b.configuredMu.Unlock()
	for key := range b.configured {
		if sameCanonicalPath(key.sourceDir, source) {
			delete(b.configured, key)
		}
	}
	return nil
}
