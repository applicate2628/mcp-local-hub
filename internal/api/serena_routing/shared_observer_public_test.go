package serena_routing_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/lsp_routing"
	"mcp-local-hub/internal/api/serena_routing"
	"mcp-local-hub/internal/config"
)

func TestSharedRegistryObserver_GUILockedWriterAndReadersSettle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	save := func(port int) error {
		reg := api.NewRegistry(path)
		unlock, err := reg.Lock()
		if err != nil {
			return err
		}
		defer unlock()
		if err := reg.PutSerena(api.WorkspaceEntry{
			WorkspaceKey: "workspace", Language: api.SerenaLanguageSentinel, Port: port,
		}); err != nil {
			return err
		}
		if err := reg.PutLSP(api.WorkspaceEntry{
			WorkspaceKey: "workspace", Language: "go", Port: port + 100,
		}); err != nil {
			return err
		}
		return reg.Save()
	}
	if err := save(9300); err != nil {
		t.Fatal(err)
	}
	observer := api.NewRegistrySnapshotObserver(api.NewRegistry(path), path, false)
	serena := serena_routing.NewWorkspaceResolverWithObserver(observer, path)
	lsp := lsp_routing.NewWorkspaceResolverWithObserver(observer, nil)
	if rows := serena.ListWorkspaces(); len(rows) != 1 || rows[0].Port != 9300 {
		t.Fatalf("seed Serena = %+v", rows)
	}
	if row, ok := lsp.RegisteredWorkspace("workspace", "go"); !ok || row.Port != 9400 {
		t.Fatalf("seed LSP = %+v, present %v", row, ok)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 1; i <= 16; i++ {
			if err := save(9300 + i); err != nil {
				errors <- fmt.Errorf("write %d: %w", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 64; i++ {
			if rows := serena.ListWorkspaces(); len(rows) != 1 || rows[0].Port < 9300 || rows[0].Port > 9316 {
				errors <- fmt.Errorf("Serena incomplete result: %+v", rows)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 64; i++ {
			if row, ok := lsp.RegisteredWorkspace("workspace", "go"); !ok || row.Port < 9400 || row.Port > 9416 {
				errors <- fmt.Errorf("LSP incomplete result: %+v, present %v", row, ok)
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("GUI writer/readers did not settle")
	}
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	for i := 0; i < 32; i++ {
		rows := serena.ListWorkspaces()
		row, ok := lsp.RegisteredWorkspace("workspace", "go")
		if len(rows) == 1 && rows[0].Port == 9316 && ok && row.Port == 9416 {
			return
		}
	}
	t.Fatal("final GUI registry generation was not visible to both readers")
}

func TestSharedRegistryObserver_PublicResolversDoNotRegressOnOverlappingRefresh(t *testing.T) {
	for _, first := range []string{"serena", "lsp"} {
		t.Run(first+"-then-other", func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			if err := os.MkdirAll(filepath.Join(project, ".serena"), 0o700); err != nil {
				t.Fatal(err)
			}
			for path, data := range map[string]string{
				filepath.Join(project, ".serena", "project.yml"): "project_name: test\n",
				filepath.Join(project, "go.mod"):                 "module test\n",
				filepath.Join(project, "main.go"):                "package main\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			canonical, err := api.CanonicalWorkspacePath(project)
			if err != nil {
				t.Fatal(err)
			}
			key := api.WorkspaceKey(canonical)
			registryPath := filepath.Join(root, "workspaces.yaml")
			save := func(serenaPort, lspPort int) {
				t.Helper()
				reg := api.NewRegistry(registryPath)
				if err := reg.PutSerena(api.WorkspaceEntry{
					WorkspaceKey: key, WorkspacePath: canonical,
					Language: api.SerenaLanguageSentinel, Backend: "serena", Port: serenaPort,
				}); err != nil {
					t.Fatal(err)
				}
				if err := reg.PutLSP(api.WorkspaceEntry{
					WorkspaceKey: key, WorkspacePath: canonical,
					Language: "go", Backend: "gopls-mcp", Port: lspPort,
				}); err != nil {
					t.Fatal(err)
				}
				if err := reg.Save(); err != nil {
					t.Fatal(err)
				}
			}
			save(9300, 9200)
			observer := api.NewRegistrySnapshotObserver(api.NewRegistry(registryPath), registryPath, true)
			serena := serena_routing.NewWorkspaceResolverWithObserver(observer, registryPath)
			lsp := lsp_routing.NewWorkspaceResolverWithObserver(observer, []config.LanguageSpec{{
				Name: "go", Backend: "gopls-mcp", ProjectMarkers: []string{"go.mod"},
			}})
			file := filepath.Join(project, "main.go")
			resolve := func(which string) (int, error) {
				if which == "serena" {
					row, err := serena.ResolveByPath(file)
					if err != nil {
						return 0, err
					}
					return row.Port, nil
				}
				result, err := lsp.ResolveByPath(file, "go")
				if err != nil {
					return 0, err
				}
				if !result.Registered || result.Entry == nil {
					return 0, lsp_routing.ErrWorkspaceNotFound
				}
				return result.Entry.Port, nil
			}
			for which, want := range map[string]int{"serena": 9300, "lsp": 9200} {
				if got, err := resolve(which); err != nil || got != want {
					t.Fatalf("seed %s: port=%d err=%v, want %d", which, got, err, want)
				}
			}
			save(9301, 9201)
			reached := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			observer.SetPrepublicationBarrierForTest(reached, release)
			type result struct {
				port int
				err  error
			}
			aResult := make(chan result, 1)
			go func() { port, err := resolve(first); aResult <- result{port, err} }()
			select {
			case <-reached:
			case <-time.After(5 * time.Second):
				t.Fatal("A did not reach prepublication barrier")
			}
			save(9302, 9202)
			second := "lsp"
			wantB := 9202
			if first == "lsp" {
				second, wantB = "serena", 9302
			}
			enteredB := make(chan struct{})
			bResult := make(chan result, 1)
			go func() { close(enteredB); port, err := resolve(second); bResult <- result{port, err} }()
			<-enteredB
			select {
			case got := <-bResult:
				t.Fatalf("B passed paused A: %+v", got)
			case <-time.After(100 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			for label, resultChan := range map[string]<-chan result{"A": aResult, "B": bResult} {
				select {
				case got := <-resultChan:
					if got.err != nil {
						t.Fatalf("%s resolve: %v", label, got.err)
					}
					if label == "B" && got.port != wantB {
						t.Fatalf("first B public result port=%d, want %d", got.port, wantB)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s resolve did not settle", label)
				}
			}
			// No ObserveView or public lookup may heal a regressed generation here.
			published := observer.PeekPublishedViewForTest()
			serenaCandidate, ok := published.SerenaCandidate(0)
			if !ok {
				t.Fatal("published Serena candidate missing")
			}
			serenaRow, _ := published.Materialize(serenaCandidate)
			lspCandidate, ok := published.LSPCandidate(0)
			if !ok {
				t.Fatal("published LSP candidate missing")
			}
			lspRow, _ := published.Materialize(lspCandidate)
			if serenaRow.Port != 9302 || lspRow.Port != 9202 {
				t.Fatalf("published generation regressed: Serena=%d LSP=%d", serenaRow.Port, lspRow.Port)
			}
			otherWant := 9302
			if first == "serena" {
				otherWant = 9302
			} else {
				otherWant = 9202
			}
			if got, err := resolve(first); err != nil || got != otherWant {
				t.Fatalf("later %s result port=%d err=%v want %d", first, got, err, otherWant)
			}
		})
	}
}
