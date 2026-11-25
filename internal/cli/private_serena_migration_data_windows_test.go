//go:build windows

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

const privateSerenaMigrationPurpose = "private-serena-migration-integration"
const privateSerenaMigrationInputEnv = "MCPHUB_PRIVATE_SERENA_MIGRATION_INPUT"
const privateSerenaMigrationInputSHAEnv = "MCPHUB_PRIVATE_SERENA_MIGRATION_INPUT_SHA256"
const privateSerenaMigrationCaseEnv = "MCPHUB_PRIVATE_SERENA_MIGRATION_CASE_CONFIG"

type privateSerenaMigrationArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type privateSerenaMigrationInput struct {
	Schema           string                          `json:"schema"`
	Purpose          string                          `json:"purpose"`
	SourceRoot       string                          `json:"source_root"`
	SourceEpoch      string                          `json:"source_epoch"`
	SourceCommit     string                          `json:"source_commit"`
	TestStatePathEnv bool                            `json:"test_state_path_env"`
	Image            privateSerenaMigrationArtifact  `json:"image"`
	ProductFixture   privateSerenaMigrationArtifact  `json:"product_fixture"`
	Catalog          privateSerenaMigrationArtifact  `json:"catalog"`
	UV               privateSerenaMigrationArtifact  `json:"uv"`
	UVX              privateSerenaMigrationArtifact  `json:"uvx"`
	Python           privateSerenaMigrationArtifact  `json:"python"`
	IssuanceReceipt  privateSerenaMigrationArtifact  `json:"issuance_receipt"`
	EvidenceParent   string                          `json:"evidence_parent"`
	CacheSeed        *privateSerenaMigrationArtifact `json:"cache_seed,omitempty"`
}

type privateSerenaMigrationRoot struct {
	Name         string
	Preparations int
	Subtests     []string
	Outcomes     []string
}

// This inventory is the original fourteen bodies, not a run-prefix selection.
// The restart refusal is DATA; only the other restart subcase prepares a runtime.
var privateSerenaMigrationRoots = []privateSerenaMigrationRoot{
	{Name: "TestMigrateSerena_CatalogDynamicPool_RuntimeLegacyMissing_Proceeds", Preparations: 1},
	{Name: "TestMigrateSerena_CallsInstallParsedManifest_NotDiskWrite", Preparations: 1},
	{Name: "TestMigrateSerena_DrivesSupervisorRestart", Preparations: 1, Subtests: []string{
		"reap_fires_BEFORE_the_intent_write;_start_fires_AFTER",
		"fail-loud_when_the_prior_supervisor_cannot_be_reaped_—_intent_NOT_written",
	}},
	{Name: "TestMigrateSerena_NilSpecRowsHealedBeforeSpawn", Preparations: 1},
	{Name: "TestMigrateSerena_LiveSupervisor_ReapClearsTheGateBeforeWrite", Preparations: 1},
	{Name: "TestMigrateSerena_StartFailureAfterIntentCommit_DoesNotRollBackRegistry", Preparations: 1},
	{Name: "TestMigrateSerena_PostCommitReconcileReadyTimeout_ExitsZeroWithWarning", Preparations: 1},
	{Name: "TestMigrateSerena_PostCommitTimeoutSupervisorNotAlive_FailsLoud", Preparations: 2, Subtests: []string{
		"dead_supervisor", "undeterminable_probe",
	}},
	{Name: "TestMigrateSerena_NoRunningSupervisor_SkipsReap_WritesAndStarts", Preparations: 1},
	{Name: "TestMigrateSerena_ConcurrentRegisterInWindow_AppearsInCommittedIntent", Preparations: 1},
	{Name: "TestMigrateSerena_EmptyFirstSnapshot_WorkspaceRegisteredInWindow_RecomputesStart", Preparations: 1},
	{Name: "TestMigrateSerena_AlreadyMigrated_RegistryDrift_RefansOut", Preparations: 2, Outcomes: []string{privateSerenaMigrationInstalled, privateSerenaMigrationAuditRefusal}},
	{Name: "TestMigrateSerena_EmptyFirstSnapshot_SupervisorRunning_LateReapClearsGate", Preparations: 1},
	{Name: "TestMigrateSerena_Interlock_BlocksConcurrentSupervisorStartInWindow", Preparations: 3, Outcomes: []string{privateSerenaMigrationInstalled, privateSerenaMigrationInterlockRefusal, privateSerenaMigrationSpecRefusal}},
}

const (
	privateSerenaMigrationInstalled        = "installed"
	privateSerenaMigrationAuditRefusal     = "audit-refused-before-intent"
	privateSerenaMigrationInterlockRefusal = "foreign-lock-refused-before-install"
	privateSerenaMigrationSpecRefusal      = "api-spec-bearing-refused-without-token"
)

func (root privateSerenaMigrationRoot) outcome(index int) string {
	if index < 0 || index >= root.Preparations {
		return ""
	}
	if len(root.Outcomes) == 0 {
		return privateSerenaMigrationInstalled
	}
	if len(root.Outcomes) != root.Preparations {
		return ""
	}
	return root.Outcomes[index]
}

func privateSerenaMigrationRootFor(name string) (privateSerenaMigrationRoot, error) {
	for _, root := range privateSerenaMigrationRoots {
		if root.Name == name {
			return root, nil
		}
	}
	return privateSerenaMigrationRoot{}, errors.New("private migration requires one exact original root")
}

func privateSerenaMigrationJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.Join(err, errors.New("private migration input must be one JSON object"))
	}
	return nil
}

func privateSerenaMigrationDecodeInput(raw []byte) (privateSerenaMigrationInput, error) {
	var input privateSerenaMigrationInput
	if err := privateSerenaMigrationJSON(raw, &input); err != nil {
		return input, err
	}
	// An absent lane must not silently become the default lane.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return input, err
	}
	if _, present := fields["test_state_path_env"]; !present || string(fields["test_state_path_env"]) == "null" {
		return input, errors.New("private migration requires an explicit compiled lane")
	}
	if supplied, present := fields["cache_seed"]; present && string(bytes.TrimSpace(supplied)) == "null" {
		return input, errors.New("private migration cache seed must be an explicit path/hash object or absent")
	}
	if input.CacheSeed != nil && !privateSerenaMigrationDPath(input.CacheSeed.Path) {
		return input, errors.New("private migration cache seed must remain in the owned D area")
	}
	if input.Schema != "private-serena-migration-input-v1" || input.Purpose != privateSerenaMigrationPurpose ||
		!validLowerHexSHA256(input.SourceEpoch) || len(input.SourceCommit) != 40 ||
		!validLowerHexSHA256(input.SourceCommit+strings.Repeat("0", 24)) ||
		!privateSerenaMigrationDPath(input.SourceRoot) || !privateSerenaMigrationDPath(input.EvidenceParent) {
		return input, errors.New("private migration source/schema/purpose input differs")
	}
	if input.Catalog.Path != filepath.Join(input.SourceRoot, "servers", "serena", "manifest.yaml") {
		return input, errors.New("private migration catalog must be the original Source catalog")
	}
	for _, artifact := range input.artifacts() {
		if !cleanAbsolutePath(artifact.Path) || !validLowerHexSHA256(artifact.SHA256) {
			return input, errors.New("private migration requires exact absolute artifact paths and digests")
		}
	}
	for _, artifact := range []privateSerenaMigrationArtifact{input.Image, input.ProductFixture, input.Catalog, input.IssuanceReceipt} {
		if !privateSerenaMigrationDPath(artifact.Path) {
			return input, errors.New("private migration evidence/source/image must remain in the owned D area")
		}
	}
	if !samePath(filepath.Dir(input.UV.Path), filepath.Dir(input.UVX.Path)) ||
		!strings.EqualFold(filepath.Base(input.UV.Path), "uv.exe") ||
		!strings.EqualFold(filepath.Base(input.UVX.Path), "uvx.exe") ||
		!strings.EqualFold(filepath.Base(input.Python.Path), "python.exe") {
		return input, errors.New("private migration requires the exact uv/uvx siblings and Python input")
	}
	return input, nil
}

func (input privateSerenaMigrationInput) artifacts() []privateSerenaMigrationArtifact {
	artifacts := []privateSerenaMigrationArtifact{input.Image, input.ProductFixture, input.Catalog, input.UV, input.UVX, input.Python, input.IssuanceReceipt}
	if input.CacheSeed != nil {
		artifacts = append(artifacts, *input.CacheSeed)
	}
	return artifacts
}

func privateSerenaMigrationDPath(path string) bool {
	if !cleanAbsolutePath(path) {
		return false
	}
	relative, err := filepath.Rel(`D:\dev\ChatGPT`, path)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func privateSerenaMigrationBuildSettings(info *debug.BuildInfo, state bool) (map[string]string, error) {
	if info == nil || info.Path != "mcp-local-hub/internal/cli.test" || info.GoVersion == "" {
		return nil, errors.New("private migration requires actual cli.test build information")
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		if _, duplicate := settings[setting.Key]; duplicate {
			return nil, errors.New("duplicate Go build setting")
		}
		settings[setting.Key] = setting.Value
	}
	wantTags := ""
	if state {
		wantTags = "test_state_path_env"
	}
	if settings["-tags"] != wantTags || settings["GOOS"] != "windows" || settings["GOARCH"] != "amd64" ||
		(settings["-race"] != "" && settings["-race"] != "true") {
		return nil, errors.New("private migration actual compiled lane differs")
	}
	return settings, nil
}

func privateSerenaMigrationMatchBuild(parent, child *debug.BuildInfo, state bool) error {
	left, err := privateSerenaMigrationBuildSettings(parent, state)
	if err != nil {
		return err
	}
	right, err := privateSerenaMigrationBuildSettings(child, state)
	if err != nil {
		return err
	}
	if parent.GoVersion != child.GoVersion {
		return errors.New("private migration Go compiler version differs from the actual parent")
	}
	for _, key := range []string{"GOOS", "GOARCH", "GOAMD64", "GOEXPERIMENT", "CGO_ENABLED", "-race", "-tags"} {
		if left[key] != right[key] {
			return fmt.Errorf("private migration build setting %s differs from the actual parent", key)
		}
	}
	return nil
}

// Work includes the original preparation cleanup reserve. preparationContext
// itself subtracts that reserve; the helper must not subtract it twice.
func privateSerenaMigrationDeadlines(parentEnd, now time.Time) (work, cleanup time.Time, err error) {
	cleanup = parentEnd.Add(-time.Second)
	work = cleanup.Add(-10 * time.Second)
	if parentEnd.IsZero() || !now.Before(work.Add(-api.DefaultSerenaPreparationPolicy().CleanupLimit)) {
		return time.Time{}, time.Time{}, errors.New("private migration original deadline has no preparation/cleanup interval")
	}
	return work, cleanup, nil
}

type privateSerenaMigrationTestCount struct {
	Runs   int `json:"runs"`
	Passes int `json:"passes"`
	Fails  int `json:"fails"`
	Skips  int `json:"skips"`
}

func privateSerenaMigrationLedger(raw []byte, root privateSerenaMigrationRoot) (map[string]privateSerenaMigrationTestCount, error) {
	counts := map[string]privateSerenaMigrationTestCount{root.Name: {}}
	for _, subtest := range root.Subtests {
		counts[root.Name+"/"+subtest] = privateSerenaMigrationTestCount{}
	}
	overallPass := 0
	for _, frame := range bytes.Split(raw, []byte{0x16})[1:] {
		end := bytes.IndexByte(frame, '\n')
		if end < 0 {
			return counts, errors.New("private migration truncated a test2json control frame")
		}
		line := strings.TrimSpace(string(frame[:end]))
		if line == "PASS" {
			overallPass++
			continue
		}
		if line == "FAIL" {
			return counts, errors.New("private migration child suite failed")
		}
		var action, name string
		for _, prefix := range []string{"=== RUN   ", "--- PASS: ", "--- FAIL: ", "--- SKIP: "} {
			if strings.HasPrefix(line, prefix) {
				action, name = prefix, strings.TrimPrefix(line, prefix)
				break
			}
		}
		if action == "" {
			continue
		}
		if action != "=== RUN   " {
			if i := strings.LastIndex(name, " ("); i >= 0 {
				name = name[:i]
			}
		}
		value, expected := counts[name]
		if !expected {
			return counts, fmt.Errorf("private migration received an unexpected test: %q", name)
		}
		if action != "=== RUN   " && value.Runs != 1 {
			return counts, fmt.Errorf("private migration terminal arrived without one original RUN: %q", name)
		}
		switch action {
		case "=== RUN   ":
			value.Runs++
		case "--- PASS: ":
			value.Passes++
		case "--- FAIL: ":
			value.Fails++
		case "--- SKIP: ":
			value.Skips++
		}
		counts[name] = value
		if value.Runs > 1 || value.Passes+value.Fails+value.Skips > 1 {
			return counts, fmt.Errorf("private migration duplicate test outcome: %q", name)
		}
	}
	for name, value := range counts {
		if value != (privateSerenaMigrationTestCount{Runs: 1, Passes: 1}) {
			return counts, fmt.Errorf("private migration missing or unsuccessful fixed test: %s: %+v", name, value)
		}
	}
	if overallPass != 1 {
		return counts, fmt.Errorf("private migration requires one actual suite PASS, got %d", overallPass)
	}
	return counts, nil
}

// This transformation preserves the original template and pins its source.
// A supplied private cache seed declares offline use before the existing build
// projection; manifest DATA never grants a prepared/runtime receipt.
func privateSerenaMigrationManifest(catalog []byte, original string, cacheSeed ...bool) ([]byte, error) {
	if len(cacheSeed) > 1 {
		return nil, errors.New("private migration cache seed declaration is singular")
	}
	seeded := len(cacheSeed) == 1 && cacheSeed[0]
	if original != legacy2DaemonManifestYAML && original != alreadyMigratedManifestYAML {
		return nil, errors.New("private migration refuses an unlisted original manifest template")
	}
	lookup := func(string) (string, bool) { return "", false }
	actual, err := config.ParseManifestWithEnvironment(bytes.NewReader(catalog), lookup)
	if err != nil {
		return nil, err
	}
	if err := actual.Validate(); err != nil {
		return nil, err
	}
	if actual.Name != api.SerenaServerName || actual.Transport != config.TransportNativeHTTP || actual.Command != "uvx" ||
		len(actual.BaseArgs) < 4 || actual.BaseArgs[0] != "--from" || actual.BaseArgs[2] != "serena" || actual.BaseArgs[3] != "start-mcp-server" {
		return nil, errors.New("private migration catalog lacks the original uvx materializer prefix")
	}
	for _, argument := range actual.BaseArgs {
		if argument == "--context" || strings.HasPrefix(argument, "--context=") {
			return nil, errors.New("private migration catalog source must not carry a runtime context")
		}
	}
	source := actual.BaseArgs[1]
	at := strings.LastIndex(source, "@")
	if at < 0 || len(source[at+1:]) != 40 || !validLowerHexSHA256(source[at+1:]+strings.Repeat("0", 24)) {
		return nil, errors.New("private migration catalog source requires the original exact commit pin")
	}
	repository, err := url.Parse(strings.TrimPrefix(source[:at], "git+"))
	if err != nil || repository.Scheme != "https" || repository.Host == "" || repository.Path == "" ||
		repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" {
		return nil, errors.New("private migration catalog repository is not the original HTTPS source")
	}
	base, err := config.ParseManifestWithEnvironment(strings.NewReader(original), lookup)
	if err != nil {
		return nil, err
	}
	base.Command = actual.Command
	base.BaseArgs = append([]string(nil), actual.BaseArgs...)
	if seeded {
		if base.Env == nil {
			base.Env = map[string]string{}
		}
		for key := range base.Env {
			if strings.EqualFold(key, "UV_OFFLINE") {
				return nil, errors.New("private migration original template already defines UV_OFFLINE")
			}
		}
		base.Env["UV_OFFLINE"] = "true"
	}
	if _, err := api.BuildInMemorySerenaDynamicPoolManifest(base); err != nil {
		return nil, err
	}
	// Preserve the original YAML field presence: marshaling ServerManifest
	// directly would introduce forbidden empty remote-HTTP fields.
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(original), &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("private migration original manifest is not a mapping")
	}
	changed := 0
	mapping := document.Content[0]
	for i := 0; i < len(mapping.Content); i += 2 {
		switch mapping.Content[i].Value {
		case "command":
			mapping.Content[i+1].Value = actual.Command
			changed++
		case "base_args":
			args := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, argument := range actual.BaseArgs {
				args.Content = append(args.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: argument})
			}
			mapping.Content[i+1] = args
			changed++
		}
	}
	if seeded {
		var environment *yaml.Node
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == "env" {
				environment = mapping.Content[i+1]
			}
		}
		if environment == nil {
			environment = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "env"}, environment)
		}
		if environment.Kind != yaml.MappingNode {
			return nil, errors.New("private migration original env is not a mapping")
		}
		environment.Content = append(environment.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "UV_OFFLINE"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "true"})
	}
	if changed != 2 {
		return nil, errors.New("private migration original command/argument fields differ")
	}
	raw, err := yaml.Marshal(&document)
	if err != nil {
		return nil, err
	}
	roundtrip, err := config.ParseManifestWithEnvironment(bytes.NewReader(raw), lookup)
	if err != nil || !reflect.DeepEqual(roundtrip, base) {
		return nil, errors.Join(err, errors.New("private migration manifest data did not survive the original parser"))
	}
	return raw, nil
}

func privateSerenaMigrationDigest(raw []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
