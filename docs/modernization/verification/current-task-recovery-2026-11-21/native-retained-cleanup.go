//go:build windows

// One authorized cleanup of the original retained U8 fixture. Never create,
// update, enable, run or stop a task. Requires exact retained provenance and
// definition, current SID and disabled state. Assumes no concurrent external
// writes to this isolated name; read-then-delete is not compare-and-delete.
package main

import (
    "bytes"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "encoding/xml"
    "errors"
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
	runID     = "e8b3694319384cb5af9cf33f6b1396ef"
	outputDir = `<RECOVERY_ROOT>\native-r21-e8b3694319384cb5af9cf33f6b1396ef`
	nameUTF8  = `\MCPHub-R21-Native-e8b3694319384cb5af9cf33f6b1396ef-U8`
	nameUTF16 = `\MCPHub-R21-Native-e8b3694319384cb5af9cf33f6b1396ef-U16`
	command   = outputDir + `\NEVER-EXECUTE-e8b3694319384cb5af9cf33f6b1396ef.exe`
	arguments = `--fixture "§ Привет 漢字 🙂" --quoted "\"literal\"" --path "<WORK_ROOT>\path with spaces\value"`
)

const taskRoot = `<RECOVERY_ROOT>`
const expectedManifestSHA256 = "266d268b6e62c69918aaf8e167096a327fa50449ef1446d214876c592b9fb49b"
const expectedEventsSHA256 = "d7ff7695beb9a674de0f977cccd401f086ac7069789d4aee9dcb79467e3925e2"
const expectedExportSHA256 = "5bbf10501bf82e822aa692bc99ac27017249e75fbee34ab59eafa4fb032231ce"
const expectedFailedReceiptSHA256 = "93a6333b7b5c89b970f819f5aa506f659e72c865cf22c5c65c3255b70f8bb487"
const expectedObserverReceiptSHA256 = "c6485bb1479866ad6d7097946578a7fc93db1abf9f2be6273a100c2660ee588a"

type cleanupState struct {
    Attempted bool
    Deleted int
    Absent bool
}

func main() {
    state := &cleanupState{}
    err := cleanup(state)
    result := map[string]any{"final": true, "completed": err == nil, "name": nameUTF8, "delete_attempted": state.Attempted, "deleted_tasks": state.Deleted, "absence_confirmed": state.Absent, "counter_basis": "successful Delete return; absence is observed separately", "assumption": "no concurrent external writes to this isolated original fixture; read-then-delete is not atomic compare-and-delete"}
    if err != nil { result["error"] = err.Error() }
    if err := json.NewEncoder(os.Stdout).Encode(result); err != nil { os.Exit(1) }
    if err != nil { os.Exit(1) }
}

func cleanup(state *cleanupState) (err error) {
    if len(os.Args) != 1 { return errors.New("retained cleanup takes no arguments") }
    log, err := os.OpenFile(filepath.Join(taskRoot, "evidence", "native-retained-cleanup", "ownedevents.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
    if err != nil { return err }
    defer func() { if closeErr := log.Close(); err == nil { err = closeErr } }()
    emit := func(event map[string]any) error {
        event["utc"] = time.Now().UTC().Format(time.RFC3339Nano)
        if err := json.NewEncoder(log).Encode(event); err != nil { return err }
        if err := log.Sync(); err != nil { return err }
        return json.NewEncoder(os.Stdout).Encode(event)
    }
    if err := emit(map[string]any{"operation": "cleanup_intent", "name": nameUTF8, "expected_export_sha256": expectedExportSHA256, "expected_export_bytes": 1702, "manifest_sha256": expectedManifestSHA256, "original_events_sha256": expectedEventsSHA256, "assumption": "No concurrent external writes to this isolated original fixture; no atomic compare-and-delete guarantee."}); err != nil { return err }
    account, err := user.Current()
    if err != nil || !strings.HasPrefix(account.Uid, "S-1-") { return fmt.Errorf("current Windows SID unavailable: %v", err) }
    manifestRaw, err := readPinned(filepath.Join(outputDir, "manifest.json"), 1353, expectedManifestSHA256)
    if err != nil { return err }
    eventsRaw, err := readPinned(filepath.Join(outputDir, "observations.jsonl"), 915, expectedEventsSHA256)
    if err != nil { return err }
    if _, err := readPinned(filepath.Join(taskRoot, "evidence", "native-qualification", "receipt.json"), 113326, expectedFailedReceiptSHA256); err != nil { return err }
    if _, err := readPinned(filepath.Join(taskRoot, "evidence", "native-observer", "receipt.json"), 112742, expectedObserverReceiptSHA256); err != nil { return err }
    if _, err := readPinned(filepath.Join(taskRoot, "diagnostics", "native-observation", "retained-task.xml"), 1702, expectedExportSHA256); err != nil { return err }
    if err := verifyProvenance(manifestRaw, eventsRaw, account.Uid); err != nil { return err }
    if err := emit(map[string]any{"operation": "provenance_verified", "name": nameUTF8, "original_create_acknowledged": true, "failed_receipt_sha256": expectedFailedReceiptSHA256, "observer_receipt_sha256": expectedObserverReceiptSHA256}); err != nil { return err }
    s, err := scheduler.New()
    if err != nil { return err }
    current, readErr := s.ExportXML(nameUTF8)
    status, statusErr := s.Status(nameUTF8)
    definitionErr := verifyDefinition(current, nameUTF8, account.Uid)
    owner, ownerErr := user.Lookup(status.Owner)
    ownerMatches := ownerErr == nil && owner.Uid == account.Uid
    hashMatches := len(current) == 1702 && hash(current) == expectedExportSHA256
    disabled := statusErr == nil && status.RuntimeState == scheduler.TaskRuntimeDisabled
    if readErr != nil || statusErr != nil || definitionErr != nil || !hashMatches || !disabled || !ownerMatches {
        cause := errors.Join(readErr, statusErr, definitionErr, ownerErr, errors.New("retained cleanup withheld: complete definition, principal or disabled state differs"))
        return errors.Join(cause, emit(map[string]any{"operation": "cleanup_retained", "name": nameUTF8, "export_error": errorText(readErr), "status_error": errorText(statusErr), "definition_error": errorText(definitionErr), "owner_lookup_error": errorText(ownerErr), "hash_matches": hashMatches, "observed_export_sha256": hash(current), "observed_export_bytes": len(current), "disabled": disabled, "owner_sid_matches": ownerMatches}))
    }
    if err := emit(map[string]any{"operation": "delete_attempt", "name": nameUTF8, "verified_export_sha256": hash(current), "verified_export_bytes": len(current), "disabled": true, "owner_sid_matches": true}); err != nil { return err }
    state.Attempted = true
    deleteErr := s.Delete(nameUTF8)
    if deleteErr == nil { state.Deleted++ }
    if err := emit(map[string]any{"operation": "delete_result", "name": nameUTF8, "error": errorText(deleteErr), "confirmed_successful_return": deleteErr == nil}); err != nil { return errors.Join(deleteErr, err) }
    if deleteErr != nil { return deleteErr }
    _, absenceErr := s.ExportXML(nameUTF8)
    state.Absent = errors.Is(absenceErr, scheduler.ErrTaskNotFound)
    if err := emit(map[string]any{"operation": "absence_observation", "name": nameUTF8, "absent": state.Absent, "error": errorText(absenceErr)}); err != nil { return err }
    if !state.Absent { return fmt.Errorf("original fixture absence unconfirmed after Delete: %v", absenceErr) }
    return nil
}

func readPinned(path string, size int, expected string) ([]byte, error) {
    raw, err := os.ReadFile(path)
    if err != nil { return nil, err }
    if len(raw) != size || hash(raw) != expected { return nil, fmt.Errorf("retained provenance differs: %s", filepath.Base(path)) }
    return raw, nil
}

func verifyProvenance(manifestRaw, eventsRaw []byte, sid string) error {
    var manifest struct {
        RunID string `json:"run_id"`
        SID string `json:"sid"`
        Fixtures []struct { Name string `json:"name"`; InputSHA256 string `json:"input_sha256"` } `json:"fixtures"`
        GloballyDisabled bool `json:"globally_disabled"`
        Hidden bool `json:"hidden"`
        Triggers string `json:"triggers"`
        ActionPath string `json:"action_path"`
    }
    if err := json.Unmarshal(manifestRaw, &manifest); err != nil { return err }
    inputHash := hash([]byte(taskXML(nameUTF8, sid, runID)))
    if manifest.RunID != runID || manifest.SID != sid || !manifest.GloballyDisabled || !manifest.Hidden || manifest.Triggers != "none" || manifest.ActionPath != command || len(manifest.Fixtures) != 2 || manifest.Fixtures[0].Name != nameUTF8 || manifest.Fixtures[1].Name != nameUTF16 || manifest.Fixtures[0].InputSHA256 != inputHash { return errors.New("original fixture manifest is inconsistent") }
    lines := bytes.Split(bytes.TrimSpace(eventsRaw), []byte{'\n'})
    if len(lines) != 5 { return errors.New("original acknowledged creation event count differs") }
    operations := []string{"preflight", "preflight", "invalid_bytes", "create_attempt", "create_result"}
    for i, line := range lines {
        var event struct { Operation string `json:"operation"`; Name string `json:"name"`; Absent bool `json:"absent"`; Corrupt bool `json:"corrupt"`; Encoding string `json:"encoding"`; InputSHA256 string `json:"input_sha256"`; Error *string `json:"error"`; OutcomeUncertain *bool `json:"outcome_uncertain"` }
        if err := json.Unmarshal(line, &event); err != nil { return err }
        expectedName := nameUTF8
        if i == 1 { expectedName = nameUTF16 }
        if event.Operation != operations[i] || event.Name != expectedName { return errors.New("original creation event identity differs") }
        if i < 2 && !event.Absent || i == 2 && !event.Corrupt || i == 3 && (event.Encoding != "UTF-8 text" || event.InputSHA256 != inputHash) || i == 4 && (event.Error == nil || *event.Error != "" || event.OutcomeUncertain == nil || *event.OutcomeUncertain) { return errors.New("original successful creation is not unambiguously acknowledged") }
    }
    return nil
}

// Original fixture constants and taskXML are retained exactly. Semantic checks
// use the independently reviewed omitted-RunLevel default, with explicit-empty
// rejection. This does not relax the separately pinned complete export hash.
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
