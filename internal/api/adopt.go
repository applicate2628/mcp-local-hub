package api

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/config"
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
	EntryName        string
	SourceClient     string
	ManifestName     string
	Port             int
	AdoptClients     []string
	AlsoPresent      []string
	SecretRoutedKeys []string
	ManifestYAML     string

	secretValues map[string]string
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

	scanOpts := adoptScanOpts(opts.ScanOpts)
	draft, err := a.ExtractManifestFromClient(sourceClient, entryName, scanOpts)
	if err != nil {
		return nil, err
	}
	m, err := config.ParseManifest(bytes.NewReader([]byte(draft)))
	if err != nil {
		return nil, fmt.Errorf("parse extracted manifest draft: %w", err)
	}
	if len(m.Daemons) == 0 {
		return nil, fmt.Errorf("extracted manifest for %q has no daemon row", entryName)
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
	m.Name = manifestName
	m.Kind = config.KindGlobal
	m.Transport = config.TransportStdioBridge
	m.Daemons = []config.DaemonSpec{{Name: "default", Port: port}}
	routedKeys, secretValues := rewriteAdoptSensitiveEnv(m.Env)

	foundClients := a.adoptClientsWithSameNameEntry(entryName, scanOpts)
	adoptClients, err := normalizeAdoptClients(opts.Clients, foundClients, sourceClient)
	if err != nil {
		return nil, err
	}
	alsoPresent := clientsOutsideSelection(foundClients, adoptClients)
	manifestYAML := renderStdioBridgeManifestYAML(manifestName, m.Command, m.BaseArgs, m.Env, port, adoptClientBindings(adoptClients))
	if _, err := config.ParseManifest(strings.NewReader(manifestYAML)); err != nil {
		return nil, fmt.Errorf("render adopted manifest: %w", err)
	}

	return &AdoptPlan{
		EntryName:        entryName,
		SourceClient:     sourceClient,
		ManifestName:     manifestName,
		Port:             port,
		AdoptClients:     adoptClients,
		AlsoPresent:      alsoPresent,
		SecretRoutedKeys: routedKeys,
		ManifestYAML:     manifestYAML,
		secretValues:     secretValues,
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
		return err
	}
	if err := a.Install(InstallOpts{
		Server:         plan.ManifestName,
		ClientsInclude: plan.AdoptClients,
		Writer:         w,
	}); err != nil {
		vaultNote := ""
		if len(plan.SecretRoutedKeys) > 0 {
			keys := append([]string(nil), plan.SecretRoutedKeys...)
			sort.Strings(keys)
			vaultNote = "; routed vault keys were left intact: " + strings.Join(keys, ",")
		}
		if cleanupErr := a.ManifestDelete(plan.ManifestName); cleanupErr != nil {
			return fmt.Errorf("adopt install failed after creating manifest %q; failed to remove the adopt-created manifest (%v), so remove it before re-running adopt%s: %w", plan.ManifestName, cleanupErr, vaultNote, err)
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
		fmt.Fprintf(w, "  secret-routed env keys: %s\n", strings.Join(plan.SecretRoutedKeys, ","))
	}
	for _, client := range plan.AlsoPresent {
		fmt.Fprintf(w, "  also present in %s - re-run with --client %s or include it via --clients\n", client, client)
	}
	fmt.Fprintln(w, "No changes made. Re-run with --yes to apply.")
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

func (a *API) adoptClientsWithSameNameEntry(entryName string, scanOpts ScanOpts) []string {
	var found []string
	for _, client := range adoptSupportedClients {
		if _, err := a.ExtractManifestFromClient(client, entryName, scanOpts); err == nil {
			found = append(found, client)
		}
	}
	return found
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
