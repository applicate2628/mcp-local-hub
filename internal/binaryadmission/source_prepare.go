package binaryadmission

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"mcp-local-hub/internal/process"
)

// PublisherSourcePreparationRequest describes inputs, never an asserted epoch.
// GeneratedSource contains literal files, not directories or patterns.
type PublisherSourcePreparationRequest struct {
	Checkout        string
	ExpectedCommit  string
	Export          string
	Snapshot        string
	GeneratedSource []string
}

type publisherPreparedInput struct {
	Schema string `json:"schema"`
	PublisherSourceInput
}

type publisherPreparationResult struct {
	Schema              string                 `json:"schema"`
	GitCommit           string                 `json:"git_commit"`
	GitTree             string                 `json:"git_tree"`
	DirtySelectedSource bool                   `json:"dirty_selected_source"`
	SourceInput         publisherPreparedInput `json:"source_input"`
}

type publisherGitEntry struct{ mode, oid string }
type publisherGitRun func(context.Context, string, ...string) ([]byte, error)

// publisherGit uses only fixed, local plumbing operations. The existing strict
// creator owns child/descendant/stream/Job settlement on every exit. Capture is
// complete up to Go's addressable []byte/string length, not a new evidence cap.
func publisherGit(ctx context.Context, checkout string, args ...string) ([]byte, error) {
	argv := []string{"--no-pager", "--no-optional-locks", "--no-replace-objects", "--literal-pathspecs", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=", "-c", "core.untrackedCache=false"}
	cmd := exec.Command("git", append(argv, args...)...)
	cmd.Dir = checkout
	for _, item := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(item, "=", 2)[0])
		if !strings.HasPrefix(key, "GIT_") && key != "GCM_INTERACTIVE" {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never", "GIT_NO_LAZY_FETCH=1")
	maxBytes := int(^uint(0) >> 1) // representation limit; never silently truncate
	result, err := process.RunStrictlyContained(ctx, process.StrictRunInvocation{Command: cmd, InputLimit: 1, StdoutLimit: maxBytes, StderrLimit: maxBytes})
	if result.Stdout.Truncated || result.Stderr.Truncated {
		err = errors.Join(err, errors.New("publisher source Git capture incomplete"))
	}
	if result.SettledExit == nil || result.SettledExit.ExitCode != 0 || result.SettledExit.TimedOut {
		err = errors.Join(err, errors.New("publisher source Git settlement unavailable or unsuccessful"))
	}
	if err != nil {
		// Preserve acquired streams and finalization alongside the causal error;
		// an exit/close failure must not erase an already received outcome.
		return nil, fmt.Errorf("publisher source Git acquisition: stdout=%q stderr=%q stdout_bytes=%d stderr_bytes=%d stdout_truncated=%t stderr_truncated=%t settled=%+v: %w", result.Stdout.Prefix, result.Stderr.Prefix, result.Stdout.Bytes, result.Stderr.Bytes, result.Stdout.Truncated, result.Stderr.Truncated, result.SettledExit, err)
	}
	return result.Stdout.Prefix, nil
}

func publisherSourceInside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// No ancestor link is admitted. Retained roots subsequently bind all reads,
// writes and cleanup, so replacing an ancestor cannot redirect an operation.
func openPublisherSourceRoot(name string) (*os.Root, os.FileInfo, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, nil, ErrPublisherSourceTableInvalid
	}
	for current := name; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, nil, err
		}
		if !info.IsDir() || publisherSourceReparse(info) {
			return nil, nil, ErrPublisherSourceTableInvalid
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	before, err := os.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	after, err := root.Stat(".")
	if err == nil && !os.SameFile(before, after) {
		err = ErrPublisherSourceTableInvalid
	}
	if err != nil {
		return nil, nil, errors.Join(err, root.Close())
	}
	return root, after, nil
}

func publisherSourceRootUnchanged(root *os.Root, name string, original os.FileInfo) error {
	current, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if publisherSourceReparse(current) || !os.SameFile(original, current) {
		return ErrPublisherSourceTableInvalid
	}
	held, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(original, held) {
		return ErrPublisherSourceTableInvalid
	}
	return nil
}

func publisherSourceHasAncestor(name string, ancestor os.FileInfo) (bool, error) {
	for current := name; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return false, err
		}
		if !info.IsDir() || publisherSourceReparse(info) {
			return false, ErrPublisherSourceTableInvalid
		}
		if os.SameFile(ancestor, info) {
			return true, nil
		}
		if filepath.Dir(current) == current {
			return false, nil
		}
	}
}

func publisherSourceRelativeInfo(root *os.Root, name string) (os.FileInfo, error) {
	if !publisherSourcePathValid(name) {
		return nil, ErrPublisherSourceTableInvalid
	}
	parts := strings.Split(name, "/")
	for n := 1; n <= len(parts); n++ {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:n], "/")))
		if err != nil {
			return nil, err
		}
		if publisherSourceReparse(info) || (n < len(parts) && !info.IsDir()) {
			return nil, ErrPublisherSourceTableInvalid
		}
		if n == len(parts) {
			return info, nil
		}
	}
	return nil, ErrPublisherSourceTableInvalid
}

func readPublisherSourceRelative(ctx context.Context, root *os.Root, name string) ([]byte, os.FileInfo, error) {
	info, err := publisherSourceRelativeInfo(root, name)
	if err != nil {
		return nil, nil, err
	}
	raw, err := readPublisherSourceOpenedFile(ctx, info, func() (*os.File, error) { return root.Open(filepath.FromSlash(name)) })
	return raw, info, err
}

func publisherSelectablePath(name string) bool {
	if !publisherSourcePathValid(name) || name == "cmd/mcphub/zz_product_versioninfo.syso" {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, "node_modules") || strings.EqualFold(part, ".env") {
			return false
		}
	}
	return !(strings.HasPrefix(name, ".scratch/windows-versioninfo-") && strings.Count(name, "/") == 2 && strings.HasSuffix(name, "/product.json"))
}

func publisherParseGitEntries(raw []byte, tree bool) (map[string]publisherGitEntry, error) {
	entries := map[string]publisherGitEntry{}
	if len(raw) == 0 || raw[len(raw)-1] != 0 {
		return nil, ErrPublisherSourceTableInvalid
	}
	for _, record := range bytes.Split(raw[:len(raw)-1], []byte{0}) {
		pair := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(pair) != 2 {
			return nil, ErrPublisherSourceTableInvalid
		}
		fields := strings.Split(string(pair[0]), " ")
		name := string(pair[1])
		if len(fields) != 3 || !publisherSelectablePath(name) {
			return nil, ErrPublisherSourceTableInvalid
		}
		mode, oid := fields[0], fields[1]
		if tree {
			if fields[1] != "blob" {
				return nil, ErrPublisherSourceTableInvalid
			}
			oid = fields[2]
		} else if fields[2] != "0" {
			return nil, errors.New("publisher source index has unresolved entries")
		}
		if (mode != "100644" && mode != "100755") || !validLowerHex(oid, 40, 40) {
			return nil, ErrPublisherSourceTableInvalid
		}
		if _, exists := entries[name]; exists {
			return nil, ErrPublisherSourceTableInvalid
		}
		entries[name] = publisherGitEntry{mode, oid}
	}
	return entries, nil
}

type publisherGitObservation struct {
	commit, tree string
	index, base  []byte
}

func observePublisherSourceGit(ctx context.Context, checkout string, run publisherGitRun) (publisherGitObservation, error) {
	var result publisherGitObservation
	top, err := run(ctx, checkout, "rev-parse", "--show-toplevel")
	if err != nil {
		return result, err
	}
	info, err := os.Stat(strings.TrimSuffix(strings.TrimSuffix(string(top), "\n"), "\r"))
	if err != nil {
		return result, err
	}
	want, err := os.Stat(checkout)
	if err != nil || !os.SameFile(info, want) {
		return result, errors.Join(ErrPublisherSourceTableInvalid, err)
	}
	resolve := func(expression string) (string, error) {
		raw, err := run(ctx, checkout, "rev-parse", "--verify", expression)
		if err != nil {
			return "", err
		}
		identity := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		if !validLowerHex(identity, 40, 40) {
			return "", ErrPublisherSourceTableInvalid
		}
		return identity, nil
	}
	result.commit, err = resolve("HEAD")
	if err != nil {
		return result, err
	}
	// Bind the tree to the acquired immutable commit. A later HEAD read can
	// name a different commit even when both before/after tuples repeat.
	result.tree, err = resolve(result.commit + "^{tree}")
	if err != nil {
		return result, err
	}
	result.base, err = run(ctx, checkout, "ls-tree", "-r", "-z", "--full-tree", result.tree)
	if err != nil {
		return result, err
	}
	result.index, err = run(ctx, checkout, "ls-files", "--stage", "-z", "--cached")
	return result, err
}

// Git's blob object identity is only a base-content relation. SourceEpoch is
// still issued exclusively by VerifyPublisherSourceSnapshot (SHA-256).
func publisherGitBlob(raw []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d%c", len(raw), byte(0))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

type publisherOwnedEntry struct {
	name string
	info os.FileInfo
}
type publisherOwnedExport struct {
	parent      *os.Root
	root        *os.Root
	leaf        string
	info        os.FileInfo
	files, dirs []publisherOwnedEntry
}

// Cleanup is manifest-based through retained roots, never recursive by path.
func (owned *publisherOwnedExport) remove() error {
	var result error
	if owned.info == nil {
		return nil
	}
	if owned.root == nil {
		var err error
		owned.root, err = owned.parent.OpenRoot(owned.leaf)
		if err != nil {
			return err
		}
		current, err := owned.root.Stat(".")
		if err != nil || !os.SameFile(owned.info, current) {
			return errors.Join(errors.New("publisher source cleanup export identity changed"), err, owned.root.Close())
		}
	}
	if owned.root != nil {
		for i := len(owned.files) - 1; i >= 0; i-- {
			result = errors.Join(result, removePublisherOwned(owned.root, owned.files[i]))
		}
		for i := len(owned.dirs) - 1; i >= 0; i-- {
			result = errors.Join(result, removePublisherOwned(owned.root, owned.dirs[i]))
		}
		result = errors.Join(result, owned.root.Close())
		owned.root = nil
	}
	if owned.info != nil {
		result = errors.Join(result, removePublisherOwned(owned.parent, publisherOwnedEntry{owned.leaf, owned.info}))
	}
	return result
}

func removePublisherOwned(parent *os.Root, entry publisherOwnedEntry) error {
	current, err := publisherSourceRelativeInfo(parent, filepath.ToSlash(entry.name))
	if err != nil {
		return err
	}
	if publisherSourceReparse(current) || !os.SameFile(entry.info, current) {
		return errors.New("publisher source cleanup identity changed")
	}
	return parent.Remove(entry.name)
}

func publisherWriteExclusive(root *os.Root, name string, raw []byte, mode os.FileMode, retained *publisherOwnedEntry) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, statErr := f.Stat()
	if statErr == nil {
		*retained = publisherOwnedEntry{name, info}
	}
	var writeErr, syncErr, modeErr error
	if statErr == nil {
		var n int
		n, writeErr = f.Write(raw)
		if writeErr == nil && n != len(raw) {
			writeErr = io.ErrShortWrite
		}
		if writeErr == nil {
			modeErr = f.Chmod(mode.Perm())
			syncErr = f.Sync()
		}
	}
	return errors.Join(statErr, writeErr, modeErr, syncErr, f.Close())
}

// PreparePublisherSource writes its one tagged row only after persisted export,
// source rechecks, Git rechecks and issuer readback. All owned handles close on
// every exit; failures remove only identity-bound newly created leaves.
func PreparePublisherSource(ctx context.Context, request PublisherSourcePreparationRequest, output io.Writer) error {
	if err := preparePublisherSource(ctx, request, output, publisherGit); err != nil {
		return errors.Join(ErrPublisherSourceTableInvalid, err)
	}
	return nil
}

func preparePublisherSource(ctx context.Context, request PublisherSourcePreparationRequest, output io.Writer, run publisherGitRun) (returned error) {
	if ctx == nil || output == nil || run == nil || !validLowerHex(request.ExpectedCommit, 40, 40) {
		return ErrPublisherSourceTableInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, name := range []string{request.Checkout, request.Export, request.Snapshot} {
		if !filepath.IsAbs(name) || filepath.Clean(name) != name {
			return ErrPublisherSourceTableInvalid
		}
	}
	if publisherSourceInside(request.Checkout, request.Export) || publisherSourceInside(request.Export, request.Checkout) || publisherSourceInside(request.Checkout, request.Snapshot) || publisherSourceInside(request.Export, request.Snapshot) || publisherSourceInside(request.Snapshot, request.Export) {
		return errors.New("publisher source paths overlap")
	}
	generated := map[string]bool{}
	for _, name := range request.GeneratedSource {
		if !publisherSelectablePath(name) || generated[name] {
			return ErrPublisherSourceTableInvalid
		}
		generated[name] = true
	}
	checkout, checkoutInfo, err := openPublisherSourceRoot(request.Checkout)
	if err != nil {
		return err
	}
	closeRoot := func(root **os.Root) error {
		if *root == nil {
			return nil
		}
		held := *root
		*root = nil
		return held.Close()
	}
	defer func() { returned = errors.Join(returned, closeRoot(&checkout)) }()
	parentName, snapshotParentName := filepath.Dir(request.Export), filepath.Dir(request.Snapshot)
	parent, parentInfo, err := openPublisherSourceRoot(parentName)
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, closeRoot(&parent)) }()
	snapshotParent, snapshotParentInfo, err := openPublisherSourceRoot(snapshotParentName)
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, closeRoot(&snapshotParent)) }()
	leaf, snapshotLeaf := filepath.Base(request.Export), filepath.Base(request.Snapshot)
	// Ordinary aliases (including Windows short names) must not make lexical
	// containment checks admit an output physically inside the checkout.
	for _, name := range []string{parentName, snapshotParentName} {
		inside, err := publisherSourceHasAncestor(name, checkoutInfo)
		if err != nil {
			return err
		}
		if inside {
			return errors.New("publisher source output parent overlaps checkout")
		}
	}
	sameLeaf := leaf == snapshotLeaf || runtime.GOOS == "windows" && strings.EqualFold(leaf, snapshotLeaf)
	if os.SameFile(parentInfo, snapshotParentInfo) && sameLeaf {
		return errors.New("publisher source output targets overlap")
	}
	for _, target := range []struct {
		root *os.Root
		name string
	}{{parent, leaf}, {snapshotParent, snapshotLeaf}} {
		if _, err := target.root.Lstat(target.name); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(errors.New("publisher source target is not fresh"), err)
		}
	}
	before, err := observePublisherSourceGit(ctx, request.Checkout, run)
	if err != nil {
		return err
	}
	if before.commit != request.ExpectedCommit {
		return errors.New("publisher source expected commit mismatch")
	}
	index, err := publisherParseGitEntries(before.index, false)
	if err != nil {
		return err
	}
	base, err := publisherParseGitEntries(before.base, true)
	if err != nil {
		return err
	}
	for name := range generated {
		if _, exists := index[name]; !exists {
			index[name] = publisherGitEntry{}
		}
	}
	names, folded := []string{}, map[string]bool{}
	for name := range index {
		fold := strings.ToLower(name)
		if folded[fold] {
			return ErrPublisherSourceTableInvalid
		}
		folded[fold] = true
		// Validate every declared input before creating any output.
		info, err := publisherSourceRelativeInfo(checkout, name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrPublisherSourceTableInvalid
		}
		names = append(names, name)
	}
	sort.Strings(names)
	owned := publisherOwnedExport{parent: parent, leaf: leaf}
	var snapshotOwned publisherOwnedEntry
	defer func() {
		if returned != nil {
			// Output can fail after all handles have closed. Reopen only the
			// same ordinary parents, then remove our exact retained identities.
			reopen := func(root **os.Root, name string, original os.FileInfo) error {
				if *root != nil {
					return nil
				}
				held, info, err := openPublisherSourceRoot(name)
				if err != nil {
					return err
				}
				if !os.SameFile(original, info) {
					return errors.Join(errors.New("publisher source cleanup parent identity changed"), held.Close())
				}
				*root = held
				return nil
			}
			if snapshotOwned.info != nil {
				if err := reopen(&snapshotParent, snapshotParentName, snapshotParentInfo); err != nil {
					returned = errors.Join(returned, err)
				} else {
					returned = errors.Join(returned, removePublisherOwned(snapshotParent, snapshotOwned))
				}
			}
			if owned.info != nil {
				if err := reopen(&parent, parentName, parentInfo); err != nil {
					returned = errors.Join(returned, err, closeRoot(&owned.root))
				} else {
					owned.parent = parent
					returned = errors.Join(returned, owned.remove())
				}
			}
		} else {
			returned = errors.Join(returned, closeRoot(&owned.root))
		}
	}()
	if err := parent.Mkdir(leaf, 0700); err != nil {
		return err
	}
	owned.info, err = parent.Lstat(leaf)
	if err != nil {
		return err
	}
	owned.root, err = parent.OpenRoot(leaf)
	if err != nil {
		return err
	}
	if err := publisherSourceRootUnchanged(owned.root, request.Export, owned.info); err != nil {
		return err
	}
	type snapshotFile struct {
		SHA256 string `json:"sha256"`
	}
	table := struct {
		Schema string                  `json:"schema"`
		Files  map[string]snapshotFile `json:"files"`
	}{"reused-graphify-source-snapshot-v1", map[string]snapshotFile{}}
	inputs := map[string]os.FileInfo{}
	dirty := false
	createdDirs := map[string]bool{}
	for _, name := range names {
		raw, info, err := readPublisherSourceRelative(ctx, checkout, name)
		if err != nil {
			return err
		}
		inputs[name] = info
		hash := sha256.Sum256(raw)
		table.Files[name] = snapshotFile{hex.EncodeToString(hash[:])}
		entry, tracked := base[name]
		selected := index[name]
		mode := info.Mode().Perm()
		physicalMode := "100644"
		if mode&0111 != 0 {
			physicalMode = "100755"
		}
		// Windows does not expose POSIX executable bits; Git owns that mode.
		if runtime.GOOS == "windows" && selected.mode != "" {
			physicalMode = selected.mode
		}
		if !tracked || publisherGitBlob(raw) != entry.oid || physicalMode != entry.mode {
			dirty = true
		}
		parts := strings.Split(name, "/")
		for n := 1; n < len(parts); n++ {
			dir := filepath.FromSlash(strings.Join(parts[:n], "/"))
			if createdDirs[dir] {
				continue
			}
			if err := owned.root.Mkdir(dir, 0700); err != nil {
				return err
			}
			dirInfo, err := owned.root.Lstat(dir)
			if err != nil {
				return err
			}
			owned.dirs = append(owned.dirs, publisherOwnedEntry{dir, dirInfo})
			createdDirs[dir] = true
		}
		var retained publisherOwnedEntry
		err = publisherWriteExclusive(owned.root, filepath.FromSlash(name), raw, mode, &retained)
		if retained.info != nil {
			owned.files = append(owned.files, retained)
		}
		if err != nil {
			return err
		}
		written, err := publisherSourceRelativeInfo(owned.root, name)
		if err != nil || !os.SameFile(retained.info, written) || (runtime.GOOS != "windows" && written.Mode().Perm() != mode) {
			return errors.Join(ErrPublisherSourceTableInvalid, err)
		}
		owned.files[len(owned.files)-1].info = written
	}
	rawSnapshot, err := json.Marshal(table)
	if err != nil {
		return err
	}
	if err := publisherWriteExclusive(snapshotParent, snapshotLeaf, append(rawSnapshot, '\n'), 0600, &snapshotOwned); err != nil {
		return err
	}
	persisted, snapshotInfo, err := readPublisherSourceRelative(ctx, snapshotParent, snapshotLeaf)
	if err != nil {
		return err
	}
	if !os.SameFile(snapshotOwned.info, snapshotInfo) || !bytes.Equal(persisted, append(rawSnapshot, '\n')) {
		return ErrPublisherSourceTableInvalid
	}
	after, err := observePublisherSourceGit(ctx, request.Checkout, run)
	if err != nil {
		return err
	}
	if before.commit != after.commit || before.tree != after.tree || !bytes.Equal(before.base, after.base) || !bytes.Equal(before.index, after.index) {
		return errors.New("publisher source Git identity changed")
	}
	for _, name := range names {
		raw, info, err := readPublisherSourceRelative(ctx, checkout, name)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		if !os.SameFile(inputs[name], info) || inputs[name].Mode() != info.Mode() || hex.EncodeToString(hash[:]) != table.Files[name].SHA256 {
			return errors.New("publisher source input changed")
		}
	}
	for _, held := range []struct {
		root *os.Root
		name string
		info os.FileInfo
	}{{checkout, request.Checkout, checkoutInfo}, {parent, parentName, parentInfo}, {snapshotParent, snapshotParentName, snapshotParentInfo}, {owned.root, request.Export, owned.info}} {
		if err := publisherSourceRootUnchanged(held.root, held.name, held.info); err != nil {
			return err
		}
	}
	input, err := VerifyPublisherSourceSnapshot(ctx, request.Export, persisted)
	if err != nil {
		return err
	}
	for _, entry := range owned.files {
		info, err := publisherSourceRelativeInfo(owned.root, filepath.ToSlash(entry.name))
		if err != nil || !os.SameFile(entry.info, info) || entry.info.Mode() != info.Mode() {
			return errors.Join(ErrPublisherSourceTableInvalid, err)
		}
	}
	finalSnapshot, finalSnapshotInfo, err := readPublisherSourceRelative(ctx, snapshotParent, snapshotLeaf)
	if err != nil {
		return err
	}
	if !os.SameFile(snapshotOwned.info, finalSnapshotInfo) || !bytes.Equal(persisted, finalSnapshot) {
		return errors.New("publisher source snapshot changed")
	}
	// Close every acquisition handle before publishing any success bytes.
	if err := errors.Join(closeRoot(&owned.root), closeRoot(&checkout), closeRoot(&parent), closeRoot(&snapshotParent)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	row := publisherPreparationResult{"windows-publisher-source-preparation-v1", before.commit, before.tree, dirty, publisherPreparedInput{"windows-publisher-source-input-v1", input}}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	n, err := output.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	return err
}
