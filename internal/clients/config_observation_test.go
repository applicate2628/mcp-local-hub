package clients

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func TestEntryRemovalInspectionReadOnly(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	const valid = `{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`
	for _, tc := range []struct {
		name         string
		contents     string
		existingLock bool
		wantReason   string
	}{
		{name: "present", contents: valid},
		{name: "existing-lock", contents: valid, existingLock: true},
		{name: "absent-config", wantReason: "REMOVAL_INSPECTION_FAILED"},
		{name: "absent-entry", contents: `{}`, wantReason: "REMOVAL_SOURCE_CONFLICT"},
		{name: "malformed-config", contents: `{invalid`, wantReason: "REMOVAL_INSPECTION_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mcp.json")
			if tc.contents != "" {
				if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.existingLock {
				if err := os.WriteFile(path+".lock", []byte("existing lock owner"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			type observedFile struct {
				info os.FileInfo
				data []byte
			}
			before := make(map[string]observedFile)
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				before[entry.Name()] = observedFile{info: info, data: data}
			}
			parentBefore, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			c := newLockingClient(&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"})
			cap, err := DescribeEntryRemovalContext(context.Background(), c, "one")
			if tc.wantReason == "" {
				if err != nil || !cap.Supported || cap.SourceToken == "" {
					t.Fatalf("inspection: cap=%+v err=%v", cap, err)
				}
			} else if err == nil || cap.Supported || cap.SourceToken != "" || cap.Reason != tc.wantReason {
				t.Fatalf("inspection: cap=%+v err=%v, want refusal %s", cap, err, tc.wantReason)
			}
			after, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("inspection changed directory entries: before=%v after=%v", before, after)
			}
			for _, entry := range after {
				prior, ok := before[entry.Name()]
				if !ok {
					t.Fatalf("inspection created %q", entry.Name())
				}
				info, err := entry.Info()
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(prior.info, info) || !prior.info.ModTime().Equal(info.ModTime()) || prior.info.Mode() != info.Mode() || !bytes.Equal(prior.data, data) {
					t.Errorf("inspection replaced or modified %q", entry.Name())
				}
			}
			parentAfter, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !parentBefore.ModTime().Equal(parentAfter.ModTime()) {
				t.Fatal("inspection modified the parent directory")
			}
		})
	}
}

func TestConfigObservationExistingLockExcludesWriter(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	lockPath := path + ".lock"
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writer := flock.New(lockPath)
	t.Cleanup(func() { _ = writer.Unlock() })
	execution, err := withConfigLockExecutionContextMode(path, context.Background(), func() error {
		locked, err := writer.TryLock()
		if err != nil {
			return err
		}
		if locked {
			t.Error("writer acquired the existing lock during inspection")
		}
		return nil
	}, false)
	if err != nil || !execution.bodyEntered || !execution.releaseDefined || !execution.releaseConfirmed {
		t.Fatalf("inspection execution=%+v err=%v", execution, err)
	}
	locked, err := writer.TryLock()
	if err != nil || !locked {
		t.Fatalf("writer could not acquire after inspection: locked=%v err=%v", locked, err)
	}
}

func TestConfigObservationRequiredPolicyLockIsCreated(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	execution, err := withConfigLockExecutionContextPolicy(path, context.Background(), func() error {
		_, err := os.Stat(path + ".lock")
		return err
	}, false, true)
	if err != nil || !execution.bodyEntered || !execution.releaseDefined || !execution.releaseConfirmed {
		t.Fatalf("required policy exclusion execution=%+v err=%v", execution, err)
	}
}
