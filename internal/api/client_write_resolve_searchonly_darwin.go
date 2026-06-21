//go:build darwin

// client_write_resolve_searchonly_darwin.go — Darwin (macOS) search-only
// open flags for the resolved-symlink intermediate-component descent.
//
// macOS has no O_PATH, and golang.org/x/sys/unix does not expose darwin's
// O_SEARCH constant, so there is no portable search-only directory open here.
// macOS is the "preview" support tier (Windows GA / Linux beta / macOS
// preview per the v0.5.0 supervisor spec), so the Finding 2 read-gate
// fallback — open the ancestor with the same O_RDONLY flags the descent
// already used — is the accepted behavior on darwin. The Linux leg
// (client_write_resolve_searchonly_linux.go) uses O_PATH to drop the READ
// requirement; darwin keeps the read-gate.
//
// O_NOFOLLOW|O_DIRECTORY are still applied so the F1 symlink-swap refusal is
// identical to the Linux leg — only the search-vs-read posture differs.

package api

import "golang.org/x/sys/unix"

// searchOnlyDirOpenFlags returns the open() flags for an intermediate
// ancestor directory in the resolved-symlink descent. On darwin there is no
// O_PATH / exposed O_SEARCH, so this falls back to the read-fd flags
// (O_RDONLY) — the documented preview-tier read-gate. The F1 swap refusal
// (O_NOFOLLOW|O_DIRECTORY) is preserved.
func searchOnlyDirOpenFlags() int {
	return unix.O_DIRECTORY | unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
}
