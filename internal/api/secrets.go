package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"mcp-local-hub/internal/secrets"
)

// SecretsEnvelope is the GET /api/secrets response body. See memo §5.2.
type SecretsEnvelope struct {
	VaultState     string          `json:"vault_state"`
	Secrets        []SecretRow     `json:"secrets"`
	ManifestErrors []ManifestError `json:"manifest_errors"`
}

// SecretRow is one row in the registry. State distinguishes:
//   - "present"             — key exists in vault (vault_state == "ok")
//   - "referenced_missing"  — manifest references key, vault doesn't (vault_state == "ok")
//   - "referenced_unverified" — manifest references key, vault not readable
type SecretRow struct {
	Name   string     `json:"name"`
	State  string     `json:"state"`
	UsedBy []UsageRef `json:"used_by"`
}

// SecretsInitResult is the body of POST /api/secrets/init. VaultState
// is omitempty so case 2c (cleanup-failed 500) can omit it — the vault
// state is undefined when manual cleanup is required (memo §5.1).
type SecretsInitResult struct {
	VaultState    string `json:"vault_state,omitempty"`
	CleanupStatus string `json:"cleanup_status,omitempty"`
	Error         string `json:"error,omitempty"`
	Code          string `json:"code,omitempty"`
	OrphanPath    string `json:"orphan_path,omitempty"`
}

// SecretsRotateResult is the body of PUT /api/secrets/:key.
type SecretsRotateResult struct {
	VaultUpdated   bool            `json:"vault_updated"`
	RestartResults []RestartResult `json:"restart_results"`
}

// SecretsDeleteError is returned by SecretsDelete when the no-confirm
// path is blocked by refs or scan errors (memo §5.5). The handler
// serializes UsedBy / ManifestErrors into the 409 body.
type SecretsDeleteError struct {
	Code           string          `json:"code"`
	Message        string          `json:"message"`
	UsedBy         []UsageRef      `json:"used_by,omitempty"`
	ManifestErrors []ManifestError `json:"manifest_errors,omitempty"`
}

func (e *SecretsDeleteError) Error() string { return e.Message }

// SecretsInitFailed is the typed error SecretsInit returns when
// InitVault failed mid-way and the wrapper attempted cleanup.
// Promoted to exported up front (plan-R2 P1) so Task 1 tests compile;
// fields are populated per memo §5.1 case 2b/2c.
type SecretsInitFailed struct {
	CleanupStatus string // "ok" | "failed"
	OrphanPath    string // populated only on cleanup-failed
	Cause         error
}

func (e *SecretsInitFailed) Error() string { return e.Cause.Error() }

// Unwrap participates in errors.Is/errors.As chains so callers can
// inspect the underlying InitVault failure (disk full, permission, etc.)
// without parsing Error() strings.
func (e *SecretsInitFailed) Unwrap() error { return e.Cause }

// initVaultFn is the function the wrapper calls to perform the
// underlying init. Tests override this to inject failures and verify
// cleanup behavior (memo R7). Codex plan-R3 P1: declared with the
// exported type so Step 1.C.5's SecretsInit body compiles.
var initVaultFn = secrets.InitVault

// vaultMutex serializes all vault mutations (init / Set / Rotate /
// Restart / Delete) so concurrent calls cannot interleave the
// underlying OpenVault → mutate → save sequence and corrupt the
// vault file (Codex PR #18 P1 round 2 + consult).
//
// Note: this is a process-local lock. Cross-process races (CLI +
// GUI simultaneously) are still possible — that's the documented
// LWW limitation in work-items/bugs/a3a-vault-concurrent-edit-lww.md
// and an OS-level advisory file lock is a separate effort.
var vaultMutex sync.Mutex

// SecretsInit implements the D2 four-case classifier (memo §5.1):
//
//	case 1 → 200 ok, no-op
//	case 2a → 200 ok, fresh init
//	case 2b → returns SecretsInitFailed{CleanupStatus:"ok"}, handler maps to 200
//	case 2c → returns SecretsInitFailed{CleanupStatus:"failed"}, handler maps to 500
//	cases 3/4 → returns 409-style typed errors via wrapper return value
func (a *API) SecretsInit() (SecretsInitResult, error) {
	vaultMutex.Lock()
	defer vaultMutex.Unlock()

	keyPath := secrets.DefaultKeyPath()
	vaultPath := secrets.DefaultVaultPath()

	// Case 1: vault already opens cleanly → idempotent no-op.
	if v, err := secrets.OpenVault(keyPath, vaultPath); err == nil {
		// secrets.Vault holds no OS resources beyond its in-memory map; no Close needed.
		_ = v
		return SecretsInitResult{VaultState: "ok"}, nil
	}

	keyExists := fileExists(keyPath)
	vaultExists := fileExists(vaultPath)

	// Cases 3 and 4: pre-existing files we did not create. Refuse.
	if keyExists || vaultExists {
		return SecretsInitResult{}, &SecretsInitBlocked{
			KeyExists:   keyExists,
			VaultExists: vaultExists,
		}
	}

	// Case 2: both missing. Ensure parent dir exists (Codex memo-R8 P1:
	// secrets.InitVault does not MkdirAll itself; CLI does it explicitly).
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return SecretsInitResult{}, fmt.Errorf("create vault dir: %w", err)
	}

	if err := initVaultFn(keyPath, vaultPath); err != nil {
		// Codex PR #18 P1 race fix: if initVaultFn refused because the
		// target files already exist, a concurrent request just created
		// them — we did NOT, and must NOT clean them up. Route to
		// SECRETS_INIT_BLOCKED (cases 3/4 path) instead.
		if initVaultRefused(err) {
			return SecretsInitResult{}, &SecretsInitBlocked{
				KeyExists:   fileExists(keyPath),
				VaultExists: fileExists(vaultPath),
			}
		}
		// Genuine partial init we own. Clean up whatever InitVault may
		// have created. Order: vault first (because the key file alone
		// is benign; an orphan vault is the harder-to-explain artifact).
		cleanupOK := true
		var orphan string
		if rmErr := os.Remove(vaultPath); rmErr != nil && !os.IsNotExist(rmErr) {
			cleanupOK = false
			orphan = vaultPath
		}
		if rmErr := os.Remove(keyPath); rmErr != nil && !os.IsNotExist(rmErr) {
			cleanupOK = false
			if orphan == "" {
				orphan = keyPath
			}
		}
		if cleanupOK {
			return SecretsInitResult{}, &SecretsInitFailed{
				CleanupStatus: "ok",
				Cause:         err,
			}
		}
		return SecretsInitResult{}, &SecretsInitFailed{
			CleanupStatus: "failed",
			OrphanPath:    orphan,
			Cause:         err,
		}
	}
	return SecretsInitResult{VaultState: "ok"}, nil
}

// SecretsInitBlocked is the typed error for D2 cases 3 and 4 (pre-existing
// orphan or unreadable vault). Handler maps to 409 + SECRETS_INIT_BLOCKED.
type SecretsInitBlocked struct {
	KeyExists   bool
	VaultExists bool
}

func (e *SecretsInitBlocked) Error() string {
	switch {
	case e.KeyExists && e.VaultExists:
		return "vault and key files exist but cannot be opened"
	case e.KeyExists:
		return "orphan key file exists; vault file missing"
	case e.VaultExists:
		return "orphan vault file exists; key file missing"
	default:
		return "init blocked"
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// initVaultRefused returns true if the underlying secrets.InitVault
// refused because one of its target files already exists. The error
// strings come from internal/secrets/vault.go:28-33. Used to
// distinguish "concurrent request created these files" from
// "we created them and vault.save() failed mid-write" — only the
// latter justifies cleanup.
func initVaultRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "identity file already exists") ||
		strings.Contains(msg, "vault file already exists")
}

// secretNameRE allows lowercase identifiers (memo §5.3 Codex memo-R8 P1:
// repo ships `secret:wolfram_app_id` and `secret:unpaywall_email`).
var secretNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// SecretsSet writes a key/value pair to the vault. Wrapper for vault.Set.
// Validates name regex and non-empty value (memo §5.3). Returns typed
// errors the handler maps to 400/409.
func (a *API) SecretsSet(name, value string) error {
	if !secretNameRE.MatchString(name) {
		return &SecretsOpError{Code: "SECRETS_INVALID_NAME", Msg: fmt.Sprintf("name %q does not match %s", name, secretNameRE.String())}
	}
	if value == "" {
		return &SecretsOpError{Code: "SECRETS_EMPTY_VALUE", Msg: "value must not be empty"}
	}
	vaultMutex.Lock()
	defer vaultMutex.Unlock()
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return &SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr == nil {
		return &SecretsOpError{Code: "SECRETS_KEY_EXISTS", Msg: fmt.Sprintf("secret %q already exists; use Rotate to update", name)}
	}
	if err := v.Set(name, value); err != nil {
		return &SecretsOpError{Code: "SECRETS_SET_FAILED", Msg: err.Error()}
	}
	return nil
}

// SecretsOpError is the typed error returned by every coded Secrets API
// wrapper (Set, Rotate, Restart, Delete) — Code maps to HTTP status per
// memo §5.7.
type SecretsOpError struct {
	Code string
	Msg  string
}

func (e *SecretsOpError) Error() string { return e.Msg }

// SecretsListWithUsage builds the registry envelope (memo §5.2).
func (a *API) SecretsListWithUsage() (SecretsEnvelope, error) {
	usage, manifestErrs, err := ScanManifestEnv()
	if err != nil {
		return SecretsEnvelope{}, fmt.Errorf("scan manifests: %w", err)
	}
	if manifestErrs == nil {
		manifestErrs = []ManifestError{}
	}

	keyPath := secrets.DefaultKeyPath()
	vaultPath := secrets.DefaultVaultPath()

	vaultMutex.Lock()
	defer vaultMutex.Unlock()
	state, keys := classifyVault(keyPath, vaultPath)

	rows := buildSecretRows(state, keys, usage)
	return SecretsEnvelope{
		VaultState:     state,
		Secrets:        rows,
		ManifestErrors: manifestErrs,
	}, nil
}

// classifyVault maps OpenVault outcomes to the four-state vault model
// (memo §5.2). Returns (state, keys); keys is non-nil only when state == "ok".
//
// Codex plan-R1 P3: capture the first OpenVault error and re-use it
// instead of calling OpenVault twice (the second call is redundant).
func classifyVault(keyPath, vaultPath string) (string, []string) {
	v, err := secrets.OpenVault(keyPath, vaultPath)
	if err == nil {
		return "ok", v.List()
	}
	keyExists := fileExists(keyPath)
	vaultExists := fileExists(vaultPath)
	if !keyExists || !vaultExists {
		return "missing", nil
	}
	// Files exist but OpenVault failed: distinguish corrupt (key parse
	// error or post-decrypt JSON garbage) from decrypt_failed (key fine,
	// age decrypt rejected the cipher) by inspecting the captured error
	// string. Brittle but acceptable for the GET path.
	msg := err.Error()
	switch {
	case containsAny(msg, "parse identity", "no identity", "not X25519"):
		return "corrupt", nil
	case containsAny(msg, "unmarshal vault"):
		return "corrupt", nil
	case containsAny(msg, "age decrypt"):
		return "decrypt_failed", nil
	default:
		return "corrupt", nil
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// buildSecretRows merges vault keys with manifest usage into the row
// slice. Sorted alphabetically by name.
func buildSecretRows(vaultState string, keys []string, usage map[string][]UsageRef) []SecretRow {
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}

	var rows []SecretRow
	switch vaultState {
	case "ok":
		for _, k := range keys {
			rows = append(rows, SecretRow{Name: k, State: "present", UsedBy: nonNilUsage(usage[k])})
		}
		for k, refs := range usage {
			if _, ok := keySet[k]; ok {
				continue
			}
			rows = append(rows, SecretRow{Name: k, State: "referenced_missing", UsedBy: refs})
		}
	default:
		// Vault unavailable; only manifest-only rows, all unverified.
		for k, refs := range usage {
			rows = append(rows, SecretRow{Name: k, State: "referenced_unverified", UsedBy: refs})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

func nonNilUsage(in []UsageRef) []UsageRef {
	if in == nil {
		return []UsageRef{}
	}
	return in
}

// SecretsRotate writes value into the vault and optionally restarts the
// affected running daemons. Returns SecretsRotateResult; if restart was
// requested and orchestration crashed mid-loop, returns
// (partial-result, err) so the handler can map to 500 + RESTART_FAILED
// while still surfacing vault_updated:true.
func (a *API) SecretsRotate(name, value string, restart bool) (SecretsRotateResult, error) {
	if !secretNameRE.MatchString(name) {
		return SecretsRotateResult{}, &SecretsOpError{Code: "SECRETS_INVALID_NAME", Msg: fmt.Sprintf("name %q does not match %s", name, secretNameRE.String())}
	}
	if value == "" {
		return SecretsRotateResult{}, &SecretsOpError{Code: "SECRETS_EMPTY_VALUE", Msg: "value must not be empty"}
	}

	// Vault write phase — serialized. Lock released BEFORE the restart
	// phase so a long restart loop does not block concurrent vault ops.
	vaultMutex.Lock()
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		vaultMutex.Unlock()
		return SecretsRotateResult{}, &SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr != nil {
		vaultMutex.Unlock()
		return SecretsRotateResult{}, &SecretsOpError{Code: "SECRETS_KEY_NOT_FOUND", Msg: getErr.Error()}
	}
	if err := v.Set(name, value); err != nil {
		vaultMutex.Unlock()
		return SecretsRotateResult{}, &SecretsOpError{Code: "SECRETS_SET_FAILED", Msg: err.Error()}
	}
	vaultMutex.Unlock()
	// Vault is committed. Restart phase is external orchestration that
	// does not touch the vault, so vaultMutex is not held here.

	res := SecretsRotateResult{VaultUpdated: true, RestartResults: []RestartResult{}}
	if !restart {
		return res, nil
	}
	results, err := a.restartServersForKey(name)
	res.RestartResults = results
	return res, err
}

// SecretsRestart runs the restart phase only — used by POST /api/secrets/:key/restart.
// Does NOT modify the vault.
func (a *API) SecretsRestart(name string) ([]RestartResult, error) {
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return nil, &SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr != nil {
		return nil, &SecretsOpError{Code: "SECRETS_KEY_NOT_FOUND", Msg: getErr.Error()}
	}
	return a.restartServersForKey(name)
}

// restartServersForKey iterates manifests, finds (server, daemon) pairs
// whose env references the key AND whose daemon status is "Running",
// and calls api.Restart(server, daemonName) per-daemon (memo §D9).
// Returns accumulated []RestartResult plus a non-nil error on the first
// orchestration failure (per-task failures are non-fatal).
//
// Codex plan-R2 P2: returns []RestartResult{} (empty slice, not nil)
// even on early errors so callers and the handler can serialize a
// well-formed JSON array per the memo wire contract.
func (a *API) restartServersForKey(key string) ([]RestartResult, error) {
	results := []RestartResult{}
	usage, _, err := ScanManifestEnv()
	if err != nil {
		return results, fmt.Errorf("scan manifests: %w", err)
	}
	refs := usage[key]
	if len(refs) == 0 {
		return results, nil
	}
	statuses, err := a.Status()
	if err != nil {
		return results, fmt.Errorf("read daemon status: %w", err)
	}
	runningByServer := make(map[string]map[string]bool) // server → daemon → running?
	for _, st := range statuses {
		if st.Server == "" || st.Daemon == "" {
			continue
		}
		if runningByServer[st.Server] == nil {
			runningByServer[st.Server] = map[string]bool{}
		}
		runningByServer[st.Server][st.Daemon] = (st.State == "Running") // Codex plan-R1 P1: capital R per types.go:21
	}

	// Determine the affected (server, daemon) set. Each manifest may
	// have multiple daemons; we must restart all running daemons of
	// each affected server because the env is shared across the
	// server's daemons in the current schema.
	//
	// Codex plan-R2 P2: de-duplicate on (server, daemon) so a single
	// secret referenced via multiple env vars in one manifest does
	// NOT trigger duplicate restarts.
	type sd struct{ server, daemon string }
	seen := map[sd]bool{}
	for _, ref := range refs {
		daemons := runningByServer[ref.Server]
		for daemon, running := range daemons {
			if !running {
				continue
			}
			pair := sd{ref.Server, daemon}
			if seen[pair] {
				continue
			}
			seen[pair] = true
			subres, restartErr := a.Restart(ref.Server, daemon)
			results = append(results, subres...)
			if restartErr != nil {
				return results, fmt.Errorf("restart %s/%s: %w", ref.Server, daemon, restartErr)
			}
		}
	}
	return results, nil
}

// SecretsDelete enforces the D5 escalation guard. With confirm=false,
// returns *SecretsDeleteError when refs exist or scan was incomplete;
// returns nil on successful delete. With confirm=true, bypasses both
// guards.
func (a *API) SecretsDelete(name string, confirm bool) error {
	vaultMutex.Lock()
	defer vaultMutex.Unlock()
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		return &SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: err.Error()}
	}
	if _, getErr := v.Get(name); getErr != nil {
		return &SecretsOpError{Code: "SECRETS_KEY_NOT_FOUND", Msg: getErr.Error()}
	}
	if !confirm {
		usage, manifestErrs, scanErr := ScanManifestEnv()
		if scanErr != nil {
			// Surface scan failure as an OpError so the handler can map it to
			// a meaningful code instead of falling into the generic delete-
			// failed catch-all (Codex Task-3 quality review).
			return &SecretsOpError{Code: "SECRETS_LIST_FAILED", Msg: scanErr.Error()}
		}
		// Precedence per §5.5: scan-incomplete BEFORE refs.
		if len(manifestErrs) > 0 {
			return &SecretsDeleteError{
				Code:           "SECRETS_USAGE_SCAN_INCOMPLETE",
				Message:        fmt.Sprintf("manifest scan returned %d error(s); cannot verify refs", len(manifestErrs)),
				ManifestErrors: manifestErrs,
			}
		}
		if refs := usage[name]; len(refs) > 0 {
			return &SecretsDeleteError{
				Code:    "SECRETS_HAS_REFS",
				Message: fmt.Sprintf("secret %q is referenced by %d manifest(s)", name, len(refs)),
				UsedBy:  refs,
			}
		}
	}
	if err := v.Delete(name); err != nil {
		return &SecretsOpError{Code: "SECRETS_DELETE_FAILED", Msg: err.Error()}
	}
	return nil
}
