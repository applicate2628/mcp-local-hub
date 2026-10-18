package api

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"
)

// RegistrySnapshotObserver owns one process-local publication of registry
// content. The durable writer remains Registry.Save; the observer never writes.
// ObserveView serializes read, decode and publication across its consumers.
type RegistrySnapshotObserver struct {
	mu       sync.Mutex
	registry *Registry
	path     string
	readOnly bool
	loaded   bool
	info     os.FileInfo
	bytes    []byte
	current  *registrySnapshotGeneration
	lookups  uint8
	// Deterministic package-test seams; production leaves these nil.
	readFnForTest func(string, func(string, string, map[string]any) error) ([]byte, error)
	barrier       *registrySnapshotTestBarrier
}

type registrySnapshotTestBarrier struct {
	reached chan<- struct{}
	release <-chan struct{}
}

type registrySnapshotGeneration struct {
	rows         []WorkspaceEntry
	serena       []int
	serenaSorted []int
	lsp          []int
}

// RegistrySnapshotView retains one privately immutable content generation.
// It exposes scalar candidates and deep-copies only a selected public row.
type RegistrySnapshotView struct{ generation *registrySnapshotGeneration }

// RegistrySnapshotCandidate contains only value metadata needed for routing.
// Its private identity prevents materialization against another generation.
type RegistrySnapshotCandidate struct {
	generation    *registrySnapshotGeneration
	index         int
	WorkspaceKey  string
	WorkspacePath string
	Language      string
}

func (v RegistrySnapshotView) SerenaCount() int {
	if v.generation == nil {
		return 0
	}
	return len(v.generation.serena)
}

func (v RegistrySnapshotView) LSPCount() int {
	if v.generation == nil {
		return 0
	}
	return len(v.generation.lsp)
}

func (v RegistrySnapshotView) SerenaCandidate(i int) (RegistrySnapshotCandidate, bool) {
	if v.generation == nil {
		return RegistrySnapshotCandidate{}, false
	}
	return v.candidateAt(v.generation.serena, i)
}

// SerenaCandidateByPath follows the precomputed stable workspace-path order.
func (v RegistrySnapshotView) SerenaCandidateByPath(i int) (RegistrySnapshotCandidate, bool) {
	if v.generation == nil {
		return RegistrySnapshotCandidate{}, false
	}
	return v.candidateAt(v.generation.serenaSorted, i)
}

func (v RegistrySnapshotView) LSPCandidate(i int) (RegistrySnapshotCandidate, bool) {
	if v.generation == nil {
		return RegistrySnapshotCandidate{}, false
	}
	return v.candidateAt(v.generation.lsp, i)
}

func (v RegistrySnapshotView) candidateAt(indices []int, i int) (RegistrySnapshotCandidate, bool) {
	if i < 0 || i >= len(indices) {
		return RegistrySnapshotCandidate{}, false
	}
	index := indices[i]
	row := v.generation.rows[index]
	return RegistrySnapshotCandidate{generation: v.generation, index: index,
		WorkspaceKey: row.WorkspaceKey, WorkspacePath: row.WorkspacePath, Language: row.Language}, true
}

func (v RegistrySnapshotView) Materialize(candidate RegistrySnapshotCandidate) (WorkspaceEntry, bool) {
	if v.generation == nil || candidate.generation != v.generation || candidate.index < 0 || candidate.index >= len(v.generation.rows) {
		return WorkspaceEntry{}, false
	}
	return cloneRegistryRow(v.generation.rows[candidate.index]), true
}

func newRegistrySnapshotGeneration(rows []WorkspaceEntry) *registrySnapshotGeneration {
	g := &registrySnapshotGeneration{rows: rows}
	for i, row := range rows {
		if row.Language == SerenaLanguageSentinel {
			g.serena = append(g.serena, i)
		} else {
			g.lsp = append(g.lsp, i)
		}
	}
	g.serenaSorted = append([]int(nil), g.serena...)
	sort.SliceStable(g.serenaSorted, func(i, j int) bool {
		return rows[g.serenaSorted[i]].WorkspacePath < rows[g.serenaSorted[j]].WorkspacePath
	})
	return g
}

const registrySnapshotCompareEvery = 32

// SetPrepublicationBarrierForTest installs a one-shot, channel-only pause.
// It receives no row or byte pointers and must be set before concurrent calls.
func (o *RegistrySnapshotObserver) SetPrepublicationBarrierForTest(reached chan<- struct{}, release <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.barrier = &registrySnapshotTestBarrier{reached: reached, release: release}
}

// PeekPublishedViewForTest does no I/O and does not advance lookup cadence.
func (o *RegistrySnapshotObserver) PeekPublishedViewForTest() RegistrySnapshotView {
	o.mu.Lock()
	defer o.mu.Unlock()
	return RegistrySnapshotView{o.current}
}

// NewRegistrySnapshotObserver selects the existing GUI lock-taking or route
// lock-free read posture. Both modes use the same capped inode-anchored reader.
func NewRegistrySnapshotObserver(registry *Registry, path string, readOnly bool) *RegistrySnapshotObserver {
	return &RegistrySnapshotObserver{registry: registry, path: path, readOnly: readOnly}
}

// ObserveView returns an opaque immutable view and the current failure state.
// A missing file clears the view; read/parse/lock/churn failures retain last good.
// Equal metadata is only a hint: every 32nd lookup compares complete bytes.
func (o *RegistrySnapshotObserver) ObserveView() (view RegistrySnapshotView, state string, err error) {
	if o == nil || o.registry == nil || o.path == "" {
		return RegistrySnapshotView{}, "registry_snapshot_absent", nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	info, err := os.Stat(o.path)
	if err != nil {
		return o.statFailure(err)
	}
	if o.loaded && sameRegistryFileInfo(o.info, info) && o.lookups < registrySnapshotCompareEvery-1 {
		o.lookups++
		return RegistrySnapshotView{o.current}, "", nil
	}
	if !o.readOnly {
		unlock, lockErr := o.registry.Lock()
		if lockErr != nil {
			return o.failure("registry_snapshot_lock_failed", lockErr)
		}
		defer func() {
			if unlockErr := unlock(); unlockErr != nil {
				o.report("registry_snapshot_unlock_failed", unlockErr)
				err = errors.Join(err, unlockErr)
				if state == "" {
					state = "registry_snapshot_unlock_failed"
				}
			}
		}()
	}

	// An atomic replacement between stat and anchored read must not borrow the
	// later path's metadata. Retry under this same mutex with a finite bound.
	for attempt := 0; attempt < 3; attempt++ {
		before, statErr := os.Stat(o.path)
		if statErr != nil {
			return o.statFailure(statErr)
		}
		read := ReadStateFileInodeAnchoredWithAuditSink
		if o.readFnForTest != nil {
			read = o.readFnForTest
		}
		data, readErr := read(o.path, o.registry.auditSink)
		if readErr != nil {
			if registrySnapshotMissing(readErr) {
				return o.clearAbsent()
			}
			return o.failure("registry_snapshot_read_failed", readErr)
		}
		after, statErr := os.Stat(o.path)
		if statErr != nil {
			return o.statFailure(statErr)
		}
		if !sameRegistryFileInfo(before, after) {
			continue
		}
		if o.loaded && bytes.Equal(o.bytes, data) {
			o.info = after
			o.lookups = 0
			return RegistrySnapshotView{o.current}, "", nil
		}
		_, rows, parseErr := decodeRegistryBytes(data, o.path)
		if parseErr != nil {
			return o.failure("registry_snapshot_parse_failed", parseErr)
		}
		if o.barrier != nil {
			barrier := o.barrier
			o.barrier = nil
			close(barrier.reached)
			<-barrier.release
		}
		o.bytes = data
		o.current = newRegistrySnapshotGeneration(rows)
		o.info = after
		o.loaded = true
		o.lookups = 0
		return RegistrySnapshotView{o.current}, "", nil
	}
	return o.failure("registry_snapshot_churn", fmt.Errorf("registry changed during anchored read"))
}

func sameRegistryFileInfo(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func registrySnapshotMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || isHubMcpStateMissingErr(err)
}

func (o *RegistrySnapshotObserver) statFailure(err error) (RegistrySnapshotView, string, error) {
	if registrySnapshotMissing(err) {
		return o.clearAbsent()
	}
	return o.failure("registry_snapshot_read_failed", err)
}

func (o *RegistrySnapshotObserver) clearAbsent() (RegistrySnapshotView, string, error) {
	o.loaded = false
	o.info = nil
	o.bytes = nil
	o.current = nil
	o.lookups = 0
	return RegistrySnapshotView{}, "registry_snapshot_absent", nil
}

func (o *RegistrySnapshotObserver) failure(id string, err error) (RegistrySnapshotView, string, error) {
	o.report(id, err)
	return RegistrySnapshotView{o.current}, id, err
}

func (o *RegistrySnapshotObserver) report(id string, err error) {
	if o.registry.auditSink != nil {
		_ = o.registry.auditSink("warn", id, map[string]any{"reason": err.Error()})
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
}

func cloneRegistryRow(row WorkspaceEntry) WorkspaceEntry {
	if row.ClientEntries != nil {
		entries := make(map[string]string, len(row.ClientEntries))
		for key, value := range row.ClientEntries {
			entries[key] = value
		}
		row.ClientEntries = entries
	}
	if row.Languages != nil {
		row.Languages = append([]string(nil), row.Languages...)
	}
	return row
}
