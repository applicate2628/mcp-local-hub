//go:build windows

// Read-only observer of the one retained U8 fixture. Scheduler calls are
// exactly ExportXML and Status. Raw COM export stays in a fresh private folder.
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

const observerDir = `<RECOVERY_ROOT>\diagnostics\native-observation`
const originalHarnessSHA256 = "7691f5c760e0b024eb1d45c145d2f729efac9fd3e54daae29d3cc88fa4913c27"

func main() {
    result := map[string]any{"read_only": true, "name": nameUTF8, "directory": observerDir, "original_harness_sha256": originalHarnessSHA256}
    err := observe(result)
    result["completed"] = err == nil
    if err != nil { result["observer_error"] = err.Error() }
    if err := json.NewEncoder(os.Stdout).Encode(result); err != nil { os.Exit(1) }
    if err != nil { os.Exit(1) }
}

func observe(result map[string]any) error {
    if len(os.Args) != 1 { return errors.New("observer takes no arguments") }
    account, err := user.Current()
    if err != nil || !strings.HasPrefix(account.Uid, "S-1-") { return fmt.Errorf("current Windows SID unavailable: %v", err) }
    manifestRaw, err := os.ReadFile(filepath.Join(outputDir, "manifest.json"))
    if err != nil { return err }
    var manifest struct { RunID string `json:"run_id"`; SID string `json:"sid"` }
    if err := json.Unmarshal(manifestRaw, &manifest); err != nil { return err }
    if manifest.RunID != runID || manifest.SID != account.Uid { return errors.New("original manifest run/current principal differs") }
    result["original_manifest_sha256"] = hash(manifestRaw)
    result["current_principal_matches_manifest"] = true
    result["expected_input_sha256"] = hash([]byte(taskXML(nameUTF8, account.Uid, runID)))
    if err := os.Mkdir(observerDir, 0700); err != nil { return err }
    intent := map[string]any{"name": nameUTF8, "utc": time.Now().UTC().Format(time.RFC3339Nano), "read_only": true, "operations": []string{"ExportXML", "Status"}, "original_harness_sha256": originalHarnessSHA256, "original_manifest_sha256": hash(manifestRaw)}
    intentRaw, err := json.MarshalIndent(intent, "", "  ")
    if err != nil { return err }
    if err := writeNewSynced(filepath.Join(observerDir, "intent.json"), append(intentRaw, '\n')); err != nil { return err }
    s, err := scheduler.New()
    if err != nil { return err }
    raw, exportErr := s.ExportXML(nameUTF8)
    result["export_error"] = errorText(exportErr)
    result["export_bytes"] = len(raw)
    result["export_sha256"] = hash(raw)
    // Preserve exactly the returned bytes, before XML decoding or validation.
    // The untouched declaration may say UTF-16 while COM text is UTF-8 bytes.
    if err := writeNewSynced(filepath.Join(observerDir, "retained-task.xml"), raw); err != nil { return err }
    status, statusErr := s.Status(nameUTF8)
    result["status_error"] = errorText(statusErr)
    result["status_runtime_state"] = status.RuntimeState
    result["status_state"] = status.State
    result["status_disabled"] = statusErr == nil && status.RuntimeState == scheduler.TaskRuntimeDisabled
    result["status_owner_matches_sid"] = statusErr == nil && status.Owner == account.Uid
    result["status_owner_sha256"] = hash([]byte(status.Owner))
    result["status_owner_bytes"] = len(status.Owner)
    result["original_verify_error"] = errorText(verifyDefinition(raw, nameUTF8, account.Uid))
    matches, details, parseErr := compareFields(raw, account.Uid)
    result["field_matches"] = matches
    result["field_details"] = details
    result["parse_error"] = errorText(parseErr)
    result["observed_utc"] = time.Now().UTC().Format(time.RFC3339Nano)
    readErr := errors.Join(exportErr, statusErr, parseErr)
    result["completed"] = readErr == nil
    if readErr != nil { result["observer_error"] = readErr.Error() }
    report, err := json.MarshalIndent(result, "", "  ")
    if err != nil { return err }
    if err := writeNewSynced(filepath.Join(observerDir, "observation.json"), append(report, '\n')); err != nil { return err }
    return readErr
}

func compareFields(raw []byte, sid string) (map[string]bool, map[string]any, error) {
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
			RunLevel  string
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

    if err := decoder.Decode(&task); err != nil { return nil, nil, err }
    matches := map[string]bool{
        "task_element": task.XMLName.Local == "Task",
        "namespace": task.XMLName.Space == "http://schemas.microsoft.com/windows/2004/02/mit/task",
        "uri": task.URI == nameUTF8,
        "source": task.Source == runID,
        "no_triggers": strings.TrimSpace(task.Trigger.Inner) == "",
        "one_principal": len(task.Principal) == 1,
        "one_exec_action": len(task.Actions.Exec) == 1,
        "disabled": task.Settings.Enabled != nil && !*task.Settings.Enabled,
        "hidden": task.Settings.Hidden != nil && *task.Settings.Hidden,
        "demand_disabled": task.Settings.AllowStartOnDemand != nil && !*task.Settings.AllowStartOnDemand,
    }
    details := map[string]any{"principal_count": len(task.Principal), "exec_count": len(task.Actions.Exec)}
    if len(task.Principal) == 1 {
        p := task.Principal[0]
        matches["user_id"] = p.UserID == sid
        matches["principal_id"] = p.ID == "Fixture"
        matches["logon_type"] = p.LogonType == "InteractiveToken"
        matches["run_level"] = p.RunLevel == "LeastPrivilege"
        matches["action_context"] = task.Actions.Context == p.ID
        details["principal_id"] = p.ID
        details["logon_type"] = p.LogonType
        details["run_level"] = p.RunLevel
        details["action_context"] = task.Actions.Context
        details["user_id_bytes"] = len(p.UserID)
        details["user_id_sha256"] = hash([]byte(p.UserID))
    }
    if len(task.Actions.Exec) == 1 {
        action := task.Actions.Exec[0]
        matches["command"] = action.Command == command
        matches["arguments"] = action.Arguments == arguments
        matches["working_directory"] = action.WorkingDirectory == outputDir
        details["arguments_bytes"] = len(action.Arguments)
        details["expected_arguments_bytes"] = len(arguments)
        details["arguments_sha256"] = hash([]byte(action.Arguments))
        details["expected_arguments_sha256"] = hash([]byte(arguments))
        first := -1
        for i := 0; i < len(action.Arguments) || i < len(arguments); i++ {
            if i >= len(action.Arguments) || i >= len(arguments) || action.Arguments[i] != arguments[i] { first = i; break }
        }
        details["arguments_first_different_byte"] = first
    }
    return matches, details, nil
}

// The following helpers and the fixture constants above are copied verbatim
// from the reviewed original harness, whose SHA-256 is pinned above.
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
			RunLevel  string
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
	if p.UserID != sid || p.ID != "Fixture" || p.LogonType != "InteractiveToken" || p.RunLevel != "LeastPrivilege" || task.Actions.Context != p.ID || action.Command != command || action.Arguments != arguments || action.WorkingDirectory != outputDir {
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
