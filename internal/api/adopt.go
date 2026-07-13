package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// presentAtBuild is the set of clients whose same-name entry was PRESENT and
	// adoptable at BuildAdoptPlan time (= clientScan.Matching, which always
	// includes the source client). captureAdoptProvenance fails CLOSED if any
	// SELECTED client in this set reads absent/no-entry at capture: that is a
	// Build->capture change (the entry was deleted / renamed / edited away), and
	// since Install still writes the hub relay to the client, recording it `absent`
	// with no snapshot would let de-adopt "restore to absence" and DELETE the
	// operator's adopted entry unrecoverably (security F4). A client NOT in this
	// set is a legitimate entryless-fanout target and may classify `absent`.
	//
	// UNEXPORTED (codex bot PR #528 finding 6): gui/adopt.go embeds *AdoptPlan into
	// the /api/adopt/plan response, so an EXPORTED field would serialize onto the
	// wire (regressing design claim 9 — byte-unchanged plan response). An unexported
	// field is structurally un-serializable and can never leak through any embed.
	presentAtBuild []string

	secretValues map[string]string
}

type ExecuteAdoptOpts struct {
	SymlinkConsents []ResolvedSymlinkConsent
}

type AdoptClientSignatureMismatch struct {
	Client string
	Reason string
}

type AdoptClientDisabled struct {
	Client string
}

// AdoptClientErrored records a same-name client whose config is PRESENT but
// UNREADABLE (parse failure, permission denied, or an already-hub-managed relay
// entry) — the class that would trip a downstream AddEntry rollback if the
// operator explicitly repointed it. Reason is a PATH-FREE class label (never raw
// err.Error(), which may embed an absolute filesystem path / username), so it is
// safe on the /api wire — mirroring the fail-closed path-redaction posture of the
// client-config write path (PR #516). bug 2026-07-08 adopt Area-3.
type AdoptClientErrored struct {
	Client string
	Reason string
}

// adoptExtractionErrorClass maps an extractStdioEntryFromClient failure to a
// PATH-FREE reason class and reports whether it represents a CORRUPTED /
// unadoptable client config. Only corrupted configs are recorded in the Errored
// bucket; an ABSENT config (no file) or an entry-not-present in an otherwise-valid
// config is a normal not-a-candidate that stays a silent skip — so the adopt is
// not blocked by every unconfigured client and the accidental fan-out to a
// valid-but-entryless client is preserved.
//
// Classification is BY TYPED SENTINEL ONLY (errors.Is) — it NEVER substring-matches
// err.Error(). The text of a failure may embed an absolute filesystem path (a
// *fs.PathError from os.ReadFile, OR a MiMoCode parse error that wraps its layer
// path via `fmt.Errorf("parse %s: %w", ...)`), and an adversarial config path
// could otherwise contain one of the classifier phrases and force a wrong verdict
// — a permission-denied / malformed config at C:\...\not found in client\mcp.json
// would misclassify as "entry not present" and reopen the silent partial-apply
// class (codex D2/D3 P1). The returned reason is always a fixed path-free label.
func adoptExtractionErrorClass(err error) (reason string, corrupted bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, fs.ErrNotExist):
		return "", false // absent config file — normal not-a-candidate
	case errors.Is(err, ErrClientEntryNotPresent):
		return "", false // valid config, entry simply absent — preserve fan-out
	case errors.Is(err, fs.ErrPermission):
		return "config file could not be read (permission denied)", true
	case errors.Is(err, ErrClientEntryNotStdio):
		return "same-name entry is HTTP-only or already hub-managed (demigrate it first)", true
	case errors.Is(err, ErrClientEntryHubRelay):
		return "already a hub-managed relay entry — cannot re-adopt", true
	default:
		// Any other failure — a parse error, an unreadable file, or an
		// unrecognized shape — is treated as corrupted (fail closed), so a
		// genuinely-broken config is never silently fanned-out to, and no
		// path-bearing error text is ever inspected.
		return "config could not be read or parsed", true
	}
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
	adoptClients, err := normalizeAdoptClients(opts.Clients, clientScan.Matching, sourceClient, clientScan.Mismatched, clientScan.Disabled, clientScan.Errored)
	if err != nil {
		return nil, err
	}
	alsoPresent := clientsOutsideSelection(clientScan.Matching, adoptClients)
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
		SignatureMismatches: clientScan.Mismatched,
		DisabledSameName:    clientScan.Disabled,
		SecretRoutedKeys:    routedKeys,
		ManifestYAML:        manifestYAML,
		presentAtBuild:      append([]string(nil), clientScan.Matching...),
		secretValues:        secretValues,
	}, nil
}

// promoteAdoptProvenanceFn is the post-Install provenance promote step, injected
// as a package var ONLY so a test can exercise the non-fatal flip-failure path
// (a promote-flip failure must NOT roll back a committed adopt — design claim 10,
// AC C5). Production always uses promoteAdoptProvenanceToAdopted.
var promoteAdoptProvenanceFn = promoteAdoptProvenanceToAdopted

// gcOrphanedAdoptingProvenanceFn is the step-0a cross-manifest orphan GC, injected
// as a package var ONLY so a test can exercise the non-fatal path (a GC failure
// must NOT block a fresh adopt — AC D3). Production always uses
// gcOrphanedAdoptingProvenance. Same seam idiom as promoteAdoptProvenanceFn (C5).
var gcOrphanedAdoptingProvenanceFn = gcOrphanedAdoptingProvenance

// abortProvenanceNote folds a best-effort abortAdoptProvenance result into an
// operator-facing suffix appended to the adopt error, mirroring the existing
// secret/manifest cleanup notes. It returns "" when abort succeeded (the common
// case), so the existing error text and errors.Is chains stay byte-unchanged on
// the happy cleanup path; a genuine abort failure is surfaced as a note without
// masking the caller's original error.
func abortProvenanceNote(err error) string {
	if err == nil {
		return ""
	}
	return "; additionally failed to clean up pre-adopt provenance: " + err.Error()
}

// ExecuteAdopt applies a plan built by BuildAdoptPlan.
func (a *API) ExecuteAdopt(plan *AdoptPlan, w io.Writer) error {
	return a.ExecuteAdoptWithOpts(plan, w, ExecuteAdoptOpts{})
}

// ExecuteAdoptWithOpts applies a plan built by BuildAdoptPlan with scoped
// request data that should affect only this execution.
func (a *API) ExecuteAdoptWithOpts(plan *AdoptPlan, w io.Writer, opts ExecuteAdoptOpts) error {
	if plan == nil {
		return fmt.Errorf("adopt plan is nil")
	}
	if w == nil {
		w = io.Discard
	}
	// Step 0a — best-effort reap of stale CROSS-manifest `adopting` orphans (a
	// prior adopt that hard-crashed between capture and promote/abort left an
	// owner-only, secret-bearing snapshot with no reaper). NON-FATAL: a GC failure
	// must never block a fresh adopt (design "Orphan lifecycle" (b)); the
	// same-manifest UPSERT in captureAdoptProvenance is the complement.
	_, _ = gcOrphanedAdoptingProvenanceFn(adoptOrphanGCThreshold)
	// Step 0b — acquire the per-manifest adopt LEASE (design r2 Signal 1). Held
	// (flock) across capture -> Install -> promote / abort so no concurrent
	// same-manifest adopt or reaper can touch this adopt's `adopting` row (claim 16):
	// a reaper that cannot TryLock the lease skips; a second same-manifest adopt
	// FAILs CLOSED here. Released on EVERY exit path via defer. The lease is
	// PER-MANIFEST — adopts of DIFFERENT manifests never contend.
	lease, leased, leaseErr := tryAcquireAdoptManifestLease(plan.ManifestName)
	if leaseErr != nil {
		return fmt.Errorf("adopt: acquire per-manifest lease for %q: %w", plan.ManifestName, leaseErr)
	}
	if !leased {
		return fmt.Errorf("adopt: a concurrent adopt of manifest %q is already in progress; retry after it completes", plan.ManifestName)
	}
	defer func() { _ = lease.Unlock() }()
	// Durable pre-adopt provenance capture, BEFORE the first irreversible mutation
	// (persistAdoptRoutedSecrets, below). A capture failure fails the adopt CLOSED
	// with ZERO side effects — no vault key, no manifest, no client-config write —
	// so a currently-successful adopt is never regressed (design "Fail-closed
	// capture seam"; claims 1-2). Every failure branch after this point aborts the
	// captured provenance so no secret-bearing snapshot orphan survives a failed
	// adopt.
	rec, err := a.captureAdoptProvenance(plan)
	if err != nil {
		// Classify the data-preservation-gate refusal (a prior committed-looking adopt
		// whose configs were mutated since capture) distinctly from an ordinary capture
		// I/O failure so the audit trail is diagnosable. Reason stays a path-free class
		// (NAMES/COUNTS only — no secret values, no filesystem paths).
		reason := "pre-adopt provenance capture failed"
		if errors.Is(err, errAdoptPriorConfigMutated) {
			reason = "prior_adopt_config_mutated"
		}
		emitAdoptProvenanceCaptureFailed(plan.ManifestName, "", reason)
		return fmt.Errorf("adopt: capture pre-adopt provenance before any mutation: %w", err)
	}
	if err := persistAdoptRoutedSecrets(plan.secretValues); err != nil {
		if note := abortProvenanceNote(abortAdoptProvenance(rec)); note != "" {
			return fmt.Errorf("%w%s", err, note)
		}
		return err
	}
	if err := a.ManifestCreate(plan.ManifestName, plan.ManifestYAML); err != nil {
		abortNote := abortProvenanceNote(abortAdoptProvenance(rec))
		if len(plan.SecretRoutedKeys) == 0 {
			if abortNote == "" {
				return err
			}
			return fmt.Errorf("%w%s", err, abortNote)
		}
		if cleanupErr := deleteAdoptRoutedSecrets(plan.SecretRoutedKeys); cleanupErr != nil {
			return fmt.Errorf("adopt manifest create failed after writing routed vault keys; failed to remove routed vault keys %s: %v%s: %w", strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ","), cleanupErr, abortNote, err)
		}
		return fmt.Errorf("adopt manifest create failed after writing routed vault keys; removed routed vault keys %s so adopt can be re-run%s: %w", strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ","), abortNote, err)
	}
	if err := a.Install(InstallOpts{
		Server:          plan.ManifestName,
		ClientsInclude:  plan.AdoptClients,
		Writer:          w,
		SymlinkConsents: opts.SymlinkConsents,
	}); err != nil {
		// PRESERVE branch (bug 2026-07-12): Install's own client-config rollback could
		// not CONFIRM the pre-adopt state of ≥1 client (it may have been rewritten to
		// the hub relay and not reversed, OR a restore write failed on an
		// otherwise-untouched config). Aborting here would delete the pre-adopt
		// provenance snapshot needed to reverse a still-committed client — a data-loss
		// window. Keep the WHOLE
		// partially-committed state (row `adopting` + snapshots + manifest + routed
		// vault keys) so the post-#532 GC reclaim gates (classifyDeadAdoptingRow
		// Signal 2b + adoptRowProvablyUnmutated) reclaim it once the operator
		// reverses the partial commit; do NOT abort, delete the manifest, or drop the
		// keys. Do NOT promote to `adopted` — `adopting`+manifest-present is exactly
		// the Signal-2b committed-keep state.
		var rbErr *InstallClientRollbackIncompleteError
		if errors.As(err, &rbErr) {
			emitAdoptProvenancePreserved(plan.ManifestName, rbErr.Clients) // NAMES/COUNTS only
			return fmt.Errorf(
				"adopt install failed and its client-config rollback could not be fully reversed "+
					"(clients whose pre-adopt restoration could not be confirmed: %s); the pre-adopt provenance snapshot, "+
					"manifest %q, and routed vault keys were PRESERVED so the state stays recoverable — "+
					"restore those clients from the timestamped .bak-mcp-local-hub-* backup printed above, "+
					"then remove manifest %q, or reverse it with de-adopt once available: %w",
				strings.Join(sortedAdoptStrings(rbErr.Clients), ","), plan.ManifestName, plan.ManifestName, err)
		}
		// ABORT — rollback provably complete OR nothing client-side mutated. Existing
		// cleanup UNCHANGED below.
		vaultNote := ""
		abortNote := abortProvenanceNote(abortAdoptProvenance(rec))
		if cleanupErr := a.ManifestDelete(plan.ManifestName); cleanupErr != nil {
			if len(plan.SecretRoutedKeys) > 0 {
				vaultNote = "; routed vault keys were left intact because the manifest still exists: " + strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ",")
			}
			return fmt.Errorf("adopt install failed after creating manifest %q; failed to remove the adopt-created manifest (%v), so remove it before re-running adopt%s%s: %w", plan.ManifestName, cleanupErr, vaultNote, abortNote, err)
		}
		if cleanupErr := deleteAdoptRoutedSecrets(plan.SecretRoutedKeys); cleanupErr != nil {
			vaultNote = "; failed to remove routed vault keys " + strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ",") + ": " + cleanupErr.Error()
		} else if len(plan.SecretRoutedKeys) > 0 {
			vaultNote = "; removed routed vault keys: " + strings.Join(sortedAdoptStrings(plan.SecretRoutedKeys), ",")
		}
		return fmt.Errorf("adopt install failed after creating manifest %q; removed the adopt-created manifest so adopt can be re-run%s%s: %w", plan.ManifestName, vaultNote, abortNote, err)
	}
	// Install committed. Promote adopting -> adopted. A flip-write failure here is
	// NON-FATAL: it leaves a recoverable `adopting` row (both manifest hashes were
	// populated at capture, so de-adopt's hash-gate stays usable) and the adopt
	// still returns success — a committed install is never rolled back for a
	// provenance bookkeeping write (design claim 10).
	if err := promoteAdoptProvenanceFn(rec.ManifestName); err != nil {
		emitAdoptProvenanceCommitFailed(rec.ManifestName)
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
		fmt.Fprintf(w, "  %s in %s differs (%s) - not adopted; make that client entry match %s before adopting it\n", plan.EntryName, mismatch.Client, mismatch.Reason, plan.SourceClient)
	}
	for _, disabled := range plan.DisabledSameName {
		fmt.Fprintf(w, "  %s in %s is disabled - not adopted; enable that client entry before adopting it\n", plan.EntryName, disabled.Client)
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

type adoptClientScanResult struct {
	Found      []string
	Matching   []string
	Mismatched []AdoptClientSignatureMismatch
	Disabled   []AdoptClientDisabled
	Errored    []AdoptClientErrored
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
			// A client whose config is PRESENT but UNREADABLE (corrupted) is
			// recorded so normalizeAdoptClients can fail LOUD if the operator
			// explicitly repointed it — silently excluding it would leave that
			// client running its old direct stdio entry (a duplicate process) after
			// a "successful" adopt (bug 2026-07-08 adopt Area-3). An ABSENT config or
			// entry-not-present is a normal not-a-candidate and stays a silent skip.
			// The source client is exempt: its extraction was already validated in
			// BuildAdoptPlan and it is force-added to Found/Matching below.
			if reason, corrupted := adoptExtractionErrorClass(err); corrupted && client != sourceClient {
				result.Errored = append(result.Errored, AdoptClientErrored{Client: client, Reason: reason})
			}
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

func normalizeAdoptClients(requested, found []string, sourceClient string, mismatches []AdoptClientSignatureMismatch, disabled []AdoptClientDisabled, errored []AdoptClientErrored) ([]string, error) {
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
	// Fail LOUD if a SELECTED client's config is corrupted/unreadable. Silently
	// excluding it would leave that client running its old direct stdio entry (a
	// duplicate process) after a "successful" adopt — and --yes / the GUI never
	// print the dry-run plan (bug 2026-07-08 adopt Area-3). This only fires for an
	// EXPLICITLY-requested client (a corrupted client is never in `found`, so the
	// default no-clients mode never trips it). Reason is a path-free class label.
	for _, e := range errored {
		if containsAdoptString(selected, e.Client) {
			return nil, fmt.Errorf("cannot adopt into client %q: %s; fix that config or drop %q from --clients", e.Client, e.Reason, e.Client)
		}
	}
	return filterAdoptExcludedClients(selected, sourceClient, mismatches, disabled), nil
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

func filterAdoptExcludedClients(selected []string, sourceClient string, mismatches []AdoptClientSignatureMismatch, disabled []AdoptClientDisabled) []string {
	excluded := make(map[string]bool, len(mismatches)+len(disabled))
	for _, mismatch := range mismatches {
		excluded[mismatch.Client] = true
	}
	for _, disabledClient := range disabled {
		excluded[disabledClient.Client] = true
	}
	if len(excluded) == 0 {
		return selected
	}
	out := make([]string, 0, len(selected))
	for _, client := range selected {
		if client != sourceClient && excluded[client] {
			continue
		}
		out = append(out, client)
	}
	return out
}

// adoptDefaultDaemonName / adoptDefaultURLPath are the adopt-v1 client-binding
// constants: every adopt-created manifest binds each client to the single
// "default" daemon at url_path "/mcp" (see adoptClientBindings +
// renderStdioBridgeManifestYAML's "default" daemon). Named once here so the
// crash-consistency classifier (adopted_entries.go) can reconstruct the EXPECTED
// hub binding from a row's IMMUTABLE port WITHOUT re-reading the mutable manifest
// file (codex bot PR #528 r3 finding A) — single source of truth, no drift.
const (
	adoptDefaultDaemonName = "default"
	adoptDefaultURLPath    = "/mcp"
)

func adoptClientBindings(clientNames []string) []map[string]any {
	bindings := make([]map[string]any, 0, len(clientNames))
	for _, client := range clientNames {
		bindings = append(bindings, map[string]any{
			"client":   client,
			"daemon":   adoptDefaultDaemonName,
			"url_path": adoptDefaultURLPath,
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
