//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// These tests exercise DATA decisions before any child or preparation. They
// neither construct a successful private publisher nor assert native admission.
func TestPrivateSerenaMigrationInputRefusesBeforeEffects(t *testing.T) {
	digest := strings.Repeat("a", 64)
	artifact := func(path string) privateSerenaMigrationArtifact {
		return privateSerenaMigrationArtifact{Path: path, SHA256: digest}
	}
	valid := privateSerenaMigrationInput{
		Schema: "private-serena-migration-input-v1", Purpose: privateSerenaMigrationPurpose,
		SourceRoot: `D:\dev\ChatGPT\data-only\source`, SourceEpoch: digest, SourceCommit: strings.Repeat("b", 40),
		Image:          artifact(`D:\dev\ChatGPT\data-only\cli.test.exe`),
		ProductFixture: artifact(`D:\dev\ChatGPT\data-only\product.json`),
		Catalog:        artifact(`D:\dev\ChatGPT\data-only\source\servers\serena\manifest.yaml`),
		UV:             artifact(`C:\read-only-tools\uv.exe`), UVX: artifact(`C:\read-only-tools\uvx.exe`),
		Python:          artifact(`C:\read-only-python\python.exe`),
		IssuanceReceipt: artifact(`D:\dev\ChatGPT\data-only\compiler-inputs.json`),
		EvidenceParent:  `D:\dev\ChatGPT\data-only\e`,
	}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := privateSerenaMigrationDecodeInput(raw); err != nil {
		t.Fatalf("well-formed DATA (not native authority): %v", err)
	}
	for _, test := range []struct {
		name, want string
		change     func(*privateSerenaMigrationInput)
	}{
		{"wrong-purpose", "source/schema/purpose", func(v *privateSerenaMigrationInput) { v.Purpose = "private-readonly-observation" }},
		{"wrong-schema", "source/schema/purpose", func(v *privateSerenaMigrationInput) { v.Schema = "other" }},
		{"short-source-commit", "source/schema/purpose", func(v *privateSerenaMigrationInput) { v.SourceCommit = "abc" }},
		{"uppercase-source-epoch", "source/schema/purpose", func(v *privateSerenaMigrationInput) { v.SourceEpoch = strings.Repeat("A", 64) }},
		{"foreign-catalog", "original Source catalog", func(v *privateSerenaMigrationInput) { v.Catalog.Path = `D:\dev\ChatGPT\other\manifest.yaml` }},
		{"writable-C-evidence", "source/schema/purpose", func(v *privateSerenaMigrationInput) { v.EvidenceParent = `C:\not-owned` }},
		{"unhashed-image", "paths and digests", func(v *privateSerenaMigrationInput) { v.Image.SHA256 = "" }},
		{"different-uvx-sibling", "uv/uvx siblings", func(v *privateSerenaMigrationInput) { v.UVX.Path = `C:\other\uvx.exe` }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := valid
			test.change(&value)
			body, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := privateSerenaMigrationDecodeInput(body); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected the discriminating pre-effect input refusal %q: %v", test.want, err)
			}
		})
	}
	t.Run("missing-explicit-lane", func(t *testing.T) {
		body := bytes.Replace(raw, []byte(`"test_state_path_env":false,`), nil, 1)
		if bytes.Equal(body, raw) {
			t.Fatal("negative fixture did not remove the lane")
		}
		if _, err := privateSerenaMigrationDecodeInput(body); err == nil || !strings.Contains(err.Error(), "explicit compiled lane") {
			t.Fatalf("missing lane was not refused: %v", err)
		}
	})
	t.Run("additional-json-object", func(t *testing.T) {
		if _, err := privateSerenaMigrationDecodeInput(append(append([]byte(nil), raw...), []byte("\n{}")...)); err == nil {
			t.Fatal("trailing JSON object was accepted")
		}
	})
}

func TestPrivateSerenaMigrationCompiledLaneMustMatchParent(t *testing.T) {
	base := debug.BuildInfo{Path: "mcp-local-hub/internal/cli.test", GoVersion: "go1.27.4", Settings: []debug.BuildSetting{
		{Key: "GOOS", Value: "windows"}, {Key: "GOARCH", Value: "amd64"}, {Key: "CGO_ENABLED", Value: "1"},
	}}
	if err := privateSerenaMigrationMatchBuild(&base, &base, false); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*debug.BuildInfo)
	}{
		{"race-child-only", func(v *debug.BuildInfo) {
			v.Settings = append(v.Settings, debug.BuildSetting{Key: "-race", Value: "true"})
		}},
		{"tag-child-only", func(v *debug.BuildInfo) {
			v.Settings = append(v.Settings, debug.BuildSetting{Key: "-tags", Value: "test_state_path_env"})
		}},
		{"wrong-test-package", func(v *debug.BuildInfo) { v.Path = "mcp-local-hub/internal/api.test" }},
		{"different-compiler", func(v *debug.BuildInfo) { v.GoVersion = "go1.27.5" }},
		{"duplicate-build-setting", func(v *debug.BuildInfo) {
			v.Settings = append(v.Settings, debug.BuildSetting{Key: "GOOS", Value: "windows"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			child := base
			child.Settings = append([]debug.BuildSetting(nil), base.Settings...)
			test.change(&child)
			if err := privateSerenaMigrationMatchBuild(&base, &child, false); err == nil {
				t.Fatal("mismatched actual-build DATA accepted")
			}
		})
	}
	tagged := base
	tagged.Settings = append(append([]debug.BuildSetting(nil), base.Settings...), debug.BuildSetting{Key: "-tags", Value: "test_state_path_env"})
	if err := privateSerenaMigrationMatchBuild(&tagged, &tagged, true); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateSerenaMigrationLedgerRequiresFixedSubtests(t *testing.T) {
	roots, preparations, subtests := 0, 0, 0
	for _, root := range privateSerenaMigrationRoots {
		roots++
		preparations += root.Preparations
		subtests += len(root.Subtests)
	}
	if roots != 14 || preparations != 15 || subtests != 4 {
		t.Fatalf("original mechanical inventory drifted: roots=%d preparations=%d subtests=%d", roots, preparations, subtests)
	}
	root, err := privateSerenaMigrationRootFor("TestMigrateSerena_PostCommitTimeoutSupervisorNotAlive_FailsLoud")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	out.WriteString("\x16=== RUN   " + root.Name + "\n")
	for _, sub := range root.Subtests {
		out.WriteString("\x16=== RUN   " + root.Name + "/" + sub + "\n")
		out.WriteString("\x16    --- PASS: " + root.Name + "/" + sub + " (0.01s)\n")
	}
	out.WriteString("\x16--- PASS: " + root.Name + " (0.01s)\n\x16PASS\n")
	valid := out.String()
	if _, err := privateSerenaMigrationLedger([]byte(valid), root); err != nil {
		t.Fatal(err)
	}
	last := root.Name + "/" + root.Subtests[1]
	withoutLast := strings.ReplaceAll(valid, "\x16=== RUN   "+last+"\n", "")
	withoutLast = strings.ReplaceAll(withoutLast, "\x16    --- PASS: "+last+" (0.01s)\n", "")
	for _, test := range []struct{ name, raw string }{
		{"whole-second-subcase-missing", withoutLast},
		{"plain-PASS-is-not-a-control-frame", strings.ReplaceAll(valid, "\x16", "")},
		{"duplicate-terminal", valid + "\x16--- PASS: " + root.Name + " (0.01s)\n"},
		{"duplicate-run", valid + "\x16=== RUN   " + root.Name + "\n"},
		{"unexpected-subcase", valid + "\x16=== RUN   " + root.Name + "/extra\n"},
		{"skip", strings.Replace(valid, "--- PASS: "+last, "--- SKIP: "+last, 1)},
		{"failure", strings.Replace(valid, "--- PASS: "+last, "--- FAIL: "+last, 1)},
		{"truncated-control-frame", strings.TrimSuffix(valid, "\n")},
		{"terminal-before-run", "\x16--- PASS: " + root.Name + " (0.01s)\n" + valid},
		{"duplicate-suite-terminal", valid + "\x16PASS\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := privateSerenaMigrationLedger([]byte(test.raw), root); err == nil {
				t.Fatal("incomplete or false fixed coverage was accepted")
			}
		})
	}
	if _, err := privateSerenaMigrationRootFor(root.Name + "/dead_supervisor"); err == nil {
		t.Fatal("subtest selector accepted as an original root")
	}
}

func TestPrivateSerenaMigrationDeadlineKeepsOriginalBound(t *testing.T) {
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	work, cleanup, err := privateSerenaMigrationDeadlines(end, start)
	if err != nil || !work.Equal(end.Add(-11*time.Second)) || !cleanup.Equal(end.Add(-time.Second)) {
		t.Fatalf("original owner bounds differ: %s %s %v", work, cleanup, err)
	}
	// The existing private preparation method performs this one subtraction.
	if got := work.Add(-api.DefaultSerenaPreparationPolicy().CleanupLimit); !got.Equal(end.Add(-56 * time.Second)) {
		t.Fatalf("preparation reserve was doubled/renewed: %s", got)
	}
	for _, now := range []time.Time{end.Add(-56 * time.Second), end, end.Add(time.Second)} {
		if _, _, err := privateSerenaMigrationDeadlines(end, now); err == nil {
			t.Fatal("expired original preparation interval was renewed")
		}
	}
}

func TestPrivateSerenaMigrationManifestUsesOriginalCatalogPrefix(t *testing.T) {
	catalog, err := os.ReadFile(filepath.Join("..", "..", "servers", "serena", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	parse := func(raw []byte) *config.ServerManifest {
		t.Helper()
		value, err := config.ParseManifestWithEnvironment(bytes.NewReader(raw), func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	actual := parse(catalog)
	for _, original := range []string{legacy2DaemonManifestYAML, alreadyMigratedManifestYAML} {
		body, err := privateSerenaMigrationManifest(catalog, original)
		if err != nil {
			t.Fatal(err)
		}
		got, before := parse(body), parse([]byte(original))
		if got.Command != actual.Command || !reflect.DeepEqual(got.BaseArgs, actual.BaseArgs) || len(got.BaseArgs) < 4 || got.BaseArgs[3] != "start-mcp-server" {
			t.Fatal("genuine catalog command/full materializer prefix was not received")
		}
		got.Command, got.BaseArgs = before.Command, before.BaseArgs
		if !reflect.DeepEqual(got, before) {
			t.Fatal("data conversion changed an original tested shape/port/environment field")
		}
	}
	rewrite := func(command string, args []string) []byte {
		t.Helper()
		var document yaml.Node
		if err := yaml.Unmarshal(catalog, &document); err != nil {
			t.Fatal(err)
		}
		mapping := document.Content[0]
		for i := 0; i < len(mapping.Content); i += 2 {
			switch mapping.Content[i].Value {
			case "command":
				mapping.Content[i+1].Value = command
			case "base_args":
				value := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				for _, arg := range args {
					value.Content = append(value.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: arg})
				}
				mapping.Content[i+1] = value
			}
		}
		body, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, test := range []struct {
		name, command, want string
		args                []string
	}{
		{"old-go-command", "go", "uvx materializer prefix", actual.BaseArgs},
		{"short-old-prefix", "uvx", "uvx materializer prefix", actual.BaseArgs[:3]},
		{"unpinned-source", "uvx", "exact commit pin", []string{"--from", "git+https://example/serena", "serena", "start-mcp-server"}},
		{"non-https-source", "uvx", "HTTPS source", []string{"--from", "git+http://example/serena@" + strings.Repeat("a", 40), "serena", "start-mcp-server"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := privateSerenaMigrationManifest(rewrite(test.command, test.args), legacy2DaemonManifestYAML); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected materializer/source refusal %q: %v", test.want, err)
			}
		})
	}
	if _, err := privateSerenaMigrationManifest(catalog, malformedManifestYAML); err == nil || !strings.Contains(err.Error(), "unlisted original") {
		t.Fatalf("unlisted original DATA template accepted: %v", err)
	}
}
