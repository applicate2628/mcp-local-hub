package api

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

var adoptSupportedClients = []string{
	"claude-code",
	"codex-cli",
	"cursor",
	"vscode",
	"gemini-cli",
	"qwen-cli",
	"antigravity",
	"opencode",
	"mimocode",
}

// AdoptSupportedClients returns the client ids accepted by mcphub adopt.
func AdoptSupportedClients() []string {
	out := make([]string, len(adoptSupportedClients))
	copy(out, adoptSupportedClients)
	return out
}

// AdoptOpts describes an unmanaged direct stdio entry to absorb into mcphub.
type AdoptOpts struct {
	EntryName    string
	Client       string
	ManifestName string
	Port         int
	Clients      []string
	ScanOpts     ScanOpts
}

// AdoptPlan is the side-effect-free preview returned by BuildAdoptPlan.
type AdoptPlan struct {
	EntryName           string
	SourceClient        string
	ManifestName        string
	Port                int
	AdoptClients        []string
	AlsoPresent         []string
	SignatureMismatches []AdoptClientSignatureMismatch
	DisabledSameName    []AdoptClientDisabled
	SecretRoutedKeys    []string
	ManifestYAML        string

	secretValues map[string]string
}

type AdoptClientSignatureMismatch struct {
	Client string
	Reason string
}

type AdoptClientDisabled struct {
	Client string
}

// BuildAdoptPlan extracts an existing direct stdio client entry and renders the
// manifest that ExecuteAdopt would persist. It mutates no disk state.
func (a *API) BuildAdoptPlan(opts AdoptOpts) (*AdoptPlan, error) {
	entryName := strings.TrimSpace(opts.EntryName)
	if entryName == "" {
		return nil, fmt.Errorf("adopt entry name is required")
	}
	sourceClient := strings.TrimSpace(opts.Client)
	if !isAdoptSupportedClient(sourceClient) {
		return nil, fmt.Errorf("--client must be one of %s", strings.Join(adoptSupportedClients, " | "))
	}
	manifestName := strings.TrimSpace(opts.ManifestName)
	if manifestName == "" {
		manifestName = entryName
	}
	if manifestName != entryName {
		return nil, fmt.Errorf("adopt v1 requires --name to equal entry name %q (got %q)", entryName, manifestName)
	}
	if err := CheckManifestName(manifestName); err != nil {
		return nil, err
	}
	if embeddedManifestNamesContains(manifestName) {
		return nil, fmt.Errorf("manifest %q collides with a shipped (built-in) server; adopt refuses to shadow shipped manifests", manifestName)
	}
	if exists, err := manifestExistsIn(defaultManifestDir(), manifestName); err != nil {
		return nil, fmt.Errorf("adopt: check existing disk manifest %q: %w", manifestName, err)
	} else if exists {
		return nil, fmt.Errorf("adopt refuses to create manifest %q because a disk manifest already exists; remove or rename the existing manifest before re-running adopt", manifestName)
	}

	scanOpts := adoptScanOpts(opts.ScanOpts)
	entry, err := a.extractStdioEntryFromClient(sourceClient, entryName, scanOpts)
	if err != nil {
		return nil, err
	}
	if entry.Disabled {
		return nil, fmt.Errorf("server %q in source client %q is disabled; enable it first before adopting", entryName, sourceClient)
	}
	port := opts.Port
	if port == 0 {
		port, err = pickNextFreeAdoptPort()
		if err != nil {
			return nil, err
		}
	} else if err := validateExplicitAdoptPort(port); err != nil {
		return nil, err
	}

	env := cloneStringMap(entry.Env)
	routedKeys, secretValues, err := rewriteAdoptSensitiveEnv(manifestName, env)
	if err != nil {
		return nil, err
	}

	clientScan := a.adoptClientsWithSameNameEntry(entryName, scanOpts, sourceClient, newAdoptEntrySignature(entry))
	foundClients := clientScan.Matching
	mismatches := clientScan.Mismatched
	disabledSameName := clientScan.Disabled
	if len(opts.Clients) > 0 {
		foundClients = clientScan.Found
		mismatches = nil
		disabledSameName = nil
	}
	adoptClients, err := normalizeAdoptClients(opts.Clients, foundClients, sourceClient)
	if err != nil {
		return nil, err
	}
	alsoPresent := clientsOutsideSelection(foundClients, adoptClients)
	manifestYAML := renderStdioBridgeManifestYAML(manifestName, entry.Command, entry.Args, env, port, adoptClientBindings(adoptClients))
	if _, err := a.ManifestValidateMode(manifestYAML, ValidateModeStrict); err != nil {
		return nil, fmt.Errorf("entry name %q is not a valid manifest name: %w; adopt with a valid --name is not supported in v1", manifestName, err)
	}

	return &AdoptPlan{
		EntryName:           entryName,
		SourceClient:        sourceClient,
		ManifestName:        manifestName,
		Port:                port,
		AdoptClients:        adoptClients,
		AlsoPresent:         alsoPresent,
		SignatureMismatches: mismatches,
		DisabledSameName:    disabledSameName,
		SecretRoutedKeys:    routedKeys,
		ManifestYAML:        manifestYAML,
		secretValues:        secretValues,
	}, nil
}

// ExecuteAdopt applies a plan built by BuildAdoptPlan.
func (a *API) ExecuteAdopt(plan *AdoptPlan, w io.Writer) error {
	if plan == nil {
		return fmt.Errorf("adopt plan is nil")
	}
	if w == nil {
		w = io.Discard
	}
	if err := persistAdoptRoutedSecrets(plan.secretValues); err != nil {
		return err
	}
	if err := a.ManifestCreate(plan.ManifestName, plan.ManifestYAML); err != nil {
		if len(plan.SecretRoutedKeys) == 0 {
			return err
		}
		if cleanupErr := deleteAdoptRoutedSecrets(plan.SecretRoutedKeys); cleanupErr != nil {
			return fmt.Errorf("adopt manifest create failed after writing routed vault keys; failed to remove routed vault keys %s: %v: %w", strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ","), cleanupErr, err)
		}
		return fmt.Errorf("adopt manifest create failed after writing routed vault keys; removed routed vault keys %s so adopt can be re-run: %w", strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ","), err)
	}
	if err := a.Install(InstallOpts{
		Server:         plan.ManifestName,
		ClientsInclude: plan.AdoptClients,
		Writer:         w,
	}); err != nil {
		vaultNote := ""
		if cleanupErr := a.ManifestDelete(plan.ManifestName); cleanupErr != nil {
			if len(plan.SecretRoutedKeys) > 0 {
				vaultNote = "; routed vault keys were left intact because the manifest still exists: " + strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ",")
			}
			return fmt.Errorf("adopt install failed after creating manifest %q; failed to remove the adopt-created manifest (%v), so remove it before re-running adopt%s: %w", plan.ManifestName, cleanupErr, vaultNote, err)
		}
		if cleanupErr := deleteAdoptRoutedSecrets(plan.SecretRoutedKeys); cleanupErr != nil {
			vaultNote = "; failed to remove routed vault keys " + strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ",") + ": " + cleanupErr.Error()
		} else if len(plan.SecretRoutedKeys) > 0 {
			vaultNote = "; removed routed vault keys: " + strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ",")
		}
		return fmt.Errorf("adopt install failed after creating manifest %q; removed the adopt-created manifest so adopt can be re-run%s: %w", plan.ManifestName, vaultNote, err)
	}
	emitAdoptExecutedEvent(plan)
	fmt.Fprintf(w, "Adopted %q from %s as manifest %q on port %d.\n", plan.EntryName, plan.SourceClient, plan.ManifestName, plan.Port)
	return nil
}

// PrintAdoptPlan writes a redacted dry-run summary for CLI callers.
func PrintAdoptPlan(w io.Writer, plan *AdoptPlan) {
	if w == nil || plan == nil {
		return
	}
	fmt.Fprintf(w, "Adopt plan for entry %q (dry-run):\n", plan.EntryName)
	fmt.Fprintf(w, "  source client: %s\n", plan.SourceClient)
	fmt.Fprintf(w, "  manifest: %s\n", plan.ManifestName)
	fmt.Fprintf(w, "  port: %d\n", plan.Port)
	fmt.Fprintf(w, "  clients: %s\n", strings.Join(plan.AdoptClients, ","))
	if len(plan.SecretRoutedKeys) > 0 {
		fmt.Fprintf(w, "  secret-routed vault keys: %s\n", strings.Join(plan.SecretRoutedKeys, ","))
	}
	for _, client := range plan.AlsoPresent {
		fmt.Fprintf(w, "  also present in %s - re-run with --client %s or include it via --clients\n", client, client)
	}
	for _, mismatch := range plan.SignatureMismatches {
		fmt.Fprintf(w, "  %s in %s differs (%s) - re-run with --clients %s to adopt it explicitly\n", plan.EntryName, mismatch.Client, mismatch.Reason, adoptExplicitClientsArg(plan.SourceClient, mismatch.Client))
	}
	for _, disabled := range plan.DisabledSameName {
		fmt.Fprintf(w, "  %s in %s is disabled - not adopted; use --clients %s to override\n", plan.EntryName, disabled.Client, adoptExplicitClientsArg(plan.SourceClient, disabled.Client))
	}
	fmt.Fprintln(w, "No changes made. Re-run with --yes to apply.")
}

func adoptExplicitClientsArg(sourceClient, otherClient string) string {
	if sourceClient == "" || sourceClient == otherClient {
		return otherClient
	}
	return sourceClient + "," + otherClient
}

func adoptScanOpts(opts ScanOpts) ScanOpts {
	paths := opts.effectiveConfigPaths()
	if len(paths) == 0 {
		paths = DefaultScanConfigPaths()
	}
	out := opts
	out.ConfigPaths = paths
	out.ClaudeConfigPath = paths["claude-code"]
	out.CodexConfigPath = paths["codex-cli"]
	out.CursorConfigPath = paths["cursor"]
	out.VSCodeConfigPath = paths["vscode"]
	out.GeminiConfigPath = paths["gemini-cli"]
	out.QwenConfigPath = paths["qwen-cli"]
	out.AntigravityConfigPath = paths["antigravity"]
	out.OpenCodeConfigPath = paths["opencode"]
	out.MimoCodeConfigPath = paths["mimocode"]
	return out
}

type adoptClientScanResult struct {
	Found      []string
	Matching   []string
	Mismatched []AdoptClientSignatureMismatch
	Disabled   []AdoptClientDisabled
}

type adoptEntrySignature struct {
	Command string
	Args    []string
	Env     map[string]string
}

func newAdoptEntrySignature(entry extractedStdioEntry) adoptEntrySignature {
	return adoptEntrySignature{
		Command: entry.Command,
		Args:    append([]string(nil), entry.Args...),
		Env:     cloneStringMap(entry.Env),
	}
}

func (sig adoptEntrySignature) diffReasons(other adoptEntrySignature) []string {
	var reasons []string
	if sig.Command != other.Command {
		reasons = append(reasons, "command")
	}
	if !slices.Equal(sig.Args, other.Args) {
		reasons = append(reasons, "args")
	}
	sigKeys := sortedAdoptMapKeys(sig.Env)
	otherKeys := sortedAdoptMapKeys(other.Env)
	if !slices.Equal(sigKeys, otherKeys) {
		reasons = append(reasons, "env keys")
	} else {
		var valueDiffKeys []string
		for _, key := range sigKeys {
			if sig.Env[key] != other.Env[key] {
				valueDiffKeys = append(valueDiffKeys, key)
			}
		}
		if len(valueDiffKeys) > 0 {
			reasons = append(reasons, "env values differ for keys: "+strings.Join(valueDiffKeys, ", "))
		}
	}
	return reasons
}

func formatAdoptSignatureReasons(reasons []string) string {
	if slices.Equal(reasons, []string{"command", "args"}) {
		return "command/args"
	}
	return strings.Join(reasons, ", ")
}

func (a *API) adoptClientsWithSameNameEntry(entryName string, scanOpts ScanOpts, sourceClient string, sourceSignature adoptEntrySignature) adoptClientScanResult {
	var result adoptClientScanResult
	for _, client := range adoptSupportedClients {
		entry, err := a.extractStdioEntryFromClient(client, entryName, scanOpts)
		if err != nil {
			continue
		}
		result.Found = append(result.Found, client)
		if entry.Disabled {
			if client != sourceClient {
				result.Disabled = append(result.Disabled, AdoptClientDisabled{Client: client})
			}
			continue
		}
		reasons := sourceSignature.diffReasons(newAdoptEntrySignature(entry))
		if len(reasons) == 0 {
			result.Matching = append(result.Matching, client)
			continue
		}
		if client != sourceClient {
			result.Mismatched = append(result.Mismatched, AdoptClientSignatureMismatch{
				Client: client,
				Reason: formatAdoptSignatureReasons(reasons),
			})
		}
	}
	if !containsAdoptString(result.Matching, sourceClient) {
		result.Matching = append(result.Matching, sourceClient)
	}
	if !containsAdoptString(result.Found, sourceClient) {
		result.Found = append(result.Found, sourceClient)
	}
	return result
}

func normalizeAdoptClients(requested, found []string, sourceClient string) ([]string, error) {
	var selected []string
	if len(requested) == 0 {
		selected = append(selected, found...)
	} else {
		selected = append(selected, requested...)
	}
	selected = dedupeTrimmedClients(selected)
	if len(selected) == 0 {
		selected = []string{sourceClient}
	}
	for _, client := range selected {
		if !isAdoptSupportedClient(client) {
			return nil, fmt.Errorf("unknown adopt client %q (expected %s)", client, strings.Join(adoptSupportedClients, " | "))
		}
	}
	if !containsAdoptString(selected, sourceClient) {
		return nil, fmt.Errorf("--clients must include source --client %q", sourceClient)
	}
	return selected, nil
}

func dedupeTrimmedClients(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, client := range in {
		trimmed := strings.TrimSpace(client)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

func clientsOutsideSelection(found, selected []string) []string {
	selectedSet := map[string]bool{}
	for _, client := range selected {
		selectedSet[client] = true
	}
	var out []string
	for _, client := range found {
		if !selectedSet[client] {
			out = append(out, client)
		}
	}
	return out
}

func adoptClientBindings(clientNames []string) []map[string]any {
	bindings := make([]map[string]any, 0, len(clientNames))
	for _, client := range clientNames {
		bindings = append(bindings, map[string]any{
			"client":   client,
			"daemon":   "default",
			"url_path": "/mcp",
		})
	}
	return bindings
}

func isAdoptSupportedClient(client string) bool {
	for _, supported := range adoptSupportedClients {
		if client == supported {
			return true
		}
	}
	return false
}

func containsAdoptString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func sortedAdoptMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedAdoptStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func emitAdoptExecutedEvent(plan *AdoptPlan) {
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		return
	}
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	secretKeys := append([]string(nil), plan.SecretRoutedKeys...)
	sort.Strings(secretKeys)
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      SupervisorEventSeverityInfo,
		Source:        "adopt",
		Event:         "adopt-executed",
		Body: map[string]any{
			"client":             plan.SourceClient,
			"entry":              plan.EntryName,
			"manifest":           plan.ManifestName,
			"port":               plan.Port,
			"secret_routed_keys": secretKeys,
		},
	})
}
