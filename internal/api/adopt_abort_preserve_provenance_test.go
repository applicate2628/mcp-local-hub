package api

// Tests for bug 2026-07-12: adopt abort must PRESERVE the pre-adopt provenance
// when Install's client-config rollback is INCOMPLETE (≥1 client whose pre-adopt
// restoration could not be confirmed). Design:
// work-items/active/2026-07-12-adopt-abort-preserve-provenance/design.md +
// design-round2.md + design-round4.md §Tests.
//
// The Install-side sentinel (InstallClientRollbackIncompleteError) is exercised
// through the REAL executeInstallTo / a.Install / ExecuteAdopt paths — never a
// stubbed Install — via a per-path, per-call-ordinal clients.WriteConfigFile seam
// (installWriteSeam). For each client config path the FIRST non-backup write is the
// AddEntry write and the SECOND is the rollback restore-from-backup write (AddEntry
// and RestoreEntry... each perform exactly one clients.WriteConfigFile call per
// client — verified for codex TOML / json_mcp / claude adapters). Backup writes
// (.bak-mcp-local-hub- basename) bypass the seam, and provenance capture bypasses it
// (state-file pipeline), so the seam is scoped exactly to the client-config
// AddEntry/restore writes the fix keys on.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
)

// addEntryOutcome controls the FIRST (AddEntry) live-config write to a client path.
type addEntryOutcome int

const (
	addEntrySucceed                   addEntryOutcome = iota // realWrite, no error (client committed)
	addEntryFailMutated                                      // realWrite THEN error — reproduces SecureWriteClientConfig
	addEntryAppliedReleaseUnconfirmed                        // realWrite THEN typed lifecycle failure (client committed)
	// post-rename path #2 (config left = the hub relay, error returned)
	addEntryFailUnmutated // return error, NO write (pre-rename failure, config untouched)
	addEntryFailRemoved   // realWrite THEN os.Remove THEN error — reproduces SecureWriteClientConfig
	// post-rename path #1 (definitive verify-fail REMOVES the just-published file, so
	// the write-target FILE — target entry AND every sibling — is left ABSENT). The
	// subsequent whole-file recovery write is the n==2 write scripted by spec.restore.
)

// restoreOutcome controls the SECOND (rollback restore-from-backup) write to a path.
type restoreOutcome int

const (
	restoreSucceed                   restoreOutcome = iota // realWrite the restored bytes (client reverted)
	restoreFail                                            // return error, config left as AddEntry left it
	restoreFailMutated                                     // realWrite the restore bytes THEN error (config = restored, err returned)
	restoreAppliedReleaseUnconfirmed                       // realWrite restore bytes THEN typed lifecycle failure
	restoreFailRemoved                                     // remove the live file THEN error (Sol P2: restore DAMAGED an untouched config)
)

var (
	inducedAddEntryReleaseUnconfirmed = errors.New("induced add-entry lock release unconfirmed")
	inducedRestoreReleaseUnconfirmed  = errors.New("induced restore lock release unconfirmed")
)

// recoverOutcome controls the round-6 whole-file recovery CREATE
// (clients.CreateConfigFileIfMissing) that the rollback restore attempts when the
// write-target file is ABSENT at recovery time (addEntryFailRemoved removed it). The
// create seam is SEPARATE from the WriteConfigFile seam, so a recovery create does
// NOT increment writeCount — it increments createCount instead. recoverNone (the
// zero value) leaves the create-double delegating to the real create seam, so specs
// that never exercise the whole-file recovery (every sentinel / barrier / round-4
// test) are unaffected.
type recoverOutcome int

const (
	recoverNone     recoverOutcome = iota // delegate to the real CreateConfigFileIfMissing (no scripting)
	recoverCreated                        // realWrite(path, backupData) THEN (true, nil): whole backup published race-free
	recoverConflict                       // realWrite(path, recoverConflictBytes [external S']) THEN (false, nil): EEXIST conflict
	recoverFail                           // (false, injectedErr): no-replace-create refusal / hard fail → sentinel → PRESERVE
)

// clientWriteSpec is the per-path script: what its AddEntry write does, what its
// rollback restore write does, and — when the write-target file is gone at recovery
// time — what its whole-file recovery create does. A path with no spec succeeds both
// writes and delegates its create to the real seam.
type clientWriteSpec struct {
	addEntry addEntryOutcome
	restore  restoreOutcome
	// recover scripts the round-6 whole-file recovery create (only reached on the
	// ABSENT-file recovery path — addEntryFailRemoved). recoverNone delegates.
	recover recoverOutcome
	// recoverConflictBytes is the external sibling S' injected (in this path's own
	// adapter format) by recoverConflict to model a concurrent recreate of the file
	// between the helper's absence observation and the no-replace create.
	recoverConflictBytes []byte
}

// installWriteSeam is a per-path, per-call-ordinal clients.WriteConfigFile override,
// plus a per-path clients.CreateConfigFileIfMissing override for the round-6
// whole-file recovery create.
type installWriteSeam struct {
	mu      sync.Mutex
	specs   map[string]clientWriteSpec // key: filepath.Clean(configPath)
	writeN  map[string]int             // per-path non-backup write ordinal (1=AddEntry, 2=restore)
	createN map[string]int             // per-path whole-file recovery create count (scripted, absent-file path)
}

// seedInstallWriteSeam installs the per-path seam. `specs` is keyed by client config
// path (any spelling; cleaned internally). Backups always pass through; a path with
// no spec succeeds both its AddEntry and restore writes.
func seedInstallWriteSeam(t *testing.T, specs map[string]clientWriteSpec) *installWriteSeam {
	t.Helper()
	cleaned := make(map[string]clientWriteSpec, len(specs))
	for p, s := range specs {
		cleaned[filepath.Clean(p)] = s
	}
	seam := &installWriteSeam{specs: cleaned, writeN: map[string]int{}, createN: map[string]int{}}
	realWrite := func(path string, contents []byte) error {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		return os.WriteFile(path, contents, 0o600)
	}
	orig := clients.WriteConfigFile
	clients.WriteConfigFile = func(path string, contents []byte) error {
		cp := filepath.Clean(path)
		if strings.Contains(filepath.Base(cp), ".bak-mcp-local-hub-") {
			return realWrite(path, contents) // backups always pass through
		}
		seam.mu.Lock()
		seam.writeN[cp]++
		n := seam.writeN[cp]
		spec, hasSpec := seam.specs[cp]
		seam.mu.Unlock()
		if !hasSpec {
			return realWrite(path, contents) // unspecified path => succeed both writes
		}
		switch n {
		case 1: // AddEntry
			switch spec.addEntry {
			case addEntrySucceed:
				return realWrite(path, contents)
			case addEntryFailMutated:
				// Write the hub-relay bytes THEN fail — the config is left mutated (=
				// hub relay) and AddEntry returns an error (SecureWriteClientConfig
				// post-rename reopen path #2).
				if err := realWrite(path, contents); err != nil {
					return err
				}
				return fmt.Errorf("induced mutated add-entry failure for %s", filepath.Base(cp))
			case addEntryAppliedReleaseUnconfirmed:
				if err := realWrite(path, contents); err != nil {
					return err
				}
				return errors.Join(clients.ErrConfigLockReleaseUnconfirmed, inducedAddEntryReleaseUnconfirmed)
			case addEntryFailUnmutated:
				return fmt.Errorf("induced add-entry failure for %s", filepath.Base(cp))
			case addEntryFailRemoved:
				// Write the hub-relay bytes, then REMOVE the whole file, then fail —
				// models SecureWriteClientConfig's post-rename path #1 (definitive
				// owner/mode/DACL verify-failure removes the just-published file). The
				// write-target FILE (target entry + every sibling) is left ABSENT, so the
				// rollback restore must recover the WHOLE backup, not just the target entry.
				if err := realWrite(path, contents); err != nil {
					return err
				}
				_ = os.Remove(path)
				return fmt.Errorf("induced removed add-entry failure for %s", filepath.Base(cp))
			}
		case 2: // rollback restore-from-backup
			switch spec.restore {
			case restoreSucceed:
				return realWrite(path, contents)
			case restoreFail:
				return fmt.Errorf("induced restore failure for %s", filepath.Base(cp))
			case restoreFailMutated:
				// Write the restore bytes THEN fail — models SecureWriteClientConfig's
				// post-rename transient-reopen path where the file is already rewritten
				// but an error is returned.
				if err := realWrite(path, contents); err != nil {
					return err
				}
				return fmt.Errorf("induced mutated restore failure for %s", filepath.Base(cp))
			case restoreAppliedReleaseUnconfirmed:
				if err := realWrite(path, contents); err != nil {
					return err
				}
				return errors.Join(clients.ErrConfigLockReleaseUnconfirmed, inducedRestoreReleaseUnconfirmed)
			case restoreFailRemoved:
				// Remove the live file THEN fail — models SecureWriteClientConfig's
				// definitive post-rename verify-failure removing the just-published file
				// (Sol P2: a restore that DAMAGES a previously-untouched config).
				_ = os.Remove(path)
				return fmt.Errorf("induced removed restore failure for %s", filepath.Base(cp))
			}
		}
		return realWrite(path, contents) // any 3rd+ write to a spec'd path => pass through
	}
	t.Cleanup(func() { clients.WriteConfigFile = orig })

	// Round-6: the whole-file recovery publishes the backup through the ATOMIC
	// create-if-absent seam clients.CreateConfigFileIfMissing (NOT WriteConfigFile), so
	// script it separately. The double keys on LIVE file-presence to disambiguate an
	// early InitEmpty stub-create on a PRESENT file (idempotent no-op ⇒ delegate to the
	// real create, which returns (false,nil) for an existing regular file) from the
	// recovery create on an ABSENT file (the helper only creates after observing ENOENT
	// ⇒ apply the scripted `recover` outcome). Unspecified paths delegate.
	origCreate := clients.CreateConfigFileIfMissing
	clients.CreateConfigFileIfMissing = func(path string, stub []byte) (bool, error) {
		cp := filepath.Clean(path)
		seam.mu.Lock()
		spec, hasSpec := seam.specs[cp]
		seam.mu.Unlock()
		if !hasSpec {
			return origCreate(path, stub) // unspecified path => real create
		}
		if _, statErr := os.Stat(path); statErr == nil {
			return origCreate(path, stub) // PRESENT => InitEmpty stub-create; delegate (idempotent (false,nil))
		}
		if spec.recover == recoverNone {
			return origCreate(path, stub) // ABSENT but unscripted => real create
		}
		// ABSENT + scripted => this is the round-6 whole-file recovery create.
		seam.mu.Lock()
		seam.createN[cp]++
		seam.mu.Unlock()
		switch spec.recover {
		case recoverCreated:
			// The whole backup is published race-free (no external recreate).
			if err := realWrite(path, stub); err != nil {
				return false, err
			}
			return true, nil
		case recoverConflict:
			// An external process recreated the file with a NEW sibling S' between the
			// helper's absence observation and the no-replace create ⇒ the create sees
			// EEXIST. Model it: publish S' as the recreate side effect, then report
			// (false, nil) so the backup bytes are treated as NOT published. The body
			// then falls through to the surgical restore, which must re-read the live
			// file and preserve S'.
			if err := realWrite(path, spec.recoverConflictBytes); err != nil {
				return false, err
			}
			return false, nil
		case recoverFail:
			// A no-replace-create refusal (symlink/reparse) or hard I/O failure — the
			// helper maps (false,err) to (true,err) ⇒ sentinel ⇒ PRESERVE.
			return false, fmt.Errorf("induced recovery-create failure for %s", filepath.Base(cp))
		}
		return origCreate(path, stub)
	}
	t.Cleanup(func() { clients.CreateConfigFileIfMissing = origCreate })
	return seam
}

// writeCount returns the number of non-backup client-config writes recorded for
// path (1 = the AddEntry attempt; 2 = a rollback restore write also fired). The
// per-path ordinal is incremented at the START of each seam call, so a scripted
// pre-write failure (addEntryFailUnmutated) still counts the attempt.
func (s *installWriteSeam) writeCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeN[filepath.Clean(path)]
}

// createCount returns the number of scripted round-6 whole-file recovery creates
// (clients.CreateConfigFileIfMissing calls on the ABSENT-file recovery path) recorded
// for path. An early InitEmpty stub-create on a PRESENT file delegates to the real
// seam and is NOT counted.
func (s *installWriteSeam) createCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createN[filepath.Clean(path)]
}

// seedTwoPresentClients seeds a codex source config AND a same-signature cursor
// config (both PRESENT with a routed env secret), returns their paths. Both are
// captured as PRESENT provenance clients (each with a pinned snapshot).
func seedTwoPresentClients(t *testing.T, entry string) (codexPath, cursorPath, manifestRoot, stateRoot string) {
	t.Helper()
	codexPath, manifestRoot, stateRoot = setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath = filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{entry: map[string]any{
			"command": "go", "args": []any{"version"},
			"env": map[string]any{"API_KEY": "literal-secret-value"},
		}},
	})
	return codexPath, cursorPath, manifestRoot, stateRoot
}

// --- NEW load-bearing P1 repro: preserve when the failing client is left mutated
//     AND its restore also fails (MUST fail on the pre-FIX-1 code) ----------------

func TestExecuteAdopt_PreservesWhenFailingClientLeftMutatedAndUnrestorable(t *testing.T) {
	entry := "preserve-mutated"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	// Capture the exact pre-adopt config bytes BEFORE adopt mutates codex, so the
	// preserved snapshot can be asserted byte-identical to it (Sol/round-1 P2).
	codexPre := mustReadFileForAdoptTest(t, codexPath)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to assert 'keys preserved'")
	}

	// The ONLY client's AddEntry writes the hub relay THEN errors (post-rename #2),
	// and its rollback restore ALSO fails — so the client is left MUTATED and
	// unrestorable. Pre-FIX-1 the restore closure was registered only after AddEntry
	// success, so this mutated-then-error AddEntry registered NO restore => plain
	// error => adopt ABORTED => snapshot deleted while the client stayed on the hub
	// relay (the P1 data-loss). Post-FIX-1 the restore is registered first, fails,
	// and feeds the sentinel => PRESERVE.
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailMutated, restore: restoreFail},
	})

	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want a preserved rollback-incomplete failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if !errors.As(err, &rb) {
		t.Fatalf("adopt error does not wrap the Install sentinel (pre-FIX-1 aborts here): %T %v", err, err)
	}
	if len(rb.Clients) != 1 || rb.Clients[0] != "codex-cli" {
		t.Fatalf("sentinel Clients = %#v, want [codex-cli]", rb.Clients)
	}
	if !strings.Contains(err.Error(), "PRESERVED") ||
		!strings.Contains(err.Error(), "restoration could not be confirmed") ||
		!strings.Contains(err.Error(), "de-adopt once available") {
		t.Errorf("adopt error missing preserve/recovery wording: %v", err)
	}

	// Non-vacuity: the client was left MUTATED (still the hub relay), proving this
	// test actually exercises the mutated-then-error path the fix guards.
	after := string(mustReadFileForAdoptTest(t, codexPath))
	if !strings.Contains(after, strconv.Itoa(port)) {
		t.Fatalf("codex config was NOT left mutated (hub relay port %d absent); test does not exercise the P1 path:\n%s", port, after)
	}

	// PRESERVE: row `adopting`, snapshot dir + every recorded SnapshotRef file (each
	// byte-identical to the pre-adopt config), manifest, and the exact routed vault
	// keys all survive.
	assertPreservedProvenance(t, entry, plan, manifestRoot, map[string][]byte{
		filepath.Clean(codexPath): codexPre,
	})

	// Event emitted with exact clients/count.
	assertPreservedEvent(t, entry, []string{"codex-cli"})
}

// --- NEW: no over-preserve — FAIL_MUTATED but restorable ⇒ clean abort ----------

func TestExecuteAdopt_AbortsWhenFailingClientMutatedButRestorable(t *testing.T) {
	entry := "mutated-restorable"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: routed secret needed to assert deleteAdoptRoutedSecrets fired")
	}

	// AddEntry writes the hub relay THEN errors, but the rollback restore SUCCEEDS —
	// the client is provably reverted, so no sentinel, and adopt ABORTS cleanly.
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailMutated, restore: restoreSucceed},
	})

	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain (rollback-complete) install failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("client was provably restored, yet adopt saw a sentinel and preserved: %v", err)
	}
	if !strings.Contains(err.Error(), "adopt can be re-run") {
		t.Errorf("abort error missing re-run guidance: %v", err)
	}

	// Config reverted to the pre-adopt stdio entry (hub relay port gone).
	after := string(mustReadFileForAdoptTest(t, codexPath))
	if strings.Contains(after, strconv.Itoa(port)) {
		t.Errorf("restore did not revert the hub relay (port %d still present):\n%s", port, after)
	}
	for _, want := range []string{"command", "go"} {
		if !strings.Contains(after, want) {
			t.Errorf("restore did not bring back the original stdio entry; missing %q:\n%s", want, after)
		}
	}

	// Abort cleanup fired: no provenance residue, manifest gone, routed keys gone.
	assertNoAdoptProvenanceResidue(t, entry)
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("manifest survived the abort branch: stat err = %v", err)
	}
	vault, verr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if verr != nil {
		t.Fatalf("OpenVault: %v", verr)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Errorf("routed vault keys survived the abort branch: %v", keys)
	}
}

// --- NEW load-bearing (Sol P2 match-guard): a FAIL_UNMUTATED client whose restore
//     WOULD damage the untouched config is SKIPPED — no sentinel, clean abort.
//     MUST fail on the pre-guard code (restore runs, removes the file, wrongful
//     preserve). ------------------------------------------------------------------

func TestExecuteAdopt_MatchGuardSkipsRestoreOnUnmutatedClient(t *testing.T) {
	entry := "matchguard-skip"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	// Exact pre-adopt config bytes: the damaging restore must never run, so the live
	// config must be byte-identical to this after the (skipped-restore) abort.
	preAdopt := mustReadFileForAdoptTest(t, codexPath)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to assert the abort dropped the keys")
	}

	// The ONLY client's AddEntry FAILS UNMUTATED (nothing written — the live config
	// stays = the seeded pre-adopt bytes), and its rollback restore is scripted to
	// REMOVE the live file THEN error. Post-fix (design round-4): the entry-scoped
	// skip is FOLDED INTO the restore body under its withConfigLock hold — it sees
	// the write-target entry == the backup entry and returns nil BEFORE the write
	// seam, so the damaging removal never runs, no sentinel, adopt ABORTS. Pre-fix:
	// the restore ran unconditionally, removed the file, appended to the sentinel =>
	// wrongful PRESERVE + a destroyed client config (the Sol P2 damage).
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailUnmutated, restore: restoreFailRemoved},
	})

	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain (guard-skipped) install failure")
	}

	// (1) NO sentinel — the guard skipped the restore, so nothing fed the sentinel.
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("match-guard did not skip: adopt saw a sentinel and PRESERVED an unmutated client: %v", err)
	}

	// (2) Live config byte-identical to the seeded pre-adopt config — the damaging
	// restore (restoreFailRemoved) never executed.
	after := mustReadFileForAdoptTest(t, codexPath)
	if !bytes.Equal(after, preAdopt) {
		t.Fatalf("live config was altered by a restore that should have been skipped:\n got: %q\nwant: %q", after, preAdopt)
	}

	// (2b) NEW-mechanism proof (design round-4): the entry-scoped skip is folded
	// INTO the restore body and returns nil BEFORE the write seam, so the restore
	// WRITE is never invoked for the unmutated client — exactly ONE seam write (the
	// AddEntry attempt) is recorded for codex. Neuter the skip (e.g. `if false &&
	// allowHubEntry` in codex restoreEntryFromBackupWithWriter) and the restore
	// write fires (count == 2), restoreFailRemoved runs, and assertions (1)+(2)
	// above break — so this test is non-vacuous.
	if n := seam.writeCount(codexPath); n != 1 {
		t.Fatalf("restore WRITE was invoked for the unmutated client (seam writes=%d, want 1 — the AddEntry attempt only); the folded entry-scoped skip did not fire before the write seam", n)
	}

	// (3) ABORT branch fired: no provenance residue, manifest gone, routed keys gone.
	assertNoAdoptProvenanceResidue(t, entry)
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("manifest survived the abort branch: stat err = %v", err)
	}
	vault, verr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if verr != nil {
		t.Fatalf("OpenVault: %v", verr)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Errorf("routed vault keys survived the abort branch: %v", keys)
	}
}

// --- NEW (design round-5, Sol P1): whole-file-gone recovery ahead of the entry skip.
//     SecureWrite path #1 REMOVES the just-published config file (target entry AND
//     siblings). The rollback restore must recover the WHOLE backup (all siblings),
//     not surgically recreate only the target entry (loses siblings) or false-skip the
//     both-absent case. Each *_Sibling test MUST fail if wholeFileRestoreIfWriteTargetGone
//     is neutered to always return (false, nil). ------------------------------------

// TargetPresent (E + S both in the backup): a path-#1 removal must recover BOTH the
// adopted entry E (restored to its pre-adopt stdio shape) AND the sibling S. Pre-fix
// the surgical restore base-read the now-missing live config and rewrote a file with
// only E, silently losing S; and it reported success ⇒ abort deleted the snapshot.
func TestExecuteAdopt_Path1WholeFileGone_TargetPresentSibling(t *testing.T) {
	entry := "path1-present-sibling"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n\n"+
		"[mcp_servers.sibling-S]\n"+
		"url = \"http://sibling.invalid/mcp\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to exercise the full adopt path")
	}

	// codex AddEntry publishes the hub relay THEN path #1 REMOVES the whole file
	// (E + sibling S). The rollback whole-file recovery re-creates the entire backup via
	// the atomic create-if-absent seam and SUCCEEDS ⇒ no sentinel ⇒ clean abort
	// (snapshot deleted, no false preserve).
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailRemoved, recover: recoverCreated},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain (whole-file-recovered) install failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("whole-file recovery succeeded, yet adopt saw a sentinel and PRESERVED: %v", err)
	}

	// (1) Live codex parses AND holds BOTH the pre-adopt stdio E and the sibling S.
	codex := clients.AllClients()["codex-cli"]
	if eEntry, gerr := codex.GetEntry(entry); gerr != nil {
		t.Fatalf("live codex config does not parse after whole-file recovery: %v", gerr)
	} else if eEntry == nil {
		t.Fatalf("target entry %q lost after whole-file recovery", entry)
	}
	if sEntry, gerr := codex.GetEntry("sibling-S"); gerr != nil {
		t.Fatalf("live codex config does not parse after whole-file recovery: %v", gerr)
	} else if sEntry == nil {
		t.Fatal("SIBLING entry S was LOST after whole-file recovery (a surgical restore recreates only the target entry)")
	}
	// Pre-adopt stdio shape recovered (hub relay port gone, `command` back).
	after := string(mustReadFileForAdoptTest(t, codexPath))
	if strings.Contains(after, strconv.Itoa(port)) {
		t.Errorf("recovery left the hub relay port %d; want the pre-adopt stdio shape:\n%s", port, after)
	}
	if !strings.Contains(after, "command") || !strings.Contains(after, "sibling.invalid") {
		t.Errorf("recovered config is not the pre-adopt shape with the sibling:\n%s", after)
	}

	// (2) The whole-file recovery CREATE ran via the atomic create-if-absent seam
	// (createCount==1); the WriteConfigFile seam saw only the AddEntry attempt
	// (writeCount==1). Neuter wholeFileRestoreIfWriteTargetGone (return false,nil) and
	// the surgical restore recreates only E through WriteConfigFile — assertion (1) on S
	// breaks and createCount drops to 0 — proving non-vacuity.
	if n := seam.writeCount(codexPath); n != 1 {
		t.Fatalf("unexpected WriteConfigFile count (seam writes=%d, want 1 — AddEntry attempt only; recovery uses the create seam)", n)
	}
	if n := seam.createCount(codexPath); n != 1 {
		t.Fatalf("whole-file recovery create did not fire (seam creates=%d, want 1)", n)
	}

	// (3) Clean abort: no provenance residue, manifest gone.
	assertNoAdoptProvenanceResidue(t, entry)
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("manifest survived the abort branch: stat err = %v", err)
	}
}

// TargetAbsent (entryless fanout: only sibling S in the backup, E absent): path #1
// removes the write-target file ⇒ live E absent == backup E absent, which the
// entry-scoped skip would treat as a no-op and SILENTLY DROP S. The whole-file recovery
// must fire FIRST and restore S.
func TestExecuteAdopt_Path1WholeFileGone_TargetAbsentSibling(t *testing.T) {
	entry := "path1-absent-sibling"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	// cursor is an ENTRYLESS fanout target: it holds ONLY a sibling S (no entry E).
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{
			"sibling-S": map[string]any{"url": "http://sibling.invalid/mcp"},
		},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !containsStr(plan.AdoptClients, "cursor") {
		t.Fatalf("precondition: cursor must be an entryless-fanout adopt client; AdoptClients=%#v", plan.AdoptClients)
	}

	// cursor AddEntry publishes the hub relay THEN path #1 removes the whole cursor file
	// (S gone). The rollback whole-file recovery re-creates S via the atomic
	// create-if-absent seam and SUCCEEDS ⇒ clean abort.
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		cursorPath: {addEntry: addEntryFailRemoved, recover: recoverCreated},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain (whole-file-recovered) install failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("whole-file recovery succeeded, yet adopt saw a sentinel and PRESERVED: %v", err)
	}

	// (1) Live cursor parses AND still holds the sibling S.
	cursor := clients.AllClients()["cursor"]
	if sEntry, gerr := cursor.GetEntry("sibling-S"); gerr != nil {
		t.Fatalf("live cursor config does not parse after whole-file recovery: %v", gerr)
	} else if sEntry == nil {
		t.Fatal("SIBLING entry S was LOST — the entry-scoped skip false-skipped the both-absent case instead of recovering the whole file")
	}

	// (2) createCount==1 proves the recovery CREATE fired (no false-skip); the
	// WriteConfigFile seam saw only the AddEntry attempt (writeCount==1). Neuter
	// wholeFileRestoreIfWriteTargetGone (return false,nil) and the both-absent
	// entry-scoped skip drops S with no recovery create ⇒ createCount==0 and assertion
	// (1) on S breaks — proving non-vacuity.
	if n := seam.writeCount(cursorPath); n != 1 {
		t.Fatalf("unexpected WriteConfigFile count for the entryless-fanout target (seam writes=%d, want 1 — AddEntry attempt only)", n)
	}
	if n := seam.createCount(cursorPath); n != 1 {
		t.Fatalf("whole-file recovery create did not fire for the entryless-fanout target (seam creates=%d, want 1)", n)
	}

	// (3) Clean abort.
	assertNoAdoptProvenanceResidue(t, entry)
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("manifest survived the abort branch: stat err = %v", err)
	}
}

// MimoTopLayer: the MiMoCode restore body compares + recovers against the TOP
// write-target layer o.path (NOT the merged read view). A path-#1 removal of o.path
// must whole-restore o.path, recovering both the adopted entry E and the sibling S.
func TestExecuteAdopt_Path1WholeFileGone_MimoTopLayerSibling(t *testing.T) {
	entry := "path1-mimo-sibling"
	// codex is seeded WITHOUT entry E so it is not a same-name adopt candidate; mimo is
	// the source.
	setupAdoptTestEnv(t, entry, "[profile.default]\nmodel = \"gpt-5\"\n")
	// Clear mimo env overrides so the adapter resolves the HOME-derived write target.
	for _, k := range []string{"MIMOCODE_CONFIG", "MIMOCODE_CONFIG_DIR", "MIMOCODE_HOME", "MIMOCODE_CONFIG_CONTENT", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	mimo := clients.AllClients()["mimocode"]
	if mimo == nil {
		t.Skip("mimocode adapter not constructable on this host")
	}
	mimoPath := mimo.ConfigPath() // the TOP write-target layer (o.path)
	// Seed the TOP write-target with a local entry E (secret under `environment`) AND a
	// remote sibling S. Both live in o.path, so o.path is the only read+write layer.
	writeJSONForAdoptTest(t, mimoPath, map[string]any{
		"mcp": map[string]any{
			entry: map[string]any{
				"type":        "local",
				"command":     []any{"go", "version"},
				"environment": map[string]any{"API_KEY": "literal-secret-value"},
				"enabled":     true,
			},
			"sibling-S": map[string]any{
				"type":    "remote",
				"url":     "http://sibling.invalid/mcp",
				"enabled": true,
			},
		},
	})
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "mimocode", ManifestName: entry, Port: port,
		ScanOpts: ScanOpts{MimoCodeConfigPath: mimoPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !containsStr(plan.AdoptClients, "mimocode") {
		t.Fatalf("precondition: mimocode must be the source adopt client; AdoptClients=%#v", plan.AdoptClients)
	}

	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		mimoPath: {addEntry: addEntryFailRemoved, recover: recoverCreated},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain (whole-file-recovered) install failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("whole-file recovery succeeded, yet adopt saw a sentinel and PRESERVED: %v", err)
	}

	// (1) The whole-restore wrote the TOP layer o.path — BOTH E and S are back in o.path.
	after := string(mustReadFileForAdoptTest(t, mimoPath))
	if !strings.Contains(after, entry) || !strings.Contains(after, "sibling-S") || !strings.Contains(after, "sibling.invalid") {
		t.Fatalf("whole-file recovery did not restore o.path with both E and the sibling S:\n%s", after)
	}
	if strings.Contains(after, strconv.Itoa(port)) {
		t.Errorf("recovery left the hub relay port %d in o.path; want the pre-adopt shape:\n%s", port, after)
	}
	if eEntry, gerr := mimo.GetEntry(entry); gerr != nil {
		t.Fatalf("live mimocode config does not parse after whole-file recovery: %v", gerr)
	} else if eEntry == nil {
		t.Fatalf("target entry %q lost after whole-file recovery", entry)
	}

	// (2) The recovery CREATE fired via the atomic create-if-absent seam
	// (createCount==1); the WriteConfigFile seam saw only the AddEntry attempt
	// (writeCount==1). Non-vacuity: neuter wholeFileRestoreIfWriteTargetGone ⇒ the
	// surgical restore recreates only E in o.path through WriteConfigFile ⇒ sibling S
	// lost + createCount==0 ⇒ assertion (1) breaks.
	if n := seam.writeCount(mimoPath); n != 1 {
		t.Fatalf("unexpected WriteConfigFile count for the mimo top layer (seam writes=%d, want 1 — AddEntry attempt only)", n)
	}
	if n := seam.createCount(mimoPath); n != 1 {
		t.Fatalf("whole-file recovery create did not fire for the mimo top layer (seam creates=%d, want 1)", n)
	}

	// (3) Clean abort.
	assertNoAdoptProvenanceResidue(t, entry)
}

// Preserve variant: the whole-file recovery WRITE itself fails (path #1 recurs) ⇒ the
// error propagates to the Install sentinel ⇒ adopt PRESERVES the provenance (snapshot,
// manifest, keys) and names the restore-unconfirmed client — never a false success.
func TestExecuteAdopt_Path1WholeFileGone_PreservesWhenWholeRestoreFails(t *testing.T) {
	entry := "path1-preserve"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n\n"+
		"[mcp_servers.sibling-S]\n"+
		"url = \"http://sibling.invalid/mcp\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	codexPre := mustReadFileForAdoptTest(t, codexPath)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to assert 'keys preserved'")
	}

	// AddEntry path #1 removes the file; the whole-file recovery CREATE itself hard-fails
	// (a no-replace-create refusal / path #1 recurs) ⇒ the helper maps (false,err) to
	// (true,err) ⇒ the restore is unconfirmed ⇒ codex enters the sentinel ⇒ PRESERVE.
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailRemoved, recover: recoverFail},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want a preserved rollback-incomplete failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if !errors.As(err, &rb) {
		t.Fatalf("adopt error does not wrap the Install sentinel: %T %v", err, err)
	}
	if len(rb.Clients) != 1 || rb.Clients[0] != "codex-cli" {
		t.Fatalf("sentinel Clients = %#v, want [codex-cli]", rb.Clients)
	}
	if !strings.Contains(err.Error(), "PRESERVED") || !strings.Contains(err.Error(), "de-adopt once available") {
		t.Errorf("adopt error missing preserve/recovery wording: %v", err)
	}

	// The recovery CREATE was attempted exactly once and its hard failure drove the
	// sentinel (createCount==1). Neuter the helper (return false,nil) and no create fires
	// ⇒ createCount==0 and the surgical restore would silently lose the sibling.
	if n := seam.createCount(codexPath); n != 1 {
		t.Fatalf("whole-file recovery create was not attempted (seam creates=%d, want 1)", n)
	}

	// PRESERVE: row `adopting`, snapshot dir + every SnapshotRef file (byte-identical to
	// the pre-adopt config), manifest, and the exact routed vault keys all survive.
	assertPreservedProvenance(t, entry, plan, manifestRoot, map[string][]byte{
		filepath.Clean(codexPath): codexPre,
	})
	assertPreservedEvent(t, entry, []string{"codex-cli"})
}

// --- NEW (design round-6, Sol r5 P1): concurrent-recreate CONFLICT during whole-file
//     recovery. path #1 removes the write-target file; between the helper's absence
//     observation and the ATOMIC create-if-absent publish an EXTERNAL non-lock-honoring
//     process recreates the file with a NEW sibling S'. The no-replace create returns
//     EEXIST ⇒ the backup bytes are NOT published ⇒ the body falls through to the
//     surgical restore, which must RE-READ the now-live file and preserve S' while
//     restoring E. A whole-file overwrite (round-5) or a stale-liveMap surgical write
//     would clobber S' (silent data-loss) yet return success — the exact Sol r5 P1. ---

// _Codex proves CHANGE 2: the codex body reads its liveMap ONCE up top, so on the
// EEXIST fall-through it MUST re-read to see S'. Remove the codex re-read and the stale
// (empty) liveMap clobbers S' — the S' assertion below then fails.
func TestExecuteAdopt_Path1WholeFileGone_ConcurrentRecreateConflict_Codex(t *testing.T) {
	entry := "path1-conflict-codex"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to exercise the full adopt path")
	}

	// The external recreate injects a codex-TOML sibling S' (absent from the pre-adopt backup).
	sPrimeTOML := []byte("[mcp_servers.sibling-Sprime]\nurl = \"http://sprime.invalid/mcp\"\n")

	// codex AddEntry publishes the hub relay THEN path #1 REMOVES the whole file; the
	// whole-file recovery create sees the file recreated with S' (EEXIST) ⇒ falls through
	// to the surgical restore, which re-reads {S'} and sets E ⇒ {S', E}. restoreSucceed
	// scripts that surgical write. Surgical reconciled ⇒ no sentinel ⇒ clean abort.
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailRemoved, recover: recoverConflict, restore: restoreSucceed, recoverConflictBytes: sPrimeTOML},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain (conflict-recovered) install failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("surgical fall-through reconciled codex, yet adopt saw a sentinel and PRESERVED: %v", err)
	}

	// (1) Live codex parses AND holds BOTH the externally-recreated sibling S' AND the
	//     pre-adopt entry E (restored to its stdio shape). WITHOUT the codex re-read
	//     (CHANGE 2) the stale empty liveMap clobbers S' — this is the proof assertion.
	codex := clients.AllClients()["codex-cli"]
	if eEntry, gerr := codex.GetEntry(entry); gerr != nil {
		t.Fatalf("live codex config does not parse after conflict recovery: %v", gerr)
	} else if eEntry == nil {
		t.Fatalf("target entry %q lost after conflict recovery", entry)
	}
	if sEntry, gerr := codex.GetEntry("sibling-Sprime"); gerr != nil {
		t.Fatalf("live codex config does not parse after conflict recovery: %v", gerr)
	} else if sEntry == nil {
		t.Fatal("externally-recreated sibling S' was CLOBBERED by the stale liveMap (the codex re-read on the EEXIST fall-through did not run)")
	}
	after := string(mustReadFileForAdoptTest(t, codexPath))
	if strings.Contains(after, strconv.Itoa(port)) {
		t.Errorf("conflict recovery left the hub relay port %d; want the pre-adopt stdio shape for E:\n%s", port, after)
	}
	if !strings.Contains(after, "command") || !strings.Contains(after, "sprime.invalid") {
		t.Errorf("recovered config is not the surgical {S', E} shape:\n%s", after)
	}

	// (2) createCount==1 (recovery create attempted, saw EEXIST) + writeCount==2 (AddEntry
	//     attempt + the surgical fall-through write).
	if n := seam.createCount(codexPath); n != 1 {
		t.Fatalf("whole-file recovery create was not attempted (seam creates=%d, want 1)", n)
	}
	if n := seam.writeCount(codexPath); n != 2 {
		t.Fatalf("surgical fall-through write did not fire (seam writes=%d, want 2 — AddEntry + surgical)", n)
	}

	// (3) Clean abort: no provenance residue (surgical restored E, no false whole
	//     clobber ⇒ snapshot deleted), manifest gone.
	assertNoAdoptProvenanceResidue(t, entry)
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("manifest survived the abort branch: stat err = %v", err)
	}
}

// --- NEW (design round-7, Sol+Terra P1): concurrent-recreate CONFLICT where the
//     freshly-recreated file is UNREADABLE (a transient / partial TOML mid-write).
//     path #1 removes the write-target file; the racing external recreate leaves a
//     PARTIAL S' on disk (an unterminated string) so the codex body's fresh re-read
//     parse-ERRORS. The codex whole-map surgical write must treat that fresh read as
//     AUTHORITATIVE: on read failure it ABORTS with the error → Install rollback
//     sentinel → adopt PRESERVES (and S' is left byte-untouched). The pre-round-7
//     code silently RETAINED the stale (pre-recreate, empty) liveMap, wrote {E} over
//     S', and reported rollback SUCCESS → adopt deleted the provenance = silent
//     sibling data-loss. This test FAILS on that code and PASSES with the fix.
func TestExecuteAdopt_Path1WholeFileGone_ConcurrentRecreate_CodexReadFailPreserves(t *testing.T) {
	entry := "path1-conflict-codex-readfail"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	codexPre := mustReadFileForAdoptTest(t, codexPath)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to assert 'keys preserved'")
	}

	// The racing external recreate leaves a PARTIAL codex-TOML S' on disk: a valid
	// table header followed by an UNTERMINATED basic string (no closing quote), so
	// the codex body's fresh re-read via readTOML() → toml.Unmarshal parse-ERRORS.
	// It stands in for the "transient / partial TOML" fresh-read failure in the
	// round-7 finding.
	sPrimePartialTOML := []byte("[mcp_servers.sibling-Sprime]\nurl = \"http://sprime.invalid/mcp")

	// codex AddEntry publishes the hub relay THEN path #1 REMOVES the whole file; the
	// whole-file recovery create sees the file recreated with the PARTIAL S' (EEXIST)
	// ⇒ falls through to the surgical restore, whose fresh re-read parse-errors. With
	// the fix the restore returns that error (surgical write never fires) ⇒ sentinel ⇒
	// PRESERVE. restore:restoreSucceed is scripted only to prove the pre-fix path
	// (which DOES reach the surgical write with a stale map) would realWrite {E} over
	// S' and report success.
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailRemoved, recover: recoverConflict, restore: restoreSucceed, recoverConflictBytes: sPrimePartialTOML},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want a preserved rollback-incomplete failure")
	}

	// (1) Sentinel present, naming exactly codex-cli (the restore-unconfirmed client).
	//     Pre-round-7 the codex restore reported SUCCESS off the stale map, so NO
	//     sentinel fired — this assertion is the primary non-vacuity proof.
	var rb *InstallClientRollbackIncompleteError
	if !errors.As(err, &rb) {
		t.Fatalf("adopt error does not wrap the Install sentinel (pre-round-7 the stale-map surgical write reports success here): %T %v", err, err)
	}
	if len(rb.Clients) != 1 || rb.Clients[0] != "codex-cli" {
		t.Fatalf("sentinel Clients = %#v, want [codex-cli]", rb.Clients)
	}
	if !strings.Contains(err.Error(), "PRESERVED") || !strings.Contains(err.Error(), "de-adopt once available") {
		t.Errorf("adopt error missing preserve/recovery wording: %v", err)
	}

	// (2) The externally-recreated PARTIAL S' is left BYTE-UNTOUCHED — the codex body
	//     returned the read error BEFORE the surgical write, so it never clobbered S'
	//     with {E}. Pre-round-7 the stale empty liveMap wrote {E} over these bytes.
	after := mustReadFileForAdoptTest(t, codexPath)
	if !bytes.Equal(after, sPrimePartialTOML) {
		t.Fatalf("recreated file was CLOBBERED by a stale-map surgical write (S' not preserved):\n got: %q\nwant: %q", after, sPrimePartialTOML)
	}

	// (3) The recovery CREATE fired once and saw EEXIST (createCount==1); the surgical
	//     WriteConfigFile did NOT fire (writeCount==1 — the AddEntry attempt only),
	//     because the fresh-read failure aborts before the write. Pre-round-7 the
	//     surgical write DOES fire ⇒ writeCount==2.
	if n := seam.createCount(codexPath); n != 1 {
		t.Fatalf("whole-file recovery create was not attempted (seam creates=%d, want 1)", n)
	}
	if n := seam.writeCount(codexPath); n != 1 {
		t.Fatalf("surgical write fired despite the fresh-read failure (seam writes=%d, want 1 — AddEntry attempt only); the fix must abort before the write", n)
	}

	// (4) PRESERVE lifecycle: row `adopting`, snapshot dir + every SnapshotRef file
	//     (byte-identical to the pre-adopt config), manifest, and the exact routed
	//     vault keys all survive.
	assertPreservedProvenance(t, entry, plan, manifestRoot, map[string][]byte{
		filepath.Clean(codexPath): codexPre,
	})
	assertPreservedEvent(t, entry, []string{"codex-cli"})
}

// _JSON proves the member-set fall-through on the same conflict: the JSONC surgical
// writer (setMember → readRawConfig at mutate time) ALWAYS re-reads fresh, so it
// preserves S' on the EXISTING code. This is the regression guard that a future edit
// must not break the member-set path.
func TestExecuteAdopt_Path1WholeFileGone_ConcurrentRecreateConflict_JSON(t *testing.T) {
	entry := "path1-conflict-json"
	codexPath, cursorPath, manifestRoot, _ := seedTwoPresentClients(t, entry)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !containsStr(plan.AdoptClients, "cursor") {
		t.Fatalf("precondition: cursor must be an adopt client; AdoptClients=%#v", plan.AdoptClients)
	}

	// The external recreate injects a cursor-JSON sibling S' (absent from the pre-adopt backup).
	sPrimeJSON := []byte(`{"mcpServers":{"sibling-Sprime":{"url":"http://sprime.invalid/mcp"}}}`)

	// cursor AddEntry publishes the hub relay THEN path #1 REMOVES the whole cursor file;
	// the whole-file recovery create sees the file recreated with S' (EEXIST) ⇒ falls
	// through to the surgical member-set, which re-reads {S'} and patches E ⇒ {S', E}.
	// codex is unspecced (AddEntry succeeds, then rolled back cleanly).
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		cursorPath: {addEntry: addEntryFailRemoved, recover: recoverConflict, restore: restoreSucceed, recoverConflictBytes: sPrimeJSON},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain (conflict-recovered) install failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("surgical member-set reconciled cursor, yet adopt saw a sentinel and PRESERVED: %v", err)
	}

	// (1) Live cursor parses AND holds BOTH the externally-recreated sibling S' AND the
	//     restored entry E.
	cursor := clients.AllClients()["cursor"]
	if eEntry, gerr := cursor.GetEntry(entry); gerr != nil {
		t.Fatalf("live cursor config does not parse after conflict recovery: %v", gerr)
	} else if eEntry == nil {
		t.Fatalf("target entry %q lost after conflict recovery", entry)
	}
	if sEntry, gerr := cursor.GetEntry("sibling-Sprime"); gerr != nil {
		t.Fatalf("live cursor config does not parse after conflict recovery: %v", gerr)
	} else if sEntry == nil {
		t.Fatal("externally-recreated sibling S' was LOST — the member-set fall-through did not re-read the recreated file")
	}
	after := string(mustReadFileForAdoptTest(t, cursorPath))
	if strings.Contains(after, strconv.Itoa(port)) {
		t.Errorf("conflict recovery left the hub relay port %d in cursor:\n%s", port, after)
	}
	if !strings.Contains(after, "sprime.invalid") {
		t.Errorf("recovered cursor config is missing the external sibling S':\n%s", after)
	}

	// (2) createCount==1 (recovery create saw EEXIST) + writeCount==2 (AddEntry attempt +
	//     the surgical member-set fall-through write).
	if n := seam.createCount(cursorPath); n != 1 {
		t.Fatalf("whole-file recovery create was not attempted for cursor (seam creates=%d, want 1)", n)
	}
	if n := seam.writeCount(cursorPath); n != 2 {
		t.Fatalf("surgical member-set fall-through write did not fire for cursor (seam writes=%d, want 2 — AddEntry + surgical)", n)
	}

	// (3) Clean abort: no provenance residue, manifest gone.
	assertNoAdoptProvenanceResidue(t, entry)
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("manifest survived the abort branch: stat err = %v", err)
	}
}

// --- Test 1 (reshaped): Install sentinel present, precisely naming only the
//     restore-unconfirmed client (B FAIL_UNMUTATED+restore SUCCEED does NOT appear)

func TestExecuteInstallTo_SentinelWhenClientRestoreFails(t *testing.T) {
	entry := "preserve-t1"
	codexPath, cursorPath, _, _ := seedTwoPresentClients(t, entry)
	// cursor just needs a valid config lacking the entry so its AddEntry (client B)
	// reaches the seam.
	writeJSONForAdoptTest(t, cursorPath, map[string]any{"mcpServers": map[string]any{}})

	// A (codex, processed first) commits then FAILS its restore => restore-unconfirmed.
	// B (cursor, processed second) FAIL_UNMUTATED triggers rollback but its own
	// restore SUCCEEDS (nothing was mutated) => must NOT appear in the sentinel.
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath:  {addEntry: addEntrySucceed, restore: restoreFail},
		cursorPath: {addEntry: addEntryFailUnmutated, restore: restoreSucceed},
	})

	m := &config.ServerManifest{Name: entry}
	plan := &Plan{ClientUpdates: []ClientUpdatePlan{
		{Client: "codex-cli", URL: "http://127.0.0.1:9310/mcp"}, // A
		{Client: "cursor", URL: "http://127.0.0.1:9310/mcp"},    // B
	}}

	var out bytes.Buffer
	err := executeInstallTo(&out, m, plan, 1, false, nil, true, true)
	if err == nil {
		t.Fatalf("executeInstallTo succeeded; want a client-restore-incomplete failure\n%s", out.String())
	}
	var rb *InstallClientRollbackIncompleteError
	if !errors.As(err, &rb) {
		t.Fatalf("error is not *InstallClientRollbackIncompleteError: %T %v", err, err)
	}
	if len(rb.Clients) != 1 || rb.Clients[0] != "codex-cli" {
		t.Fatalf("sentinel Clients = %#v, want [codex-cli] only (cursor was restored)", rb.Clients)
	}
	if !strings.Contains(err.Error(), "add entry to cursor") {
		t.Errorf("error does not carry the cause (add entry to cursor): %v", err)
	}
	// Non-vacuity: A was left MUTATED (its restore failed), so the codex config still
	// holds the hub-relay URL rather than the original stdio entry.
	after := string(mustReadFileForAdoptTest(t, codexPath))
	if !strings.Contains(after, "9310") {
		t.Errorf("codex config was restored despite induced restore failure (want left-mutated):\n%s", after)
	}
}

// --- Test 2 (reshaped): no sentinel when every restore succeeds; bare err -------

func TestExecuteInstallTo_NoSentinelWhenRestoreSucceeds(t *testing.T) {
	entry := "preserve-t2"
	codexPath, cursorPath, _, _ := seedTwoPresentClients(t, entry)
	writeJSONForAdoptTest(t, cursorPath, map[string]any{"mcpServers": map[string]any{}})

	// A commits then restores cleanly; B FAIL_UNMUTATED triggers rollback + restores
	// cleanly => zero restore failures => bare cause, no sentinel.
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath:  {addEntry: addEntrySucceed, restore: restoreSucceed},
		cursorPath: {addEntry: addEntryFailUnmutated, restore: restoreSucceed},
	})

	m := &config.ServerManifest{Name: entry}
	plan := &Plan{ClientUpdates: []ClientUpdatePlan{
		{Client: "codex-cli", URL: "http://127.0.0.1:9310/mcp"},
		{Client: "cursor", URL: "http://127.0.0.1:9310/mcp"},
	}}

	var out bytes.Buffer
	err := executeInstallTo(&out, m, plan, 1, false, nil, true, true)
	if err == nil {
		t.Fatalf("executeInstallTo succeeded; want the client-B AddEntry failure\n%s", out.String())
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("rollback completed cleanly but error wrapped a sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "add entry to cursor") {
		t.Errorf("error = %v, want the bare add-entry cause", err)
	}
	if strings.Contains(err.Error(), "rollback incomplete") {
		t.Errorf("error carries sentinel framing on the rollback-complete path: %v", err)
	}
	// Non-vacuity: A was RESTORED to its original stdio entry.
	after := string(mustReadFileForAdoptTest(t, codexPath))
	for _, want := range []string{"command", "go", "args", "version"} {
		if !strings.Contains(after, want) {
			t.Errorf("rollback did not restore original codex entry; missing %q:\n%s", want, after)
		}
	}
	if strings.Contains(after, "9310") {
		t.Errorf("rollback left the installed hub URL after clean rollback:\n%s", after)
	}
}

// --- Test 3: no client write ⇒ no sentinel (unchanged) -------------------------

func TestExecuteInstallTo_NoSentinelWhenNoClientWrite(t *testing.T) {
	entry := "preserve-t3"
	setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"version\"]\n")

	m := &config.ServerManifest{Name: entry}
	plan := &Plan{ClientUpdates: nil} // zero client updates — failure is pre-client-loop
	var out bytes.Buffer
	err := executeInstallTo(&out, m, plan, 1, false, func() (func(), error) {
		return nil, errors.New("synthetic pre-client failure")
	}, true, true)
	if err == nil {
		t.Fatalf("executeInstallTo succeeded; want the synthetic intermediate failure\n%s", out.String())
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("no client was ever restored, yet the error wrapped a sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "synthetic pre-client failure") {
		t.Errorf("error = %v, want the bare intermediate failure", err)
	}
}

// --- Test 4 (reshaped): sentinel survives the full a.Install chain --------------

func TestInstall_SentinelSurvivesFullChain(t *testing.T) {
	entry := "preserve-t4"
	codexPath, cursorPath, _, _ := seedTwoPresentClients(t, entry)

	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.AdoptClients) != 2 {
		t.Fatalf("precondition: need 2 adopt clients, got %#v", plan.AdoptClients)
	}
	if err := NewAPI().ManifestCreate(entry, plan.ManifestYAML); err != nil {
		t.Fatalf("ManifestCreate: %v", err)
	}

	// codex (processed first) commits then restore-fails; cursor FAIL_UNMUTATED +
	// restore SUCCEED. Sentinel must survive a.Install → installPlanCore →
	// installPlan → executeInstallTo, naming exactly [codex-cli].
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath:  {addEntry: addEntrySucceed, restore: restoreFail},
		cursorPath: {addEntry: addEntryFailUnmutated, restore: restoreSucceed},
	})
	err = NewAPI().Install(InstallOpts{Server: entry, ClientsInclude: plan.AdoptClients, Writer: io.Discard})
	if err == nil {
		t.Fatal("a.Install succeeded; want a client-restore-incomplete failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if !errors.As(err, &rb) {
		t.Fatalf("sentinel was stripped in the a.Install→installPlanCore→installPlan→executeInstallTo chain: %T %v", err, err)
	}
	if len(rb.Clients) != 1 || rb.Clients[0] != "codex-cli" {
		t.Fatalf("sentinel Clients = %#v, want [codex-cli]", rb.Clients)
	}
}

// --- NEW: multi-client sentinel is the sorted union of the restore-unconfirmed clients

func TestExecuteInstallTo_SentinelUnionOfUnrestoredClients(t *testing.T) {
	entry := "preserve-union"
	codexPath, cursorPath, _, _ := seedTwoPresentClients(t, entry)
	writeJSONForAdoptTest(t, cursorPath, map[string]any{"mcpServers": map[string]any{}})
	home := filepath.Dir(filepath.Dir(codexPath))
	claudePath := filepath.Join(home, ".claude.json")
	writeJSONForAdoptTest(t, claudePath, map[string]any{"mcpServers": map[string]any{}})

	// codex + cursor commit then FAIL restore (both restore-unconfirmed); claude
	// FAIL_UNMUTATED triggers rollback and restores cleanly.
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath:  {addEntry: addEntrySucceed, restore: restoreFail},
		cursorPath: {addEntry: addEntrySucceed, restore: restoreFail},
		claudePath: {addEntry: addEntryFailUnmutated, restore: restoreSucceed},
	})

	m := &config.ServerManifest{Name: entry}
	plan := &Plan{ClientUpdates: []ClientUpdatePlan{
		{Client: "codex-cli", URL: "http://127.0.0.1:9311/mcp"},
		{Client: "cursor", URL: "http://127.0.0.1:9311/mcp"},
		{Client: "claude-code", URL: "http://127.0.0.1:9311/mcp"},
	}}

	var out bytes.Buffer
	err := executeInstallTo(&out, m, plan, 1, false, nil, true, true)
	if err == nil {
		t.Fatalf("executeInstallTo succeeded; want a client-restore-incomplete failure\n%s", out.String())
	}
	var rb *InstallClientRollbackIncompleteError
	if !errors.As(err, &rb) {
		t.Fatalf("error is not *InstallClientRollbackIncompleteError: %T %v", err, err)
	}
	gotSorted := append([]string(nil), rb.Clients...)
	sort.Strings(gotSorted)
	wantUnion := []string{"codex-cli", "cursor"}
	if !reflect.DeepEqual(gotSorted, wantUnion) {
		t.Fatalf("sentinel Clients (sorted) = %#v, want union %#v (claude was restored)", gotSorted, wantUnion)
	}
	// The rendered sentinel error names each restore-unconfirmed client.
	for _, name := range wantUnion {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("sentinel error does not name restore-unconfirmed client %q: %v", name, err)
		}
	}
}

// --- Test 5 (reshaped): adopt PRESERVES + full Sol P2 lifecycle assertions ------

func TestExecuteAdopt_PreservesProvenanceOnRollbackIncomplete(t *testing.T) {
	entry := "preserve-t5"
	codexPath, cursorPath, manifestRoot, _ := seedTwoPresentClients(t, entry)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	// Pre-adopt config bytes per client, captured before adopt mutates them, so the
	// preserved snapshots can be asserted byte-identical (Sol/round-1 P2).
	preAdopt := map[string][]byte{
		filepath.Clean(codexPath):  mustReadFileForAdoptTest(t, codexPath),
		filepath.Clean(cursorPath): mustReadFileForAdoptTest(t, cursorPath),
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to assert 'keys present'")
	}

	// codex commits then FAILS restore (restore-unconfirmed); cursor FAIL_UNMUTATED triggers
	// rollback but restores cleanly (must NOT appear in the sentinel).
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath:  {addEntry: addEntrySucceed, restore: restoreFail},
		cursorPath: {addEntry: addEntryFailUnmutated, restore: restoreSucceed},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want a preserved rollback-incomplete failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if !errors.As(err, &rb) {
		t.Fatalf("adopt error does not wrap the Install sentinel: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "PRESERVED") || !strings.Contains(err.Error(), "de-adopt once available") {
		t.Errorf("adopt error missing operator recovery guidance: %v", err)
	}
	if len(rb.Clients) != 1 || rb.Clients[0] != "codex-cli" {
		t.Fatalf("sentinel Clients = %#v, want [codex-cli] only (cursor restored cleanly)", rb.Clients)
	}
	if !strings.Contains(err.Error(), "codex-cli") {
		t.Errorf("adopt error must name the restore-unconfirmed client: %v", err)
	}

	// PRESERVE lifecycle: row `adopting`, snapshot dir + EVERY SnapshotRef file
	// (byte-identical to its pre-adopt config), manifest present, EXACT routed vault
	// keys present.
	assertPreservedProvenance(t, entry, plan, manifestRoot, preAdopt)
	// Event with EXACT clients/count + redaction.
	assertPreservedEvent(t, entry, []string{"codex-cli"})
	stateDir, _ := DaemonStateDir()
	logPath := filepath.Join(stateDir, SupervisorEventLogFileLeaf)
	if raw, _ := os.ReadFile(logPath); bytes.Contains(raw, []byte("literal-secret-value")) {
		t.Error("preserved event log leaked the secret value")
	}
}

// --- Test 6 (reshaped abort): single client FAIL_UNMUTATED + restore SUCCEED ----

func TestExecuteAdopt_AbortsOnPlainInstallError(t *testing.T) {
	entry := "preserve-t6"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
		"command = \"go\"\n"+
		"args = [\"version\"]\n\n"+
		"[mcp_servers."+entry+".env]\n"+
		"API_KEY = \"literal-secret-value\"\n")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: routed secret needed to assert deleteAdoptRoutedSecrets fired")
	}
	// Single client: AddEntry FAILS UNMUTATED (nothing written) and its restore
	// SUCCEEDS, so Install's rollback is provably complete and returns a PLAIN error
	// (no sentinel) — the ABORT branch must fire unchanged.
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailUnmutated, restore: restoreSucceed},
	})
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want the plain install failure")
	}
	var rb *InstallClientRollbackIncompleteError
	if errors.As(err, &rb) {
		t.Fatalf("no client was mutated, yet adopt saw a sentinel and preserved: %v", err)
	}
	// abortAdoptProvenance fired: row + snapshot gone.
	assertNoAdoptProvenanceResidue(t, entry)
	// ManifestDelete fired.
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Errorf("manifest survived the abort branch: stat err = %v", err)
	}
	// deleteAdoptRoutedSecrets fired.
	vault, verr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if verr != nil {
		t.Fatalf("OpenVault: %v", verr)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Errorf("routed vault keys survived the abort branch: %v", keys)
	}
}

// --- Test 7: pre-Install branches abort unconditionally (unchanged) -------------

func TestExecuteAdopt_PreInstallBranchesAbortUnconditionally(t *testing.T) {
	t.Run("persist-secrets-failure", func(t *testing.T) {
		entry := "preserve-t7-secrets"
		setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\n"+
			"command = \"go\"\n"+
			"args = [\"version\"]\n\n"+
			"[mcp_servers."+entry+".env]\n"+
			"API_KEY = \"literal-secret-value\"\n")
		// NO SecretsInit → persistAdoptRoutedSecrets fails BEFORE Install; the Install
		// sentinel branch is never reached, so this must abort.
		port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
		plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
		if err != nil {
			t.Fatalf("BuildAdoptPlan: %v", err)
		}
		if err := NewAPI().ExecuteAdopt(plan, io.Discard); err == nil {
			t.Fatal("ExecuteAdopt succeeded; want a persist-secrets failure")
		}
		assertNoAdoptProvenanceResidue(t, entry)
	})

	t.Run("manifest-create-failure", func(t *testing.T) {
		entry := "preserve-t7-manifest"
		_, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"version\"]\n")
		port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
		plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port})
		if err != nil {
			t.Fatalf("BuildAdoptPlan: %v", err)
		}
		// Pre-create the manifest so ManifestCreate fails BEFORE Install.
		mdir := filepath.Join(manifestRoot, entry)
		if err := os.MkdirAll(mdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mdir, "manifest.yaml"), []byte("name: "+entry+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewAPI().ExecuteAdopt(plan, io.Discard); err == nil {
			t.Fatal("ExecuteAdopt succeeded; want a manifest-create failure")
		}
		assertNoAdoptProvenanceResidue(t, entry)
	})
}

// --- Test 8 (reshaped): preserve → operator reversal → GC reclaim, incl. the two
//     new GC states; routed keys deferred to de-adopt (GC reaps row+snapshot only) -

func TestExecuteAdopt_PreservedRowReclaimableAfterOperatorReversal(t *testing.T) {
	entry := "preserve-t8"
	codexPath, cursorPath, manifestRoot, _ := seedTwoPresentClients(t, entry)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	// Pre-adopt config bytes per client, captured before adopt mutates them, so both
	// the pinned snapshot AND the post-reversal live config can be asserted byte-exact.
	preAdopt := map[string][]byte{
		filepath.Clean(codexPath):  mustReadFileForAdoptTest(t, codexPath),
		filepath.Clean(cursorPath): mustReadFileForAdoptTest(t, cursorPath),
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 {
		t.Fatal("precondition: a routed secret is needed to prove routed keys are NOT autonomously deleted by the GC")
	}
	// codex commits then FAILS restore (left MUTATED = hub relay); cursor
	// FAIL_UNMUTATED triggers rollback + restores cleanly (byte-frozen to snapshot).
	seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath:  {addEntry: addEntrySucceed, restore: restoreFail},
		cursorPath: {addEntry: addEntryFailUnmutated, restore: restoreSucceed},
	})
	if err := NewAPI().ExecuteAdopt(plan, io.Discard); err == nil {
		t.Fatal("ExecuteAdopt succeeded; want a preserved rollback-incomplete failure")
	}
	rec, found, rerr := ReadAdoptProvenance(entry)
	if rerr != nil || !found {
		t.Fatalf("preserved row missing: found=%v err=%v", found, rerr)
	}

	// Seed one UNRELATED vault key to prove the GC's routed-key cleanup is selective.
	const unrelatedKey = "UNRELATED_SURVIVOR_KEY"
	vault, verr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if verr != nil {
		t.Fatalf("OpenVault: %v", verr)
	}
	if err := vault.Set(unrelatedKey, "keep-me"); err != nil {
		t.Fatalf("seed unrelated vault key: %v", err)
	}

	// Step A — manifest still on disk (Signal 2b committed-keep) ⇒ GC KEEPS the row.
	if reaped, err := gcOrphanedAdoptingProvenance(0); err != nil {
		t.Fatalf("gc(0) #A: %v", err)
	} else if reaped != 0 {
		t.Fatalf("gc(0) #A reaped %d preserved rows; want 0 (manifest present ⇒ committed-keep)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Fatal("preserved row was reaped while the manifest still existed")
	}

	// Step B (NEW GC state) — remove the manifest but leave codex STILL MUTATED. The
	// crash-classifier now sees manifest-absent, but adoptRowProvablyUnmutated is
	// FALSE (codex config drifted from its snapshot) ⇒ GC still KEEPS (reap 0).
	if err := NewAPI().ManifestDelete(entry); err != nil {
		t.Fatalf("ManifestDelete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest not removed: stat err = %v", err)
	}
	if reaped, err := gcOrphanedAdoptingProvenance(0); err != nil {
		t.Fatalf("gc(0) #B: %v", err)
	} else if reaped != 0 {
		t.Fatalf("gc(0) #B reaped %d rows; want 0 (manifest absent + a client still mutated ⇒ KEEP)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); !found {
		t.Fatal("row reaped while a present client was still mutated (unprovable ⇒ must KEEP)")
	}

	// Operator reversal: restore every PRESENT client's live config to its pinned
	// pre-adopt snapshot bytes. os.WriteFile bypasses the install seam.
	stateDir, _ := DaemonStateDir()
	all := clients.AllClients()
	for _, c := range rec.Clients {
		if c.OriginalState != AdoptOriginalStatePresent || c.SnapshotRef == "" {
			continue
		}
		snapBytes, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(c.SnapshotRef)))
		if err != nil {
			t.Fatalf("read snapshot for %s: %v", c.Client, err)
		}
		// Snapshot BYTE assertion: the pinned snapshot is a verbatim copy of the
		// pre-adopt config, so it must equal the bytes captured before adopt ran.
		if want, ok := preAdopt[filepath.Clean(all[c.Client].ConfigPath())]; ok && !bytes.Equal(snapBytes, want) {
			t.Errorf("snapshot for %s not byte-identical to pre-adopt config:\n got: %q\nwant: %q", c.Client, snapBytes, want)
		}
		livePath := all[c.Client].ConfigPath()
		if err := os.WriteFile(livePath, snapBytes, 0o600); err != nil {
			t.Fatalf("restore %s live config to snapshot: %v", c.Client, err)
		}
		// Reversal BYTE assertion: the live config now equals the pinned snapshot.
		liveBytes, rerr := os.ReadFile(livePath)
		if rerr != nil {
			t.Fatalf("re-read %s after reversal: %v", c.Client, rerr)
		}
		if !bytes.Equal(liveBytes, snapBytes) {
			t.Errorf("live config for %s not byte-equal to the snapshot after reversal", c.Client)
		}
	}

	// Step C (NEW GC state) — manifest gone + every present client byte-frozen ⇒ the
	// sha-gate proves a pre-install crash and GC reaps the row + snapshot ONLY (routed
	// keys are left for de-adopt — the GC must not autonomously drop secret material).
	if reaped, err := gcOrphanedAdoptingProvenance(0); err != nil {
		t.Fatalf("gc(0) #C: %v", err)
	} else if reaped != 1 {
		t.Fatalf("gc(0) #C reaped %d rows; want 1 (manifest gone + configs byte-frozen ⇒ crash-reap)", reaped)
	}
	if _, found, _ := ReadAdoptProvenance(entry); found {
		t.Error("preserved row survived the reclaiming GC pass")
	}
	if snapDir, _ := adoptSnapshotDir(entry); snapDir != "" {
		if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
			t.Errorf("snapshot dir survived the reclaiming GC: stat err = %v", err)
		}
	}

	// Routed keys deferred to de-adopt (GC reaps row+snapshot only): a background GC
	// must NEVER autonomously drop secret material a live adopt could still reference
	// (Sol P1 cross-manifest key deletion; Terra P2 crash-safety — design round-3
	// REVERTED the in-GC key deletion). Prove EVERY routed key REMAINS after the
	// reclaim GC, and the unrelated key SURVIVES too.
	vault2, verr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if verr != nil {
		t.Fatalf("OpenVault (after reclaim): %v", verr)
	}
	remaining := vault2.List()
	for _, routed := range plan.SecretRoutedKeys {
		if !containsStr(remaining, routed) {
			t.Errorf("routed key %q was deleted by the reclaiming GC; want it to REMAIN (routed-key cleanup is deferred to de-adopt): %v", routed, remaining)
		}
	}
	if !containsStr(remaining, unrelatedKey) {
		t.Errorf("unrelated vault key %q was deleted by the reclaiming GC; want SURVIVES: %v", unrelatedKey, remaining)
	}
}

func TestExecuteAdopt_ForwardCommittedPreservesPartialPrefixForRecovery(t *testing.T) {
	entry := "adopt-forward-prefix"
	codexPath, cursorPath, manifestRoot, _ := seedTwoPresentClients(t, entry)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	preAdopt := map[string][]byte{
		filepath.Clean(codexPath):  mustReadFileForAdoptTest(t, codexPath),
		filepath.Clean(cursorPath): mustReadFileForAdoptTest(t, cursorPath),
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	opts := AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	}
	plan, err := NewAPI().BuildAdoptPlan(opts)
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) == 0 || len(plan.AdoptClients) != 2 {
		t.Fatalf("forward-prefix precondition failed: keys=%v clients=%v", plan.SecretRoutedKeys, plan.AdoptClients)
	}
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryAppliedReleaseUnconfirmed},
	})
	classifyInstallReleaseForTest(t, inducedAddEntryReleaseUnconfirmed)
	promoteCalls := 0
	previousPromote := promoteAdoptProvenanceFn
	promoteAdoptProvenanceFn = func(string) error {
		promoteCalls++
		return nil
	}
	t.Cleanup(func() { promoteAdoptProvenanceFn = previousPromote })

	var out bytes.Buffer
	err = NewAPI().ExecuteAdopt(plan, &out)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want forward-committed lifecycle failure")
	}
	var forwardCommitted *InstallForwardCommittedError
	if !errors.As(err, &forwardCommitted) || forwardCommitted.Client != "codex-cli" {
		t.Fatalf("adopt forward outcome = %#v err=%v, want codex-cli InstallForwardCommittedError", forwardCommitted, err)
	}
	var rollbackIncomplete *InstallClientRollbackIncompleteError
	if errors.As(err, &rollbackIncomplete) {
		t.Fatalf("forward prefix was falsely reported rollback-incomplete: %v", err)
	}
	for _, want := range []string{"later requested clients may have been skipped", "PRESERVED", "without promoting", "restart the process"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("adopt forward error missing %q: %v", want, err)
		}
	}
	if promoteCalls != 0 || strings.Contains(out.String(), "Adopted ") {
		t.Fatalf("partial plan was promoted as full success: promote=%d output=%q", promoteCalls, out.String())
	}
	if got := seam.writeCount(codexPath); got != 1 {
		t.Fatalf("codex writes=%d, want one applied write and no restore", got)
	}
	if got := seam.writeCount(cursorPath); got != 0 {
		t.Fatalf("skipped cursor was mutated after forward commit: writes=%d", got)
	}
	if raw := mustReadFileForAdoptTest(t, codexPath); !bytes.Contains(raw, []byte(strconv.Itoa(port))) {
		t.Fatalf("applied codex entry lacks adopted port %d: %s", port, raw)
	}
	if raw := mustReadFileForAdoptTest(t, cursorPath); !bytes.Equal(raw, preAdopt[filepath.Clean(cursorPath)]) {
		t.Fatalf("skipped cursor changed:\n got: %q\nwant: %q", raw, preAdopt[filepath.Clean(cursorPath)])
	}
	assertPreservedProvenance(t, entry, plan, manifestRoot, preAdopt)

	rec, found, readErr := ReadAdoptProvenance(entry)
	if readErr != nil || !found {
		t.Fatalf("read preserved provenance: found=%v err=%v", found, readErr)
	}
	if got := classifyDeadAdoptingRow(*rec); got != adoptRowCommittedKeep {
		t.Fatalf("GC classification = %v, want committed-keep", got)
	}
	if _, retryErr := NewAPI().BuildAdoptPlan(opts); retryErr == nil {
		t.Fatal("same-process retry rebuilt a plan over the preserved manifest")
	}
	appliedDisposition, _, _ := mapDeAdoptClientDisposition(
		DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyStillHub)
	skippedDisposition, _, _ := mapDeAdoptClientDisposition(
		DeAdoptRoutingFresh, AdoptOriginalStatePresent, deAdoptSnapshotAvailable, false, clients.ClassifyRestoreDone)
	if appliedDisposition != DeAdoptClientRestorePending || skippedDisposition != DeAdoptClientRestoreDone {
		t.Fatalf("partial-prefix de-adopt dispositions: applied=%s skipped=%s", appliedDisposition, skippedDisposition)
	}
	stateDir, _ := DaemonStateDir()
	if raw, _ := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf)); bytes.Contains(raw, []byte(`"event":"adopt-executed"`)) {
		t.Fatalf("partial plan emitted adopt-executed success event: %s", raw)
	}
}

// --- shared preserve assertions ------------------------------------------------

// assertPreservedProvenance asserts the four preserved artifacts: row `adopting`,
// snapshot dir + EVERY recorded SnapshotRef file (each byte-identical to the KNOWN
// pre-adopt config passed in wantSnapshots, keyed by cleaned client config path),
// manifest on disk, and the EXACT plan.SecretRoutedKeys present in the vault.
func assertPreservedProvenance(t *testing.T, entry string, plan *AdoptPlan, manifestRoot string, wantSnapshots map[string][]byte) {
	t.Helper()
	rec, found, rerr := ReadAdoptProvenance(entry)
	if rerr != nil || !found {
		t.Fatalf("provenance row was deleted by the preserve branch: found=%v err=%v", found, rerr)
	}
	if rec.OperationState != AdoptOperationStateAdopting {
		t.Errorf("operation_state = %q, want adopting (NOT promoted, NOT aborted)", rec.OperationState)
	}
	// Snapshot dir present.
	snapDir, derr := adoptSnapshotDir(entry)
	if derr != nil {
		t.Fatal(derr)
	}
	if _, err := os.Stat(snapDir); err != nil {
		t.Errorf("snapshot dir was deleted by the preserve branch: %v", err)
	}
	// EVERY recorded present-client SnapshotRef FILE exists (not just the dir).
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		t.Fatalf("DaemonStateDir: %v", sdErr)
	}
	sawSnapshot := false
	all := clients.AllClients()
	for _, c := range rec.Clients {
		if c.OriginalState != AdoptOriginalStatePresent {
			continue
		}
		if c.SnapshotRef == "" {
			t.Errorf("present client %q has no SnapshotRef", c.Client)
			continue
		}
		sawSnapshot = true
		snapBytes, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(c.SnapshotRef)))
		if err != nil {
			t.Errorf("SnapshotRef file for %q missing after preserve: %v", c.Client, err)
			continue
		}
		// BYTE assertion: the pinned snapshot is a verbatim copy of the pre-adopt
		// config, so it must equal the KNOWN pre-adopt bytes captured before adopt ran.
		adapter := all[c.Client]
		if adapter == nil {
			t.Errorf("client %q not resolvable via AllClients", c.Client)
			continue
		}
		want, ok := wantSnapshots[filepath.Clean(adapter.ConfigPath())]
		if !ok {
			t.Errorf("no expected pre-adopt bytes provided for present client %q (path %s)", c.Client, adapter.ConfigPath())
			continue
		}
		if !bytes.Equal(snapBytes, want) {
			t.Errorf("snapshot for %q not byte-identical to the pre-adopt config:\n got: %q\nwant: %q", c.Client, snapBytes, want)
		}
	}
	if !sawSnapshot {
		t.Error("no present-client snapshot recorded; test cannot prove snapshot preservation")
	}
	// Manifest on disk.
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); err != nil {
		t.Errorf("manifest was deleted by the preserve branch: %v", err)
	}
	// EXACT routed vault keys present (not merely len>0).
	vault, verr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if verr != nil {
		t.Fatalf("OpenVault: %v", verr)
	}
	got := vault.List()
	want := append([]string(nil), plan.SecretRoutedKeys...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("vault keys after preserve = %#v, want exactly the routed keys %#v", got, want)
	}
}

// assertPreservedEvent asserts an adopt-provenance-preserved event with the EXACT
// restore-unconfirmed client names + count under the `adopt` source.
func assertPreservedEvent(t *testing.T, entry string, wantClients []string) {
	t.Helper()
	stateDir, _ := DaemonStateDir()
	logPath := filepath.Join(stateDir, SupervisorEventLogFileLeaf)
	ev, _ := findSupervisorEventByName(t, logPath, "adopt-provenance-preserved")
	if ev == nil {
		t.Fatal("no adopt-provenance-preserved event emitted")
	}
	if ev["source"] != "adopt" {
		t.Errorf("preserved event source = %v, want adopt", ev["source"])
	}
	body, _ := ev["body"].(map[string]any)
	if body == nil {
		t.Fatalf("preserved event body not an object: %v", ev["body"])
	}
	if body["manifest"] != entry {
		t.Errorf("preserved event manifest = %v, want %q", body["manifest"], entry)
	}
	if cc, _ := body["client_count"].(float64); int(cc) != len(wantClients) {
		t.Errorf("preserved event client_count = %v, want %d", body["client_count"], len(wantClients))
	}
	rawClients, _ := body["clients"].([]any)
	var gotClients []string
	for _, c := range rawClients {
		if s, ok := c.(string); ok {
			gotClients = append(gotClients, s)
		}
	}
	sort.Strings(gotClients)
	want := append([]string(nil), wantClients...)
	sort.Strings(want)
	if !reflect.DeepEqual(gotClients, want) {
		t.Errorf("preserved event clients = %#v, want %#v", gotClients, want)
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
