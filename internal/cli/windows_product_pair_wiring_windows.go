//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/scheduler"
)

type windowsProductPairTxnRequest struct {
	Context             context.Context
	StateDir            string
	CLIPath             string
	WindowlessPath      string
	StagedCLI           string
	StagedWindowless    string
	StartSupervisor     func(string) error
	WaitSupervisorReady func(context.Context, string, binaryadmission.WindowsProductPair) error
	RestartPrior        func(string) error
}

var newWindowsProductPairTxnFn = newWindowsProductPairTxn

func newWindowsProductPairTxn(req windowsProductPairTxnRequest) (*WindowsProductPairTxn, error) {
	if req.Context == nil {
		req.Context = context.Background()
	}
	if req.StateDir == "" || req.CLIPath == "" || req.WindowlessPath == "" || req.StagedCLI == "" || req.StagedWindowless == "" {
		return nil, errors.New("Windows product pair wiring: state and artifact paths are required")
	}
	version, commit, buildDate, err := currentProductBuildMetadata()
	if err != nil {
		return nil, err
	}
	store, err := newWindowsProductPairTaskStore(req.StateDir)
	if err != nil {
		return nil, err
	}
	taskTxn, err := scheduler.BeginOwnedEntrypointTaskTxnWithStore(
		req.Context,
		filepath.Join(req.StateDir, "windows-product-pair-entrypoint.lock"),
		req.CLIPath,
		req.WindowlessPath,
		store,
	)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	tasks := &windowsProductPairTaskOwner{txn: taskTxn, store: store}
	return &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
		CLIPath:          req.CLIPath,
		WindowlessPath:   req.WindowlessPath,
		StagedCLI:        req.StagedCLI,
		StagedWindowless: req.StagedWindowless,
		ReceiptPath:      filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json"),
		Version:          version,
		Commit:           commit,
		BuildDate:        buildDate,
		Tasks:            tasks,
		Deps: WindowsProductPairTxnDeps{
			StartSupervisor:     req.StartSupervisor,
			WaitSupervisorReady: req.WaitSupervisorReady,
			RestartPrior:        req.RestartPrior,
		},
	}}, nil
}

func currentProductBuildMetadata() (version, commit, buildDate string, err error) {
	version, commit, buildDate = buildinfo.Get()
	for _, field := range []struct{ name, value string }{
		{name: "version", value: version},
		{name: "commit", value: commit},
		{name: "build_date", value: buildDate},
	} {
		trimmed := strings.TrimSpace(field.value)
		if trimmed == "" || strings.EqualFold(trimmed, "dev") || strings.EqualFold(trimmed, "unknown") {
			return "", "", "", fmt.Errorf("admit local product build: %s is placeholder %q", field.name, field.value)
		}
	}
	return version, commit, buildDate, nil
}

type windowsProductPairTaskOwner struct {
	txn   *scheduler.OwnedEntrypointTaskTxn
	store *windowsProductPairTaskStore
}

func (o *windowsProductPairTaskOwner) InventoryExport() error { return o.txn.InventoryExport() }
func (o *windowsProductPairTaskOwner) RewriteCommand() error  { return o.txn.RewriteCommand() }
func (o *windowsProductPairTaskOwner) VerifyRuntime() error   { return o.txn.VerifyRuntime() }
func (o *windowsProductPairTaskOwner) RestoreImport() error   { return o.txn.RestoreImport() }
func (o *windowsProductPairTaskOwner) Close() error {
	return errors.Join(o.txn.Close(), o.store.Close())
}

// windowsProductPairTaskStore keeps exact task XML only for the lifetime of one
// pair transaction. Writes and reads use the hardened state-file owner; Close
// removes only the exact leaves created by this instance.
type windowsProductPairTaskStore struct {
	mu     sync.Mutex
	root   string
	hashes map[string]string
	closed bool
}

func newWindowsProductPairTaskStore(stateDir string) (*windowsProductPairTaskStore, error) {
	root, err := os.MkdirTemp(stateDir, "windows-product-pair-task-rollback-")
	if err != nil {
		return nil, fmt.Errorf("create Windows product pair task rollback store: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.Remove(root)
		return nil, fmt.Errorf("protect Windows product pair task rollback store: %w", err)
	}
	return &windowsProductPairTaskStore{root: root, hashes: make(map[string]string)}, nil
}

func (s *windowsProductPairTaskStore) Retain(ctx context.Context, xml []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(xml))
	ref := "task-" + digest + ".xml"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("Windows product pair task rollback store is closed")
	}
	if err := api.WriteStateFileBytesAtomic(filepath.Join(s.root, ref), xml); err != nil {
		return "", err
	}
	readBack, err := api.ReadStateFileBeneathRootNoFollow(ctx, s.root, []string{ref}, digest)
	if err != nil || !bytes.Equal(readBack, xml) {
		if err == nil {
			err = errors.New("retained task XML readback differs")
		}
		return "", err
	}
	s.hashes[ref] = digest
	return ref, nil
}

func (s *windowsProductPairTaskStore) Load(ctx context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	digest, ok := s.hashes[ref]
	closed := s.closed
	s.mu.Unlock()
	if closed || !ok {
		return nil, errors.New("Windows product pair task rollback ref is unavailable")
	}
	return api.ReadStateFileBeneathRootNoFollow(ctx, s.root, []string{ref}, digest)
}

func (s *windowsProductPairTaskStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	refs := make([]string, 0, len(s.hashes))
	for ref := range s.hashes {
		refs = append(refs, ref)
	}
	s.mu.Unlock()
	var errs []error
	for _, ref := range refs {
		for _, leaf := range []string{ref, ref + ".lock"} {
			if err := os.Remove(filepath.Join(s.root, leaf)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if err := os.Remove(s.root); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
