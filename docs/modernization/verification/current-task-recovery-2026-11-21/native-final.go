//go:build windows

// Copy into cmd/r21-native-qualification in the temporary source overlay before
// building. This harness has not been run. Optional cleanup is limited to its
// two confirmed creations and assumes no concurrent external fixture writers;
// it is not an atomic compare-and-delete. All names are durably recorded first.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/scheduler"
)

const (
	runID     = "3a9720bbbd994a5a815f6e093fb38d79"
	outputDir = `<RECOVERY_ROOT>\native-r21-3a9720bbbd994a5a815f6e093fb38d79`
	nameUTF8  = `\MCPHub-R21-Native-3a9720bbbd994a5a815f6e093fb38d79-U8`
	nameUTF16 = `\MCPHub-R21-Native-3a9720bbbd994a5a815f6e093fb38d79-U16`
	command   = outputDir + `\NEVER-EXECUTE-3a9720bbbd994a5a815f6e093fb38d79.exe`
	arguments = `--fixture "§ Привет 漢字 🙂" --quoted "\"literal\"" --path "<WORK_ROOT>\path with spaces\value"`
)

type fixture struct {
	Name        string `json:"name"`
	CaseAlias   string `json:"case_alias"`
	Encoding    string `json:"encoding"`
	InputSHA256 string `json:"input_sha256"`
	input       []byte
}

type runState struct {
	Created int
	Deleted int
}

type ownedFixture struct {
	Name    string
	XMLHash string
}

func main() {
	cleanup := flag.Bool("cleanup-owned", false, "Remove the two verified fixtures after PASS, assuming no concurrent external writers")
	flag.Parse()
	state := &runState{}
	err := run(*cleanup, state)
	result := map[string]any{"final": true, "passed": err == nil, "directory": outputDir, "created_tasks": state.Created, "deleted_tasks": state.Deleted, "cleanup_requested": *cleanup, "cleanup_complete": *cleanup && state.Created == 2 && state.Deleted == 2}
	if err != nil {
		result["error"] = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if err != nil {
		os.Exit(1)
	}
}

func run(cleanup bool, state *runState) error {
	account, err := user.Current()
	if err != nil || !strings.HasPrefix(account.Uid, "S-1-") {
		return fmt.Errorf("current Windows SID unavailable: %v", err)
	}
	if !filepath.IsAbs(outputDir) || !strings.HasPrefix(outputDir, `<WORK_ROOT>\`) {
		return errors.New("qualification output must be beneath <WORK_ROOT>")
	}
	if _, err := os.Stat(command); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("nonexistent action path is not confirmed absent: %v", err)
	}
	if err := os.Mkdir(outputDir, 0700); err != nil {
		return fmt.Errorf("create fresh output directory (never reuse): %w", err)
	}
	fixtures := []fixture{{Name: nameUTF8, CaseAlias: strings.ToLower(nameUTF8), Encoding: "UTF-8 text"}, {Name: nameUTF16, CaseAlias: strings.ToLower(nameUTF16), Encoding: "UTF-16LE with BOM"}}
	for i := range fixtures {
		text := taskXML(fixtures[i].Name, account.Uid, runID)
		fixtures[i].input = []byte(text)
		if i == 1 {
			fixtures[i].input = scheduler.EncodeXMLUTF16LEBOM(text)
		}
		fixtures[i].InputSHA256 = hash(fixtures[i].input)
	}
	manifest := map[string]any{
		"run_id": runID, "created_utc": time.Now().UTC().Format(time.RFC3339Nano),
		"base_commit": "c55d9ca619f6f9301ba1113615370fc6218d608f", "account": account.Username, "sid": account.Uid,
		"fixtures": fixtures, "globally_disabled": true, "hidden": true, "triggers": "none", "action_path": command,
		"cleanup_requested": cleanup, "cleanup": "Only confirmed created fixtures after all checks pass; preserve uncertain or replaced tasks. Assumes no concurrent external writes to these isolated random names; read-then-delete is not atomic compare-and-delete.",
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNewSynced(filepath.Join(outputDir, "manifest.json"), append(rawManifest, '\n')); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(outputDir, "observations.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	emit := func(event map[string]any) error {
		event["utc"] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := json.NewEncoder(log).Encode(event); err != nil {
			return err
		}
		if err := log.Sync(); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(event)
	}
	s, err := scheduler.New()
	if err != nil {
		return err
	}
	creator, ok := s.(scheduler.XMLCreator)
	if !ok {
		return errors.New("scheduler does not implement XMLCreator")
	}
	// Confirm both names first. Native TASK_CREATE still decides occupancy at the
	// write; a successful preflight is never permission to replace an occupant.
	for _, f := range fixtures {
		if _, err := s.ExportXML(f.Name); !errors.Is(err, scheduler.ErrTaskNotFound) {
			return fmt.Errorf("fresh name %s not confirmed absent: %v", f.Name, err)
		}
		if err := emit(map[string]any{"operation": "preflight", "name": f.Name, "absent": true}); err != nil {
			return err
		}
	}
	owned := make([]ownedFixture, 0, 2)
	for i, f := range fixtures {
		invalid := []byte{0xff}
		if i == 1 {
			invalid = []byte{0xff, 0xfe, 0x00, 0xd8}
		}
		err := creator.CreateXMLIfAbsent(f.Name, invalid)
		if err := emit(map[string]any{"operation": "invalid_bytes", "name": f.Name, "error": errorText(err), "corrupt": errors.Is(err, scheduler.ErrTaskCorrupt)}); err != nil {
			return err
		}
		if !errors.Is(err, scheduler.ErrTaskCorrupt) {
			return fmt.Errorf("invalid input was not rejected: %v", err)
		}
		if _, err := s.ExportXML(f.Name); !errors.Is(err, scheduler.ErrTaskNotFound) {
			return fmt.Errorf("task appeared after invalid-input attempt: %v", err)
		}
		if err := emit(map[string]any{"operation": "create_attempt", "name": f.Name, "encoding": f.Encoding, "input_sha256": f.InputSHA256}); err != nil {
			return err
		}
		createErr := creator.CreateXMLIfAbsent(f.Name, f.input)
		if createErr == nil {
			state.Created++
		}
		if err := emit(map[string]any{"operation": "create_result", "name": f.Name, "error": errorText(createErr), "outcome_uncertain": errors.Is(createErr, scheduler.ErrUnavailable)}); err != nil {
			return err
		}
		if createErr != nil {
			return createErr
		}
		before, err := s.ExportXML(f.Name)
		if err != nil {
			return err
		}
		// Retain the returned COM text before semantic checks can fail. The file
		// is UTF-16 with BOM; comparisons still use all original UTF-8 bytes.
		fileBytes := scheduler.EncodeXMLUTF16LEBOM(string(before))
		if err := writeNewSynced(filepath.Join(outputDir, fmt.Sprintf("export-%d.xml", i+1)), fileBytes); err != nil {
			return err
		}
		if err := verifyDefinition(before, f.Name, account.Uid); err != nil {
			return err
		}
		status, err := s.Status(f.Name)
		if err != nil || status.RuntimeState != scheduler.TaskRuntimeDisabled {
			return fmt.Errorf("created task not observed disabled: status=%+v error=%v", status, err)
		}
		if err := emit(map[string]any{"operation": "export_observation", "name": f.Name, "xml_text_sha256": hash(before), "xml_file_sha256": hash(fileBytes), "status": status}); err != nil {
			return err
		}
		owned = append(owned, ownedFixture{Name: f.Name, XMLHash: hash(before)})
		for _, attemptName := range []string{f.Name, f.CaseAlias} {
			conflicting := []byte(taskXML(f.Name, account.Uid, runID+"-CONFLICT"))
			if err := emit(map[string]any{"operation": "conflict_attempt", "name": f.Name, "attempt_name": attemptName, "input_sha256": hash(conflicting)}); err != nil {
				return err
			}
			conflictErr := creator.CreateXMLIfAbsent(attemptName, conflicting)
			after, exportErr := s.ExportXML(f.Name)
			unchanged := exportErr == nil && bytes.Equal(before, after)
			if err := emit(map[string]any{"operation": "conflict", "name": f.Name, "attempt_name": attemptName, "error": errorText(conflictErr), "already_exists": errors.Is(conflictErr, scheduler.ErrTaskAlreadyExists), "full_xml_unchanged": unchanged, "after_sha256": hash(after), "export_error": errorText(exportErr)}); err != nil {
				return err
			}
			if !errors.Is(conflictErr, scheduler.ErrTaskAlreadyExists) || !unchanged {
				return errors.New("occupied name or case alias did not preserve its full exported definition")
			}
		}
	}
	if cleanup {
		if err := cleanupOwned(s, owned, account.Uid, state, emit); err != nil {
			return err
		}
	}
	return emit(map[string]any{"operation": "complete", "passed": true, "created_tasks": state.Created, "deleted_tasks": state.Deleted, "cleanup_requested": cleanup, "manifest": filepath.Join(outputDir, "manifest.json")})
}

// Called only after the entire qualification passes. An initial creation
// conflict/uncertain result never reaches this function. Expected refusal tests
// preserve eligibility only because their complete re-export stayed identical.
// This bounded cleanup assumes no external writer to the isolated fixture names.
// The final read followed by Delete is not atomic compare-and-delete.
func cleanupOwned(s scheduler.Scheduler, owned []ownedFixture, sid string, state *runState, emit func(map[string]any) error) error {
	if len(owned) != 2 {
		return errors.New("cleanup requires both confirmed fixture creations")
	}
	for _, f := range owned {
		if f.Name != nameUTF8 && f.Name != nameUTF16 {
			return errors.New("cleanup name outside fixture manifest")
		}
		current, readErr := s.ExportXML(f.Name)
		status, statusErr := s.Status(f.Name)
		definitionErr := verifyDefinition(current, f.Name, sid)
		owner, ownerErr := user.Lookup(status.Owner)
		ownerMatches := ownerErr == nil && owner.Uid == sid
		if readErr != nil || statusErr != nil || definitionErr != nil || hash(current) != f.XMLHash || status.RuntimeState != scheduler.TaskRuntimeDisabled || !ownerMatches {
			if err := emit(map[string]any{"operation": "cleanup_retained", "name": f.Name, "export_error": errorText(readErr), "status_error": errorText(statusErr), "definition_error": errorText(definitionErr), "hash_matches": hash(current) == f.XMLHash, "status": status, "owner_lookup_error": errorText(ownerErr), "owner_sid_matches": ownerMatches}); err != nil {
				return err
			}
			return errors.New("cleanup withheld: fixture identity, full XML, principal, or disabled state is unconfirmed")
		}
		if err := emit(map[string]any{"operation": "cleanup_attempt", "name": f.Name, "verified_xml_sha256": f.XMLHash, "assumption": "no concurrent external fixture writes; read-then-delete is not atomic compare-and-delete"}); err != nil {
			return err
		}
		deleteErr := s.Delete(f.Name)
		if deleteErr != nil {
			_ = emit(map[string]any{"operation": "cleanup_uncertain", "name": f.Name, "error": deleteErr.Error()})
			return deleteErr
		}
		if _, err := s.ExportXML(f.Name); !errors.Is(err, scheduler.ErrTaskNotFound) {
			_ = emit(map[string]any{"operation": "cleanup_unconfirmed", "name": f.Name, "error": errorText(err)})
			return fmt.Errorf("post-cleanup absence unconfirmed: %v", err)
		}
		state.Deleted++
		if err := emit(map[string]any{"operation": "cleanup_complete", "name": f.Name, "absent": true}); err != nil {
			return err
		}
	}
	return nil
}

func taskXML(name, sid, marker string) string {
	return `<?xml version="1.0" encoding="UTF-16"?><Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><RegistrationInfo><URI>` + escape(name) + `</URI><Source>` + escape(marker) + `</Source><Description>R21 disabled fixture § Привет 漢字 🙂</Description></RegistrationInfo><Triggers/><Principals><Principal id="Fixture"><UserId>` + escape(sid) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><AllowStartOnDemand>false</AllowStartOnDemand><StartWhenAvailable>false</StartWhenAvailable><Enabled>false</Enabled><Hidden>true</Hidden><WakeToRun>false</WakeToRun><ExecutionTimeLimit>PT1M</ExecutionTimeLimit></Settings><Actions Context="Fixture"><Exec><Command>` + escape(command) + `</Command><Arguments>` + escape(arguments) + `</Arguments><WorkingDirectory>` + escape(outputDir) + `</WorkingDirectory></Exec></Actions></Task>`
}

func verifyDefinition(raw []byte, name, sid string) error {
	var task struct {
		XMLName xml.Name
		URI     string `xml:"RegistrationInfo>URI"`
		Source  string `xml:"RegistrationInfo>Source"`
		Trigger struct {
			Inner string `xml:",innerxml"`
		} `xml:"Triggers"`
		Principal []struct {
			ID        string `xml:"id,attr"`
			UserID    string `xml:"UserId"`
			LogonType string
			RunLevel  *string
		} `xml:"Principals>Principal"`
		Settings struct {
			Enabled            *bool
			Hidden             *bool
			AllowStartOnDemand *bool
		}
		Actions struct {
			Context string `xml:"Context,attr"`
			Exec    []struct {
				Command          string
				Arguments        string
				WorkingDirectory string
			}
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	// COM text is already Unicode decoded; only its declaration may say UTF-16.
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(label, "UTF-16") || strings.EqualFold(label, "UTF-16LE") {
			return input, nil
		}
		return nil, fmt.Errorf("unexpected exported charset %q", label)
	}
	if err := decoder.Decode(&task); err != nil {
		return err
	}
	if task.XMLName.Local != "Task" || task.XMLName.Space != "http://schemas.microsoft.com/windows/2004/02/mit/task" || task.URI != name || task.Source != runID || strings.TrimSpace(task.Trigger.Inner) != "" || len(task.Principal) != 1 || len(task.Actions.Exec) != 1 {
		return errors.New("export does not match the fixture identity or structure")
	}
	p, action := task.Principal[0], task.Actions.Exec[0]
	// MS-TSCH SchRpcRegisterTask: an omitted RunLevel defaults to LeastPrivilege.
	// A present empty or other value is rejected; presence is retained by *string.
	if p.UserID != sid || p.ID != "Fixture" || p.LogonType != "InteractiveToken" || (p.RunLevel != nil && *p.RunLevel != "LeastPrivilege") || task.Actions.Context != p.ID || action.Command != command || action.Arguments != arguments || action.WorkingDirectory != outputDir {
		return errors.New("export changed the principal, logon type, action, arguments, or working directory")
	}
	if task.Settings.Enabled == nil || *task.Settings.Enabled || task.Settings.Hidden == nil || !*task.Settings.Hidden || task.Settings.AllowStartOnDemand == nil || *task.Settings.AllowStartOnDemand {
		return errors.New("export did not retain disabled, hidden, demand-disabled settings")
	}
	return nil
}

func writeNewSynced(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func escape(text string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(text))
	return out.String()
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
