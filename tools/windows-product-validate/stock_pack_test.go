package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func stockSourceFixture(t *testing.T, files map[string][]byte) (root, snapshot, epoch string) {
	t.Helper()
	parent := t.TempDir()
	root, snapshot = filepath.Join(parent, "source"), filepath.Join(parent, "snapshot.json")
	table := make(map[string]map[string]string, len(files))
	for name, raw := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, raw, 0600); err != nil {
			t.Fatal(err)
		}
		table[name] = map[string]string{"sha256": binaryadmission.RuntimeDigest(raw)}
	}
	raw, err := json.Marshal(map[string]any{"schema": "reused-graphify-source-snapshot-v1", "files": table})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, raw, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := binaryadmission.VerifyPublisherSourceSnapshot(context.Background(), root, raw)
	if err != nil {
		t.Fatal(err)
	}
	return root, snapshot, source.SourceEpoch
}

func TestRuntimeFetchPlanUsesStrictSnapshotBoundYAML(t *testing.T) {
	registry, err := os.ReadFile(filepath.Join("..", "..", "windows-product-artifacts.json"))
	if err != nil {
		t.Fatal(err)
	}
	notice := []byte("synthetic notice\n")
	descriptor := func(component, version, release, repo, hash, pythonFields string) []byte {
		return []byte(fmt.Sprintf(`schema: hub-runtime-release-v1
component: %s
version: "%s"
release: "%s"
target: win32-x64
archive_url: https://github.com/astral-sh/%s/releases/download/%s/example.zip
archive_sha256: %s
archive_size: 9007199254740993
archive_format: zip
layout: [{source: runtime.exe, destination: runtime.exe}]
original_artifacts: [{archive_url: "https://github.com/astral-sh/%s/releases/download/%s/example.zip", archive_sha256: %s}]
original_files: [{source_path: notice.txt, path: LICENSE.txt, sha256: %s}]
executable: runtime.exe
native_requirements: [runtime.exe]
licenses: [LICENSE.txt]
%s`, component, version, release, repo, release, hash, repo, release, hash, binaryadmission.RuntimeDigest(notice), pythonFields))
	}
	python := descriptor("python", "3.13.123", "20990909", "python-build-standalone", strings.Repeat("a", 64),
		"implementation: cpython\nfamily: '3.13'\nabi: cp313\nfree_threaded: false\n")
	uv := descriptor("uv", "9.8.7", "9.8.7", "uv", strings.Repeat("b", 64), "")
	const second = "runtimes/uv/release.yaml"
	files := map[string][]byte{"windows-product-artifacts.json": registry,
		"runtimes/python/release.yaml": python, second: uv, "notice.txt": notice}
	fixture := func() ([]string, string) {
		root, snapshot, epoch := stockSourceFixture(t, files)
		return []string{"--runtime-fetch-plan", "--source-root", root, "--source-snapshot", snapshot, "--expected-source-epoch", epoch}, root
	}
	args, root := fixture()
	var stdout, stderr bytes.Buffer
	if exit := run(args, &stdout, &stderr); exit != 0 {
		t.Fatalf("valid YAML exit=%d stderr=%s", exit, &stderr)
	}
	want := fmt.Sprintf(`{"schema":"windows-runtime-fetch-plan-v1","source_epoch":%q,"archives":[`+
		`{"component":"python","release":"20990909","url":"https://github.com/astral-sh/python-build-standalone/releases/download/20990909/example.zip","sha256":%q,"size":"9007199254740993"},`+
		`{"component":"uv","release":"9.8.7","url":"https://github.com/astral-sh/uv/releases/download/9.8.7/example.zip","sha256":%q,"size":"9007199254740993"}]}`+"\n",
		args[6], strings.Repeat("a", 64), strings.Repeat("b", 64))
	if stdout.String() != want {
		t.Fatalf("invalid fetch plan encoding: %s", &stdout)
	}
	reject := func(args []string, code int, message string) {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if exit := run(args, &stdout, &stderr); exit != code || stdout.Len() != 0 || !strings.Contains(stderr.String(), message) {
			t.Fatalf("expected %d/%q and empty stdout, got %d/%s: %s", code, message, exit, &stderr, &stdout)
		}
	}
	reject(append(append([]string(nil), args...), "--stage-runtime"), 2, "RUNTIME_FETCH_PLAN_USAGE_INVALID")
	badEpoch := append([]string(nil), args...)
	badEpoch[6] = strings.Repeat("c", 64)
	reject(badEpoch, 1, "PUBLISHER_SOURCE_EPOCH_MISMATCH")
	drift := []byte(strings.Replace(string(uv), "archive_size: 9007199254740993", "archive_size: 17", 1))
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(second)), drift, 0600); err != nil {
		t.Fatal(err)
	}
	reject(args, 1, "changed source file")
	files[second] = []byte(string(uv) + "unexpected_field: invalid\n")
	args, _ = fixture()
	reject(args, 1, "unexpected_field")
}

func TestMetaPackVerifiesSnapshotMembersAndSameStreamReceipt(t *testing.T) {
	const version = "1.2.3-beta.4"
	const nameField = `"name":"mcp-local-hub"`
	const versionField = `"version":"` + version + `"`
	const dependency = `"@applicate2628/mcp-local-hub-win32-x64":"` + version + `"`
	const optionalField = `"optionalDependencies":{` + dependency + `}`
	const manifest = "{\n  " + optionalField + ",\n  " + versionField + ",\n  " + nameField +
		",\n  \"description\":\"plain <fixture> & source\",\n  \"ordinary\":{\"name\":\"nested\"}\n}\n"
	files := map[string][]byte{
		"npm/README.md": []byte("readme\n"), "npm/bin/cli.js": []byte("cli\n"),
		"npm/lib/platform-binary.js": []byte("install route\n"), "npm/scripts/postinstall.js": []byte("postinstall\n"),
		"npm/package.json": []byte(manifest),
	}
	root, snapshot, epoch := stockSourceFixture(t, files)
	filename := "mcp-local-hub-" + version + ".tgz"
	archive := filepath.Join(t.TempDir(), filename)
	args := []string{"--meta-pack", archive, "--pack-source-root", root, "--pack-source-snapshot", snapshot,
		"--expected-source-epoch", epoch, "--expected-version", version}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	writeArchive := func(tamper, tarTail, gzipTail bool) []byte {
		t.Helper()
		var raw bytes.Buffer
		g := gzip.NewWriter(&raw)
		w := tar.NewWriter(g)
		for _, name := range names {
			body := files[name]
			if tamper && name == "npm/bin/cli.js" {
				body = []byte("bad\n")
			}
			if err := w.WriteHeader(&tar.Header{Name: "package/" + strings.TrimPrefix(name, "npm/"), Mode: 0644, Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if tarTail {
			if _, err := g.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
		if gzipTail {
			raw.WriteByte(1)
		}
		if err := os.WriteFile(archive, raw.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		return raw.Bytes()
	}
	raw := writeArchive(false, false, false)
	var stdout, stderr bytes.Buffer
	if exit := run(args, &stdout, &stderr); exit != 0 {
		t.Fatalf("valid archive exit=%d stderr=%s", exit, &stderr)
	}
	want := fmt.Sprintf("{\"schema\":\"npm-meta-pack-verification-v1\",\"archive_filename\":%q,\"archive_size\":%d,\"archive_sha256\":%q}\n",
		filename, len(raw), binaryadmission.RuntimeDigest(raw))
	if stdout.String() != want {
		t.Fatalf("receipt does not bind the packed bytes: %s", &stdout)
	}
	for _, check := range []struct {
		tamper, tarTail, gzipTail bool
		diagnostic                string
	}{
		{true, false, false, "PACK_FILE_DIGEST_MISMATCH: bin/cli.js"},
		{false, true, false, "PACK_TAR_TRAILING_DATA"},
		{false, false, true, "PACK_GZIP_TRAILING_DATA"},
	} {
		writeArchive(check.tamper, check.tarTail, check.gzipTail)
		stdout.Reset()
		stderr.Reset()
		if exit := run(args, &stdout, &stderr); exit != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), check.diagnostic) {
			t.Fatalf("expected %q: exit=%d stdout=%s stderr=%s", check.diagnostic, exit, &stdout, &stderr)
		}
	}
	for _, check := range []struct{ field, ambiguous string }{
		{nameField, `"name":"other",` + nameField},
		{versionField, `"version":"0.0.0",` + versionField},
		{optionalField, `"optionalDependencies":{},` + optionalField},
		{dependency, `"\u0040applicate2628/mcp-local-hub-win32-x64":"0.0.0",` + dependency},
		{dependency, dependency + `,"unexpected":"0.0.0"`},
	} {
		files["npm/package.json"] = []byte(strings.Replace(manifest, check.field, check.ambiguous, 1))
		root, snapshot, epoch = stockSourceFixture(t, files)
		writeArchive(false, false, false)
		stdout.Reset()
		stderr.Reset()
		badArgs := []string{"--meta-pack", archive, "--pack-source-root", root, "--pack-source-snapshot", snapshot,
			"--expected-source-epoch", epoch, "--expected-version", version}
		if exit := run(badArgs, &stdout, &stderr); exit != 1 || stdout.Len() != 0 ||
			!strings.Contains(stderr.String(), "PACK_META_SOURCE_MANIFEST_MISMATCH") {
			t.Fatalf("ambiguous manifest %q: exit=%d stdout=%s stderr=%s", check.ambiguous, exit, &stdout, &stderr)
		}
	}
}
