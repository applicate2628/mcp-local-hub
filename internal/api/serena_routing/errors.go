package serena_routing

import "errors"

// ErrWorkspaceNotFound is returned by WorkspaceResolver.ResolveByPath when
// no registered serena workspace can be matched against the input path.
//
// Callers in the HTTP / MCP layer translate this to a 503 ("register
// workspace first") response per Phase C.2; in CLI contexts the same
// sentinel can be used to drive auto-register on miss (Phase E).
//
// Use errors.Is(err, ErrWorkspaceNotFound) for classification; the resolver
// may wrap this sentinel with additional context (path, registry state)
// while preserving Is-equality.
var ErrWorkspaceNotFound = errors.New("serena_routing: workspace not found")

// ErrInvalidPath is returned when ResolveByPath receives an input it
// cannot interpret as either a usable absolute path or a non-empty
// relative path. Empty strings, malformed UNC roots, and unresolvable
// shapes flow through this sentinel rather than as opaque OS errors.
var ErrInvalidPath = errors.New("serena_routing: invalid path")
